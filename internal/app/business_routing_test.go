package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSelfServeBusinessRoutingFollowsUsage(t *testing.T) {
	const plan = "self_serve_business_prolite"
	business := accountFromState(testAccountWithPlan("business", 0, plan).persisted())
	personal := testAccount("personal", 20)
	pool := &Pool{accounts: []*Account{business, personal}}
	if got := business.status(time.Now()); got != accountChecking {
		t.Fatalf("initial status = %s, want checking until quota is fetched", got)
	}
	if got := pool.route(nil, nil).account; got != personal {
		t.Fatal("an account with unknown quota must not receive fresh placement")
	}
	if got := pool.route([]string{business.id()}, nil); got.account != nil || got.blocked != business.id() {
		t.Fatalf("decision = %+v, want to wait for the retained owner's quota", got)
	}

	reportedPlan := plan
	spendReached, rateReached := false, false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"plan_type": reportedPlan,
			"rate_limit": map[string]any{
				"limit_reached": rateReached,
				"primary_window": map[string]any{
					"used_percent": 2, "limit_window_seconds": 300,
				},
				"secondary_window": map[string]any{
					"used_percent": 2, "limit_window_seconds": 604800,
				},
			},
			"spend_control": map[string]any{"reached": spendReached},
		})
	}))
	defer upstream.Close()
	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = upstream.URL
	t.Cleanup(func() { accountAPIBaseURL = oldBaseURL })
	s := &server{
		pool: pool, client: upstream.Client(),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, step := range []struct {
		name         string
		plan         string
		spendReached bool
		rateReached  bool
		wantStatus   accountStatus
	}{
		{name: "healthy", plan: plan, wantStatus: accountLive},
		{name: "spend cap despite quota remaining", plan: plan, spendReached: true, wantStatus: accountCooling},
		{name: "rate cap after spend recovery", plan: plan, rateReached: true, wantStatus: accountCooling},
		{name: "recovered", plan: plan, wantStatus: accountLive},
		{name: "display-only workspace", plan: "business", wantStatus: accountNotRouted},
		{name: "self-serve plan restored", plan: plan, wantStatus: accountLive},
	} {
		t.Run(step.name, func(t *testing.T) {
			reportedPlan, spendReached, rateReached = step.plan, step.spendReached, step.rateReached
			if err := s.pollUsage(context.Background(), business); err != nil {
				t.Fatal(err)
			}
			if got := business.status(time.Now()); got != step.wantStatus {
				t.Fatalf("status = %s, want %s", got, step.wantStatus)
			}
			want := personal
			if step.wantStatus == accountLive {
				want = business
			}
			for _, owners := range [][]string{nil, {business.id()}} {
				if got := pool.route(owners, nil).account; got != want {
					t.Fatalf("picked = %v, want %s", got, want.id())
				}
			}
		})
	}
}

func TestSpendLimitCannotUseRateLimitReset(t *testing.T) {
	for _, requestAge := range []time.Duration{time.Minute, -time.Minute} {
		t.Run(requestAge.String(), func(t *testing.T) {
			account := testAccountWithPlan("business", 2, "self_serve_business_prolite")
			account.spendControl = &spendControlPayload{Reached: true}
			adoptTestResetCredit(account, time.Now().Add(time.Hour))
			s := &server{client: &http.Client{Transport: accountSettingsRoundTrip(func(*http.Request) (*http.Response, error) {
				t.Fatal("rate-limit reset requests must not try to recover a reached spend limit")
				return nil, nil
			})}}
			if s.recoverUsageLimit(context.Background(), account, time.Now().Add(-requestAge)) {
				t.Fatal("remaining rate-limit quota must not clear a reached spend limit")
			}
			if !account.routingCandidate().spent {
				t.Fatal("account must remain unavailable while its spend limit is reached")
			}
		})
	}
}

func TestSelfServeBusinessWebSocketMovesAfterSpendLimit(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	business := testAccountWithPlan("business", 2, "self_serve_business_prolite")
	personal := testAccount("personal", 20)
	s, proxy := newWebSocketProxy(t, upstream.URL, []*Account{business, personal})
	headers := codexWebSocketHeaders("session", "thread")
	first, _ := dialWebSocket(t, proxy.URL, headers)
	defer first.CloseNow()
	completeWebSocketTurn(t, first, map[string]any{"type": "response.create", "input": []any{}})
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[business]" {
		t.Fatalf("requests = %s, want the Business account to serve the first turn", got)
	}

	business.mu.Lock()
	business.spendControl = &spendControlPayload{Reached: true}
	business.mu.Unlock()
	writeWebSocketEvent(t, first, map[string]any{"type": "response.create", "input": []any{}})
	readCloseStatus(t, first, websocket.StatusServiceRestart)

	replay, _ := dialWebSocket(t, proxy.URL, headers)
	defer replay.CloseNow()
	completeWebSocketTurn(t, replay, map[string]any{"type": "response.create", "input": []any{}})
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[business personal]" {
		t.Fatalf("requests = %s, want no turn sent to Business after its spend limit", got)
	}
	owners, err := s.pool.store.routeOwners("thread", "session")
	if err != nil || fmt.Sprint(owners) != "[personal]" {
		t.Fatalf("owners = %v, error = %v, want accepted replacement ownership", owners, err)
	}
}

func TestSpendLimitUsesUrgentUsagePolling(t *testing.T) {
	now := time.Now()
	account := testAccountWithPlan("business", 2, "self_serve_business_prolite")
	account.usageFetchedAt = now
	account.resetCredits.fetchedAt = now
	account.spendControl = &spendControlPayload{Reached: true}
	if usage, resets := account.pollsDue(now.Add(2*time.Minute), 10*time.Minute); !usage || resets {
		t.Fatalf("polls due = %t, %t, want urgent usage without reset-credit polling", usage, resets)
	}
}
