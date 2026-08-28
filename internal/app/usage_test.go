package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestUsagePollLimitReachedRemovesAccountFromNewRouting(t *testing.T) {
	account := testAccount("account-a", 50)
	roomier := testAccount("account-b", 20)
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage" {
			http.NotFound(w, r)
			return
		}
		used := 100.0
		json.NewEncoder(w).Encode(map[string]any{
			"rate_limit": map[string]any{
				"limit_reached":    true,
				"primary_window":   map[string]any{"used_percent": used, "limit_window_seconds": 300},
				"secondary_window": map[string]any{"used_percent": used, "limit_window_seconds": 604800},
			},
		})
	}))
	defer usage.Close()
	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = usage.URL
	t.Cleanup(func() { accountAPIBaseURL = oldBaseURL })

	server := &server{
		pool:   &Pool{accounts: []*Account{roomier, account}},
		client: usage.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := server.pollUsage(context.Background(), account); err != nil {
		t.Fatal(err)
	}

	if picked := server.pool.route(nil, nil).account; picked == nil || picked.id() != "account-b" {
		t.Fatalf("picked = %v, want account-b", picked)
	}
}

func TestUsagePollPositiveCapacityReturnsSpentAccountToRouting(t *testing.T) {
	account := testAccount("account-a", 100)
	account.markSpent()
	account.cooldown = time.Now().Add(time.Hour)
	roomier := testAccount("account-b", 20)
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"rate_limit": map[string]any{
				"primary_window":   map[string]any{"used_percent": 99, "limit_window_seconds": 300},
				"secondary_window": map[string]any{"used_percent": 99, "limit_window_seconds": 604800},
			},
		})
	}))
	defer usage.Close()
	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = usage.URL
	t.Cleanup(func() { accountAPIBaseURL = oldBaseURL })

	server := &server{
		pool:   &Pool{accounts: []*Account{roomier, account}},
		client: usage.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := server.pollUsage(context.Background(), account); err != nil {
		t.Fatal(err)
	}

	roomier.Paused = true
	if picked := server.pool.route(nil, nil).account; picked == nil || picked.id() != "account-a" {
		t.Fatalf("picked = %v, want account-a", picked)
	}
}

func TestUsagePollAdoptsCurrentPlan(t *testing.T) {
	account := testAccountWithPlan("account-a", 20, "pro")
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "prolite",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{"used_percent": 20, "limit_window_seconds": 300},
			},
		})
	}))
	defer usage.Close()
	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = usage.URL
	t.Cleanup(func() { accountAPIBaseURL = oldBaseURL })
	server := &server{
		pool:   &Pool{accounts: []*Account{account}},
		stats:  newStatsWithPrices(testPriceSnapshot(t)),
		client: usage.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := server.pollUsage(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	stats := server.currentStats(time.Now())
	if account.plan() != "prolite" || len(stats.Accounts) != 1 || stats.Accounts[0].Plan != "prolite" {
		t.Fatalf("account plan = %q, stats = %+v", account.plan(), stats.Accounts)
	}
}

func TestUsagePollAdoptsManagedWorkspaceSpendControl(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	resetAt := now.Add(4 * 24 * time.Hour)
	account := testAccountWithPlan("account-a", 20, "business")
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"plan_type":  "business",
			"rate_limit": nil,
			"spend_control": map[string]any{
				"reached": false,
				"individual_limit": map[string]any{
					"source":              "account_user_spend_controls",
					"limit":               "150000",
					"used":                "71549.42661845684",
					"remaining":           "78450.57338154316",
					"used_percent":        48,
					"remaining_percent":   52,
					"reset_after_seconds": int64((4 * 24 * time.Hour) / time.Second),
					"reset_at":            resetAt.Unix(),
				},
			},
		})
	}))
	defer usage.Close()
	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = usage.URL
	t.Cleanup(func() { accountAPIBaseURL = oldBaseURL })
	server := &server{
		pool:   &Pool{accounts: []*Account{account}},
		stats:  newStatsWithPrices(testPriceSnapshot(t)),
		client: usage.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := server.pollUsage(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	stats := server.currentStats(now)
	if len(stats.Accounts) != 1 || stats.Accounts[0].Status != accountNotRouted {
		t.Fatalf("accounts = %+v, want one not-routed workspace", stats.Accounts)
	}
	spend := stats.Accounts[0].SpendControl
	if spend == nil || spend.Source != "account_user_spend_controls" || spend.Limit != "150000" || spend.Used != "71549.42661845684" || spend.Remaining != "78450.57338154316" {
		t.Fatalf("spend control = %+v", spend)
	}
	if spend.UsedPercent == nil || *spend.UsedPercent != 48 || spend.RemainingPercent == nil || *spend.RemainingPercent != 52 || spend.ResetAt == nil || !spend.ResetAt.Equal(resetAt) {
		t.Fatalf("spend control percentages/reset = %+v", spend)
	}
	if picked := server.pool.route(nil, nil).account; picked != nil {
		t.Fatalf("picked = %s, want managed workspace excluded", picked.id())
	}
}

func TestUsagePollRefreshesExpiringAccessToken(t *testing.T) {
	refreshCalls := useOAuthRefreshServer(t)
	server := newTestServer(t, []*Account{testAccount("account-a", 20)})
	account := server.pool.find("account-a")
	account.mu.Lock()
	account.AccessToken = accessTokenExpiringAt(time.Now().Add(tokenRefreshLead))
	account.mu.Unlock()
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer refreshed-token" {
			t.Errorf("authorization = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "pro",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{"used_percent": 20, "limit_window_seconds": 300},
			},
		})
	}))
	defer usage.Close()
	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = usage.URL
	t.Cleanup(func() { accountAPIBaseURL = oldBaseURL })

	if err := server.pollUsage(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if refreshCalls() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls())
	}
}

func TestExpiringResetCreditChoosesEarliestEligibleCredit(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Minute)
	soon := now.Add(2 * time.Minute)
	threshold := now.Add(24 * time.Hour)
	later := now.Add(24*time.Hour + time.Second)
	credits := []resetCredit{
		{ID: "future", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: &later},
		{ID: "wrong-type", ResetType: "other", Status: "available", ExpiresAt: &soon},
		{ID: "used", ResetType: "codex_rate_limits", Status: "redeemed", ExpiresAt: &soon},
		{ID: "expired", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: &expired},
		{ID: "threshold", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: &threshold},
		{ID: "soon", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: &soon},
	}

	credit, ok := expiringResetCredit(credits, now)
	if !ok || credit.ID != "soon" {
		t.Fatalf("credit = %q, %v", credit.ID, ok)
	}
	if credit, ok := expiringResetCredit([]resetCredit{{ID: "threshold", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: &threshold}}, now); !ok || credit.ID != "threshold" {
		t.Fatalf("threshold credit = %q, %v", credit.ID, ok)
	}
	if credit, ok := expiringResetCredit([]resetCredit{{ID: "future", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: &later}}, now); ok {
		t.Fatalf("future credit = %q", credit.ID)
	}
}

func TestCreditBurnPollSumsCurrentCycle(t *testing.T) {
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.FixedZone("BST", 60*60))
	resetAt := time.Date(2026, time.August, 22, 12, 0, 0, 0, now.Location())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/analytics/daily-workspace-usage-counts" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query(); got.Get("start_date") != "2026-08-15" || got.Get("end_date") != "2026-08-20" || got.Get("group_by") != "day" || got.Get("workspace_user") != "true" {
			t.Errorf("query = %v", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-account-a" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("chatgpt-account-id"); got != "account-a" {
			t.Errorf("account = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"totals": map[string]any{"credits": 1_234.5}},
				{"totals": map[string]any{"credits": 67.89}},
			},
		})
	}))
	defer upstream.Close()

	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = upstream.URL
	t.Cleanup(func() { accountAPIBaseURL = oldBaseURL })
	account := testAccount("account-a", 0)
	account.secondary = window{usedPercent: 20, minutes: 7 * 24 * 60, resetsAt: resetAt, seenAt: now}
	server := &server{client: upstream.Client()}
	if err := server.pollCreditBurn(context.Background(), account, now); err != nil {
		t.Fatal(err)
	}
	got, since, known := account.creditBurnSinceReset(now)
	if !known || got != 1_302.39 || !since.Equal(resetAt.Add(-7*24*time.Hour)) {
		t.Fatalf("credit burn = %v since %v, %v", got, since, known)
	}
	if got, since, known := account.creditBurnSinceReset(resetAt.Add(time.Second)); known {
		t.Fatalf("next cycle credit burn = %v since %v, true, want unknown", got, since)
	}
}

func TestPollAllUsageRefreshesResetCreditsWithoutConsuming(t *testing.T) {
	now := time.Now().UTC()
	resetAt := now.Add(3 * 24 * time.Hour)
	calls := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer token-account-a" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("chatgpt-account-id"); got != "account-a" {
			t.Errorf("account = %q", got)
		}

		switch r.Method + " " + r.URL.Path {
		case "GET /usage":
			json.NewEncoder(w).Encode(map[string]any{
				"rate_limit": map[string]any{
					"primary_window":   map[string]any{"used_percent": 80, "limit_window_seconds": 300},
					"secondary_window": map[string]any{"used_percent": 80, "limit_window_seconds": 604800, "reset_at": resetAt.Unix()},
				},
				"rate_limit_reset_credits": map[string]any{"available_count": 1},
			})
		case "GET /analytics/daily-workspace-usage-counts":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"totals": map[string]any{"credits": 12.5}}}})
		case "GET /rate-limit-reset-credits":
			json.NewEncoder(w).Encode(map[string]any{
				"available_count": 1,
				"credits": []map[string]any{{
					"id":         "credit-a",
					"reset_type": "codex_rate_limits",
					"status":     "available",
					"expires_at": now.Add(30 * time.Minute).Format(time.RFC3339),
				}},
			})
		case "POST /rate-limit-reset-credits/consume":
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("content type = %q", got)
			}
			var request consumeResetCreditRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if want := (consumeResetCreditRequest{RedeemRequestID: "credit-a", CreditID: "credit-a"}); request != want {
				t.Errorf("request = %+v, want %+v", request, want)
			}
			json.NewEncoder(w).Encode(consumeResetCreditResponse{Code: "reset", WindowsReset: 2})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = upstream.URL
	t.Cleanup(func() {
		accountAPIBaseURL = oldBaseURL
	})

	account := testAccount("account-a", 0)
	s := &server{
		pool:   &Pool{accounts: []*Account{account}},
		stats:  newStatsWithPrices(priceSnapshot{}),
		client: upstream.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s.pollAllUsage(context.Background())

	wantCalls := []string{
		"GET /usage",
		"GET /analytics/daily-workspace-usage-counts",
		"GET /rate-limit-reset-credits",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
	if got, _, known := account.creditBurnSinceReset(time.Now()); !known || got != 12.5 {
		t.Fatalf("credit burn = %v, %v, want 12.5, true", got, known)
	}
}

func TestPollAllUsageSkipsAccountsNeedingReauth(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer upstream.Close()

	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = upstream.URL
	t.Cleanup(func() {
		accountAPIBaseURL = oldBaseURL
	})

	account := testAccount("account-a", 0)
	account.Reauth = "reauth required"
	s := &server{
		pool:   &Pool{accounts: []*Account{account}},
		stats:  newStatsWithPrices(priceSnapshot{}),
		client: upstream.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	s.pollAllUsage(context.Background())

	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestUsageSnapshotsSurviveRestartAndSuppressFreshPolls(t *testing.T) {
	now := time.Now().UTC()
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/usage":
			json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "prolite",
				"rate_limit": map[string]any{
					"primary_window":   map[string]any{"used_percent": 25, "limit_window_seconds": 300, "reset_at": now.Add(4 * time.Minute).Unix()},
					"secondary_window": map[string]any{"used_percent": 40, "limit_window_seconds": 604800, "reset_at": now.Add(5 * 24 * time.Hour).Unix()},
				},
			})
		case "/analytics/daily-workspace-usage-counts":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"totals": map[string]any{"credits": 12.5}}}})
		case "/rate-limit-reset-credits":
			json.NewEncoder(w).Encode(map[string]any{"available_count": 2, "credits": []map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = upstream.URL
	t.Cleanup(func() { accountAPIBaseURL = oldBaseURL })

	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.add(testAccount("account-a", 0)); err != nil {
		t.Fatal(err)
	}
	s := &server{
		pool:   pool,
		stats:  newStatsWithPrices(priceSnapshot{}),
		client: upstream.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s.pollAllUsage(context.Background())
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reloaded, err := loadPool(reopened)
	if err != nil {
		t.Fatal(err)
	}
	restored := reloaded.find("account-a")
	if restored == nil || restored.plan() != "prolite" {
		t.Fatalf("restored account = %v", restored)
	}
	if count, _, known := restored.bankedResets(); !known || count != 2 {
		t.Fatalf("reset credits = %d, %v", count, known)
	}
	if credits, _, known := restored.creditBurnSinceReset(time.Now()); !known || credits != 12.5 {
		t.Fatalf("credit burn = %v, %v", credits, known)
	}
	restarted := &server{
		pool:   reloaded,
		stats:  newStatsWithPrices(priceSnapshot{}),
		client: upstream.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	restarted.pollDueUsage(context.Background(), 10*time.Minute)
	if requests != 3 {
		t.Fatalf("restart requests = %d, want 3", requests)
	}
}

func TestManagedWorkspaceSpendControlSurvivesRestart(t *testing.T) {
	resetAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"plan_type":  "enterprise",
			"rate_limit": nil,
			"spend_control": map[string]any{
				"reached": true,
				"individual_limit": map[string]any{
					"limit":        "250000",
					"used":         "250000",
					"remaining":    "0",
					"used_percent": 100,
					"reset_at":     resetAt.Unix(),
				},
			},
		})
	}))
	defer upstream.Close()
	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = upstream.URL
	t.Cleanup(func() { accountAPIBaseURL = oldBaseURL })

	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	account := testAccountWithPlan("account-a", 0, "enterprise")
	if err := pool.add(account); err != nil {
		t.Fatal(err)
	}
	server := &server{
		pool:   pool,
		client: upstream.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := server.pollUsage(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reloaded, err := loadPool(reopened)
	if err != nil {
		t.Fatal(err)
	}
	candidate := reloaded.find("account-a").routingCandidate()
	if candidate.status(time.Now()) != accountNotRouted || candidate.spendControl == nil || !candidate.spendControl.Reached {
		t.Fatalf("restored candidate = %+v", candidate)
	}
	limit := candidate.spendControl.IndividualLimit
	if limit == nil || limit.Limit != "250000" || limit.Used != "250000" || limit.Remaining != "0" || limit.ResetAt != resetAt.Unix() {
		t.Fatalf("restored spend limit = %+v", limit)
	}
}

func TestAccountPollScheduleKeepsOnlyUrgentUsageFrequent(t *testing.T) {
	now := time.Now()
	account := testAccount("account-a", 20)
	account.usageFetchedAt = now
	account.resetCredits.fetchedAt = now
	account.creditBurn.fetchedAt = now
	account.secondary.minutes = 7 * 24 * 60
	account.secondary.resetsAt = now.Add(5 * 24 * time.Hour)

	if usage, creditBurn, resets := account.pollsDue(now.Add(2*time.Minute), 10*time.Minute); usage || creditBurn || resets {
		t.Fatalf("fresh polls due = %v, %v, %v", usage, creditBurn, resets)
	}
	account.primary.usedPercent = 95
	if usage, creditBurn, resets := account.pollsDue(now.Add(2*time.Minute), 10*time.Minute); !usage || creditBurn || resets {
		t.Fatalf("urgent polls due = %v, %v, %v", usage, creditBurn, resets)
	}
	account.primary.usedPercent = 20
	if usage, creditBurn, resets := account.pollsDue(now.Add(time.Hour), 10*time.Minute); !usage || !creditBurn || !resets {
		t.Fatalf("hourly polls due = %v, %v, %v", usage, creditBurn, resets)
	}
}
