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

const (
	frame      = 500 * time.Millisecond
	totalsWide = 24
	columnGap  = 3
)

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
	sDim     = lipgloss.NewStyle().Foreground(cDim)
	sText    = lipgloss.NewStyle().Foreground(cText)
	sBad     = lipgloss.NewStyle().Foreground(cBad)
	sWarn    = lipgloss.NewStyle().Foreground(cWarn)
	sHot     = lipgloss.NewStyle().Foreground(cHot)
	sGood    = lipgloss.NewStyle().Foreground(cGood)
	sNum     = lipgloss.NewStyle().Foreground(cText).Bold(true)
	sSpark   = lipgloss.NewStyle().Foreground(cAccent)
	sPanel   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cMuted).Padding(0, 2)
	sSection = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
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
	if d.width < 80 || d.height < 16 {
		return sDim.Render("terminal too small")
	}
	head := d.header()
	accounts := d.accounts()

	spare := d.width - totalsWide - 2*columnGap
	threadsWide := spare * 9 / 20
	rows := max(d.height-lipgloss.Height(head)-lipgloss.Height(accounts)-3, 5)

	bottom := lipgloss.JoinHorizontal(lipgloss.Top,
		d.threads(threadsWide, rows),
		strings.Repeat(" ", columnGap),
		d.events(spare-threadsWide, rows),
		strings.Repeat(" ", columnGap),
		d.totals(totalsWide),
	)
	return lipgloss.JoinVertical(lipgloss.Left, head, "", accounts, bottom)
}

func (d dashboard) header() string {
	live, cooling, dead := 0, 0, 0
	now := time.Now()
	for _, a := range d.pool.accounts {
		switch _, _, cooldown, reauth := a.health(); {
		case reauth != "":
			dead++
		case now.Before(cooldown):
			cooling++
		default:
			live++
		}
	}

	parts := []string{sGood.Render(fmt.Sprintf("%d live", live))}
	if cooling > 0 {
		parts = append(parts, sHot.Render(fmt.Sprintf("%d cooling", cooling)))
	}
	if dead > 0 {
		parts = append(parts, sBad.Render(fmt.Sprintf("%d need reauth", dead)))
	}

	left := sTitle.Render("CODEX BALANCER") + sDim.Render("   "+strings.Join(parts, sDim.Render(" · ")))
	right := sDim.Render(fmt.Sprintf("%s · %s · up %s · q quits", d.addr, rate(d.snap), short(d.snap.Uptime)))
	gap := max(d.width-6-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return sPanel.Width(d.width).Render(left + strings.Repeat(" ", gap) + right)
}

func rate(s Snapshot) string {
	var recent int64
	for _, a := range s.Accounts {
		for _, v := range a.Activity {
			recent += v
		}
	}
	span := float64(activityLen) * activitySpan.Minutes()
	return fmt.Sprintf("%.1f turns/min", float64(recent)/span)
}

func (d dashboard) accounts() string {
	accounts := d.pool.sorted()
	if len(accounts) == 0 {
		return sSection.Render("ACCOUNTS") + "\n" + sDim.Render("  (none)")
	}

	nameW := 9
	for _, a := range accounts {
		nameW = max(nameW, lipgloss.Width(label(a)))
	}

	gap := 2
	planW := 6
	statusW := 16
	weeklyW := 8
	turnsW := 7
	trafficW := 8
	limitsW := 6

	fixedCols := planW + statusW + weeklyW + turnsW + trafficW + limitsW + 6*gap + 2
	activityW := max(8, d.width-fixedCols-nameW)

	pad2 := strings.Repeat(" ", gap)

	hdr := fmt.Sprintf("  %s%s%s%s%s%s%s%s%s%s%s%s%s%s%s",
		sSection.Render(pad("Account", nameW)), pad2,
		sSection.Render(pad("Plan", planW)), pad2,
		sSection.Render(pad("Status", statusW)), pad2,
		sSection.Render(pad("Weekly", weeklyW)), pad2,
		sSection.Render(pad("Turns", turnsW)), pad2,
		sSection.Render(pad("Traffic", trafficW)), pad2,
		sSection.Render(pad("Limits", limitsW)), pad2,
		sSection.Render(pad("Activity", activityW)))

	sep := sDim.Render(strings.Repeat("─", d.width-2))

	rows := []string{sSection.Render("ACCOUNTS"), "", hdr, sep}

	for _, a := range accounts {
		w, _, cooldown, reauth := a.health()
		stat := d.snap.Accounts[a.id()]

		var status string
		switch {
		case reauth != "":
			status = sBad.Render(pad("✕ "+reauth, statusW))
		case time.Now().Before(cooldown):
			status = sHot.Render(pad("◐ "+short(time.Until(cooldown)), statusW))
		default:
			status = sGood.Render(pad("● live", statusW))
		}

		turns := ""
		if stat.Turns > 0 {
			turns = fmt.Sprintf("%d", stat.Turns)
		}
		traffic := ""
		if d.snap.Turns > 0 {
			traffic = fmt.Sprintf("%d%%", stat.Turns*100/d.snap.Turns)
		}
		limits := ""
		if stat.Limited > 0 {
			limits = fmt.Sprintf("%d", stat.Limited)
		}

		var weekly string
		if !w.known() {
			weekly = sDim.Render(pad("--", weeklyW))
		} else {
			left := int(min(max(100-w.usedPercent, 0), 100))
			style := sGood
			switch {
			case left <= 10:
				style = sBad
			case left <= 30:
				style = sWarn
			}
			weekly = style.Render(pad(fmt.Sprintf("%d%%", left), weeklyW))
		}

		rows = append(rows, fmt.Sprintf("  %s  %s  %s  %s  %s  %s  %s  %s",
			sText.Render(pad(label(a), nameW)),
			sDim.Render(pad(a.plan(), planW)),
			status,
			weekly,
			sNum.Render(pad(turns, turnsW)),
			sDim.Render(pad(traffic, trafficW)),
			sBad.Render(pad(limits, limitsW)),
			spark(stat.Activity)))
	}
	return strings.Join(rows, "\n")
}

func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func label(a *Account) string {
	return cmp.Or(a.email(), a.id())
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

func spark(activity []int64) string {
	peak := int64(1)
	for _, v := range activity {
		peak = max(peak, v)
	}
	var b strings.Builder
	for i := len(activity) - 1; i >= 0; i-- {
		b.WriteRune(sparkRunes[activity[i]*int64(len(sparkRunes)-1)/peak])
	}
	return sSpark.Render(b.String())
}

func column(title string, rows []string, width, limit int) string {
	out := []string{sSection.Render(title), ""}
	for _, row := range rows {
		if len(out) >= limit {
			break
		}
		out = append(out, truncate(row, width))
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(out, "\n"))
}

func (d dashboard) threads(width, height int) string {
	threads := slices.Clone(d.snap.Threads)
	slices.SortFunc(threads, func(x, y ThreadSnapshot) int { return cmp.Compare(y.Last.UnixNano(), x.Last.UnixNano()) })

	names := d.shortNames()
	rows := []string{}
	for _, t := range threads {
		rows = append(rows, fmt.Sprintf("%s %s %s %s %s",
			sText.Render(pad(shortKey(t.Key), 9)),
			sDim.Render("→"),
			sSpark.Render(pad(names[t.Account], 14)),
			sDim.Render(pad(plural(t.Turns, "turn"), 9)),
			sDim.Render(ago(t.Last))))
	}
	if len(rows) == 0 {
		rows = []string{sDim.Render("nothing routed yet")}
	}
	return column(fmt.Sprintf("THREADS  %d", len(threads)), rows, width, height)
}

func (d dashboard) events(width, height int) string {
	names := d.shortNames()
	rows := []string{}
	for i := len(d.snap.Events) - 1; i >= 0; i-- {
		e := d.snap.Events[i]
		style := sDim
		switch e.Kind {
		case "failover":
			style = sBad
		case "rate limited":
			style = sHot
		}
		rows = append(rows, fmt.Sprintf("%s %s %s %s",
			sDim.Render(e.At.Format("15:04:05")),
			style.Render(pad(e.Kind, 13)),
			sText.Render(pad(names[e.Account], 14)),
			sDim.Render(e.Detail)))
	}
	if len(rows) == 0 {
		rows = []string{sDim.Render("quiet")}
	}
	return column("EVENTS", rows, width, height)
}

func (d dashboard) totals(width int) string {
	s := d.snap
	rows := []string{
		stat("turns", fmt.Sprintf("%d", s.Turns)),
		stat("threads", fmt.Sprintf("%d", len(s.Threads))),
		stat("accounts", fmt.Sprintf("%d", len(d.pool.accounts))),
		"",
		stat("failovers", fmt.Sprintf("%d", s.Failures)),
		stat("rate limits", fmt.Sprintf("%d", s.Limited)),
		"",
		stat("ttfb", short(s.TTFB)),
		stat("uptime", short(s.Uptime)),
	}
	return column("TOTALS", rows, width, len(rows)+2)
}

func stat(name, value string) string {
	if name == "" {
		return ""
	}
	return sDim.Render(pad(name, 12)) + sNum.Render(value)
}

func (d dashboard) shortNames() map[string]string {
	out := map[string]string{}
	for _, a := range d.pool.accounts {
		name, _, _ := strings.Cut(label(a), "@")
		out[a.id()] = name
	}
	return out
}

func pad(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func truncate(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(n).Render(s)
}

func shortKey(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
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
	if since := time.Since(t); since >= time.Second {
		return short(since) + " ago"
	}
	return "now"
}
