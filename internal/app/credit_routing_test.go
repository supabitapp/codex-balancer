package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testAccountWithCredits(id string) *Account {
	account := testAccount(id, 100)
	account.primary.minutes = 5 * 60
	account.secondary.minutes = 7 * 24 * 60
	account.spent = true
	account.credits = &creditsPayload{HasCredits: true, Balance: "10.50"}
	return account
}

func TestCreditRoutingUsesIncludedQuotaFirst(t *testing.T) {
	credit := testAccountWithCredits("credit")
	credit.RoutingMode = routingModePriority
	included := testAccount("included", 90)
	pool := &Pool{accounts: []*Account{credit, included}}
	for _, owners := range [][]string{nil, {credit.id()}, {included.id()}} {
		decision := pool.route(owners, nil)
		if decision.account != included || decision.creditFallback {
			t.Fatalf("decision = %+v, want included quota before credits", decision)
		}
	}
}

func TestCreditRoutingRetainsOwnerAndHonorsPriority(t *testing.T) {
	owner := testAccountWithCredits("owner")
	priority := testAccountWithCredits("priority")
	priority.RoutingMode = routingModePriority
	pool := &Pool{accounts: []*Account{owner, priority}}
	for _, step := range []struct {
		owners []string
		skip   map[string]bool
		want   *Account
	}{
		{want: priority},
		{owners: []string{owner.id()}, want: owner},
		{owners: []string{owner.id()}, skip: map[string]bool{owner.id(): true}, want: priority},
	} {
		decision := pool.route(step.owners, step.skip)
		if decision.account != step.want || !decision.creditFallback || decision.blocked != "" {
			t.Fatalf("decision = %+v, want credit fallback to %s", decision, step.want.id())
		}
	}
	if got := owner.status(time.Now()); got != accountCredits {
		t.Fatalf("status = %s, want credits", got)
	}
	// Weekly usage itself is sufficient even before limit_reached is polled.
	owner.spent = false
	if decision := pool.route([]string{owner.id()}, nil); decision.account != owner || !decision.creditFallback {
		t.Fatalf("decision = %+v, want exhausted weekly quota recognized", decision)
	}
}

func TestCreditRoutingRejectsIneligibleAccounts(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Account)
	}{
		{"paused", func(a *Account) { a.Paused = true }},
		{"reauth", func(a *Account) { a.Reauth = "login required" }},
		{"managed business", func(a *Account) { a.planType = "business" }},
		{"enterprise", func(a *Account) { a.planType = "enterprise" }},
		{"cooldown", func(a *Account) { a.cooldown = time.Now().Add(time.Hour) }},
		{"spend cap", func(a *Account) { a.spendControl = &spendControlPayload{Reached: true} }},
		{"overage cap", func(a *Account) { a.credits.OverageLimitReached = true }},
		{"upstream rejection", func(a *Account) { a.rejectCredits() }},
		{"unknown usage", func(a *Account) { a.primary, a.secondary = window{}, window{} }},
		{"only five hour usage exhausted", func(a *Account) { a.secondary.usedPercent = 50 }},
		{"unknown weekly duration", func(a *Account) { a.secondary.minutes = 0 }},
		{"invalid weekly usage", func(a *Account) { a.secondary.usedPercent = math.Inf(1) }},
		{"missing credits", func(a *Account) { a.credits = nil }},
		{"no credits", func(a *Account) { a.credits.HasCredits = false }},
		{"zero balance", func(a *Account) { a.credits.Balance = "0" }},
		{"negative balance", func(a *Account) { a.credits.Balance = "-1" }},
		{"invalid balance", func(a *Account) { a.credits.Balance = "unknown" }},
		{"unknown balance", func(a *Account) { a.credits.Balance = "" }},
		{"NaN balance", func(a *Account) { a.credits.Balance = "NaN" }},
		{"infinite balance", func(a *Account) { a.credits.Balance = "Inf" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := testAccountWithCredits("credit")
			test.change(account)
			pool := &Pool{accounts: []*Account{account}}
			for _, owners := range [][]string{nil, {account.id()}} {
				if decision := pool.route(owners, nil); decision.account != nil || decision.creditFallback {
					t.Fatalf("decision = %+v, want no credit fallback", decision)
				}
			}
		})
	}
}

func TestCreditRoutingUnlimitedCreditsAndOwnerCooldown(t *testing.T) {
	credit := testAccountWithCredits("credit")
	credit.credits = &creditsPayload{Unlimited: true}
	pool := &Pool{accounts: []*Account{credit}}
	if decision := pool.route(nil, nil); decision.account != credit || !decision.creditFallback {
		t.Fatalf("decision = %+v, want unlimited credits accepted without a balance", decision)
	}
	if decision := pool.route(nil, map[string]bool{credit.id(): true}); decision.account != nil {
		t.Fatal("credit fallback bypassed the skip list")
	}
	owner := testAccount("owner", 20)
	owner.cooldown = time.Now().Add(time.Minute)
	pool.accounts = append(pool.accounts, owner)
	if decision := pool.route([]string{owner.id()}, nil); decision.account != nil || decision.blocked != owner.id() || decision.creditFallback {
		t.Fatalf("decision = %+v, want temporary owner cooldown respected", decision)
	}
}

func TestCreditRoutingRefreshesRejectedAndDepletedBalances(t *testing.T) {
	account := testAccountWithCredits("credit")
	srv := newTestServer(t, []*Account{account})
	balance := "10.50"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"credits": map[string]any{"has_credits": balance != "0", "balance": balance},
			"rate_limit": map[string]any{
				"limit_reached":    true,
				"secondary_window": map[string]any{"used_percent": 100, "limit_window_seconds": 604800},
			},
		})
	}))
	defer upstream.Close()
	previous := accountAPIBaseURL
	accountAPIBaseURL = upstream.URL
	t.Cleanup(func() { accountAPIBaseURL = previous })
	account.rejectCredits()
	if decision := srv.pool.route(nil, nil); decision.account != nil {
		t.Fatal("rejected credits used before a fresh usage poll")
	}
	if err := srv.pollUsage(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if decision := srv.pool.route(nil, nil); decision.account != account || !decision.creditFallback {
		t.Fatalf("decision = %+v, want fresh credit balance retried", decision)
	}
	balance = "0"
	if err := srv.pollUsage(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if decision := srv.pool.route(nil, nil); decision.account != nil {
		t.Fatal("depleted credits still routed after usage polling")
	}
}

func TestCreditRoutingHTTPAndTools(t *testing.T) {
	for _, path := range append([]string{"/v1/responses"}, toolEndpoints...) {
		t.Run(path, func(t *testing.T) {
			credit := testAccountWithCredits("credit")
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Chatgpt-Account-Id") != credit.id() || r.Header.Get("Authorization") != "Bearer token-credit" {
					t.Error("credit fallback did not forward the account's credentials")
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"response","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
			}))
			defer upstream.Close()
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{credit})
			response, err := http.Post(proxy.URL+path, "application/json", strings.NewReader(`{"model":"m","input":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			body := readHTTPBody(t, response)
			if response.StatusCode != 200 || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, calls.Load(), body)
			}
			if path == "/v1/responses" && srv.stats.snapshot().Turns != 1 {
				t.Fatal("credit-backed response was not counted")
			}
		})
	}
}

func TestCreditRoutingWebSocketRetainsOwner(t *testing.T) {
	credit := testAccountWithCredits("credit")
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{credit})
	conn, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "thread"))
	defer conn.CloseNow()
	for range 2 {
		completeWebSocketTurn(t, conn, map[string]any{"type": "response.create", "model": "m", "input": []any{}})
	}
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[credit credit]" {
		t.Fatalf("requests = %s, want credit account to serve both turns", got)
	}
	if owners, err := srv.pool.store.routeOwners("thread", "session"); err != nil || fmt.Sprint(owners) != "[credit]" {
		t.Fatalf("owners = %v, error = %v, want credit account affinity persisted", owners, err)
	}
}

func TestCreditRoutingWebSocketReturnsToIncludedQuota(t *testing.T) {
	credit := testAccountWithCredits("credit")
	included := testAccount("included", 20)
	included.Paused = true
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{credit, included})
	headers := codexWebSocketHeaders("session", "thread")
	conn, _ := dialWebSocket(t, proxy.URL, headers)
	defer conn.CloseNow()
	turn := map[string]any{"type": "response.create", "model": "m", "input": []any{}}
	completeWebSocketTurn(t, conn, turn)
	if err := srv.pool.setPaused(included, false); err != nil {
		t.Fatal(err)
	}
	writeWebSocketEvent(t, conn, turn)
	readCloseStatus(t, conn, websocket.StatusServiceRestart)
	reconnected, _ := dialWebSocket(t, proxy.URL, headers)
	defer reconnected.CloseNow()
	completeWebSocketTurn(t, reconnected, turn)
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[credit included]" {
		t.Fatalf("requests = %s, want included quota restored before more credits", got)
	}
}

func TestCreditRoutingRespectsModelAndTierCompatibility(t *testing.T) {
	compatible := testAccountWithCredits("compatible")
	incompatible := testAccountWithCredits("incompatible")
	incompatible.RoutingMode = routingModePriority
	srv := newTestServer(t, []*Account{compatible, incompatible})
	srv.catalog.replace([]string{compatible.id(), incompatible.id()}, map[string][]modelEntry{
		compatible.id():   {testModelEntry("m", "priority", "default")},
		incompatible.id(): {testModelEntry("other", "default")},
	}, "0.1.0")
	decision := srv.pickAccount("thread", nil, "m", "priority", nil, 0)
	if decision.account != compatible || !decision.creditFallback {
		t.Fatalf("decision = %+v, want compatible credit account", decision)
	}
}

func TestCreditRoutingStopsAfterUpstreamUsageRejection(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/alpha/search"} {
		for _, code := range []string{"usage_limit_reached", "credit_balance_exhausted"} {
			t.Run(path+"/"+code, func(t *testing.T) {
				credit := testAccountWithCredits("credit")
				var calls atomic.Int64
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprintf(w, `{"error":{"code":%q}}`, code)
				}))
				defer upstream.Close()
				_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{credit})
				for _, status := range []int{429, 503} {
					response, err := http.Post(proxy.URL+path, "application/json", strings.NewReader(`{"model":"m","input":[]}`))
					if err != nil {
						t.Fatal(err)
					}
					body := readHTTPBody(t, response)
					if response.StatusCode != status {
						t.Fatalf("status=%d, want %d; body=%s", response.StatusCode, status, body)
					}
				}
				if calls.Load() != 1 {
					t.Fatalf("upstream calls = %d, want one attempt until usage refresh", calls.Load())
				}
			})
		}
	}
}

func TestCreditRoutingStopsAfterWebSocketHandshakeRejection(t *testing.T) {
	credit := testAccountWithCredits("credit")
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"credit_balance_exhausted"}}`)
	}))
	defer upstream.Close()
	srv := newTestServer(t, []*Account{credit})
	srv.upstream = upstream.URL
	for range 2 {
		dialer, err := newResponsesWebSocketDialer(srv, httptest.NewRequest(http.MethodGet, "/v1/responses", nil), websocketRoute{}, "m", "")
		if err != nil {
			t.Fatal(err)
		}
		dial, response, err := dialer.dial()
		if dial != nil || response != nil || !errors.Is(err, errNoAccountAvailable) {
			t.Fatalf("dial=%v response=%v error=%v, want no credit account available", dial, response, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want credit rejection cached until usage poll", calls.Load())
	}
}

func TestCreditRoutingRateLimitEventClearsDepletedBalance(t *testing.T) {
	credit := testAccountWithCredits("credit")
	pool := &Pool{accounts: []*Account{credit}}
	data := []byte(`{"type":"codex.rate_limits","credits":{"has_credits":false,"balance":"0"},"rate_limits":{"secondary":{"used_percent":100,"window_minutes":10080}}}`)
	var event websocketEnvelope
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	forwarded := pool.clientUsageEvent(credit, data, event)
	if strings.Contains(string(forwarded), `"credits"`) {
		t.Fatal("upstream account's private credit balance leaked to the client")
	}
	if decision := pool.route(nil, nil); decision.account != nil {
		t.Fatal("depleted balance from rate-limit event was ignored")
	}
}
