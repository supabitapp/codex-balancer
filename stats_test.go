package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSnapshotIncludesAllActiveThreads(t *testing.T) {
	stats := newStatsWithPrices(priceSnapshot{})
	now := time.Now()
	stats.applyRouted(now, "inactive", "", "account", "", "", "", turnMetadata{})
	for i := range 150 {
		thread := fmt.Sprintf("active-%d", i)
		stats.activateThread(thread)
		stats.applyRouted(now, thread, "", "account", "", "", "", turnMetadata{})
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

func TestStatsEndpointReportsPriorityRoutingMode(t *testing.T) {
	account := testAccount("account-a", 20)
	account.RoutingMode = routingModePriority
	server := &server{pool: &Pool{accounts: []*Account{account}}, stats: newStatsWithPrices(priceSnapshot{})}
	request := httptest.NewRequest(http.MethodGet, "/stats", nil)
	response := httptest.NewRecorder()

	server.routes().ServeHTTP(response, request)
	var payload statsResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(payload.Accounts) != 1 || payload.Accounts[0].Status != accountPriority || payload.Accounts[0].RoutingMode != routingModePriority {
		t.Fatalf("status = %d, payload = %+v", response.Code, payload)
	}
}

func TestSnapshotKeepsThreadsUntilTheirLastLiveReferenceCloses(t *testing.T) {
	stats := newStatsWithPrices(priceSnapshot{})
	now := time.Now()
	stats.activateThread("019f02")
	stats.activateThread("019f02")
	stats.activateThread("019f01")
	stats.applyRouted(now.Add(-24*time.Hour), "019f02", "", "account", "", "", "", turnMetadata{})
	stats.applyRouted(now, "019f01", "", "account", "", "", "", turnMetadata{})

	snapshot := stats.snapshot()
	if len(snapshot.Threads) != 2 || snapshot.Threads[0].Key != "019f01" || snapshot.Threads[1].Key != "019f02" {
		t.Fatalf("threads = %+v", snapshot.Threads)
	}
	stats.deactivateThread("019f01")
	stats.deactivateThread("019f02")
	if threads := stats.snapshot().Threads; len(threads) != 1 || threads[0].Key != "019f02" {
		t.Fatalf("threads after first closes = %+v", threads)
	}
	stats.deactivateThread("019f02")
	if threads := stats.snapshot().Threads; len(threads) != 0 {
		t.Fatalf("threads after all close = %+v", threads)
	}
}

func TestSnapshotSortsActiveThreadsByLastActivity(t *testing.T) {
	stats := newStatsWithPrices(priceSnapshot{})
	now := time.Now()
	for _, thread := range []struct {
		key  string
		last time.Time
	}{
		{"oldest", now.Add(-time.Hour)},
		{"newest", now},
		{"middle", now.Add(-time.Minute)},
	} {
		stats.activateThread(thread.key)
		stats.applyRouted(thread.last, thread.key, "", "account", "", "", "", turnMetadata{})
	}

	threads := stats.snapshot().Threads
	if len(threads) != 3 || threads[0].Key != "newest" || threads[1].Key != "middle" || threads[2].Key != "oldest" {
		t.Fatalf("threads = %+v", threads)
	}
}

func TestAccountSnapshotUsesLast24Hours(t *testing.T) {
	stats := newStatsWithPrices(priceSnapshot{})
	now := time.Now()
	stats.applyRouted(now.Add(-25*time.Hour), "", "", "account-a", "", "", "", turnMetadata{})
	stats.applyRouted(now.Add(-23*time.Hour-30*time.Minute), "", "", "account-a", "", "", "", turnMetadata{})
	stats.applyRouted(now.Add(-30*time.Minute), "", "", "account-a", "", "", "", turnMetadata{})
	stats.applyRouted(now.Add(-30*time.Minute), "", "", "account-b", "", "", "", turnMetadata{})
	stats.applyRateLimited(now.Add(-25*time.Hour), "account-a")
	stats.applyRateLimited(now.Add(-time.Hour), "account-a")

	snapshot := stats.snapshot()
	account := snapshot.Accounts["account-a"]
	if account.Turns != 3 || account.Limited != 1 {
		t.Fatalf("account totals = %+v, want three lifetime turns and one recent limit", account)
	}
	if len(account.Activity) != 24 || account.Activity[0] != 1 || account.Activity[23] != 1 {
		t.Fatalf("account activity = %v, want recent and oldest hourly buckets", account.Activity)
	}
	if snapshot.Turns != 4 || snapshot.Limited != 2 {
		t.Fatalf("lifetime totals = %+v", snapshot)
	}
}

func TestThreadUsageFollowsCurrentLiveRoute(t *testing.T) {
	stats := newStatsWithPrices(priceSnapshot{})
	now := time.Now()
	old := now.Add(-time.Hour)
	stats.activateThread("thread")
	stats.applyRouted(old, "thread", "", "account", "old", "medium", "", turnMetadata{})
	stats.applyUsageAt(old, "thread", "account", "unknown", "medium", "default", responseUsage{InputTokens: 100})
	stats.deactivateThread("thread")
	stats.activateThread("thread")
	stats.applyRouted(now, "thread", "", "account", "gpt-5.6-sol", "xhigh", "", turnMetadata{})
	routed := stats.snapshot()
	if len(routed.Threads) != 1 || routed.Threads[0].Model != "gpt-5.6-sol" || routed.Threads[0].Effort != "xhigh" {
		t.Fatalf("routed thread = %+v", routed.Threads)
	}
	usage := responseUsage{InputTokens: 2_000, OutputTokens: 300}
	usage.InputDetails.CachedTokens = 1_500
	stats.applyUsageAt(now, "thread", "account", "unknown", "xhigh", "default", usage)

	snapshot := stats.snapshot()
	if len(snapshot.Threads) != 1 || snapshot.Threads[0].Model != "unknown" || snapshot.Threads[0].Turns != 1 || snapshot.Threads[0].Usage != usage {
		t.Fatalf("threads = %+v, want current routing window usage %+v", snapshot.Threads, usage)
	}
}

func TestThreadRouteSegmentResetsWhenAccountChanges(t *testing.T) {
	stats := newStatsWithPrices(priceSnapshot{})
	now := time.Now()
	metadata := turnMetadata{RequestKind: "compaction", ThreadID: "codex-thread", TurnID: "compact-turn"}
	stats.activateThread("thread")
	stats.applyRouted(now, "thread", "client", "account-a", "gpt-5.6-sol", "xhigh", "", metadata)
	sourceUsage := responseUsage{InputTokens: 100, OutputTokens: 10}
	sourceUsage.InputDetails.CachedTokens = 90
	stats.applyUsageAt(now.Add(time.Second), "thread", "account-a", "gpt-5.6-sol", "xhigh", "default", sourceUsage)
	stats.applyAnswered(now.Add(time.Second), "thread", "account-a", 100*time.Millisecond)
	stats.applyCompleted(now.Add(time.Second), "thread", "account-a", metadata.RequestKind, time.Second)

	stats.applyRouted(now.Add(2*time.Second), "thread", "client", "account-b", "gpt-5.6-sol", "xhigh", "", turnMetadata{RequestKind: "normal", ThreadID: "codex-thread", TurnID: "next-turn"})
	switched := stats.snapshot().Threads[0]
	if switched.Account != "account-b" || switched.Turns != 1 || !switched.Usage.empty() || !switched.LatestUsage.empty() || len(switched.models) != 0 || switched.TTFB != 0 || switched.Latency != 0 || switched.Compactions != 1 {
		t.Fatalf("switched segment = %+v", switched)
	}
	if got := dashboardCacheRate(switched.Usage); got != "--" {
		t.Fatalf("cache rate before target usage = %q, want --", got)
	}

	targetUsage := responseUsage{InputTokens: 100, OutputTokens: 20}
	stats.applyUsageAt(now.Add(3*time.Second), "thread", "account-b", "gpt-5.6-sol", "xhigh", "default", targetUsage)
	stats.applyUsageAt(now.Add(4*time.Second), "thread", "account-a", "gpt-5.6-sol", "xhigh", "default", sourceUsage)
	current := stats.snapshot().Threads[0]
	if current.Usage != targetUsage || current.LatestUsage != targetUsage || len(current.models) != 1 || current.models[0].name != "gpt-5.6-sol" || len(current.models[0].efforts) != 1 || current.models[0].efforts[0] != "xhigh" {
		t.Fatalf("current segment usage = %+v, want %+v", current.Usage, targetUsage)
	}
	if got := dashboardCacheRate(current.Usage); got != "0" {
		t.Fatalf("cache rate after target usage = %q, want 0", got)
	}
}

func TestCodexThreadsKeepSeparateMetadataWithinOneRoute(t *testing.T) {
	stats := newStatsWithPrices(priceSnapshot{})
	now := time.Now()
	mainMetadata := turnMetadata{RequestKind: "turn", ThreadID: "main-thread"}
	subagentMetadata := turnMetadata{RequestKind: "turn", ThreadID: "subagent-thread", SubagentKind: "thread_spawn"}
	stats.activateThread("main-thread")
	stats.activateThread("subagent-thread")
	stats.applyRouted(now, statsThreadKey("session", mainMetadata), "client", "account", "gpt-5.6-sol", "xhigh", "", mainMetadata)
	stats.applyRouted(now, statsThreadKey("session", subagentMetadata), "client", "account", "gpt-5.6-sol", "xhigh", "", subagentMetadata)

	threads := stats.snapshot().Threads
	if len(threads) != 2 || threads[0].Key != "main-thread" || threads[1].Key != "subagent-thread" {
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

func TestThreadCostPricesEachResponse(t *testing.T) {
	prices := testPriceSnapshot(t)
	stats := newStatsWithPrices(prices)
	stats.activateThread("thread")
	now := time.Now()
	stats.applyRouted(now, "thread", "client", "account", "gpt-5.6-sol", "xhigh", "default", turnMetadata{})
	usage := responseUsage{InputTokens: 200_000, OutputTokens: 1_000}
	stats.applyUsageAt(now, "thread", "account", "gpt-5.6-sol", "xhigh", "default", usage)
	stats.applyUsageAt(now, "thread", "account", "gpt-5.6-sol", "xhigh", "default", usage)

	want, known := prices.estimate("gpt-5.6-sol", "default", usage)
	if !known {
		t.Fatal("test model has no price")
	}
	thread := stats.snapshot().Threads[0]
	if thread.apiCostNanoDollars != want*2 || thread.unpricedResponses != 0 {
		t.Fatalf("thread cost = %d with %d unpriced, want %d with none", thread.apiCostNanoDollars, thread.unpricedResponses, want*2)
	}
}

func TestThreadCostRepricesAfterCatalogRefresh(t *testing.T) {
	store, err := openStateStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stats, err := newPersistentStats(store, priceSnapshot{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats.activateThread("thread")
	stats.routed("thread", "client", "account", "gpt-5.4", "high", "default", turnMetadata{})
	usage := responseUsage{InputTokens: 1_000, OutputTokens: 100}
	stats.recordUsage("thread", "account", "gpt-5.4", "high", "default", usage)
	before := stats.snapshot().Threads[0]
	if before.apiCostNanoDollars != 0 || before.unpricedResponses != 1 {
		t.Fatalf("thread cost before refresh = %d with %d unpriced", before.apiCostNanoDollars, before.unpricedResponses)
	}

	prices := testPriceSnapshot(t)
	if err := stats.reprice(prices); err != nil {
		t.Fatal(err)
	}
	want, _ := prices.estimate("gpt-5.4", "default", usage)
	after := stats.snapshot().Threads[0]
	if after.apiCostNanoDollars != want || after.unpricedResponses != 0 {
		t.Fatalf("thread cost after refresh = %d with %d unpriced, want %d with none", after.apiCostNanoDollars, after.unpricedResponses, want)
	}
}

func TestMonthlyUsageResetsAtMonthBoundary(t *testing.T) {
	prices := testPriceSnapshot(t)
	stats := newStatsWithPrices(prices)
	previousMonth := time.Date(2026, time.July, 31, 23, 59, 0, 0, time.UTC)
	currentMonth := previousMonth.Add(time.Minute)
	stats.usageMonth = calendarMonth(previousMonth)
	stats.applyUsageAt(previousMonth, "", "", "unknown", "", "default", responseUsage{InputTokens: 1_000})
	usage := responseUsage{InputTokens: 2_000, OutputTokens: 300}
	usage.InputDetails.CachedTokens = 1_500
	stats.applyUsageAt(currentMonth, "", "", "gpt-5.6-sol", "", "default", usage)
	unpricedUsage := responseUsage{InputTokens: 400, OutputTokens: 50}
	stats.applyUsageAt(currentMonth, "", "", "unknown", "", "default", unpricedUsage)
	wantUsage := usage
	wantUsage.InputTokens += unpricedUsage.InputTokens
	wantUsage.OutputTokens += unpricedUsage.OutputTokens
	want, _ := prices.estimate("gpt-5.6-sol", "default", usage)
	if stats.monthlyUsage != wantUsage {
		t.Fatalf("monthly usage = %+v, want %+v", stats.monthlyUsage, wantUsage)
	}
	if stats.apiCostNanoDollars != want || stats.unpricedResponses != 1 {
		t.Fatalf("API estimate = %d with %d unpriced, want %d with one", stats.apiCostNanoDollars, stats.unpricedResponses, want)
	}
}
