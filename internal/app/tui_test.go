package app

import (
	"image/color"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestDashboardUsesTerminalPalette(t *testing.T) {
	styles := (dashboard{}).styles()
	for name, style := range map[string]lipgloss.Style{
		"text": styles.text,
		"num":  styles.num,
		"dim":  styles.dim,
	} {
		if got := style.GetForeground(); got != (lipgloss.NoColor{}) {
			t.Fatalf("%s foreground = %v, want terminal default", name, got)
		}
	}
	if !styles.dim.GetFaint() {
		t.Fatal("dim style is not faint")
	}
	assertColor(t, styles.title.GetForeground(), lipgloss.Blue)
	assertColor(t, styles.good.GetForeground(), lipgloss.Green)
	assertColor(t, styles.warn.GetForeground(), lipgloss.Yellow)
	assertColor(t, styles.hot.GetForeground(), lipgloss.Magenta)
	assertColor(t, styles.bad.GetForeground(), lipgloss.Red)
}

func TestDashboardCyclesSelectedAccountRoutingMode(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	account := testAccount("account-a", 20)
	if err := pool.add(account); err != nil {
		t.Fatal(err)
	}
	dashboard := dashboard{pool: pool, stats: newStatsWithPrices(testPriceSnapshot(t)), width: 160}
	press := tea.KeyPressMsg(tea.Key{Text: "r", Code: 'r'})

	for _, want := range []struct {
		mode   routingMode
		status accountStatus
		text   string
	}{
		{routingModePriority, accountPriority, "priority"},
		{routingModeNormal, accountLive, "live"},
	} {
		dashboard.Update(press)
		if got := account.routingCandidate().mode; got != want.mode {
			t.Fatalf("mode = %s, want %s", got, want.mode)
		}
		if got := account.status(time.Now()); got != want.status {
			t.Fatalf("status = %s, want %s", got, want.status)
		}
		if row := dashboard.accounts(1); !strings.Contains(row, want.text) {
			t.Fatalf("account row missing %q:\n%s", want.text, row)
		}
	}
	if row := dashboard.accounts(1); !strings.Contains(row, "r mode") {
		t.Fatalf("account controls missing routing key:\n%s", row)
	}
}

func TestAccountTableShowsRoutedValue(t *testing.T) {
	now := time.Now()
	account := testAccount("account-a", 20)
	account.secondary = window{usedPercent: 20, minutes: 7 * 24 * 60, resetsAt: now.Add(3 * 24 * time.Hour), seenAt: now}
	stats := newStatsWithPrices(testPriceSnapshot(t))
	stats.applyUsageAt(now, "", "account-a", "gpt-5.6-sol", "", "default", responseUsage{InputTokens: 12_345_600})
	dashboard := dashboard{
		pool:  &Pool{accounts: []*Account{account}},
		stats: stats,
		width: 160,
	}

	rendered := dashboard.accounts(1)
	for _, expected := range []string{"Routed value≈", "$49.38"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("account table missing %q:\n%s", expected, rendered)
		}
	}
	if strings.Contains(rendered, "Turns") {
		t.Fatalf("account table still shows Turns:\n%s", rendered)
	}
}

func assertColor(t *testing.T, got, want color.Color) {
	t.Helper()
	gotR, gotG, gotB, gotA := got.RGBA()
	wantR, wantG, wantB, wantA := want.RGBA()
	if gotR != wantR || gotG != wantG || gotB != wantB || gotA != wantA {
		t.Fatalf("color = #%02x%02x%02x, want #%02x%02x%02x", gotR>>8, gotG>>8, gotB>>8, wantR>>8, wantG>>8, wantB>>8)
	}
}

func TestShortOmitsSecondsAfterOneMinute(t *testing.T) {
	for duration, want := range map[time.Duration]string{
		time.Minute:                     "1m",
		time.Minute + time.Second:       "1m",
		13*time.Minute + 33*time.Second: "13m",
		59*time.Minute + 59*time.Second: "59m",
	} {
		if got := short(duration); got != want {
			t.Errorf("short(%s) = %q, want %q", duration, got, want)
		}
	}
}

func TestRoutingShowsFullThreadDetails(t *testing.T) {
	account := testAccount("account-a", 20)
	usage := responseUsage{InputTokens: 2_000, OutputTokens: 300}
	usage.InputDetails.CachedTokens = 1_500
	dashboard := dashboard{
		pool:      &Pool{accounts: []*Account{account}},
		countries: &countryResolver{states: map[string]countryState{"203.0.113.42": {code: "US", ready: true}}},
		snap: Snapshot{Threads: []ThreadSnapshot{{
			Key:                "019fe5c2private",
			ClientIP:           "203.0.113.42",
			APIKeySuffix:       "ret",
			Account:            "account-a",
			Model:              "gpt-5.6-sol",
			Effort:             "xhigh",
			ServiceTier:        serviceTierFast,
			Turns:              39,
			Compactions:        2,
			Usage:              usage,
			apiCostNanoDollars: 24_500_000,
			Latency:            5420 * time.Millisecond,
			Last:               time.Now(),
		}}},
	}

	view := dashboard.threads(220, 8)
	for _, expected := range []string{
		"Thread", "Client", "IP", "Account", "Model", "Fast", "Uncached", "Cache%", "Output", "Used/Cmp", "Latency", "Reqs", "Cost", "Active",
		"019fe5c2", "🇺🇸 ret", "203.0.113.42", "account-a@example.com", "☀️ xhigh", "⚡", "500", "75", "300", "-- (2)", "5.42s", "39", "$0.025",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("routing missing %q:\n%s", expected, view)
		}
	}
	if strings.Contains(view, "FAST") {
		t.Fatalf("routing still shows FAST text:\n%s", view)
	}
	clientColumn := strings.Index(view, "Client")
	ipColumn := strings.Index(view, "IP")
	accountColumn := strings.Index(view, "Account")
	if clientColumn < 0 || ipColumn < clientColumn || accountColumn < ipColumn {
		t.Fatalf("routing column order is not Client, IP, Account:\n%s", view)
	}
	compact := dashboard.threads(120, 8)
	for _, expected := range []string{"Client", "🇺🇸 ret", "IP", "203.0.113.42", "Used/Cmp", "5.42s", "Reqs", "39", "Cost", "$0.025", "Active"} {
		if !strings.Contains(compact, expected) {
			t.Fatalf("compact routing missing %q:\n%s", expected, compact)
		}
	}
}

func TestTotalsColumnsStayCompactOnWideTerminals(t *testing.T) {
	dashboard := dashboard{}
	positions := func(width int) [3]int {
		line := strings.Split(dashboard.totals(width), "\n")[2]
		return [3]int{
			strings.Index(line, "turns"),
			strings.Index(line, "connection retries"),
			strings.Index(line, "rate limits"),
		}
	}

	narrow := positions(120)
	wide := positions(300)
	if narrow != wide {
		t.Fatalf("totals columns moved from %v to %v when the terminal widened", narrow, wide)
	}
}

func TestTotalsRenderLabelValueTable(t *testing.T) {
	dashboard := dashboard{snap: Snapshot{Turns: 70_600, Failures: 12, Limited: 34, WSOpen: 777}}
	dashboard.snap.MonthlyUsage.InputDetails.CachedTokens = 8_510_000_000
	lines := strings.Split(dashboard.totals(120), "\n")

	turnsEnd := textColumn(t, lines[2], "70600") + len("70600")
	openEnd := textColumn(t, lines[3], "777") + len("777")
	cachedEnd := textColumn(t, lines[4], "8.51B") + len("8.51B")
	if turnsEnd != openEnd || openEnd != cachedEnd {
		t.Fatalf("first value column is not right-aligned: %d, %d, %d", turnsEnd, openEnd, cachedEnd)
	}
	labelEnd := textColumn(t, lines[4], "cached input") + len("cached input")
	if valueStart := textColumn(t, lines[4], "8.51B"); valueStart <= labelEnd {
		t.Fatalf("cached input label ends at %d but value starts at %d", labelEnd, valueStart)
	}
}

func TestEventsRenderCompactTable(t *testing.T) {
	dashboard := dashboard{
		pool: &Pool{},
		snap: Snapshot{Events: []Event{
			{
				At:      time.Date(2026, time.August, 11, 9, 26, 15, 0, time.UTC),
				Kind:    "account reset failed",
				Account: "vuonghoainam.work",
				Detail:  "reset credits returned 429 Too Many Requests",
			},
		}},
	}
	rendered := dashboard.events(120, 4)
	lines := strings.Split(rendered, "\n")
	header := lines[2]
	row := lines[3]
	for label, value := range map[string]string{
		"Time":    "09:26:15",
		"Event":   "account reset failed",
		"Account": "vuonghoa",
		"Detail":  "reset credits returned 429 Too Many Requests",
	} {
		if got, want := textColumn(t, row, value), textColumn(t, header, label); got != want {
			t.Fatalf("%s column starts at %d, want %d", label, got, want)
		}
	}
	if detailColumn := textColumn(t, header, "Detail"); detailColumn != 39 {
		t.Fatalf("detail column starts at %d, want 39", detailColumn)
	}
	narrowHeader := strings.Split(dashboard.events(120, 4), "\n")[2]
	wideHeader := strings.Split(dashboard.events(300, 4), "\n")[2]
	for _, label := range []string{"Time", "Event", "Account", "Detail"} {
		if narrow, wide := textColumn(t, narrowHeader, label), textColumn(t, wideHeader, label); narrow != wide {
			t.Fatalf("%s column moved from %d to %d when the terminal widened", label, narrow, wide)
		}
	}
}

func textColumn(t *testing.T, line, text string) int {
	t.Helper()
	index := strings.Index(line, text)
	if index < 0 {
		t.Fatalf("row does not contain %q: %q", text, line)
	}
	return lipgloss.Width(line[:index])
}
