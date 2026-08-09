package main

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSnapshotIncludesAllActiveThreads(t *testing.T) {
	stats := newStats()
	now := time.Now()
	stats.applyRouted(now.Add(-threadActiveWindow-time.Second), "inactive", "", "account", "", "", transportHTTP)
	for i := range 150 {
		stats.applyRouted(now, fmt.Sprintf("active-%d", i), "", "account", "", "", transportHTTP)
	}

	snapshot := stats.snapshot()
	if len(snapshot.Threads) != 150 {
		t.Fatalf("threads = %d, want 150", len(snapshot.Threads))
	}
	for _, thread := range snapshot.Threads {
		if thread.Key == "inactive" {
			t.Fatal("inactive thread included")
		}
	}
}

func TestThreadUsageFollowsActiveRoutingWindow(t *testing.T) {
	stats := newStats()
	now := time.Now()
	old := now.Add(-threadActiveWindow - time.Second)
	stats.applyRouted(old, "thread", "", "account", "old", "", transportHTTP)
	stats.applyUsageAt(old, "thread", "unknown", "default", responseUsage{InputTokens: 100})
	stats.applyRouted(now, "thread", "", "account", "gpt-5.6-sol", "", transportHTTP)
	routed := stats.snapshot()
	if len(routed.Threads) != 1 || routed.Threads[0].Model != "gpt-5.6-sol" {
		t.Fatalf("routed thread = %+v", routed.Threads)
	}
	usage := responseUsage{InputTokens: 2_000, OutputTokens: 300}
	usage.InputDetails.CachedTokens = 1_500
	stats.applyUsageAt(now, "thread", "unknown", "default", usage)

	snapshot := stats.snapshot()
	if len(snapshot.Threads) != 1 || snapshot.Threads[0].Model != "unknown" || snapshot.Threads[0].Turns != 1 || snapshot.Threads[0].Usage != usage {
		t.Fatalf("threads = %+v, want current routing window usage %+v", snapshot.Threads, usage)
	}
}

func TestMaskEmailHidesLocalPartAndDomain(t *testing.T) {
	for email, want := range map[string]string{
		"khoi@example.com":     "k***i@***.com",
		"khoi@example.net":     "k***i@***.net",
		"khoi@mail.example.uk": "k***i@***.uk",
		"ab@localhost":         "a***@***",
		"a@example.com":        "***@***.com",
		"not-an-email":         "***",
		"":                     "",
	} {
		if got := maskEmail(email); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", email, got, want)
		}
	}
}

func TestRequestIPUsesLastForwardedAddress(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.RemoteAddr = "10.0.0.1:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.7, 203.0.113.9")
	if got := requestIP(request); got != "203.0.113.9" {
		t.Fatalf("requestIP() = %q, want 203.0.113.9", got)
	}
}

func TestRequestIPFallsBackToRemoteAddress(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.RemoteAddr = "[2001:db8::1]:1234"
	if got := requestIP(request); got != "2001:db8::1" {
		t.Fatalf("requestIP() = %q, want 2001:db8::1", got)
	}
}

func TestRequestClientIDHidesIP(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.Header.Set("X-Forwarded-For", "203.0.113.42")
	if got := requestClientID(request, []byte("secret")); got != "52f3c1d8" {
		t.Fatalf("requestClientID() = %q, want 52f3c1d8", got)
	}
	if got := requestClientID(request, nil); got != "" {
		t.Fatalf("requestClientID() without key = %q", got)
	}
}

func TestMonthlyUsageResetsAtMonthBoundary(t *testing.T) {
	stats := newStats()
	previousMonth := time.Date(2026, time.July, 31, 23, 59, 0, 0, time.UTC)
	currentMonth := previousMonth.Add(time.Minute)
	stats.usageMonth = calendarMonth(previousMonth)
	stats.applyUsageAt(previousMonth, "", "unknown", "default", responseUsage{InputTokens: 1_000})
	usage := responseUsage{InputTokens: 2_000, OutputTokens: 300}
	usage.InputDetails.CachedTokens = 1_500
	stats.applyUsageAt(currentMonth, "", "gpt-5.6-sol", "default", usage)
	unpricedUsage := responseUsage{InputTokens: 400, OutputTokens: 50}
	stats.applyUsageAt(currentMonth, "", "unknown", "default", unpricedUsage)
	wantUsage := usage
	wantUsage.InputTokens += unpricedUsage.InputTokens
	wantUsage.OutputTokens += unpricedUsage.OutputTokens
	want, _ := estimateAPIPrice("gpt-5.6-sol", "default", usage)
	if stats.monthlyUsage != wantUsage {
		t.Fatalf("monthly usage = %+v, want %+v", stats.monthlyUsage, wantUsage)
	}
	if stats.apiCostNanoDollars != want || stats.unpricedResponses != 1 {
		t.Fatalf("API estimate = %d with %d unpriced, want %d with one", stats.apiCostNanoDollars, stats.unpricedResponses, want)
	}
}
