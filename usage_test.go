package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	account.dead = "reauth required"
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
