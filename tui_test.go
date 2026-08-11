package main

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
)

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
	column := func(line, text string) int {
		index := strings.Index(line, text)
		if index < 0 {
			t.Fatalf("totals row does not contain %q: %q", text, line)
		}
		return lipgloss.Width(line[:index])
	}

	turnsEnd := column(lines[2], "70600") + len("70600")
	openEnd := column(lines[3], "777") + len("777")
	cachedEnd := column(lines[4], "8.51B") + len("8.51B")
	if turnsEnd != openEnd || openEnd != cachedEnd {
		t.Fatalf("first value column is not right-aligned: %d, %d, %d", turnsEnd, openEnd, cachedEnd)
	}
	labelEnd := column(lines[4], "cached input") + len("cached input")
	if valueStart := column(lines[4], "8.51B"); valueStart <= labelEnd {
		t.Fatalf("cached input label ends at %d but value starts at %d", labelEnd, valueStart)
	}
}
