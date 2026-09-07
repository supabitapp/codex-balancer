package app

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	frame     = 500 * time.Millisecond
	columnGap = 3
)

type tuiStyles struct {
	title   lipgloss.Style
	dim     lipgloss.Style
	text    lipgloss.Style
	bad     lipgloss.Style
	warn    lipgloss.Style
	hot     lipgloss.Style
	good    lipgloss.Style
	num     lipgloss.Style
	spark   lipgloss.Style
	panel   lipgloss.Style
	section lipgloss.Style
}

func newTUIStyles() tuiStyles {
	return tuiStyles{
		title:   lipgloss.NewStyle().Foreground(lipgloss.Blue).Bold(true),
		dim:     lipgloss.NewStyle().Faint(true),
		text:    lipgloss.NewStyle(),
		bad:     lipgloss.NewStyle().Foreground(lipgloss.Red),
		warn:    lipgloss.NewStyle().Foreground(lipgloss.Yellow),
		hot:     lipgloss.NewStyle().Foreground(lipgloss.Magenta),
		good:    lipgloss.NewStyle().Foreground(lipgloss.Green),
		num:     lipgloss.NewStyle().Bold(true),
		spark:   lipgloss.NewStyle().Foreground(lipgloss.Blue),
		panel:   lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.BrightBlack).Padding(0, 2),
		section: lipgloss.NewStyle().Foreground(lipgloss.Blue).Bold(true),
	}
}

type dashboard struct {
	pool      *Pool
	catalog   *modelCatalog
	stats     *Stats
	server    *server
	addr      string
	width     int
	height    int
	cursor    int
	snap      Snapshot
	countries *countryResolver
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
		case "r":
			d.cycleRoutingMode()
		}
	case tickMsg:
		d.cursor = min(d.cursor, max(d.pool.count()-1, 0))
		d.snap = d.stats.snapshot()
		d.countries.refresh(d.snap.Threads)
		return d, tea.Tick(frame, func(t time.Time) tea.Msg { return tickMsg(t) })
	}
	return d, nil
}

func (d dashboard) styles() tuiStyles {
	return newTUIStyles()
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
		if d.server != nil {
			d.server.invalidateAccount(account.id(), routingReasonOwnerPaused)
		}
	}
	d.stats.note(kind, account.id(), "")
}

func (d dashboard) cycleRoutingMode() {
	accounts := d.pool.sorted()
	if d.cursor >= len(accounts) {
		return
	}
	account := accounts[d.cursor]
	mode, err := d.pool.cycleRoutingMode(account)
	if err != nil {
		d.stats.note("save failed", account.id(), err.Error())
		return
	}
	d.stats.note("routing mode", account.id(), string(mode))
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
		if account.paused() || managedWorkspacePlan(account.plan()) {
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
	styles := d.styles()
	if d.width < 80 || d.height < 16 {
		return styles.dim.Render("terminal too small")
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
		return styles.dim.Render("terminal too small")
	}
	threadsHeight := min(max(len(d.snap.Threads)+4, detailHeight/2), detailHeight-4)
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
	styles := d.styles()
	live, priority, checking, cooling, dead, held, notRouted := 0, 0, 0, 0, 0, 0, 0
	now := time.Now()
	for _, a := range d.pool.all() {
		switch a.status(now) {
		case accountPaused:
			held++
		case accountNeedsReauth:
			dead++
		case accountNotRouted:
			notRouted++
		case accountCooling:
			cooling++
		case accountChecking:
			checking++
		case accountLive:
			live++
		case accountPriority:
			priority++
		}
	}

	parts := []string{styles.good.Render(fmt.Sprintf("%d live", live))}
	if priority > 0 {
		parts = append(parts, styles.warn.Render(fmt.Sprintf("%d priority", priority)))
	}
	if checking > 0 {
		parts = append(parts, styles.dim.Render(fmt.Sprintf("%d checking", checking)))
	}
	if notRouted > 0 {
		parts = append(parts, styles.dim.Render(fmt.Sprintf("%d not routed", notRouted)))
	}
	if cooling > 0 {
		parts = append(parts, styles.hot.Render(fmt.Sprintf("%d cooling", cooling)))
	}
	if held > 0 {
		parts = append(parts, styles.dim.Render(fmt.Sprintf("%d paused", held)))
	}
	if dead > 0 {
		parts = append(parts, styles.bad.Render(fmt.Sprintf("%d need reauth", dead)))
	}

	left := styles.title.Render("CODEX BALANCER") + styles.dim.Render("   "+strings.Join(parts, styles.dim.Render(" · ")))
	right := styles.dim.Render(fmt.Sprintf("%s · %s · up %s · q quits", d.addr, rate(d.snap), short(d.snap.Uptime)))
	gap := max(d.width-6-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return styles.panel.Width(d.width).Render(left + strings.Repeat(" ", gap) + right)
}

func rate(s Snapshot) string {
	var recent int64
	for _, a := range s.Accounts {
		recent += activityTotal(a.Activity)
	}
	span := float64(activityLen) * activitySpan.Minutes()
	return fmt.Sprintf("%.1f turns/min", float64(recent)/span)
}

func (d dashboard) accounts(limit int) string {
	styles := d.styles()
	accounts := d.pool.sorted()
	if len(accounts) == 0 {
		return styles.section.Render("ACCOUNTS") + "\n" + styles.dim.Render("  (none)")
	}
	limit = min(max(limit, 1), len(accounts))
	cursor := min(max(d.cursor, 0), len(accounts)-1)
	start := min(max(cursor-limit+1, 0), len(accounts)-limit)
	end := start + limit
	var trafficTurns int64
	for _, account := range accounts {
		trafficTurns += activityTotal(d.snap.Accounts[account.id()].Activity)
	}

	nameW := 9
	for _, a := range accounts {
		nameW = max(nameW, lipgloss.Width(label(a)))
	}

	gap := 1
	planW := 4
	statusW := 10
	weeklyW := 6
	bankedW := 6
	resetW := 8
	routedValueW := 22
	wsW := 2
	trafficW := 7
	limitsW := 6

	fixedCols := planW + statusW + weeklyW + bankedW + resetW + routedValueW + wsW + trafficW + limitsW + 10*gap + 2
	nameW = min(nameW, max(9, d.width-fixedCols-8))
	activityW := d.width - fixedCols - nameW

	pad2 := strings.Repeat(" ", gap)

	hdr := "  " + strings.Join([]string{
		styles.section.Render(fit("Account", nameW)),
		styles.section.Render(fit("Plan", planW)),
		styles.section.Render(fit("Status", statusW)),
		styles.section.Render(fit("Weekly", weeklyW)),
		styles.section.Render(fit("Banked", bankedW)),
		styles.section.Render(fit("Reset in", resetW)),
		styles.section.Render(fit("Burnt since last reset", routedValueW)),
		styles.section.Render(fit("WS", wsW)),
		styles.section.Render(fit("Traffic", trafficW)),
		styles.section.Render(fit("Limits", limitsW)),
		styles.section.Render(fit("Activity", activityW)),
	}, pad2)

	sep := styles.dim.Render(strings.Repeat("─", d.width-2))

	title := "ACCOUNTS"
	if limit < len(accounts) {
		title = fmt.Sprintf("ACCOUNTS  %d-%d/%d", start+1, end, len(accounts))
	}
	rows := []string{styles.section.Render(title) + styles.dim.Render("   ↑↓ pick · r mode · space pauses"), "", hdr, sep}

	for i, a := range accounts[start:end] {
		primary, secondary, _, reauth := a.health()
		weekly := longestWindow(primary, secondary)
		stat := d.snap.Accounts[a.id()]
		now := time.Now()

		var status string
		switch a.status(now) {
		case accountPaused:
			status = styles.dim.Render(fit("⏸ paused", statusW))
		case accountNeedsReauth:
			status = styles.bad.Render(fit("✕ "+reauth, statusW))
		case accountNotRouted:
			status = styles.dim.Render(fit("○ not routed", statusW))
		case accountCooling:
			status = styles.hot.Render(fit("◐ cooling", statusW))
		case accountChecking:
			status = styles.dim.Render(fit("◌ checking", statusW))
		case accountLive:
			status = styles.good.Render(fit("● live", statusW))
		case accountPriority:
			status = styles.warn.Render(fit("◆ priority", statusW))
		}

		routedValue := "--"
		routedValueStyle := styles.dim
		if start, known := creditCycleStart(now, primary, secondary); known {
			if routed, _, known := d.stats.routedCreditsSince(a.id(), start); known {
				routedValue = formatCreditValue(routed)
				routedValueStyle = styles.num
			}
		}
		websockets := ""
		if stat.WSOpen > 0 {
			websockets = fmt.Sprintf("%d", stat.WSOpen)
		}
		traffic := ""
		if trafficTurns > 0 {
			traffic = fmt.Sprintf("%d%%", activityTotal(stat.Activity)*100/trafficTurns)
		}
		limits := ""
		if stat.Limited > 0 {
			limits = fmt.Sprintf("%d", stat.Limited)
		}

		var weeklyCell string
		left, known := remainingPercent(weekly)
		if !known {
			weeklyCell = styles.dim.Render(fit("--", weeklyW))
		} else {
			style := styles.good
			switch {
			case left <= 10:
				style = styles.bad
			case left <= 30:
				style = styles.warn
			}
			weeklyCell = style.Render(fit(formatPercent(left), weeklyW))
		}

		banked := styles.dim.Render(fit("--", bankedW))
		if count, _, known := a.bankedResets(); known {
			banked = styles.num.Render(fit(fmt.Sprintf("%d", count), bankedW))
		}

		reset := styles.dim.Render(fit("--", resetW))
		if next := nextReset(now, primary, secondary); !next.IsZero() {
			reset = styles.dim.Render(fit(short(next.Sub(now)), resetW))
		}

		marker, name := "  ", styles.text.Render(fit(label(a), nameW))
		if start+i == cursor {
			marker, name = styles.title.Render("▸ "), styles.title.Render(fit(label(a), nameW))
		}

		rows = append(rows, marker+strings.Join([]string{
			name,
			styles.dim.Render(fit(a.plan(), planW)),
			status,
			weeklyCell,
			banked,
			reset,
			routedValueStyle.Render(fit(routedValue, routedValueW)),
			styles.good.Render(fit(websockets, wsW)),
			styles.dim.Render(fit(traffic, trafficW)),
			styles.bad.Render(fit(limits, limitsW)),
			fit(styles.spark.Render(sparkline(stat.Activity)), activityW),
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

func formatDecimal(value float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", value), "0"), ".")
}

func formatPercent(value float64) string {
	return formatDecimal(value) + "%"
}

func label(a *Account) string {
	return cmp.Or(a.email(), a.id())
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

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

func column(styles tuiStyles, title string, rows []string, width, limit int) string {
	out := []string{styles.section.Render(title), ""}
	for _, row := range rows {
		if len(out) >= limit {
			break
		}
		out = append(out, truncate(row, width))
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(out, "\n"))
}

func (d dashboard) threads(width, height int) string {
	styles := d.styles()
	type routingThreadView struct {
		dashboardThreadView
		clientIP string
	}
	names := d.accountNames()
	now := time.Now()
	views := make([]routingThreadView, 0, len(d.snap.Threads))
	for _, t := range d.snap.Threads {
		name := cmp.Or(names[t.Account], shortKey(t.Account))
		views = append(views, routingThreadView{
			dashboardThreadView: newDashboardThreadView(t, name, dashboardClientName(t, d.countries), d.catalog.contextLimits(t.Account, t.Model), now),
			clientIP:            t.ClientIP,
		})
	}
	if len(views) == 0 {
		return column(styles, "ROUTING  0", []string{styles.dim.Render("no live threads")}, width, height)
	}

	accountWidth := len("Account")
	clientWidth := len("Client")
	modelWidth := len("Model")
	ipWidth := len("IP")
	for _, view := range views {
		accountWidth = max(accountWidth, lipgloss.Width(view.Account))
		clientWidth = max(clientWidth, lipgloss.Width(view.Client))
		modelWidth = max(modelWidth, lipgloss.Width(view.Model))
		ipWidth = max(ipWidth, lipgloss.Width(view.clientIP))
	}
	accountWidth = min(accountWidth, 30)
	clientWidth = min(clientWidth, 16)
	modelWidth = min(modelWidth, 28)
	ipWidth = min(ipWidth, 39)

	type routingColumn struct {
		title string
		width int
		style lipgloss.Style
		value func(routingThreadView) string
	}
	columns := []routingColumn{
		{"Thread", 8, styles.text, func(view routingThreadView) string { return view.Key }},
		{"Client", clientWidth, styles.dim, func(view routingThreadView) string { return view.Client }},
		{"IP", ipWidth, styles.dim, func(view routingThreadView) string { return view.clientIP }},
		{"Account", accountWidth, styles.spark, func(view routingThreadView) string { return view.Account }},
		{"Model", modelWidth, styles.text, func(view routingThreadView) string { return view.Model }},
		{"Fast", 4, styles.warn, func(view routingThreadView) string {
			if view.Fast {
				return "⚡"
			}
			return ""
		}},
		{"Uncached", 8, styles.num, func(view routingThreadView) string { return view.UncachedInput }},
		{"Cache%", 6, styles.dim, func(view routingThreadView) string { return view.CacheRate }},
		{"Output", 7, styles.num, func(view routingThreadView) string { return view.Output }},
		{"Used/Cmp", 8, styles.dim, func(view routingThreadView) string { return view.ContextUsed }},
		{"Latency", 7, styles.dim, func(view routingThreadView) string { return view.Latency }},
		{"Reqs", 4, styles.num, func(view routingThreadView) string { return view.Requests }},
		{"Cost", 8, styles.num, func(view routingThreadView) string { return view.Cost }},
		{"Active", 8, styles.dim, func(view routingThreadView) string { return view.Last }},
	}
	tableWidth := func() int {
		total := len(columns) - 1
		for _, column := range columns {
			total += column.width
		}
		return total
	}
	overflow := max(tableWidth()-width, 0)
	for _, item := range []struct {
		index   int
		minimum int
	}{{3, len("Account")}, {4, len("Model")}, {2, len("IP")}} {
		shrink := min(max(columns[item.index].width-item.minimum, 0), overflow)
		columns[item.index].width -= shrink
		overflow -= shrink
	}

	header := make([]string, 0, len(columns))
	for _, column := range columns {
		header = append(header, styles.section.Render(fit(column.title, column.width)))
	}
	rows := []string{strings.Join(header, " "), styles.dim.Render(strings.Repeat("─", tableWidth()))}
	for _, view := range views {
		cells := make([]string, 0, len(columns))
		for _, column := range columns {
			cells = append(cells, column.style.Render(fit(column.value(view), column.width)))
		}
		rows = append(rows, strings.Join(cells, " "))
	}
	return column(styles, fmt.Sprintf("ROUTING  %d", len(views)), rows, width, height)
}

func (d dashboard) events(width, height int) string {
	styles := d.styles()
	type eventView struct {
		at      string
		kind    string
		account string
		detail  string
		style   lipgloss.Style
	}
	names := d.shortNames()
	limit := max(height-3, 0)
	views := make([]eventView, 0, min(len(d.snap.Events), limit))
	for i := len(d.snap.Events) - 1; i >= 0 && len(views) < limit; i-- {
		e := d.snap.Events[i]
		style := styles.dim
		switch e.Kind {
		case eventFailover:
			style = styles.bad
		case eventRateLimited:
			style = styles.hot
		}
		views = append(views, eventView{e.At.Format("15:04:05"), displayEventKind(e.Kind), eventAccountName(names, e.Account), e.Detail, style})
	}
	timeWidth := len("Time")
	kindWidth := len("Event")
	accountWidth := len("Account")
	detailWidth := len("Detail")
	for _, view := range views {
		timeWidth = max(timeWidth, lipgloss.Width(view.at))
		kindWidth = max(kindWidth, lipgloss.Width(view.kind))
		accountWidth = max(accountWidth, lipgloss.Width(view.account))
		detailWidth = max(detailWidth, lipgloss.Width(view.detail))
	}
	kindWidth = min(kindWidth, 24)
	accountWidth = min(accountWidth, 40)
	overflow := max(timeWidth+kindWidth+accountWidth+detailWidth+3-width, 0)
	for _, column := range []struct {
		width   *int
		minimum int
	}{
		{&detailWidth, len("Detail")},
		{&accountWidth, len("Account")},
		{&kindWidth, len("Event")},
	} {
		shrink := min(max(*column.width-column.minimum, 0), overflow)
		*column.width -= shrink
		overflow -= shrink
	}
	rows := []string{strings.Join([]string{
		styles.section.Render(fit("Time", timeWidth)),
		styles.section.Render(fit("Event", kindWidth)),
		styles.section.Render(fit("Account", accountWidth)),
		styles.section.Render(fit("Detail", detailWidth)),
	}, " ")}
	for _, view := range views {
		rows = append(rows, strings.Join([]string{
			styles.dim.Render(fit(view.at, timeWidth)),
			view.style.Render(fit(view.kind, kindWidth)),
			styles.text.Render(fit(view.account, accountWidth)),
			styles.dim.Render(fit(view.detail, detailWidth)),
		}, " "))
	}
	if len(views) == 0 {
		rows = append(rows, styles.dim.Render("quiet"))
	}
	return column(styles, "EVENTS", rows, width, height)
}

func (d dashboard) totals(width int) string {
	styles := d.styles()
	type total struct {
		label string
		value string
	}
	s := d.snap
	stats := [3][3]total{
		{
			{"turns", fmt.Sprintf("%d", s.Turns)},
			{"connection retries", fmt.Sprintf("%d", s.Failures)},
			{"rate limits", fmt.Sprintf("%d", s.Limited)},
		},
		{
			{"ws open", fmt.Sprintf("%d", s.WSOpen)},
			{"uptime", short(s.Uptime)},
			{"input tokens", formatTokenCount(s.MonthlyUsage.InputTokens)},
		},
		{
			{"cached input", formatTokenCount(s.MonthlyUsage.InputDetails.CachedTokens)},
			{"output tokens", formatTokenCount(s.MonthlyUsage.OutputTokens)},
			{"USD burnt this month", formatAPIPrice(s.APICostNanoDollars, s.UnpricedResponses)},
		},
	}
	labelWidths := [3]int{}
	valueWidths := [3]int{}
	maxPairWidth := (width - (len(labelWidths)-1)*columnGap) / len(labelWidths)
	for _, statRow := range stats {
		for i, stat := range statRow {
			labelWidths[i] = max(labelWidths[i], lipgloss.Width(stat.label))
			valueWidths[i] = max(valueWidths[i], lipgloss.Width(stat.value))
		}
	}
	for i := range valueWidths {
		valueWidths[i] = min(valueWidths[i], max(maxPairWidth-labelWidths[i]-1, 1))
	}
	rows := make([]string, 0, len(stats))
	for _, statRow := range stats {
		pairs := make([]string, 0, len(statRow))
		for i, stat := range statRow {
			label := styles.dim.Width(labelWidths[i]).Render(stat.label)
			value := styles.num.Width(valueWidths[i]).MaxWidth(valueWidths[i]).Align(lipgloss.Right).Render(stat.value)
			pairs = append(pairs, label+" "+value)
		}
		rows = append(rows, strings.Join(pairs, strings.Repeat(" ", columnGap)))
	}
	return column(styles, "TOTALS", rows, width, len(rows)+2)
}

func (d dashboard) shortNames() map[string]string {
	out := d.accountNames()
	for id, label := range out {
		name, _, _ := strings.Cut(label, "@")
		out[id] = name
	}
	return out
}

func (d dashboard) accountNames() map[string]string {
	out := map[string]string{}
	for _, account := range d.pool.all() {
		out[account.id()] = label(account)
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

func agoAt(now, then time.Time) string {
	if elapsed := now.Sub(then); elapsed >= time.Second {
		return short(elapsed) + " ago"
	}
	return "now"
}
