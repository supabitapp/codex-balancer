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

func TestUsagePollLimitReachedDoesNotRemoveAccountFromRouting(t *testing.T) {
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

	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{roomier, account}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp")
	})
	defer closeServer()
	if err := server.pollUsage(context.Background(), account); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !reflect.DeepEqual(calls, []string{"account-a"}) {
		t.Fatalf("calls = %v", calls)
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

	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{roomier, account}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp")
	})
	defer closeServer()
	if err := server.pollUsage(context.Background(), account); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !reflect.DeepEqual(calls, []string{"account-a"}) {
		t.Fatalf("calls = %v", calls)
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

func TestPollAllUsageRefreshesResetCreditsWithoutConsuming(t *testing.T) {
	now := time.Now().UTC()
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
					"secondary_window": map[string]any{"used_percent": 80, "limit_window_seconds": 604800},
				},
				"rate_limit_reset_credits": map[string]any{"available_count": 1},
			})
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
		stats:  newStats(),
		client: upstream.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s.pollAllUsage(context.Background())

	wantCalls := []string{
		"GET /usage",
		"GET /rate-limit-reset-credits",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
}
