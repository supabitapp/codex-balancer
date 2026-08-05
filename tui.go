package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const frame = 500 * time.Millisecond

var (
	cText   = lipgloss.Color("#e6e6e6")
	cDim    = lipgloss.Color("#6c7086")
	cAccent = lipgloss.Color("#89b4fa")
	cGood   = lipgloss.Color("#a6e3a1")
	cWarn   = lipgloss.Color("#f9e2af")
	cHot    = lipgloss.Color("#fab387")
	cBad    = lipgloss.Color("#f38ba8")
	cMuted  = lipgloss.Color("#45475a")

	sTitle   = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sHead    = lipgloss.NewStyle().Foreground(cDim).Bold(true)
	sDim     = lipgloss.NewStyle().Foreground(cDim)
	sText    = lipgloss.NewStyle().Foreground(cText)
	sBad     = lipgloss.NewStyle().Foreground(cBad)
	sGood    = lipgloss.NewStyle().Foreground(cGood)
	sNum     = lipgloss.NewStyle().Foreground(cText).Bold(true)
	sPanel   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cMuted).Padding(0, 2)
	sSection = lipgloss.NewStyle().Foreground(cAccent).Bold(true).MarginBottom(1)
)

type dashboard struct {
	pool   *Pool
	stats  *Stats
	addr   string
	width  int
	height int
	snap   Snapshot
}

type tickMsg time.Time

func (d dashboard) Init() tea.Cmd {
	return tea.Tick(frame, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (d dashboard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		d.width, d.height = msg.Width, msg.Height
	case tea.KeyPressMsg:
		if k := msg.String(); k == "q" || k == "ctrl+c" || k == "esc" {
			return d, tea.Quit
		}
	case tickMsg:
		d.snap = d.stats.snapshot()
		return d, tea.Tick(frame, func(t time.Time) tea.Msg { return tickMsg(t) })
	}
	return d, nil
}

func (d dashboard) View() tea.View {
	v := tea.NewView(d.render())
	v.AltScreen = true
	v.WindowTitle = "codex-balancer"
	return v
}

func (d dashboard) render() string {
	if d.width < 60 {
		return sDim.Render("terminal too narrow")
	}
	body := lipgloss.JoinVertical(lipgloss.Left,
		d.header(),
		"",
		d.accounts(),
		"",
		lipgloss.JoinHorizontal(lipgloss.Top, d.threads(), "  ", d.totals()),
		"",
		d.events(),
	)
	return body
}

const panelChrome = 6

func (d dashboard) header() string {
	inner := d.width - panelChrome
	left := sTitle.Render("CODEX BALANCER")
	right := sDim.Render(fmt.Sprintf("%s   up %s   q to quit", d.addr, short(d.snap.Uptime)))
	gap := max(inner-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return sPanel.Width(d.width).Render(left + strings.Repeat(" ", gap) + right)
}

func (d dashboard) accounts() string {
	rows := []string{sSection.Render("ACCOUNTS")}
	accounts := d.pool.sorted()
	widest := 0
	for _, a := range accounts {
		widest = max(widest, len(label(a)))
	}

	for _, a := range accounts {
		primary, secondary, cooldown, reauth := a.health()
		stat := d.snap.Accounts[a.id()]
		name := sText.Render(pad(label(a), widest))
		plan := sDim.Render(pad(a.plan(), 9))

		var state string
		switch {
		case reauth != "":
			state = sBad.Render("✕ needs reauth")
		case time.Now().Before(cooldown):
			state = lipgloss.NewStyle().Foreground(cHot).Render("◐ cooling " + short(time.Until(cooldown)))
		default:
			state = sGood.Render("● live")
		}

		share := ""
		if d.snap.Turns > 0 {
			share = fmt.Sprintf("  %d%%", stat.Turns*100/d.snap.Turns)
		}
		summary := sDim.Render(fmt.Sprintf("%d turns%s  %s", stat.Turns, share, tokens(stat.InTokens+stat.OutTokens)))

		rows = append(rows,
			fmt.Sprintf("  %s %s %s", name, plan, state),
			fmt.Sprintf("  %s%s", strings.Repeat(" ", widest+1), gauge("5h", primary)),
			fmt.Sprintf("  %s%s", strings.Repeat(" ", widest+1), gauge("7d", secondary)),
			fmt.Sprintf("  %s%s  %s", strings.Repeat(" ", widest+1), spark(stat.Activity), summary),
			"",
		)
	}
	return strings.Join(rows, "\n")
}

func label(a *Account) string {
	if e := a.email(); e != "" {
		return e
	}
	return a.id()
}

func gauge(name string, w window) string {
	if !w.known() {
		return sDim.Render(fmt.Sprintf("%s  %s  no data yet", name, strings.Repeat("░", 24)))
	}
	filled := int(w.usedPercent / 100 * 24)
	filled = min(max(filled, 0), 24)

	style := sGood
	switch {
	case w.usedPercent >= 90:
		style = sBad
	case w.usedPercent >= 70:
		style = lipgloss.NewStyle().Foreground(cWarn)
	}

	bar := style.Render(strings.Repeat("█", filled)) + sDim.Render(strings.Repeat("░", 24-filled))
	tail := ""
	if !w.resetsAt.IsZero() {
		tail = sDim.Render("  resets " + short(time.Until(w.resetsAt)))
	}
	age := ""
	if time.Since(w.seenAt) > time.Minute {
		age = sDim.Render(fmt.Sprintf("  (%s old)", short(time.Since(w.seenAt))))
	}
	return fmt.Sprintf("%s  %s %s%s%s", sDim.Render(name), bar, style.Render(fmt.Sprintf("%3.0f%%", w.usedPercent)), tail, age)
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

func spark(activity []int64) string {
	if len(activity) == 0 {
		return sDim.Render(strings.Repeat("▁", activityLen))
	}
	peak := int64(1)
	for _, v := range activity {
		peak = max(peak, v)
	}
	var b strings.Builder
	for i := len(activity) - 1; i >= 0; i-- {
		idx := int(activity[i] * int64(len(sparkRunes)-1) / peak)
		b.WriteRune(sparkRunes[idx])
	}
	return lipgloss.NewStyle().Foreground(cAccent).Render(b.String())
}

func (d dashboard) threads() string {
	threads := slices.Clone(d.snap.Threads)
	slices.SortFunc(threads, func(x, y ThreadSnapshot) int { return cmp.Compare(y.Last.UnixNano(), x.Last.UnixNano()) })

	rows := []string{sSection.Render("THREADS")}
	if len(threads) == 0 {
		rows = append(rows, sDim.Render("  nothing routed yet"))
	}
	byID := map[string]string{}
	for _, a := range d.pool.sorted() {
		byID[a.id()] = shortName(label(a))
	}
	for i, t := range threads {
		if i >= 8 {
			break
		}
		rows = append(rows, fmt.Sprintf("  %s  %s %s  %s  %s",
			sText.Render(pad(shortKey(t.Key), 10)),
			sDim.Render("→"),
			lipgloss.NewStyle().Foreground(cAccent).Render(pad(byID[t.Account], 12)),
			sDim.Render(pad(fmt.Sprintf("%d turns", t.Turns), 9)),
			sDim.Render(pad(tokens(t.InTokens+t.OutTokens), 7))+sDim.Render(ago(t.Last))))
	}
	return strings.Join(rows, "\n")
}

func (d dashboard) totals() string {
	s := d.snap
	rows := []string{
		sSection.Render("TOTALS"),
		stat("turns", fmt.Sprintf("%d", s.Turns)),
		stat("tokens in", tokens(s.InTokens)),
		stat("tokens out", tokens(s.OutTokens)),
		stat("failovers", fmt.Sprintf("%d", s.Failures)),
		stat("rate limits", fmt.Sprintf("%d", s.Limited)),
		stat("ttfb", short(s.TTFB)),
	}
	return strings.Join(rows, "\n")
}

func stat(name, value string) string {
	return fmt.Sprintf("  %s %s", sDim.Render(pad(name, 12)), sNum.Render(value))
}

func (d dashboard) events() string {
	rows := []string{sSection.Render("EVENTS")}
	events := d.snap.Events
	if len(events) == 0 {
		rows = append(rows, sDim.Render("  quiet"))
	}
	shown := 6
	if len(events) < shown {
		shown = len(events)
	}
	byID := map[string]string{}
	for _, a := range d.pool.sorted() {
		byID[a.id()] = shortName(label(a))
	}
	for _, e := range events[len(events)-shown:] {
		style := sDim
		if e.Kind == "failover" {
			style = sBad
		}
		rows = append(rows, fmt.Sprintf("  %s  %s  %s %s",
			sDim.Render(e.At.Format("15:04:05")),
			style.Render(pad(e.Kind, 14)),
			sText.Render(byID[e.Account]),
			sDim.Render(e.Detail)))
	}
	return strings.Join(rows, "\n")
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func shortName(s string) string {
	if at := strings.IndexByte(s, '@'); at > 0 {
		return s[:at]
	}
	return s
}

func shortKey(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func short(d time.Duration) string {
	switch {
	case d < time.Second:
		return "now"
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	if since := time.Since(t); since >= time.Second {
		return "  " + short(since) + " ago"
	}
	return "  now"
}
