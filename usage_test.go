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

func TestExpiringResetCreditChoosesEarliestEligibleCredit(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Minute)
	soon := now.Add(2 * time.Minute)
	threshold := now.Add(resetCreditLead)
	later := now.Add(resetCreditLead + time.Second)
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

func TestPollAllUsageConsumesExpiringResetCredit(t *testing.T) {
	now := time.Now().UTC()
	usageCalls := 0
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
			usageCalls++
			used, banked := 100.0, 1
			if usageCalls > 1 {
				used, banked = 0, 0
			}
			json.NewEncoder(w).Encode(map[string]any{
				"rate_limit": map[string]any{
					"primary_window":   map[string]any{"used_percent": used, "limit_window_seconds": 300},
					"secondary_window": map[string]any{"used_percent": used, "limit_window_seconds": 604800},
				},
				"rate_limit_reset_credits": map[string]any{"available_count": banked},
			})
		case "GET /rate-limit-reset-credits":
			json.NewEncoder(w).Encode(map[string]any{
				"available_count": 1,
				"credits": []map[string]any{{
					"id":         "credit-a",
					"reset_type": "codex_rate_limits",
					"status":     "available",
					"expires_at": now.Add(4 * time.Minute).Format(time.RFC3339),
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
		"POST /rate-limit-reset-credits/consume",
		"GET /usage",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
	if got := account.status(time.Now()); got != accountLive {
		t.Fatalf("status = %s, want %s", got, accountLive)
	}
	if count, credits, known := account.bankedResets(); !known || count != 0 || len(credits) != 0 {
		t.Fatalf("banked resets = %d, %v", count, known)
	}
}
