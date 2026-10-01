package app

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

func TestThreadTransportFollowsAcceptedTurns(t *testing.T) {
	stats := newStatsWithPrices(priceSnapshot{})
	stats.activateThread("thread")
	for _, step := range []struct {
		via     transport
		counted bool
		want    transport
	}{
		{transportWebSocket, true, transportWebSocket},
		{transportHTTP, true, transportHTTP},
		{transportWebSocket, false, transportHTTP}, // a warmup must not replace the last turn
		{transportWebSocket, true, transportWebSocket},
	} {
		stats.recordAccepted(time.Now(), "thread", "client", apiKeyIdentity{}, "account", "model", "", "", step.via, turnMetadata{}, step.counted)
		threads := stats.snapshot().Threads
		if len(threads) != 1 || threads[0].Via != step.want {
			t.Fatalf("after via=%s counted=%t: threads=%+v, want transport %s", step.via, step.counted, threads, step.want)
		}
	}
}

func TestStatsEndpointReportsRoutingMode(t *testing.T) {
	for _, mode := range []routingMode{routingModePriority, routingModePaused} {
		t.Run(string(mode), func(t *testing.T) {
			account := testAccount("account-a", 20)
			account.RoutingMode = routingModePriority
			account.Paused = mode == routingModePaused
			server := &server{pool: &Pool{accounts: []*Account{account}}, stats: newStatsWithPrices(priceSnapshot{})}
			server.stats.apiCostNanoDollars = 12_340_000_000
			server.stats.monthlyUsage.InputTokens = 1_234_567
			server.stats.monthlyUsage.OutputTokens = 2_345_678
			request := httptest.NewRequest(http.MethodGet, "/stats", nil)
			response := httptest.NewRecorder()

			server.routes().ServeHTTP(response, request)
			var payload statsResponse
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || payload.MonthlyAPICost != "$12.34" || payload.MonthlyInputTokens != "1.2M" || payload.MonthlyOutputTokens != "2.3M" || len(payload.Accounts) != 1 || payload.Accounts[0].Status != accountStatus(mode) || payload.Accounts[0].RoutingMode != mode {
				t.Fatalf("status = %d, payload = %+v", response.Code, payload)
			}
		})
	}
}

func TestStatsEndpointReportsTrafficAndResetCountdown(t *testing.T) {
	now := time.Now()
	first := testAccount("account-a", 20)
	first.primary.resetsAt = now.Add(4*24*time.Hour + 23*time.Hour + 20*time.Minute)
	second := testAccount("account-b", 30)
	stats := newStatsWithPrices(priceSnapshot{})
	stats.applyRouted(now, "", "", "account-a", "", "", "", turnMetadata{})
	stats.applyRouted(now, "", "", "account-b", "", "", "", turnMetadata{})
	stats.applyRouted(now, "", "", "account-b", "", "", "", turnMetadata{})
	server := &server{pool: &Pool{accounts: []*Account{first, second}}, stats: stats}
	request := httptest.NewRequest(http.MethodGet, "/stats", nil)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	var payload statsResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	accounts := make(map[string]accountStatsResponse, len(payload.Accounts))
	for _, account := range payload.Accounts {
		accounts[account.ID] = account
	}
	if accounts["account-a"].Traffic24hPercent != 33 || accounts["account-a"].ResetIn != "4d23h" || accounts["account-b"].Traffic24hPercent != 67 || accounts["account-b"].ResetIn != "--" {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestStatsEndpointReportsMonthlyCostPerAccount(t *testing.T) {
	now := time.Now()
	stats := newStatsWithPrices(testPriceSnapshot(t))
	stats.applyUsageAt(calendarMonthStart(now), "", "account-a", "gpt-5.6-sol", "", "default", responseUsage{InputTokens: 12_345_600})
	stats.applyUsageAt(now, "", "account-b", "unknown", "", "default", responseUsage{InputTokens: 1_000})
	server := &server{pool: &Pool{accounts: []*Account{
		testAccount("account-a", 20),
		testAccount("account-b", 20),
		testAccount("account-c", 20),
	}}, stats: stats}
	request := httptest.NewRequest(http.MethodGet, "/stats", nil)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	var payload statsResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	costs := make(map[string]string, len(payload.Accounts))
	for _, account := range payload.Accounts {
		costs[account.ID] = account.MonthlyAPICost
	}
	if costs["account-a"] != "$123.46" || costs["account-b"] != "--" || costs["account-c"] != "$0.00" {
		t.Fatalf("monthly account costs = %+v", costs)
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

func TestEventDetailsMaskEmails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		detail string
		want   string
	}{
		{"refresh", "Quota and banked credits refreshed for alice@example.com.", "Quota and banked credits refreshed for a***e@***.com."},
		{"multiple", "alice@example.com, bob+work@mail.example.net", "a***e@***.com, b***k@***.net"},
		{"punctuation", `failed for <first.last@example.com> ("other@example.net")`, `failed for <f***t@***.com> ("o***r@***.net")`},
		{"unicode", "tést@exämple.com", "t***t@***.com"},
		{"local domain", "ab@localhost", "a***@***"},
		{"masked", "a***e@***.com", "a***e@***.com"},
		{"no email", "upstream returned 503 Service Unavailable", "upstream returned 503 Service Unavailable"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats := newStatsWithPrices(priceSnapshot{})
			stats.note("account refresh", "account-a", tc.detail)
			stats.failedOver("account-a", tc.detail)

			events := stats.snapshot().Events
			if len(events) != 2 {
				t.Fatalf("events = %d, want 2", len(events))
			}
			for i, kind := range []string{"account refresh", eventFailover} {
				if event := events[i]; event.Detail != tc.want || event.Kind != kind || event.Account != "account-a" || event.At.IsZero() {
					t.Errorf("event = %+v, want kind %q and detail %q", event, kind, tc.want)
				}
			}
		})
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

func TestCatalogRefreshRepricesMonthlyUsageWithoutPersistingThreadHistory(t *testing.T) {
	store, err := openStateStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.add(testAccount("account", 0)); err != nil {
		t.Fatal(err)
	}
	stats, err := newPersistentStats(store, priceSnapshot{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats.activateThread("thread")
	stats.accepted("", "thread", "thread", "client", apiKeyIdentity{}, "account", "gpt-5.4", "high", "default", transportWebSocket, turnMetadata{}, true)
	usage := responseUsage{InputTokens: 1_000, OutputTokens: 100}
	stats.recordUsage("thread", "account", "gpt-5.4", "high", "default", usage)
	before := stats.snapshot().Threads[0]
	if before.apiCostNanoDollars != 0 || before.unpricedResponses != 1 {
		t.Fatalf("thread cost before refresh = %d with %d unpriced", before.apiCostNanoDollars, before.unpricedResponses)
	}
	if account := stats.snapshot().Accounts["account"]; account.APICostNanoDollars != 0 || account.UnpricedResponses != 1 {
		t.Fatalf("account cost before refresh = %+v, want one unpriced", account)
	}

	prices := testPriceSnapshot(t)
	if err := stats.reprice(prices); err != nil {
		t.Fatal(err)
	}
	want, _ := prices.estimate("gpt-5.4", "default", usage)
	after := stats.snapshot().Threads[0]
	if after.apiCostNanoDollars != 0 || after.unpricedResponses != 1 {
		t.Fatalf("thread cost after refresh = %d with %d unpriced, want live value unchanged", after.apiCostNanoDollars, after.unpricedResponses)
	}
	monthly := stats.snapshot()
	if monthly.APICostNanoDollars != want || monthly.UnpricedResponses != 0 {
		t.Fatalf("monthly cost after refresh = %d with %d unpriced, want %d with none", monthly.APICostNanoDollars, monthly.UnpricedResponses, want)
	}
	if len(monthly.ModelCosts) != 1 || monthly.ModelCosts[0] != (ModelCostSnapshot{Model: "gpt-5.4", APICostNanoDollars: want}) {
		t.Fatalf("monthly model costs after refresh = %+v, want gpt-5.4 cost %d", monthly.ModelCosts, want)
	}
	if account := monthly.Accounts["account"]; account.APICostNanoDollars != want || account.UnpricedResponses != 0 {
		t.Fatalf("account cost after refresh = %+v, want %d with none unpriced", account, want)
	}
}

func TestMonthlyAccountCostsMatchTotal(t *testing.T) {
	prices := testPriceSnapshot(t)
	stats := newStatsWithPrices(prices)
	now := time.Now()
	usage := responseUsage{InputTokens: 300_000, OutputTokens: 10_000}
	usage.InputDetails.CachedTokens = 100_000
	usage.InputDetails.CacheWriteTokens = 50_000
	stats.applyUsageAt(calendarMonthStart(now).Add(-time.Second), "", "account-a", "old-unknown", "", "default", usage)
	stats.applyUsageAt(calendarMonthStart(now), "", "account-a", "gpt-5.6-sol", "", "default", usage)
	stats.applyUsageAt(now, "", "account-a", "gpt-5.6-sol", "", "priority", usage)
	stats.applyUsageAt(now, "", "account-b", "gpt-5.4-mini", "", "default", usage)
	stats.applyUsageAt(now, "", "account-c", "unknown", "", "default", usage)
	snapshot := stats.snapshot()
	standard, _ := prices.estimate("gpt-5.6-sol", "default", usage)
	fast, _ := prices.estimate("gpt-5.6-sol", "priority", usage)
	mini, _ := prices.estimate("gpt-5.4-mini", "default", usage)
	for id, want := range map[string]usageCost{
		"account-a": {apiCostNanoDollars: standard + fast},
		"account-b": {apiCostNanoDollars: mini},
		"account-c": {unpricedResponses: 1},
	} {
		account := snapshot.Accounts[id]
		if account.APICostNanoDollars != want.apiCostNanoDollars || account.UnpricedResponses != want.unpricedResponses {
			t.Fatalf("%s monthly cost = %+v, want %+v", id, account, want)
		}
	}
	var total, unpriced int64
	for _, account := range snapshot.Accounts {
		total += account.APICostNanoDollars
		unpriced += account.UnpricedResponses
	}
	if total != snapshot.APICostNanoDollars || unpriced != snapshot.UnpricedResponses {
		t.Fatalf("account sum = %d with %d unpriced, total = %d with %d unpriced", total, unpriced, snapshot.APICostNanoDollars, snapshot.UnpricedResponses)
	}
}

func TestMonthlyUsageResetsAtMonthBoundary(t *testing.T) {
	prices := testPriceSnapshot(t)
	stats := newStatsWithPrices(prices)
	previousMonth := time.Date(2026, time.July, 31, 23, 59, 0, 0, time.UTC)
	currentMonth := previousMonth.Add(time.Minute)
	stats.usageMonth = calendarMonth(previousMonth)
	stats.applyUsageAt(previousMonth, "", "old-account", "old-unknown", "", "default", responseUsage{InputTokens: 1_000})
	stats.applyUsageAt(previousMonth, "", "account", "gpt-5.6-sol", "", "default", responseUsage{InputTokens: 1_000})
	usage := responseUsage{InputTokens: 2_000, OutputTokens: 300}
	usage.InputDetails.CachedTokens = 1_500
	stats.applyUsageAt(currentMonth, "", "account", "gpt-5.6-sol", "", "default", usage)
	unpricedUsage := responseUsage{InputTokens: 400, OutputTokens: 50}
	stats.applyUsageAt(currentMonth, "", "account", "unknown", "", "default", unpricedUsage)
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
	if len(stats.monthlyModelCosts) != 2 {
		t.Fatalf("monthly model costs = %+v, want current priced and unpriced models", stats.monthlyModelCosts)
	}
	if _, exists := stats.monthlyModelCosts["old-unknown"]; exists {
		t.Fatalf("monthly model costs retained previous month: %+v", stats.monthlyModelCosts)
	}
	if got := stats.monthlyModelCosts["gpt-5.6-sol"]; got.apiCostNanoDollars != want || got.unpricedResponses != 0 {
		t.Fatalf("priced model cost = %+v, want %d with none unpriced", got, want)
	}
	if got := stats.monthlyModelCosts["unknown"]; got.apiCostNanoDollars != 0 || got.unpricedResponses != 1 {
		t.Fatalf("unpriced model cost = %+v, want zero with one unpriced", got)
	}
	if got := stats.accounts["account"].monthlyCost; got.apiCostNanoDollars != want || got.unpricedResponses != 1 {
		t.Fatalf("account monthly cost = %+v, want %d with one unpriced", got, want)
	}
	if got := stats.accounts["old-account"].monthlyCost; got != (usageCost{}) {
		t.Fatalf("inactive account retained previous month's cost: %+v", got)
	}
}

func TestMonthlyAccountCostsResetWithoutNewUsage(t *testing.T) {
	stats := newStatsWithPrices(testPriceSnapshot(t))
	stats.usageMonth = calendarMonth(time.Now()) - 1
	stats.account("account").monthlyCost = usageCost{apiCostNanoDollars: 1_000_000_000, unpricedResponses: 1}
	account := stats.snapshot().Accounts["account"]
	if account.APICostNanoDollars != 0 || account.UnpricedResponses != 0 {
		t.Fatalf("account monthly cost after idle rollover = %+v, want zero", account)
	}
}
