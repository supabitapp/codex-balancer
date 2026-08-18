package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestServer(t *testing.T, accounts []*Account) (*server, *AffinityStore) {
	t.Helper()
	state, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(state)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	for _, account := range accounts {
		if err := pool.add(account); err != nil {
			state.Close()
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { state.Close() })
	store := &AffinityStore{store: state}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &server{
		ctx:          context.Background(),
		pool:         pool,
		catalog:      newModelCatalog(),
		affinity:     store,
		stats:        newStatsWithPrices(testPriceSnapshot(t)),
		upstream:     "http://127.0.0.1:1",
		client:       newProxyClient(),
		log:          log,
		retryBackoff: func(int) time.Duration { return 0 },
	}, store
}

func requireNoFailedAccounts(t *testing.T, server *server, accounts ...*Account) {
	t.Helper()
	if events := server.stats.snapshot().Events; len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
	for _, account := range accounts {
		_, _, cooldown, _ := account.health()
		if !cooldown.IsZero() {
			t.Fatalf("account %s cooldown = %s, want none", account.id(), cooldown)
		}
	}
}

func useNoResetCreditsAPI(t *testing.T) func() int {
	t.Helper()
	api := useResetAPI(t, 0, 100, http.StatusOK)
	return func() int { return len(api.calls()) }
}

type resetAPI struct {
	mu            sync.Mutex
	requests      []string
	expiresAfter  time.Duration
	usedPercent   float64
	consumeStatus int
}

func useResetAPI(t *testing.T, expiresAfter time.Duration, usedPercent float64, consumeStatus int) *resetAPI {
	t.Helper()
	api := &resetAPI{
		expiresAfter:  expiresAfter,
		usedPercent:   usedPercent,
		consumeStatus: consumeStatus,
	}
	server := httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = server.URL
	t.Cleanup(func() {
		accountAPIBaseURL = oldBaseURL
		server.Close()
	})
	return api
}

func (a *resetAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	request := r.Method + " " + r.URL.Path
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.mu.Unlock()
	switch request {
	case "GET /rate-limit-reset-credits":
		if a.expiresAfter <= 0 {
			json.NewEncoder(w).Encode(map[string]any{"available_count": 0, "credits": []any{}})
			return
		}
		expiresAt := time.Now().UTC().Add(a.expiresAfter)
		json.NewEncoder(w).Encode(map[string]any{
			"available_count": 1,
			"credits": []map[string]any{{
				"id":         "credit-a",
				"reset_type": "codex_rate_limits",
				"status":     "available",
				"expires_at": expiresAt.Format(time.RFC3339),
			}},
		})
	case "POST /rate-limit-reset-credits/consume":
		if a.consumeStatus != http.StatusOK {
			http.Error(w, "reset failed", a.consumeStatus)
			return
		}
		json.NewEncoder(w).Encode(consumeResetCreditResponse{Code: "reset", WindowsReset: 2})
	case "GET /usage":
		json.NewEncoder(w).Encode(map[string]any{
			"rate_limit": map[string]any{
				"primary_window":   map[string]any{"used_percent": a.usedPercent, "limit_window_seconds": 300},
				"secondary_window": map[string]any{"used_percent": a.usedPercent, "limit_window_seconds": 604800},
			},
			"rate_limit_reset_credits": map[string]any{"available_count": 0},
		})
	default:
		http.NotFound(w, r)
	}
}

func (a *resetAPI) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

func (a *resetAPI) consumeCalls() int {
	calls := 0
	for _, request := range a.calls() {
		if request == "POST /rate-limit-reset-credits/consume" {
			calls++
		}
	}
	return calls
}
