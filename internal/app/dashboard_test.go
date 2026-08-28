package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRootRedirectsToDashboard(t *testing.T) {
	server := &server{pool: &Pool{}, stats: newStatsWithPrices(testPriceSnapshot(t))}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()

	server.routes().ServeHTTP(response, request)

	if response.Code != http.StatusPermanentRedirect || response.Header().Get("Location") != "/dashboard" {
		t.Fatalf("status = %d, location = %q", response.Code, response.Header().Get("Location"))
	}
}

func TestDashboardPageConnectsHTMXSSE(t *testing.T) {
	server := &server{pool: &Pool{}, stats: newStatsWithPrices(testPriceSnapshot(t))}
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
	if !strings.Contains(response.Header.Get("Content-Security-Policy"), "connect-src 'self'") || strings.Contains(response.Header.Get("Content-Security-Policy"), "ws:") {
		t.Fatalf("dashboard CSP permits obsolete WebSocket connections: %q", response.Header.Get("Content-Security-Policy"))
	}
	for _, expected := range []string{
		`<link rel="icon" href="/favicon.svg" type="image/svg+xml">`,
		`<link rel="stylesheet" href="` + waterCSSURL + `">`,
		`src="` + dashboardAssetURL("dashboard.js") + `"`,
		`src="/dashboard/assets/htmx-2.0.10.min.js"`,
		`src="/dashboard/assets/idiomorph-0.7.4.min.js"`,
		`src="/dashboard/assets/sse-2.2.4.min.js"`,
		`hx-ext="sse,morph"`,
		`sse-connect="/dashboard/events"`,
		`sse-swap="dashboard"`,
		`hx-swap="none"`,
		`id="stream-status" data-state="connecting" aria-live="polite">connecting`,
		`id="dashboard"`,
		`table { width: max-content; min-width: 100%;`,
		`.status-mark.status-live { color: var(--green-11) }`,
		`<h2>Business &amp; Enterprise <span id="workspace-summary">0 not routed</span></h2>`,
		`id="workspaces"`,
		`<h2>Active Threads&nbsp; <span id="routing-count">0</span></h2>`,
		`no live threads`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q", expected)
		}
	}
	overview := strings.Index(body, `<h2>Overview `)
	accounts := strings.Index(body, `<h2>Accounts <span id="summary">`)
	if overview < 0 || accounts < 0 || overview > accounts {
		t.Fatalf("dashboard section order: overview = %d, accounts = %d", overview, accounts)
	}
	if strings.Contains(body, `<h2>Resources</h2>`) || strings.Contains(body, `id="resources"`) {
		t.Fatal("dashboard has a Resources section")
	}
	for _, obsolete := range []string{`ws-connect=`, `hx-ext="ws`, `/dashboard/ws`} {
		if strings.Contains(body, obsolete) {
			t.Fatalf("dashboard still contains WebSocket markup %q", obsolete)
		}
	}
}

func TestWebAssetsAreServedFromBinary(t *testing.T) {
	server := &server{pool: &Pool{}, stats: newStatsWithPrices(testPriceSnapshot(t))}
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
		{"/dashboard/assets/idiomorph-0.7.4.min.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", 1_000},
		{"/dashboard/assets/sse-2.2.4.min.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", 1_000},
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

func TestDashboardSSEStreamsEscapedHTML(t *testing.T) {
	stats := newStatsWithPrices(testPriceSnapshot(t))
	stats.activateThread("019fe5c2private")
	stats.accepted("", "019fe5c2private", "019fe5c2private", "203.0.113.42", "ret", "unused", "gpt-5.6-sol", "high", serviceTierFast, turnMetadata{}, true)
	stats.recordUsage("019fe5c2private", "unused", "gpt-5.6-sol", "high", "default", responseUsage{OutputTokens: 1_000_000})
	stats.failedOver("unused", "<script>upstream unavailable</script>")
	tokenPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"alice@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"unused","chatgpt_plan_type":"pro"}}`))
	account := accountFromState(accountState{IDToken: "x." + tokenPayload + ".x"})
	server := &server{
		pool:      &Pool{accounts: []*Account{account}},
		stats:     stats,
		countries: countryResolver{states: map[string]countryState{"203.0.113.42": {code: "US", ready: true}}},
	}
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/dashboard/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status = %s, content type = %q", response.Status, response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Cache-Control") != "no-cache" || response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("cache control = %q, buffering = %q", response.Header.Get("Cache-Control"), response.Header.Get("X-Accel-Buffering"))
	}
	event, payload, err := readSSEEvent(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if event != dashboardEventName {
		t.Fatalf("event = %q, want %q", event, dashboardEventName)
	}
	body := payload
	for _, expected := range []string{
		`id="overview" class="overview" hx-swap-oob="morph"`,
		`id="summary" hx-swap-oob="morph"`,
		`id="accounts" class="scroll" hx-swap-oob="morph"`,
		`id="workspace-summary" hx-swap-oob="morph"`,
		`id="workspaces" class="scroll" hx-swap-oob="morph"`,
		`id="routing-count" hx-swap-oob="morph"`,
		`id="threads" class="scroll" hx-swap-oob="morph"`,
		`id="events" class="scroll" hx-swap-oob="morph"`,
		`a***e@***.com`,
		`<td class="dim">pro</td>`,
		`019fe5c2`,
		`🇺🇸 US-ret`,
		`<td>☀️ high</td>`,
		`<td class="status"><span class="status-mark status-checking">◌</span> checking</td>`,
		`<span>1 checking</span>`,
		`<th>WS</th>`,
		`<span role="img" aria-label="Fast">⚡️</span>`,
		`API estimate`,
		`$30.00`,
		`<td>connection retry</td>`,
		`&lt;script&gt;upstream unavailable&lt;/script&gt;`,
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
	for _, private := range []string{"alice@example.com", "019fe5c2private", "203.0.113.42", "<script>"} {
		if strings.Contains(body, private) {
			t.Fatalf("dashboard update exposed %q", private)
		}
	}
}

func readSSEEvent(r io.Reader) (string, string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1_024), 4<<20)
	event := ""
	data := make([]string, 0)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			return event, strings.Join(data, "\n"), nil
		}
		if value, found := strings.CutPrefix(line, "event:"); found {
			event = strings.TrimSpace(value)
		}
		if value, found := strings.CutPrefix(line, "data:"); found {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}
	return "", "", io.ErrUnexpectedEOF
}

func TestWriteSSEEventPreservesMultilineHTML(t *testing.T) {
	var output bytes.Buffer
	if err := writeSSEEvent(&output, dashboardEventName, []byte("first\n\nthird")); err != nil {
		t.Fatal(err)
	}
	want := "event: dashboard\ndata: first\ndata: \ndata: third\n\n"
	if output.String() != want {
		t.Fatalf("SSE event = %q, want %q", output.String(), want)
	}
}

func TestDashboardChangesOnlyRenderChangedFragments(t *testing.T) {
	previous := make(map[string][]byte)
	view := dashboardView{}

	initial, err := renderDashboardChanges(view, previous)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"overview", "summary", "accounts", "workspace-summary", "workspaces", "routing-count", "threads", "events"} {
		if !strings.Contains(string(initial), `id="`+id+`"`) {
			t.Fatalf("initial dashboard update missing %q: %s", id, initial)
		}
	}

	unchanged, err := renderDashboardChanges(view, previous)
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged) != 0 {
		t.Fatalf("unchanged dashboard rendered %q", unchanged)
	}

	view.Summary = []dashboardCount{{Count: 1, Label: "live"}}
	changed, err := renderDashboardChanges(view, previous)
	if err != nil {
		t.Fatal(err)
	}
	body := string(changed)
	if !strings.Contains(body, `id="summary"`) || !strings.Contains(body, `1 live`) {
		t.Fatalf("changed dashboard missing summary: %s", body)
	}
	for _, id := range []string{"overview", "accounts", "workspace-summary", "workspaces", "routing-count", "threads", "events"} {
		if strings.Contains(body, `id="`+id+`"`) {
			t.Fatalf("summary change unnecessarily rendered %q: %s", id, body)
		}
	}
}

func TestDashboardStreamRootAttributesMatchPage(t *testing.T) {
	view := dashboardView{}
	page, err := renderDashboard("dashboard", view)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := renderDashboardChanges(view, make(map[string][]byte))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"overview", "summary", "accounts", "workspace-summary", "workspaces", "routing-count", "threads", "events"} {
		pageTag := dashboardOpeningTag(t, string(page), id)
		streamTag := dashboardOpeningTag(t, string(stream), id)
		streamTag = strings.Replace(streamTag, ` hx-swap-oob="morph"`, "", 1)
		if streamTag != pageTag {
			t.Errorf("%s root differs:\npage:   %s\nstream: %s", id, pageTag, streamTag)
		}
	}
}

func dashboardOpeningTag(t *testing.T, payload, id string) string {
	t.Helper()
	marker := `id="` + id + `"`
	idStart := strings.Index(payload, marker)
	if idStart < 0 {
		t.Fatalf("missing %q in %s", marker, payload)
	}
	tagStart := strings.LastIndex(payload[:idStart], "<")
	tagEnd := strings.Index(payload[idStart:], ">")
	if tagStart < 0 || tagEnd < 0 {
		t.Fatalf("invalid opening tag for %q in %s", marker, payload)
	}
	return payload[tagStart : idStart+tagEnd+1]
}

func TestDashboardDOMIDsAreStableAndOpaque(t *testing.T) {
	const secret = "private-account-or-thread-key"
	first := dashboardDOMID("thread", secret)
	second := dashboardDOMID("thread", secret)
	if first != second {
		t.Fatalf("dashboard DOM ID changed from %q to %q", first, second)
	}
	if !strings.HasPrefix(first, "thread-") || strings.Contains(first, secret) {
		t.Fatalf("dashboard DOM ID %q is not opaque", first)
	}
	if first == dashboardDOMID("thread", "other-key") {
		t.Fatalf("different keys produced dashboard DOM ID %q", first)
	}
}

func TestDashboardAccountValuesOmitRedundantUnitsAndZeros(t *testing.T) {
	now := time.Now()
	account := testAccount("account-a", 20)
	account.adoptResetCredits(now, 0, nil)
	resetAt := now.Add(3 * 24 * time.Hour)
	account.secondary = window{usedPercent: 20, minutes: 7 * 24 * 60, resetsAt: resetAt, seenAt: now}
	account.adoptCreditBurn(now, 1_234.56)
	other := testAccount("account-b", 20)
	stats := newStatsWithPrices(testPriceSnapshot(t))
	stats.applyRouted(now.Add(-25*time.Hour), "", "", "account-a", "", "", "", turnMetadata{})
	stats.applyRouted(now, "", "", "account-a", "", "", "", turnMetadata{})
	for range 100 {
		stats.applyRouted(now, "", "", "account-b", "", "", "", turnMetadata{})
	}
	server := &server{pool: &Pool{accounts: []*Account{account, other}}, stats: stats}

	view := server.currentDashboard(now)
	if len(view.Accounts) != 2 {
		t.Fatalf("accounts = %d, want two", len(view.Accounts))
	}
	accountView := view.Accounts[0]
	wantBurnInfo := "Approximate since reset at " + resetAt.Add(-7*24*time.Hour).Format("2 January 2006, 15:04 MST") + ". Daily analytics includes the full reset day."
	if accountView.Weekly != "80" || accountView.Banked != "" || accountView.CreditBurn != "1234.56" || accountView.CreditBurnInfo != wantBurnInfo || accountView.Traffic != "1" {
		t.Fatalf("account values = %+v", accountView)
	}
	if view.Accounts[1].CreditBurn != "--" || view.Accounts[1].CreditBurnInfo != "" {
		t.Fatalf("unknown credit burn = %+v", view.Accounts[1])
	}
	if view.Accounts[1].Traffic != "99" {
		t.Fatalf("other account traffic = %q, want 99", view.Accounts[1].Traffic)
	}
	payload, err := renderDashboard("dashboard", view)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, expected := range []string{"<th>Weekly %</th>", "<th>Credits burn ≈</th>", "<th>Traffic 24h %</th>", "<th>Activity 24h</th>", ">1234.56</span>"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q", expected)
		}
	}
	for _, removed := range []string{"<th>Turns</th>", "<th>Limits 24h</th>"} {
		if strings.Contains(body, removed) {
			t.Fatalf("dashboard contains removed column %q", removed)
		}
	}
}

func TestDashboardSeparatesManagedWorkspacesAndShowsSpendControl(t *testing.T) {
	now := time.Date(2026, time.August, 28, 14, 0, 0, 0, time.UTC)
	usedPercent := 48.0
	remainingPercent := 52.0
	business := testAccountWithPlan("business", 0, "business")
	business.spendControl = &spendControlPayload{
		IndividualLimit: &spendControlLimit{
			Source:           "account_user_spend_controls",
			Limit:            "150000",
			Used:             "71549.42661845684",
			Remaining:        "78450.57338154316",
			UsedPercent:      &usedPercent,
			RemainingPercent: &remainingPercent,
			ResetAt:          now.Add(4 * 24 * time.Hour).Unix(),
		},
	}
	enterprise := testAccountWithPlan("enterprise", 0, "enterprise")
	routable := testAccount("routable", 20)
	server := &server{
		pool:  &Pool{accounts: []*Account{business, enterprise, routable}},
		stats: newStatsWithPrices(testPriceSnapshot(t)),
	}

	view := server.currentDashboard(now)
	if len(view.Accounts) != 1 || view.Accounts[0].Plan != "pro" {
		t.Fatalf("routable accounts = %+v", view.Accounts)
	}
	if len(view.Workspaces) != 2 {
		t.Fatalf("workspaces = %+v, want business and enterprise", view.Workspaces)
	}
	workspace := view.Workspaces[0]
	if workspace.Plan != "business" || workspace.Status != accountNotRouted || workspace.Limit != "150000" || workspace.Used != "71549.43" || workspace.Remaining != "78450.57" || workspace.UsedPercent != "48" || workspace.ResetIn != "4d0h" {
		t.Fatalf("business workspace = %+v", workspace)
	}
	if view.Workspaces[1].Limit != "--" || view.Workspaces[1].Used != "--" || view.Workspaces[1].ResetIn != "--" {
		t.Fatalf("enterprise workspace without spend data = %+v", view.Workspaces[1])
	}
	if len(view.Summary) != 1 || view.Summary[0] != (dashboardCount{Count: 1, Label: "live"}) {
		t.Fatalf("routable summary = %+v", view.Summary)
	}

	payload, err := renderDashboard("dashboard", view)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, expected := range []string{
		`<h2>Business &amp; Enterprise <span id="workspace-summary">2 not routed</span></h2>`,
		`<th>Credit limit</th>`,
		`<th>Remaining</th>`,
		`<span class="status-mark status-not_routed">○</span> not routed`,
		`>71549.43</td>`,
		`>78450.57</td>`,
		`>4d0h</td>`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q:\n%s", expected, body)
		}
	}
}

func TestDashboardEstimatesWhetherPoolCapacityWillLast(t *testing.T) {
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		used           []float64
		wantMark       string
		wantInfo       string
		wantInfoStrong string
	}{
		{name: "yes", used: []float64{0, 20}, wantMark: "👍", wantInfo: "Lasts to reset.\nAt the average burn since reset. Expected capacity at reset: ", wantInfoStrong: "4.29%"},
		{name: "close", used: []float64{0, 30}, wantMark: "👎", wantInfo: "Runs out in 5d16h.\nAt the average burn since reset. Expected to run out: ", wantInfoStrong: "20 August 2026, 04:00 UTC"},
		{name: "no", used: []float64{20, 30}, wantMark: "👎", wantInfo: "Runs out in 3d0h.\nAt the average burn since reset. Expected to run out: ", wantInfoStrong: "17 August 2026, 12:00 UTC"},
		{name: "empty", used: []float64{100, 100}, wantMark: "👎", wantInfo: "Empty.\nNothing left until reset."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accounts := make([]*Account, len(test.used))
			for i, used := range test.used {
				account := testAccount(fmt.Sprintf("account-%d", i), used)
				account.secondary = window{
					usedPercent: used,
					minutes:     7 * 24 * 60,
					resetsAt:    now.Add(6 * 24 * time.Hour),
					seenAt:      now,
				}
				accounts[i] = account
			}
			server := &server{pool: &Pool{accounts: accounts}, stats: newStatsWithPrices(testPriceSnapshot(t))}

			view := server.currentDashboard(now)
			if view.Overview[0].Name != "Pace" || view.Overview[0].Value != test.wantMark || view.Overview[0].Info != test.wantInfo || view.Overview[0].InfoStrong != test.wantInfoStrong {
				t.Fatalf("pace metric = %+v, want %q", view.Overview[0], test.wantMark)
			}
			payload, err := renderDashboard("dashboard", view)
			if err != nil {
				t.Fatal(err)
			}
			body := string(payload)
			expected := `<dt>Pace</dt><dd><span id="metric-`
			renderedInfoStrong := strings.ReplaceAll(test.wantInfoStrong, "+", "&#43;")
			renderedWant := strings.ReplaceAll(test.wantMark, "+", "&#43;")
			checks := []string{expected, `class="has-tooltip"`, `data-tooltip="` + test.wantInfo + `"`, ">" + renderedWant + "</span>"}
			if test.wantInfoStrong != "" {
				checks = append(checks, `data-tooltip-strong="`+renderedInfoStrong+`"`)
			}
			for _, check := range checks {
				if !strings.Contains(body, check) {
					t.Fatalf("dashboard missing %q:\n%s", check, body)
				}
			}
			if strings.Contains(body, "<th>Pace</th>") {
				t.Fatalf("dashboard has per-account pace column:\n%s", body)
			}
		})
	}

	monthly := testAccount("monthly", 10)
	monthly.secondary = window{
		usedPercent: 10,
		minutes:     30 * 24 * 60,
		resetsAt:    now.Add(26 * 24 * time.Hour),
		seenAt:      now,
	}
	weekly := testAccount("weekly", 27)
	weekly.secondary = window{
		usedPercent: 27,
		minutes:     7 * 24 * 60,
		resetsAt:    now.Add(6 * 24 * time.Hour),
		seenAt:      now,
	}
	poolServer := &server{pool: &Pool{accounts: []*Account{monthly, weekly}}, stats: newStatsWithPrices(testPriceSnapshot(t))}
	if got := poolServer.currentDashboard(now).Overview[0]; got.Value != "👎" || !strings.HasPrefix(got.Info, "Runs out in ") {
		t.Fatalf("mixed limit windows on track = %+v, want close", got)
	}

	for _, test := range []struct {
		name     string
		proUsed  float64
		goUsed   float64
		wantMark string
		wantInfo string
	}{
		{name: "pro surplus", proUsed: 0, goUsed: 100, wantMark: "👍", wantInfo: "Lasts to reset."},
		{name: "pro shortfall", proUsed: 30, goUsed: 0, wantMark: "👎", wantInfo: "Runs out in "},
	} {
		t.Run(test.name, func(t *testing.T) {
			pro := testAccountWithPlan("pro", test.proUsed, "pro")
			pro.secondary = window{usedPercent: test.proUsed, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
			goAccount := testAccountWithPlan("go", test.goUsed, "go")
			goAccount.secondary = window{usedPercent: test.goUsed, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
			server := &server{pool: &Pool{accounts: []*Account{pro, goAccount}}, stats: newStatsWithPrices(testPriceSnapshot(t))}
			if got := server.currentDashboard(now).Overview[0]; got.Value != test.wantMark || !strings.HasPrefix(got.Info, test.wantInfo) {
				t.Fatalf("plan-weighted on track = %+v, want %q", got, test.wantMark)
			}
		})
	}
}

func TestWeeklyPlanCapacity(t *testing.T) {
	for plan, want := range map[string]float64{
		"go":      1_134,
		"plus":    7_560,
		"prolite": 37_800,
		"pro":     50_400,
	} {
		if got := weeklyPlanCapacity(plan); got != want {
			t.Fatalf("%s weekly capacity = %v, want %v", plan, got, want)
		}
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
	account.adoptResetCredits(now, 2, []resetCredit{
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
	server := &server{pool: &Pool{accounts: []*Account{account}}, stats: newStatsWithPrices(testPriceSnapshot(t))}

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
	server := &server{pool: &Pool{accounts: []*Account{account}}, stats: newStatsWithPrices(testPriceSnapshot(t))}

	payload, err := renderDashboard("dashboard", server.currentDashboard(now))
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, expected := range []string{
		`<span>1 priority</span>`,
		`class="status-mark status-priority"`,
		`>◆</span> priority</span>`,
		`data-tooltip="Prioritized for new connections: a banked reset expires in 30m; 20% weekly capacity remains."`,
		`aria-describedby="dashboard-tooltip"`,
		`tabindex="0"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q:\n%s", expected, body)
		}
	}
}

func TestDashboardExplainsManualRoutingStatus(t *testing.T) {
	now := time.Date(2026, time.August, 12, 20, 0, 0, 0, time.UTC)
	account := testAccount("account-a", 20)
	account.RoutingMode = routingModePriority
	server := &server{pool: &Pool{accounts: []*Account{account}}, stats: newStatsWithPrices(testPriceSnapshot(t))}

	view := server.currentDashboard(now)
	if len(view.Accounts) != 1 || view.Accounts[0].StatusInfo != "Manual priority for new connections." {
		t.Fatalf("account view = %+v", view.Accounts)
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
	stats.wsOpen = 3
	stats.apiCostNanoDollars = 7_746_820_000_000
	server := &server{pool: &Pool{}, stats: stats}
	view := server.currentDashboard(now)
	wantValues := map[string]string{
		"Pace":          "❔",
		"active WS":     "3",
		"CPU":           "--",
		"RAM":           "--",
		"network in":    "--",
		"network out":   "--",
		"input tokens":  "2K",
		"cached input":  "1.5K",
		"output tokens": "300",
		"uptime":        "28m",
	}
	wantNames := []string{"Pace", "active WS", "CPU", "RAM", "network in", "network out", "uptime", "input tokens", "cached input", "output tokens", "API estimate"}
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
			if metric.Name != "Pace" && metric.Name != "active WS" && metric.Name != "CPU" && metric.Name != "RAM" && metric.Name != "network in" && metric.Name != "network out" && metric.Name != "uptime" && metric.Info != wantInfo {
				t.Fatalf("%s overview info = %q, want %q", metric.Name, metric.Info, wantInfo)
			}
			delete(wantValues, metric.Name)
		}
		switch metric.Name {
		case "Pace":
			want := "Unknown.\nNot enough limit data to estimate whether the pool will last."
			if metric.Info != want {
				t.Fatalf("Pace overview info = %q, want %q", metric.Info, want)
			}
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
	stats.applyRouted(now, "thread", "203.0.113.42", "account", "gpt-5.6-sol", "xhigh", "", metadata)
	stats.threads["thread"].apiKeySuffix = "ret"
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
		pool:      &Pool{},
		stats:     stats,
		catalog:   catalog,
		countries: countryResolver{states: map[string]countryState{"203.0.113.42": {code: "US", ready: true}}},
	}

	view := server.currentDashboard(now)
	if len(view.Threads) != 1 {
		t.Fatalf("routing rows = %d, want one", len(view.Threads))
	}
	thread := view.Threads[0]
	if thread.Client != "🇺🇸 US-ret" || thread.Model != "☀️ xhigh" || thread.UncachedInput != "500" || thread.CacheRate != "75" || thread.Output != "300" || thread.ContextUsed != "0% (1)" || thread.Latency != "2s" || thread.Requests != "1" || thread.Cost != "$0.012" {
		t.Fatalf("routing row = %+v", thread)
	}
	if thread.Info != "Request: compaction\nCodex thread: 019fe5c2\nTurn: 019fe730\nAgent: compact" || !strings.Contains(thread.ContextInfo, "Context window: 258.4K") || !strings.Contains(thread.ContextInfo, "Auto compact at: 244.8K") || !strings.Contains(thread.ContextInfo, "Tokens used: 2.3K") || !strings.Contains(thread.ContextInfo, "Context used: 0%") || !strings.Contains(thread.ContextInfo, "Compactions: 1") || thread.LatencyInfo != "First byte: 500ms\nTotal: 2s" {
		t.Fatalf("routing details = %+v", thread)
	}
	payload, err := renderDashboard("dashboard", view)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, expected := range []string{"<td class=\"dim\">🇺🇸 US-ret</td>", "<th>Model (thinking mode)</th>", "<td>☀️ xhigh</td>", "<th>Cache %</th>", "<th>Context used<br>Compactions</th>", "<th>Cost</th>", "<td>$0.012</td>", "Codex thread: 019fe5c2", "Auto compact at: 244.8K", "Compactions: 1"} {
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
	stats.applyRouted(now, "thread", "203.0.113.42", "account", "gpt-5.6-sol", "xhigh", "", turnMetadata{})
	stats.applyRouted(now.Add(time.Second), "thread", "203.0.113.42", "account", "gpt-5.6-luna", "low", "", turnMetadata{})
	stats.applyUsageAt(now.Add(2*time.Second), "thread", "account", "gpt-5.6-sol", "xhigh", "default", responseUsage{InputTokens: 1_000})
	stats.applyUsageAt(now.Add(3*time.Second), "thread", "account", "gpt-5.6-luna", "low", "default", responseUsage{InputTokens: 1_000})
	server := &server{
		pool:    &Pool{},
		stats:   stats,
		catalog: newModelCatalog(),
	}

	view := server.currentDashboard(now.Add(3 * time.Second))
	if len(view.Threads) != 1 {
		t.Fatalf("routing rows = %d, want one", len(view.Threads))
	}
	thread := view.Threads[0]
	if thread.Model != "🔀 mixed" || thread.ModelInfo != "gpt-5.6-sol xhigh\ngpt-5.6-luna low" {
		t.Fatalf("mixed model = %q with info %q", thread.Model, thread.ModelInfo)
	}
	payload, err := renderDashboard("dashboard", view)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	if !strings.Contains(body, "data-tooltip=\"gpt-5.6-sol xhigh\ngpt-5.6-luna low\"") || !strings.Contains(body, `>🔀 mixed</span>`) {
		t.Fatalf("mixed model tooltip missing from dashboard: %s", body)
	}
}

func TestDashboardModel(t *testing.T) {
	for _, test := range []struct {
		model  string
		effort string
		want   string
	}{
		{"gpt-5.6-sol", "xhigh", "☀️ xhigh"},
		{"gpt-5.6-terra", "medium", "🌍 medium"},
		{"gpt-5.6-luna", "low", "🌙 low"},
		{"gpt-5.6-luna-2026-08-01", "", "🌙"},
		{"gpt-5.4", "", "gpt-5.4"},
	} {
		if got := dashboardModel(test.model, test.effort); got != test.want {
			t.Errorf("dashboardModel(%q, %q) = %q, want %q", test.model, test.effort, got, test.want)
		}
	}
}

func TestDashboardClientNameUsesCountryAndAPIKeySuffix(t *testing.T) {
	resolver := &countryResolver{states: map[string]countryState{"1.1.1.1": {code: "AU", ready: true}}}
	if got := dashboardClientName(ThreadSnapshot{ClientIP: "1.1.1.1", APIKeySuffix: "xyz"}, resolver); got != "🇦🇺 AU-xyz" {
		t.Fatalf("client name = %q, want country and token suffix", got)
	}
	if got := dashboardClientName(ThreadSnapshot{}, resolver); got != "Unknown" {
		t.Fatalf("unknown client name = %q", got)
	}
}

func TestDashboardContextShowsPercentAndCompactions(t *testing.T) {
	limits := modelContextLimits{Window: 112_000, AutoCompact: 100_000}
	if got := dashboardContextUsed(12_000, limits, 0); got != "0%" {
		t.Fatalf("context = %q, want 0%%", got)
	}
	if got := dashboardContextUsed(62_000, limits, 7); got != "50% (7)" {
		t.Fatalf("context = %q, want 50%% (7)", got)
	}
	if got := dashboardContextUsed(120_000, limits, 0); got != "100%" {
		t.Fatalf("context = %q, want 100%%", got)
	}
	if info := dashboardContextInfo(62_000, limits, 0); !strings.Contains(info, "Context used: 50%\nCompactions: 0") {
		t.Fatalf("context info = %q", info)
	}
}

func TestDashboardSSERejectsWhenFull(t *testing.T) {
	server := &server{pool: &Pool{}, stats: newStatsWithPrices(testPriceSnapshot(t))}
	server.dashboardStreams.Store(dashboardMaxStreams)
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/dashboard/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %s", response.Status)
	}
}
