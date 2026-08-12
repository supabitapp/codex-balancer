package main

import (
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestDashboardAdaptsPaletteToTerminalBackground(t *testing.T) {
	tests := []struct {
		name       string
		background color.Color
		text       color.RGBA
		accent     color.RGBA
		good       color.RGBA
		warn       color.RGBA
	}{
		{"light", color.White, color.RGBA{0x24, 0x24, 0x24, 0xff}, color.RGBA{0x00, 0x57, 0xb8, 0xff}, color.RGBA{0x18, 0x79, 0x4e, 0xff}, color.RGBA{0x94, 0x68, 0x00, 0xff}},
		{"dark", color.Black, color.RGBA{0xe6, 0xe6, 0xe6, 0xff}, color.RGBA{0x89, 0xb4, 0xfa, 0xff}, color.RGBA{0xa6, 0xe3, 0xa1, 0xff}, color.RGBA{0xf9, 0xe2, 0xaf, 0xff}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model, _ := (dashboard{}).Update(tea.BackgroundColorMsg{Color: test.background})
			styles := model.(dashboard).styles()
			assertColor(t, styles.text.GetForeground(), test.text)
			assertColor(t, styles.title.GetForeground(), test.accent)
			assertColor(t, styles.good.GetForeground(), test.good)
			assertColor(t, styles.warn.GetForeground(), test.warn)
		})
	}
}

func assertColor(t *testing.T, got color.Color, want color.RGBA) {
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
		pool:        &Pool{accounts: []*Account{account}},
		clientIDKey: []byte("secret"),
		snap: Snapshot{Threads: []ThreadSnapshot{{
			Key:         "019fe5c2private",
			ClientIP:    "203.0.113.42",
			Account:     "account-a",
			Model:       "gpt-5.6-sol",
			Effort:      "xhigh",
			ServiceTier: serviceTierFast,
			Turns:       39,
			Compactions: 2,
			Usage:       usage,
			Latency:     5420 * time.Millisecond,
			Last:        time.Now(),
			Via:         transportHTTP,
		}}},
	}

	view := dashboard.threads(220, 8)
	for _, expected := range []string{
		"Thread", "Client", "IP", "Account", "Model", "Via", "Fast", "Uncached", "Cache%", "Output", "Ctx/Cmp", "Latency", "Reqs", "Active",
		"019fe5c2", "52f3c1d8", "203.0.113.42", "account-a@example.com", "gpt-5.6-sol (xhigh)", "HTTP", "FAST", "500", "75", "300", "-- (2)", "5.42s", "39",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("routing missing %q:\n%s", expected, view)
		}
	}
	clientColumn := strings.Index(view, "Client")
	ipColumn := strings.Index(view, "IP")
	accountColumn := strings.Index(view, "Account")
	if clientColumn < 0 || ipColumn < clientColumn || accountColumn < ipColumn {
		t.Fatalf("routing column order is not Client, IP, Account:\n%s", view)
	}
	compact := dashboard.threads(120, 8)
	for _, expected := range []string{"Client", "52f3c1d8", "IP", "203.0.113.42", "Ctx/Cmp", "5.42s", "Reqs", "39", "Active"} {
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
			strings.Index(line, "http"),
			strings.Index(line, "ws turns"),
		}
	}

	narrow := positions(120)
	wide := positions(300)
	if narrow != wide {
		t.Fatalf("totals columns moved from %v to %v when the terminal widened", narrow, wide)
	}
}

func TestTotalsRenderLabelValueTable(t *testing.T) {
	dashboard := dashboard{snap: Snapshot{Turns: 70_600, WSTurns: 65_168, WSOpen: 777}}
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
				Kind:          eventCompactionSwitch,
				Account:       "target-account",
				SourceAccount: "source-account",
				Thread:        "019feea0private",
			},
			{
				At:      time.Date(2026, time.August, 11, 9, 26, 15, 0, time.UTC),
				Kind:    "automatic reset failed",
				Account: "vuonghoainam.work",
				Detail:  "reset credits returned 429 Too Many Requests",
			},
		}},
	}
	lines := strings.Split(dashboard.events(120, 4), "\n")
	header := lines[2]
	row := lines[3]
	for label, value := range map[string]string{
		"Time":    "09:26:15",
		"Event":   "automatic reset failed",
		"Account": "vuonghoa",
		"Detail":  "reset credits returned 429 Too Many Requests",
	} {
		if got, want := textColumn(t, row, value), textColumn(t, header, label); got != want {
			t.Fatalf("%s column starts at %d, want %d", label, got, want)
		}
	}
	if detailColumn := textColumn(t, header, "Detail"); detailColumn != 41 {
		t.Fatalf("detail column starts at %d, want 41", detailColumn)
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
