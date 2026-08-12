package main

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRootRedirectsToDashboard(t *testing.T) {
	server := &server{pool: &Pool{}, stats: newStats()}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()

	server.routes().ServeHTTP(response, request)

	if response.Code != http.StatusPermanentRedirect || response.Header().Get("Location") != "/dashboard" {
		t.Fatalf("status = %d, location = %q", response.Code, response.Header().Get("Location"))
	}
}

func TestDashboardPageConnectsHTMXWebSocket(t *testing.T) {
	server := &server{pool: &Pool{}, stats: newStats()}
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("status = %s, CSP = %q", response.Status, response.Header.Get("Content-Security-Policy"))
	}
	for _, expected := range []string{
		`<link rel="icon" href="/favicon.svg" type="image/svg+xml">`,
		`src="` + dashboardAssetURL("dashboard.js") + `"`,
		`src="/dashboard/assets/htmx-2.0.10.min.js"`,
		`src="/dashboard/assets/ws-2.0.4.min.js"`,
		`hx-ext="ws"`,
		`ws-connect="/dashboard/ws"`,
		`id="dashboard"`,
		`<h2>Active Threads&nbsp; <span id="routing-count">0</span></h2>`,
		`no live threads`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q", expected)
		}
	}
	overview := strings.Index(body, `<h2>Overview</h2>`)
	accounts := strings.Index(body, `<h2>Accounts <span id="summary">`)
	if overview < 0 || accounts < 0 || overview > accounts {
		t.Fatalf("dashboard section order: overview = %d, accounts = %d", overview, accounts)
	}
	if strings.Contains(body, `<h2>Resources</h2>`) || strings.Contains(body, `id="resources"`) {
		t.Fatal("dashboard has a Resources section")
	}
}

func TestWebAssetsAreServedFromBinary(t *testing.T) {
	server := &server{pool: &Pool{}, stats: newStats()}
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	for _, asset := range []struct {
		path         string
		contentType  string
		cacheControl string
		min          int
	}{
		{"/favicon.svg", "image/svg+xml", "public, max-age=3600", 100},
		{"/dashboard/assets/dashboard.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", 500},
		{"/dashboard/assets/htmx-2.0.10.min.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", 1_000},
		{"/dashboard/assets/ws-2.0.4.min.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", 1_000},
	} {
		response, err := http.Get(httpServer.URL + asset.path)
		if err != nil {
			t.Fatal(err)
		}
		payload, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != asset.contentType || len(payload) < asset.min {
			t.Fatalf("%s returned status %s, content type %q, length %d", asset.path, response.Status, response.Header.Get("Content-Type"), len(payload))
		}
		if response.Header.Get("Cache-Control") != asset.cacheControl {
			t.Fatalf("%s cache control = %q", asset.path, response.Header.Get("Cache-Control"))
		}
	}
}

func TestDashboardWebSocketStreamsEscapedHTML(t *testing.T) {
	stats := newStatsWithPrices(testPriceSnapshot(t))
	stats.activateThread("019fe5c2private")
	stats.routed("019fe5c2private", "203.0.113.42", "unused", "gpt-5.6-sol", "high", serviceTierFast, transportWebSocket, turnMetadata{})
	stats.recordUsage("019fe5c2private", "unused", "gpt-5.6-sol", "high", "default", responseUsage{OutputTokens: 1_000_000})
	stats.failedOver("unused", "<script>upstream unavailable</script>")
	stats.note(eventLegacyReconnect, "source", "019fe701-7a55-7760-8d38-d1cd74544ef8")
	stats.note(eventLegacyRotated, "unused", "after compaction")
	stats.compactionSwitched("019fe827-9296-7f82-b526-180a27ca764c", "source", "unused")
	tokenPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"alice@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"unused","chatgpt_plan_type":"pro"}}`))
	account := accountFromState(accountState{IDToken: "x." + tokenPayload + ".x"})
	source := testAccount("source", 20)
	server := &server{
		pool:        &Pool{accounts: []*Account{account, source}},
		stats:       stats,
		key:         "secret",
		clientIDKey: []byte("secret"),
		countries:   countryResolver{states: map[string]countryState{"203.0.113.42": {code: "US", ready: true}}},
	}
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/dashboard/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("message type = %v", messageType)
	}
	body := string(payload)
	for _, expected := range []string{
		`id="overview" hx-swap-oob="innerHTML"`,
		`id="summary" hx-swap-oob="innerHTML"`,
		`id="accounts" hx-swap-oob="innerHTML"`,
		`id="routing-count" hx-swap-oob="innerHTML"`,
		`id="threads" hx-swap-oob="innerHTML"`,
		`id="events" hx-swap-oob="innerHTML"`,
		`a***e@***.com`,
		`<td class="dim">pro</td>`,
		`019fe5c2`,
		`🇺🇸 US-1`,
		`<td>☀️ (high)</td>`,
		`<td class="status"><span class="status-mark status-checking">◌</span> checking</td>`,
		`<span>1 checking</span>`,
		`<td>WS</td>`,
		`class="fast-icon"`,
		`aria-label="Fast"`,
		`API estimate`,
		`$30.00`,
		`<td>failover</td>`,
		`&lt;script&gt;upstream unavailable&lt;/script&gt;`,
		`<td>compaction switch</td>`,
		`<td>s***e@***.com → a***e@***.com</td>`,
		`<td class="dim">019fe827</td>`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard update missing %q:\n%s", expected, body)
		}
	}
	for _, replaced := range []string{`id="dashboard"`, `hx-swap-oob="outerHTML"`} {
		if strings.Contains(body, replaced) {
			t.Fatalf("dashboard update replaces stable container %q", replaced)
		}
	}
	for _, private := range []string{"alice@example.com", "source@example.com", "019fe5c2private", "019fe701-7a55-7760-8d38-d1cd74544ef8", "019fe827-9296-7f82-b526-180a27ca764c", "rotation reconnect", "<td>rotated</td>", "after compaction", "203.0.113.42", "<script>"} {
		if strings.Contains(body, private) {
			t.Fatalf("dashboard update exposed %q", private)
		}
	}
}

func TestDashboardAccountValuesOmitRedundantUnitsAndZeros(t *testing.T) {
	now := time.Now()
	account := testAccount("account-a", 20)
	account.adoptResetCredits(0, nil)
	other := testAccount("account-b", 20)
	stats := newStats()
	stats.applyRouted(now.Add(-25*time.Hour), "", "", "account-a", "", "", "", transportHTTP, turnMetadata{})
	stats.applyRouted(now, "", "", "account-a", "", "", "", transportHTTP, turnMetadata{})
	for range 100 {
		stats.applyRouted(now, "", "", "account-b", "", "", "", transportHTTP, turnMetadata{})
	}
	server := &server{pool: &Pool{accounts: []*Account{account, other}}, stats: stats}

	view := server.currentDashboard(now)
	if len(view.Accounts) != 2 {
		t.Fatalf("accounts = %d, want two", len(view.Accounts))
	}
	accountView := view.Accounts[0]
	if accountView.Weekly != "80" || accountView.Banked != "" || accountView.Traffic != "1" {
		t.Fatalf("account values = %+v", accountView)
	}
	if view.Accounts[1].Traffic != "99" {
		t.Fatalf("other account traffic = %q, want 99", view.Accounts[1].Traffic)
	}
	payload, err := renderDashboard("dashboard", view)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, expected := range []string{"<th>Weekly %</th>", "<th>Traffic 24h %</th>", "<th>Activity 24h</th>"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q", expected)
		}
	}
	if strings.Contains(body, "<th>Limits 24h</th>") {
		t.Fatal("dashboard contains Limits 24h column")
	}
}

func TestTrafficPercentagesSumToOneHundred(t *testing.T) {
	accounts := []accountStatsResponse{
		{Activity: []int64{841}},
		{Activity: []int64{86}},
		{Activity: []int64{27_629}},
		{},
	}

	got := trafficPercentages(accounts)
	if len(got) != 4 || got[0] != 3 || got[1] != 0 || got[2] != 97 || got[3] != 0 {
		t.Fatalf("traffic percentages = %v, want [3 0 97 0]", got)
	}
}

func TestDashboardBankedResetTooltipShowsExpirations(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(4 * time.Minute)
	laterExpiresAt := now.Add(2 * time.Hour)
	account := testAccount("account-a", 20)
	account.adoptResetCredits(2, []resetCredit{
		{
			ID:          "private-credit-id",
			ResetType:   "codex_rate_limits",
			Status:      "available",
			ExpiresAt:   &expiresAt,
			Title:       "Full <reset>",
			Description: "Weekly & five-hour windows",
		},
		{
			ID:          "second-private-credit-id",
			ResetType:   "codex_rate_limits",
			Status:      "available",
			ExpiresAt:   &laterExpiresAt,
			Title:       "Another reset",
			Description: "More detail",
		},
	})
	server := &server{pool: &Pool{accounts: []*Account{account}}, stats: newStats()}

	stats := server.currentStats(now)
	if len(stats.Accounts) != 1 || len(stats.Accounts[0].ResetCredits) != 2 {
		t.Fatalf("reset credits = %+v", stats.Accounts)
	}
	view := server.currentDashboard(now)
	wantInfo := "Expires in 4m at 2026-08-09 12:04 UTC\nExpires in 2h00m at 2026-08-09 14:00 UTC"
	if len(view.Accounts) != 1 {
		t.Fatalf("accounts = %d, want one", len(view.Accounts))
	}
	if view.Accounts[0].Banked != "2" || view.Accounts[0].BankedInfo != wantInfo {
		t.Fatalf("banked account = %+v", view.Accounts[0])
	}
	payload, err := renderDashboard("dashboard", view)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, expected := range []string{`class="has-tooltip"`, `data-tooltip="Expires in 4m at 2026-08-09 12:04 UTC`, `Expires in 2h00m at 2026-08-09 14:00 UTC`, `aria-describedby="dashboard-tooltip"`, `tabindex="0"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q:\n%s", expected, body)
		}
	}
	for _, absent := range []string{"private-credit-id", "second-private-credit-id", "Full &lt;reset&gt;", "Weekly &amp; five-hour windows", "Another reset", "More detail"} {
		if strings.Contains(body, absent) {
			t.Fatalf("dashboard exposed %q", absent)
		}
	}
}

func TestDashboardExplainsResetPriorityStatus(t *testing.T) {
	now := time.Date(2026, time.August, 11, 9, 0, 0, 0, time.UTC)
	account := testAccount("account-a", 80)
	account.primary = window{usedPercent: 10, minutes: 300, resetsAt: now.Add(4 * time.Hour), seenAt: now}
	account.secondary = window{usedPercent: 80, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
	adoptTestResetCredit(account, now.Add(30*time.Minute))
	server := &server{pool: &Pool{accounts: []*Account{account}}, stats: newStats()}

	payload, err := renderDashboard("dashboard", server.currentDashboard(now))
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, expected := range []string{
		`<span>1 priority</span>`,
		`class="status-mark status-priority"`,
		`>◆</span> priority</span>`,
		`data-tooltip="Prioritized for new routing: a banked reset expires in 30m; 20% weekly capacity remains."`,
		`aria-describedby="dashboard-tooltip"`,
		`tabindex="0"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q:\n%s", expected, body)
		}
	}
}

func TestDashboardExplainsAutomaticDrainingStatus(t *testing.T) {
	now := time.Date(2026, time.August, 12, 20, 0, 0, 0, time.UTC)
	account := testAccount("account-a", 96)
	server := &server{pool: &Pool{accounts: []*Account{account}}, stats: newStats()}

	stats := server.currentStats(now)
	if len(stats.Accounts) != 1 || stats.Accounts[0].Status != accountDraining || stats.Accounts[0].RoutingMode != routingModeNormal {
		t.Fatalf("account stats = %+v", stats.Accounts)
	}
	payload, err := renderDashboard("dashboard", server.currentDashboard(now))
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, expected := range []string{
		`<span>1 draining</span>`,
		`class="status-mark status-draining"`,
		`>▼</span> draining</span>`,
		`data-tooltip="A rate-limit window has less than 5% left. Portable threads move to this account; active turns finish before reconnecting."`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q:\n%s", expected, body)
		}
	}
}

func TestDashboardExplainsManualRoutingStatus(t *testing.T) {
	now := time.Date(2026, time.August, 12, 20, 0, 0, 0, time.UTC)
	account := testAccount("account-a", 20)
	server := &server{pool: &Pool{accounts: []*Account{account}}, stats: newStats()}

	for _, test := range []struct {
		mode routingMode
		info string
	}{
		{routingModePriority, "Manual priority for fresh portable work. Existing soft-affined threads stay on their account."},
		{routingModeDraining, "Manual draining. Portable threads move to this account; active turns finish before reconnecting."},
	} {
		account.RoutingMode = test.mode
		view := server.currentDashboard(now)
		if len(view.Accounts) != 1 || view.Accounts[0].StatusInfo != test.info {
			t.Fatalf("%s account view = %+v", test.mode, view.Accounts)
		}
	}
}

func TestDashboardOverview(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.FixedZone("BST", 60*60))
	stats := newStatsWithPrices(testPriceSnapshot(t))
	stats.started = time.Now().Add(-28 * time.Minute)
	stats.usageMonth = calendarMonth(now)
	usage := responseUsage{InputTokens: 2_000, OutputTokens: 300}
	usage.InputDetails.CachedTokens = 1_500
	stats.applyUsageAt(now, "", "", "gpt-5.6-sol", "", "default", usage)
	stats.apiCostNanoDollars = 7_746_820_000_000
	server := &server{pool: &Pool{}, stats: stats}
	view := server.currentDashboard(now)
	wantValues := map[string]string{
		"CPU":           "--",
		"RAM":           "--",
		"network in":    "--",
		"network out":   "--",
		"input tokens":  "2K",
		"cached input":  "1.5K",
		"output tokens": "300",
		"uptime":        "28m",
	}
	wantNames := []string{"CPU", "RAM", "network in", "network out", "uptime", "input tokens", "cached input", "output tokens", "API estimate"}
	if len(view.Overview) != len(wantNames) {
		t.Fatalf("overview metrics = %+v", view.Overview)
	}
	wantInfo := "From Aug 1"
	wantPriceInfo := "☕ 1291 iced lattes ($6 each)\n🌮 646 tacos ($12 each)\n" + wantInfo + ". Prices from models.dev, updated 11 August 2026, 16:00 BST"
	apiEstimateFound := false
	for i, metric := range view.Overview {
		if metric.Name != wantNames[i] {
			t.Fatalf("overview metric %d = %q, want %q", i, metric.Name, wantNames[i])
		}
		if wantValue, ok := wantValues[metric.Name]; ok {
			if metric.Value != wantValue {
				t.Fatalf("%s overview metric = %+v, want value %q", metric.Name, metric, wantValue)
			}
			if metric.Name != "CPU" && metric.Name != "RAM" && metric.Name != "network in" && metric.Name != "network out" && metric.Name != "uptime" && metric.Info != wantInfo {
				t.Fatalf("%s overview info = %q, want %q", metric.Name, metric.Info, wantInfo)
			}
			delete(wantValues, metric.Name)
		}
		switch metric.Name {
		case "API estimate":
			apiEstimateFound = true
			if metric.Info != wantPriceInfo {
				t.Fatalf("API estimate info = %q, want %q", metric.Info, wantPriceInfo)
			}
		case "turns", "http", "ws turns", "ws open", "turn rate", "estimated TPS", "threads", "accounts", "failovers", "rate limits", "ttfb":
			t.Fatalf("removed overview metric %q is still present", metric.Name)
		}
	}
	if len(wantValues) != 0 {
		t.Fatalf("missing overview metrics: %v", wantValues)
	}
	if !apiEstimateFound {
		t.Fatal("API estimate overview metric missing")
	}
}

func TestFunCostEquivalents(t *testing.T) {
	for _, test := range []struct {
		cost int64
		want string
	}{
		{0, "☕ 0 iced lattes ($6 each)\n🌮 0 tacos ($12 each)"},
		{3_000_000_000, "☕ 1 iced latte ($6 each)\n🌮 0 tacos ($12 each)"},
		{9_000_000_000, "☕ 2 iced lattes ($6 each)\n🌮 1 taco ($12 each)"},
	} {
		if got := funCostEquivalents(test.cost); got != test.want {
			t.Fatalf("fun cost equivalents for %d = %q, want %q", test.cost, got, test.want)
		}
	}
}

func TestDashboardRoutingShowsTokenUsage(t *testing.T) {
	now := time.Now()
	stats := newStatsWithPrices(testPriceSnapshot(t))
	metadata := turnMetadata{RequestKind: "compaction", ThreadID: "019fe5c2private", TurnID: "019fe730private", SubagentKind: "compact"}
	stats.activateThread("thread")
	stats.applyRouted(now, "thread", "203.0.113.42", "account", "gpt-5.6-sol", "xhigh", "", transportHTTP, metadata)
	usage := responseUsage{InputTokens: 2_000, OutputTokens: 300, TotalTokens: 2_300}
	usage.InputDetails.CachedTokens = 1_500
	stats.applyUsageAt(now, "thread", "account", "gpt-5.6-sol", "xhigh", "default", usage)
	stats.applyAnswered(now, "thread", "account", 500*time.Millisecond)
	stats.applyCompleted(now, "thread", "account", metadata.RequestKind, 2*time.Second)
	catalog := newModelCatalog()
	model := testModelEntry("gpt-5.6-sol")
	model["context_window"] = 272_000
	model["effective_context_window_percent"] = 95
	catalog.replace([]string{"account"}, map[string][]modelEntry{"account": {model}}, "0.147.0")
	server := &server{
		pool:        &Pool{},
		stats:       stats,
		catalog:     catalog,
		clientIDKey: []byte("secret"),
		countries:   countryResolver{states: map[string]countryState{"203.0.113.42": {code: "US", ready: true}}},
	}

	view := server.currentDashboard(now)
	if len(view.Threads) != 1 {
		t.Fatalf("routing rows = %d, want one", len(view.Threads))
	}
	thread := view.Threads[0]
	if thread.ClientID != "🇺🇸 US-1" || thread.Model != "☀️ (xhigh)" || thread.UncachedInput != "500" || thread.CacheRate != "75" || thread.Output != "300" || thread.ContextLeft != "100% (1)" || thread.Latency != "2s" || thread.Requests != "1" || thread.Cost != "$0.012" {
		t.Fatalf("routing row = %+v", thread)
	}
	if thread.Info != "Request: compaction\nCodex thread: 019fe5c2\nTurn: 019fe730\nAgent: compact" || !strings.Contains(thread.ContextInfo, "Context window: 258.4K") || !strings.Contains(thread.ContextInfo, "Auto compact at: 244.8K") || !strings.Contains(thread.ContextInfo, "Used: 2.3K") || !strings.Contains(thread.ContextInfo, "Left: 100%") || !strings.Contains(thread.ContextInfo, "Compactions: 1") || thread.LatencyInfo != "First byte: 500ms\nTotal: 2s" {
		t.Fatalf("routing details = %+v", thread)
	}
	payload, err := renderDashboard("dashboard", view)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, expected := range []string{"<td class=\"dim\">🇺🇸 US-1</td>", "<th>Model (thinking mode)</th>", "<td>☀️ (xhigh)</td>", "<th>Cache %</th>", "<th>Context left<br>Compactions</th>", "<th>Cost</th>", "<td>$0.012</td>", "Codex thread: 019fe5c2", "Auto compact at: 244.8K", "Compactions: 1"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q", expected)
		}
	}
	for _, absent := range []string{"<th>Country</th>", ">Route</th>", "<th>Cached</th>", "<th>Reasoning</th>", ">Compacts</th>", "<th>Uncached input</th>", "<th>Output</th>", "<th>Latency</th>", "<th>Requests</th>", "First byte: 500ms", ">1 turn<"} {
		if strings.Contains(body, absent) {
			t.Fatalf("dashboard contains %q", absent)
		}
	}
	for _, private := range []string{"019fe5c2private", "019fe730private", "203.0.113.42"} {
		if strings.Contains(body, private) {
			t.Fatalf("dashboard exposed %q", private)
		}
	}
	costColumn := strings.Index(body, "<th>Cost</th>")
	lastActiveColumn := strings.Index(body, "<th>Last active</th>")
	if costColumn < 0 || lastActiveColumn < costColumn {
		t.Fatalf("cost column is not before last active: %s", body)
	}
}

func TestDashboardRoutingShowsMixedModels(t *testing.T) {
	now := time.Now()
	stats := newStatsWithPrices(testPriceSnapshot(t))
	stats.activateThread("thread")
	stats.applyRouted(now, "thread", "203.0.113.42", "account", "gpt-5.6-sol", "xhigh", "", transportHTTP, turnMetadata{})
	stats.applyRouted(now.Add(time.Second), "thread", "203.0.113.42", "account", "gpt-5.6-luna", "low", "", transportHTTP, turnMetadata{})
	stats.applyUsageAt(now.Add(2*time.Second), "thread", "account", "gpt-5.6-sol", "xhigh", "default", responseUsage{InputTokens: 1_000})
	stats.applyUsageAt(now.Add(3*time.Second), "thread", "account", "gpt-5.6-luna", "low", "default", responseUsage{InputTokens: 1_000})
	server := &server{
		pool:        &Pool{},
		stats:       stats,
		catalog:     newModelCatalog(),
		clientIDKey: []byte("secret"),
	}

	view := server.currentDashboard(now.Add(3 * time.Second))
	if len(view.Threads) != 1 {
		t.Fatalf("routing rows = %d, want one", len(view.Threads))
	}
	thread := view.Threads[0]
	if thread.Model != "mixed" || thread.ModelInfo != "gpt-5.6-sol (xhigh)\ngpt-5.6-luna (low)" {
		t.Fatalf("mixed model = %q with info %q", thread.Model, thread.ModelInfo)
	}
	payload, err := renderDashboard("dashboard", view)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	if !strings.Contains(body, "data-tooltip=\"gpt-5.6-sol (xhigh)\ngpt-5.6-luna (low)\"") || !strings.Contains(body, `>mixed</span>`) {
		t.Fatalf("mixed model tooltip missing from dashboard: %s", body)
	}
}

func TestDashboardModel(t *testing.T) {
	for _, test := range []struct {
		model  string
		effort string
		want   string
	}{
		{"gpt-5.6-sol", "xhigh", "☀️ (xhigh)"},
		{"gpt-5.6-terra", "medium", "🌍 (medium)"},
		{"gpt-5.6-luna", "low", "🌙 (low)"},
		{"gpt-5.6-luna-2026-08-01", "", "🌙"},
		{"gpt-5.4", "", "gpt-5.4"},
	} {
		if got := dashboardModel(test.model, test.effort); got != test.want {
			t.Errorf("dashboardModel(%q, %q) = %q, want %q", test.model, test.effort, got, test.want)
		}
	}
}

func TestDashboardClientNamesUseCountryAndUniqueOrdinals(t *testing.T) {
	key := []byte("secret")
	clients := []struct {
		ip      string
		country string
	}{
		{"1.1.1.1", "AU"},
		{"8.8.8.8", "US"},
		{"9.9.9.9", "US"},
		{"208.67.222.222", "US"},
		{"185.228.168.9", "CH"},
		{"76.76.2.0", "US"},
	}
	threads := make([]ThreadSnapshot, 0, len(clients)+1)
	states := make(map[string]countryState, len(clients))
	wantCountries := make(map[string]string, len(clients))
	for _, client := range clients {
		threads = append(threads, ThreadSnapshot{ClientIP: client.ip})
		states[client.ip] = countryState{code: client.country, ready: true}
		wantCountries[clientIDForIP(client.ip, key)] = countryLabel(client.country)
	}
	threads = append(threads, ThreadSnapshot{ClientIP: clients[0].ip})

	names := dashboardClientNames(threads, key, &countryResolver{states: states})
	if len(names) != 6 {
		t.Fatalf("client names = %v, want six", names)
	}
	ordinals := make(map[string]bool, len(names))
	for id, name := range names {
		country := wantCountries[id]
		if !strings.HasPrefix(name, country+"-") {
			t.Fatalf("client %s = %q, want country %q", id, name, country)
		}
		ordinals[strings.TrimPrefix(name, country+"-")] = true
	}
	for _, ordinal := range []string{"1", "2", "3", "4", "5", "6"} {
		if !ordinals[ordinal] {
			t.Fatalf("client ordinals = %v, missing %s", ordinals, ordinal)
		}
	}
}

func TestDashboardContextShowsPercentAndCompactions(t *testing.T) {
	limits := modelContextLimits{Window: 112_000, AutoCompact: 100_000}
	if got := dashboardContext(12_000, limits, 0); got != "100%" {
		t.Fatalf("context = %q, want 100%%", got)
	}
	if got := dashboardContext(62_000, limits, 7); got != "50% (7)" {
		t.Fatalf("context = %q, want 50%% (7)", got)
	}
	if got := dashboardContext(120_000, limits, 0); got != "0%" {
		t.Fatalf("context = %q, want 0%%", got)
	}
	if info := dashboardContextInfo(62_000, limits, 0); !strings.Contains(info, "Left: 50%\nCompactions: 0") {
		t.Fatalf("context info = %q", info)
	}
}

func TestDashboardWebSocketRejectsWhenFull(t *testing.T) {
	server := &server{pool: &Pool{}, stats: newStats()}
	server.dashboardConnections.Store(dashboardMaxConnections)
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/dashboard/ws", nil)
	if conn != nil {
		conn.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response = %v, error = %v", response, err)
	}
	response.Body.Close()
}
