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
	frame     = 500 * time.Millisecond
	columnGap = 3
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
	cursor int
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
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return d, tea.Quit
		case "up", "k":
			d.cursor = max(d.cursor-1, 0)
		case "down", "j":
			d.cursor = min(d.cursor+1, max(d.pool.count()-1, 0))
		case "space", "enter":
			d.toggle()
		}
	case tickMsg:
		d.cursor = min(d.cursor, max(d.pool.count()-1, 0))
		d.snap = d.stats.snapshot()
		return d, tea.Tick(frame, func(t time.Time) tea.Msg { return tickMsg(t) })
	}
	return d, nil
}

func (d dashboard) toggle() {
	accounts := d.pool.sorted()
	if d.cursor >= len(accounts) {
		return
	}
	account := accounts[d.cursor]
	paused, err := d.pool.togglePause(account)
	if err != nil {
		d.stats.note("save failed", account.id(), err.Error())
		return
	}
	kind := "resumed"
	if paused {
		kind = "paused"
	}
	d.stats.note(kind, account.id(), "")
}

func (d dashboard) View() tea.View {
	v := tea.NewView(d.render())
	v.AltScreen = true
	v.WindowTitle = d.title()
	return v
}

func (d dashboard) title() string {
	total := 0.0
	for _, account := range d.pool.all() {
		if account.paused() {
			continue
		}
		primary, secondary, _, reauth := account.health()
		if reauth != "" {
			continue
		}
		left, known := remainingPercent(longestWindow(primary, secondary))
		if !known {
			return "week --"
		}
		total += left
	}
	return "week " + formatPercent(total)
}

func (d dashboard) render() string {
	if d.width < 80 || d.height < 16 {
		return sDim.Render("terminal too small")
	}
	head := d.header()
	totals := d.totals(d.width)
	bodyHeight := d.height - lipgloss.Height(head) - lipgloss.Height(totals) - 4
	accountLimit := d.pool.count()
	accounts := d.accounts(accountLimit)
	detailHeight := bodyHeight - lipgloss.Height(accounts)
	if detailHeight < 6 && accountLimit > 1 {
		accountLimit = max(accountLimit-(6-detailHeight), 1)
		accounts = d.accounts(accountLimit)
		detailHeight = bodyHeight - lipgloss.Height(accounts)
	}
	if detailHeight < 6 {
		return sDim.Render("terminal too small")
	}
	threadsHeight := detailHeight / 2
	eventsHeight := detailHeight - threadsHeight

	return lipgloss.JoinVertical(lipgloss.Left,
		head,
		"",
		accounts,
		"",
		totals,
		"",
		d.threads(d.width, threadsHeight),
		"",
		d.events(d.width, eventsHeight),
	)
}

func (d dashboard) header() string {
	live, checking, cooling, dead, held := 0, 0, 0, 0, 0
	now := time.Now()
	for _, a := range d.pool.all() {
		switch a.status(now) {
		case accountPaused:
			held++
		case accountNeedsReauth:
			dead++
		case accountCooling:
			cooling++
		case accountChecking:
			checking++
		case accountLive:
			live++
		}
	}

	parts := []string{sGood.Render(fmt.Sprintf("%d live", live))}
	if checking > 0 {
		parts = append(parts, sDim.Render(fmt.Sprintf("%d checking", checking)))
	}
	if cooling > 0 {
		parts = append(parts, sHot.Render(fmt.Sprintf("%d cooling", cooling)))
	}
	if held > 0 {
		parts = append(parts, sDim.Render(fmt.Sprintf("%d paused", held)))
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

func (d dashboard) accounts(limit int) string {
	accounts := d.pool.sorted()
	if len(accounts) == 0 {
		return sSection.Render("ACCOUNTS") + "\n" + sDim.Render("  (none)")
	}
	limit = min(max(limit, 1), len(accounts))
	cursor := min(max(d.cursor, 0), len(accounts)-1)
	start := min(max(cursor-limit+1, 0), len(accounts)-limit)
	end := start + limit

	nameW := 9
	for _, a := range accounts {
		nameW = max(nameW, lipgloss.Width(label(a)))
	}

	gap := 1
	planW := 4
	statusW := 9
	weeklyW := 6
	bankedW := 6
	resetW := 8
	turnsW := 5
	wsW := 2
	trafficW := 7
	limitsW := 6

	fixedCols := planW + statusW + weeklyW + bankedW + resetW + turnsW + wsW + trafficW + limitsW + 10*gap + 2
	nameW = min(nameW, max(9, d.width-fixedCols-8))
	activityW := d.width - fixedCols - nameW

	pad2 := strings.Repeat(" ", gap)

	hdr := "  " + strings.Join([]string{
		sSection.Render(fit("Account", nameW)),
		sSection.Render(fit("Plan", planW)),
		sSection.Render(fit("Status", statusW)),
		sSection.Render(fit("Weekly", weeklyW)),
		sSection.Render(fit("Banked", bankedW)),
		sSection.Render(fit("Reset in", resetW)),
		sSection.Render(fit("Turns", turnsW)),
		sSection.Render(fit("WS", wsW)),
		sSection.Render(fit("Traffic", trafficW)),
		sSection.Render(fit("Limits", limitsW)),
		sSection.Render(fit("Activity", activityW)),
	}, pad2)

	sep := sDim.Render(strings.Repeat("─", d.width-2))

	title := "ACCOUNTS"
	if limit < len(accounts) {
		title = fmt.Sprintf("ACCOUNTS  %d-%d/%d", start+1, end, len(accounts))
	}
	rows := []string{sSection.Render(title) + sDim.Render("   ↑↓ pick · space pauses"), "", hdr, sep}

	for i, a := range accounts[start:end] {
		primary, secondary, _, reauth := a.health()
		weekly := longestWindow(primary, secondary)
		stat := d.snap.Accounts[a.id()]
		now := time.Now()

		var status string
		switch a.status(now) {
		case accountPaused:
			status = sDim.Render(fit("⏸ paused", statusW))
		case accountNeedsReauth:
			status = sBad.Render(fit("✕ "+reauth, statusW))
		case accountCooling:
			status = sHot.Render(fit("◐ cooling", statusW))
		case accountChecking:
			status = sDim.Render(fit("◌ checking", statusW))
		case accountLive:
			status = sGood.Render(fit("● live", statusW))
		}

		turns := ""
		if stat.Turns > 0 {
			turns = fmt.Sprintf("%d", stat.Turns)
		}
		websockets := ""
		if stat.WSOpen > 0 {
			websockets = fmt.Sprintf("%d", stat.WSOpen)
		}
		traffic := ""
		if d.snap.Turns > 0 {
			traffic = fmt.Sprintf("%d%%", stat.Turns*100/d.snap.Turns)
		}
		limits := ""
		if stat.Limited > 0 {
			limits = fmt.Sprintf("%d", stat.Limited)
		}

		var weeklyCell string
		left, known := remainingPercent(weekly)
		if !known {
			weeklyCell = sDim.Render(fit("--", weeklyW))
		} else {
			style := sGood
			switch {
			case left <= 10:
				style = sBad
			case left <= 30:
				style = sWarn
			}
			weeklyCell = style.Render(fit(formatPercent(left), weeklyW))
		}

		banked := sDim.Render(fit("--", bankedW))
		if count, _, known := a.bankedResets(); known {
			banked = sNum.Render(fit(fmt.Sprintf("%d", count), bankedW))
		}

		reset := sDim.Render(fit("--", resetW))
		if next := nextReset(now, primary, secondary); !next.IsZero() {
			reset = sDim.Render(fit(short(next.Sub(now)), resetW))
		}

		marker, name := "  ", sText.Render(fit(label(a), nameW))
		if start+i == cursor {
			marker, name = sTitle.Render("▸ "), sTitle.Render(fit(label(a), nameW))
		}

		rows = append(rows, marker+strings.Join([]string{
			name,
			sDim.Render(fit(a.plan(), planW)),
			status,
			weeklyCell,
			banked,
			reset,
			sNum.Render(fit(turns, turnsW)),
			sGood.Render(fit(websockets, wsW)),
			sDim.Render(fit(traffic, trafficW)),
			sBad.Render(fit(limits, limitsW)),
			fit(spark(stat.Activity), activityW),
		}, pad2))
	}
	return strings.Join(rows, "\n")
}

func nextReset(now time.Time, windows ...window) time.Time {
	var next time.Time
	for _, w := range windows {
		if w.resetsAt.After(now) && (next.IsZero() || w.resetsAt.Before(next)) {
			next = w.resetsAt
		}
	}
	return next
}

func longestWindow(windows ...window) window {
	var longest window
	for _, w := range windows {
		if w.known() && (!longest.known() || w.minutes > longest.minutes) {
			longest = w
		}
	}
	return longest
}

func remainingPercent(w window) (float64, bool) {
	if !w.known() {
		return 0, false
	}
	return min(max(100-w.usedPercent, 0), 100), true
}

func formatPercent(value float64) string {
	formatted := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", value), "0"), ".")
	return formatted + "%"
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
	return sSpark.Render(sparkline(activity))
}

func sparkline(activity []int64) string {
	peak := int64(1)
	for _, v := range activity {
		peak = max(peak, v)
	}
	var b strings.Builder
	for i := len(activity) - 1; i >= 0; i-- {
		b.WriteRune(sparkRunes[activity[i]*int64(len(sparkRunes)-1)/peak])
	}
	return b.String()
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
		tier := strings.Repeat(" ", 5)
		if isFastServiceTier(t.ServiceTier) {
			tier = sHot.Render("FAST ")
		}
		rows = append(rows, fmt.Sprintf("%s %s %s %s %s %s %s",
			sText.Render(pad(shortKey(t.Key), 9)),
			sDim.Render("→"),
			sSpark.Render(pad(names[t.Account], 14)),
			sGood.Render(pad(strings.ToUpper(string(t.Via)), 4)),
			tier,
			sDim.Render(pad(plural(t.Turns, "turn"), 9)),
			sDim.Render(ago(t.Last))))
	}
	if len(rows) == 0 {
		rows = []string{sDim.Render("nothing routed yet")}
	}
	return column(fmt.Sprintf("ROUTING  %d", len(threads)), rows, width, height)
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
	stats := [][]string{
		{
			stat("turns", fmt.Sprintf("%d", s.Turns)),
			stat("http", fmt.Sprintf("%d", s.Turns-s.WSTurns)),
			stat("ws turns", fmt.Sprintf("%d", s.WSTurns)),
		},
		{
			stat("ws open", fmt.Sprintf("%d", s.WSOpen)),
			stat("uptime", short(s.Uptime)),
			stat("input tokens", fmt.Sprintf("%d", s.MonthlyUsage.InputTokens)),
		},
		{
			stat("cached input", fmt.Sprintf("%d", s.MonthlyUsage.InputDetails.CachedTokens)),
			stat("output tokens", fmt.Sprintf("%d", s.MonthlyUsage.OutputTokens)),
			stat("api estimate", formatAPIPrice(s.APICostNanoDollars, s.UnpricedResponses)),
		},
	}
	cellWidth := (width - 2*columnGap) / 3
	rows := make([]string, 0, len(stats))
	for _, statRow := range stats {
		cells := make([]string, 0, len(statRow))
		for _, value := range statRow {
			cells = append(cells, lipgloss.NewStyle().Width(cellWidth).MaxWidth(cellWidth).Render(value))
		}
		rows = append(rows, strings.Join(cells, strings.Repeat(" ", columnGap)))
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
	for _, a := range d.pool.all() {
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

func fit(s string, n int) string {
	return pad(truncate(s, n), n)
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
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func ago(t time.Time) string {
	return agoAt(time.Now(), t)
}

func agoAt(now, then time.Time) string {
	if elapsed := now.Sub(then); elapsed >= time.Second {
		return short(elapsed) + " ago"
	}
	return "now"
}
