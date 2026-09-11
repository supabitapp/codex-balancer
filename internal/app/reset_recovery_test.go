package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type resetTestAPI struct {
	mu       sync.Mutex
	credits  map[string][]resetCredit
	restored map[string]bool
	failures map[string]bool
	consumed []string
}

func newResetTestAPI(t *testing.T, accounts ...*Account) *resetTestAPI {
	t.Helper()
	api := &resetTestAPI{
		credits:  map[string][]resetCredit{},
		restored: map[string]bool{},
		failures: map[string]bool{},
	}
	for _, account := range accounts {
		_, credits, _ := account.bankedResets()
		api.credits[account.id()] = credits
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		id := r.Header.Get("chatgpt-account-id")
		switch r.Method + " " + r.URL.Path {
		case "GET /rate-limit-reset-credits":
			json.NewEncoder(w).Encode(resetCreditsPayload{AvailableCount: int64(len(api.credits[id])), Credits: api.credits[id]})
		case "POST /rate-limit-reset-credits/consume":
			var request consumeResetCreditRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.CreditID == "" || request.RedeemRequestID != request.CreditID {
				t.Errorf("consume request = %+v", request)
			}
			api.consumed = append(api.consumed, request.CreditID)
			if api.failures[id] {
				http.Error(w, "reset failed", http.StatusBadGateway)
				return
			}
			api.restored[id] = true
			api.credits[id] = nil
			json.NewEncoder(w).Encode(consumeResetCreditResponse{Code: "reset", WindowsReset: 2})
		case "GET /usage":
			used := 100.0
			if api.restored[id] {
				used = 0
			}
			json.NewEncoder(w).Encode(map[string]any{
				"rate_limit": map[string]any{
					"limit_reached":    used == 100,
					"primary_window":   map[string]any{"used_percent": used, "limit_window_seconds": 18000},
					"secondary_window": map[string]any{"used_percent": used, "limit_window_seconds": 604800},
				},
				"rate_limit_reset_credits": map[string]any{"available_count": len(api.credits[id])},
			})
		default:
			t.Errorf("unexpected reset API request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	previous := accountAPIBaseURL
	accountAPIBaseURL = upstream.URL
	t.Cleanup(func() {
		upstream.Close()
		accountAPIBaseURL = previous
	})
	return api
}

func (api *resetTestAPI) assertConsumed(t *testing.T, want ...string) {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	if !reflect.DeepEqual(api.consumed, want) {
		t.Fatalf("consumed = %v, want %v", api.consumed, want)
	}
}

func testSpentAccountWithReset(id string, expiresAt time.Time) *Account {
	account := testAccount(id, 100)
	account.markSpent()
	adoptTestResetCredit(account, expiresAt)
	return account
}

func resetRecoveryServer(accounts ...*Account) *server {
	return &server{
		pool:   &Pool{accounts: accounts},
		client: http.DefaultClient,
		stats:  newStatsWithPrices(priceSnapshot{}),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestPoolRecoveryChoosesSoonestResetBeyondPriorityWindow(t *testing.T) {
	now := time.Now()
	later := testSpentAccountWithReset("later", now.Add(7*24*time.Hour))
	later.RoutingMode = routingModePriority
	soon := testSpentAccountWithReset("soon", now.Add(3*24*time.Hour))
	soon.cooldown = now.Add(time.Hour)
	api := newResetTestAPI(t, later, soon)
	// A usage poll may know the count before fetching credit details.
	soon.adoptResetCredits(time.Time{}, 1, nil)
	s := resetRecoveryServer(later, soon)
	if got := s.recoverPoolUsageLimit(context.Background(), nil); got != soon {
		t.Fatalf("recovered = %v, want soon", got)
	}
	api.assertConsumed(t, "credit-soon")
	if got := s.pool.route(nil, nil).account; got != soon {
		t.Fatalf("route = %v, want recovered account", got)
	}
	if !later.routingCandidate().spent {
		t.Fatal("later account should remain spent")
	}
}

func TestPoolRecoveryPreservesCreditsWhileCapacityIsAvailable(t *testing.T) {
	spent := testSpentAccountWithReset("spent", time.Now().Add(time.Minute))
	healthy := testAccount("healthy", 20)
	api := newResetTestAPI(t, spent, healthy)
	s := resetRecoveryServer(spent, healthy)
	if got := s.recoverPoolUsageLimit(context.Background(), nil); got != healthy {
		t.Fatalf("available = %v, want healthy", got)
	}
	api.assertConsumed(t)
}

func TestPoolRecoverySkipsIneligibleAccountsAndCredits(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Account)
	}{
		{"paused", func(a *Account) { a.Paused = true }},
		{"signed out", func(a *Account) { a.Reauth = "expired" }},
		{"workspace", func(a *Account) { a.planType = "business" }},
		{"spending limit", func(a *Account) { a.spendControl = &spendControlPayload{Reached: true} }},
		{"temporary cooldown", func(a *Account) { a.spent = false; a.cooldown = time.Now().Add(time.Hour) }},
		{"expired credit", func(a *Account) { adoptTestResetCredit(a, time.Now().Add(-time.Minute)) }},
		{"used credit", func(a *Account) { a.resetCredits.details[0].Status = "redeemed" }},
		{"wrong credit type", func(a *Account) { a.resetCredits.details[0].ResetType = "other" }},
		{"no credits", func(a *Account) { a.adoptResetCredits(time.Now(), 0, nil) }},
		{"unknown credits", func(a *Account) { a.resetCredits = resetCreditState{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := testSpentAccountWithReset("account", time.Now().Add(time.Hour))
			test.mutate(account)
			api := newResetTestAPI(t, account)
			s := resetRecoveryServer(account)
			if got := s.recoverPoolUsageLimit(context.Background(), nil); got != nil {
				t.Fatalf("recovered = %v, want none", got)
			}
			api.assertConsumed(t)
		})
	}
}

func TestPoolRecoveryTriesNextAccountAfterResetFailure(t *testing.T) {
	now := time.Now()
	first := testSpentAccountWithReset("first", now.Add(time.Hour))
	second := testSpentAccountWithReset("second", now.Add(48*time.Hour))
	api := newResetTestAPI(t, first, second)
	api.failures[first.id()] = true
	s := resetRecoveryServer(first, second)
	if got := s.recoverPoolUsageLimit(context.Background(), nil); got != second {
		t.Fatalf("recovered = %v, want second", got)
	}
	api.assertConsumed(t, "credit-first", "credit-second")
}

func TestPoolRecoveryHonorsExcludedAccounts(t *testing.T) {
	now := time.Now()
	first := testSpentAccountWithReset("first", now.Add(time.Hour))
	second := testSpentAccountWithReset("second", now.Add(48*time.Hour))
	api := newResetTestAPI(t, first, second)
	s := resetRecoveryServer(first, second)
	if got := s.recoverPoolUsageLimit(context.Background(), map[string]bool{first.id(): true}); got != second {
		t.Fatalf("recovered = %v, want second", got)
	}
	api.assertConsumed(t, "credit-second")
}

func TestPoolRecoveryStopsWhenCancelled(t *testing.T) {
	account := testSpentAccountWithReset("account", time.Now().Add(time.Hour))
	api := newResetTestAPI(t, account)
	s := resetRecoveryServer(account)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.recoverPoolUsageLimit(ctx, nil); got != nil {
		t.Fatalf("recovered = %v, want none", got)
	}
	api.assertConsumed(t)
}

func TestNextResetCreditOrdersUndatedCreditsLast(t *testing.T) {
	now := time.Now()
	later := now.Add(72 * time.Hour)
	credits := []resetCredit{
		{ID: "undated", ResetType: "codex_rate_limits", Status: "available"},
		{ID: "dated", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: &later},
	}
	if credit, ok := nextResetCredit(credits, now, 0); !ok || credit.ID != "dated" {
		t.Fatalf("credit = %q, %v, want dated", credit.ID, ok)
	}
	if credit, ok := nextResetCredit(credits[:1], now, 0); !ok || credit.ID != "undated" {
		t.Fatalf("credit = %q, %v, want undated", credit.ID, ok)
	}
	if credit, ok := expiringResetCredit(credits, now); ok {
		t.Fatalf("priority credit = %q, want none", credit.ID)
	}
}

func TestPoolRecoveryConcurrentCallsConsumeOneReset(t *testing.T) {
	now := time.Now()
	later := testSpentAccountWithReset("later", now.Add(7*24*time.Hour))
	soon := testSpentAccountWithReset("soon", now.Add(3*24*time.Hour))
	api := newResetTestAPI(t, later, soon)
	s := resetRecoveryServer(later, soon)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 12 {
		wg.Go(func() {
			<-start
			if got := s.recoverPoolUsageLimit(context.Background(), nil); got != soon {
				t.Errorf("recovered = %v, want soon", got)
			}
		})
	}
	close(start)
	wg.Wait()
	api.assertConsumed(t, "credit-soon")
}

func TestPollAllUsageRecoversExhaustedPool(t *testing.T) {
	now := time.Now()
	later := testSpentAccountWithReset("later", now.Add(7*24*time.Hour))
	soon := testSpentAccountWithReset("soon", now.Add(3*24*time.Hour))
	api := newResetTestAPI(t, later, soon)
	s := resetRecoveryServer(later, soon)
	s.pollAllUsage(context.Background())
	api.assertConsumed(t, "credit-soon")
	if got := s.pool.route(nil, nil).account; got != soon {
		t.Fatalf("route after polling = %v, want soon", got)
	}
}

func TestWebSocketRecoversExhaustedPoolBeforeDialing(t *testing.T) {
	now := time.Now()
	later := testSpentAccountWithReset("later", now.Add(7*24*time.Hour))
	soon := testSpentAccountWithReset("soon", now.Add(3*24*time.Hour))
	api := newResetTestAPI(t, later, soon)
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{later, soon})
	conn, _ := dialWebSocket(t, proxy.URL, http.Header{"Thread-Id": {"thread"}})
	defer conn.CloseNow()
	completeWebSocketTurn(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	api.assertConsumed(t, "credit-soon")
	if got := upstream.RequestAccounts(); !reflect.DeepEqual(got, []string{"soon"}) {
		t.Fatalf("upstream accounts = %v, want soon", got)
	}
}

func TestWebSocketLastAccountUsageLimitResetsEarliestCreditAcrossPool(t *testing.T) {
	now := time.Now()
	soon := testSpentAccountWithReset("soon", now.Add(30*time.Minute))
	last := testSpentAccountWithReset("last", now.Add(2*time.Hour))
	last.spent = false
	api := newResetTestAPI(t, soon, last)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("chatgpt-account-id") == last.id() {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"code":"usage_limit_reached"}}`)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		conn.Read(r.Context())
	}))
	defer upstream.Close()
	s := resetRecoveryServer(soon, last)
	s.upstream = upstream.URL
	dialer, err := newResponsesWebSocketDialer(s, httptest.NewRequest(http.MethodGet, "/v1/responses", nil), websocketRoute{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	dial, response, err := dialer.dial()
	if err != nil || response != nil || dial == nil {
		t.Fatalf("dial = %v, response = %v, error = %v", dial, response, err)
	}
	defer dial.conn.CloseNow()
	if dial.account != soon {
		t.Fatalf("account = %s, want soon", dial.account.id())
	}
	api.assertConsumed(t, "credit-soon")
}
