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
	stats.applyRouted(now.Add(-threadActiveWindow-time.Second), "inactive", "", "account", "", "", "", transportHTTP, turnMetadata{})
	for i := range 150 {
		stats.applyRouted(now, fmt.Sprintf("active-%d", i), "", "account", "", "", "", transportHTTP, turnMetadata{})
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

func TestSnapshotSortsThreadsByIDAndExpiresAfterFiveMinutes(t *testing.T) {
	stats := newStats()
	now := time.Now()
	stats.applyRouted(now.Add(-5*time.Minute), "019f03", "", "account", "", "", "", transportHTTP, turnMetadata{})
	stats.applyRouted(now.Add(-4*time.Minute), "019f02", "", "account", "", "", "", transportHTTP, turnMetadata{})
	stats.applyRouted(now, "019f01", "", "account", "", "", "", transportHTTP, turnMetadata{})

	snapshot := stats.snapshot()
	if len(snapshot.Threads) != 2 || snapshot.Threads[0].Key != "019f01" || snapshot.Threads[1].Key != "019f02" {
		t.Fatalf("threads = %+v", snapshot.Threads)
	}
}

func TestThreadUsageFollowsActiveRoutingWindow(t *testing.T) {
	stats := newStats()
	now := time.Now()
	old := now.Add(-threadActiveWindow - time.Second)
	stats.applyRouted(old, "thread", "", "account", "old", "medium", "", transportHTTP, turnMetadata{})
	stats.applyUsageAt(old, "thread", "account", "unknown", "default", responseUsage{InputTokens: 100})
	stats.applyRouted(now, "thread", "", "account", "gpt-5.6-sol", "xhigh", "", transportHTTP, turnMetadata{})
	routed := stats.snapshot()
	if len(routed.Threads) != 1 || routed.Threads[0].Model != "gpt-5.6-sol" || routed.Threads[0].Effort != "xhigh" {
		t.Fatalf("routed thread = %+v", routed.Threads)
	}
	usage := responseUsage{InputTokens: 2_000, OutputTokens: 300}
	usage.InputDetails.CachedTokens = 1_500
	stats.applyUsageAt(now, "thread", "account", "unknown", "default", usage)

	snapshot := stats.snapshot()
	if len(snapshot.Threads) != 1 || snapshot.Threads[0].Model != "unknown" || snapshot.Threads[0].Turns != 1 || snapshot.Threads[0].Usage != usage {
		t.Fatalf("threads = %+v, want current routing window usage %+v", snapshot.Threads, usage)
	}
}

func TestThreadRouteSegmentResetsWhenAccountChanges(t *testing.T) {
	stats := newStats()
	now := time.Now()
	metadata := turnMetadata{RequestKind: "compaction", ThreadID: "codex-thread", TurnID: "compact-turn"}
	stats.applyRouted(now, "thread", "client", "account-a", "gpt-5.6-sol", "xhigh", "", transportWebSocket, metadata)
	sourceUsage := responseUsage{InputTokens: 100, OutputTokens: 10}
	sourceUsage.InputDetails.CachedTokens = 90
	stats.applyUsageAt(now.Add(time.Second), "thread", "account-a", "gpt-5.6-sol", "default", sourceUsage)
	stats.applyAnswered(now.Add(time.Second), "thread", "account-a", 100*time.Millisecond)
	stats.applyCompleted(now.Add(time.Second), "thread", "account-a", metadata.RequestKind, time.Second)

	stats.applyRouted(now.Add(2*time.Second), "thread", "client", "account-b", "gpt-5.6-sol", "xhigh", "", transportWebSocket, turnMetadata{RequestKind: "normal", ThreadID: "codex-thread", TurnID: "next-turn"})
	switched := stats.snapshot().Threads[0]
	if switched.Account != "account-b" || switched.Turns != 1 || !switched.Usage.empty() || !switched.LatestUsage.empty() || switched.TTFB != 0 || switched.Latency != 0 || switched.Compactions != 1 {
		t.Fatalf("switched segment = %+v", switched)
	}
	if got := dashboardCacheRate(switched.Usage); got != "--" {
		t.Fatalf("cache rate before target usage = %q, want --", got)
	}

	targetUsage := responseUsage{InputTokens: 100, OutputTokens: 20}
	stats.applyUsageAt(now.Add(3*time.Second), "thread", "account-b", "gpt-5.6-sol", "default", targetUsage)
	stats.applyUsageAt(now.Add(4*time.Second), "thread", "account-a", "gpt-5.6-sol", "default", sourceUsage)
	current := stats.snapshot().Threads[0]
	if current.Usage != targetUsage || current.LatestUsage != targetUsage {
		t.Fatalf("current segment usage = %+v, want %+v", current.Usage, targetUsage)
	}
	if got := dashboardCacheRate(current.Usage); got != "0" {
		t.Fatalf("cache rate after target usage = %q, want 0", got)
	}
}

func TestCodexThreadsKeepSeparateTransportsWithinOneRoute(t *testing.T) {
	stats := newStats()
	now := time.Now()
	httpMetadata := turnMetadata{RequestKind: "turn", ThreadID: "http-thread"}
	webSocketMetadata := turnMetadata{RequestKind: "turn", ThreadID: "ws-thread", SubagentKind: "thread_spawn"}
	stats.applyRouted(now, statsThreadKey("session", httpMetadata), "client", "account", "gpt-5.6-sol", "xhigh", "", transportHTTP, httpMetadata)
	stats.applyRouted(now, statsThreadKey("session", webSocketMetadata), "client", "account", "gpt-5.6-sol", "xhigh", "", transportWebSocket, webSocketMetadata)

	threads := stats.snapshot().Threads
	if len(threads) != 2 || threads[0].Key != "http-thread" || threads[0].Via != transportHTTP || threads[1].Key != "ws-thread" || threads[1].Via != transportWebSocket {
		t.Fatalf("threads = %+v", threads)
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

func TestClientIDForIP(t *testing.T) {
	if got := clientIDForIP("203.0.113.42", []byte("secret")); got != "52f3c1d8" {
		t.Fatalf("clientIDForIP() = %q, want 52f3c1d8", got)
	}
	if got := clientIDForIP("203.0.113.42", nil); got != "" {
		t.Fatalf("clientIDForIP() without key = %q", got)
	}
}

func TestMonthlyUsageResetsAtMonthBoundary(t *testing.T) {
	stats := newStats()
	previousMonth := time.Date(2026, time.July, 31, 23, 59, 0, 0, time.UTC)
	currentMonth := previousMonth.Add(time.Minute)
	stats.usageMonth = calendarMonth(previousMonth)
	stats.applyUsageAt(previousMonth, "", "", "unknown", "default", responseUsage{InputTokens: 1_000})
	usage := responseUsage{InputTokens: 2_000, OutputTokens: 300}
	usage.InputDetails.CachedTokens = 1_500
	stats.applyUsageAt(currentMonth, "", "", "gpt-5.6-sol", "default", usage)
	unpricedUsage := responseUsage{InputTokens: 400, OutputTokens: 50}
	stats.applyUsageAt(currentMonth, "", "", "unknown", "default", unpricedUsage)
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
