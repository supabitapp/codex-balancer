package main

import (
	"strings"
	"testing"
	"time"
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
