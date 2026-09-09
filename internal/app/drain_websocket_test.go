package app

import (
	"fmt"
	"testing"

	"github.com/coder/websocket"
)

func TestDrainingPreservesWebSocketOwnersAndFastPolicy(t *testing.T) {
	for _, manual := range []bool{false, true} {
		for _, fast := range []fastMode{fastModeDefault, fastModeOn, fastModeOff} {
			t.Run(fmt.Sprintf("manual_%t/fast_%s", manual, fast), func(t *testing.T) {
				tiers := make(chan string, 4)
				upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, event websocketEnvelope) {
					tiers <- event.ServiceTier
					writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
					writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
				})
				defer upstream.Close()
				owner, draining := testAccount("owner", 10), testAccount("draining", 20)
				srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, draining})
				srv.fastMode.set(fast)
				srv.catalog.replace([]string{owner.id(), draining.id()}, map[string][]modelEntry{
					owner.id():    {testModelEntry("gpt-test", "default", "priority")},
					draining.id(): {testModelEntry("gpt-test", "default", "priority")},
				}, "0.1.0")
				headers := codexWebSocketHeaders("existing-session", "existing-thread")
				first, _ := dialWebSocket(t, proxy.URL, headers)
				defer first.CloseNow()
				// Activate drain after the initial handshake but before response.created.
				// Even a provisional owner must survive this preference change.
				if manual {
					if err := srv.pool.setRoutingMode(draining, routingModeDraining); err != nil {
						t.Fatal(err)
					}
				} else {
					setTestAccountUsage(draining, 97)
				}
				turn := func(conn *websocket.Conn) {
					t.Helper()
					completeWebSocketTurn(t, conn, map[string]any{
						"type": "response.create", "model": "gpt-test", "service_tier": "default", "input": []any{},
					})
					want := "default"
					if fast == fastModeOn {
						want = "priority"
					}
					if got := <-tiers; got != want {
						t.Fatalf("service tier = %q, want %q from global policy only", got, want)
					}
				}
				turn(first)
				turn(first)
				fresh, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("fresh-session", "fresh-thread"))
				defer fresh.CloseNow()
				turn(fresh)
				first.CloseNow()
				reconnected, _ := dialWebSocket(t, proxy.URL, headers)
				defer reconnected.CloseNow()
				turn(reconnected)
				if got := fmt.Sprint(upstream.RequestAccounts()); got != "[owner owner draining owner]" {
					t.Fatalf("requests = %s, want draining for only the fresh conversation", got)
				}
				owners, err := srv.pool.store.routeOwners("existing-thread", "existing-session")
				if err != nil || fmt.Sprint(owners) != "[owner]" {
					t.Fatalf("retained owners = %v, error = %v", owners, err)
				}
			})
		}
	}
}

func TestDrainRankingHonorsModelAndTierEligibility(t *testing.T) {
	for _, scenario := range []string{"model", "tier"} {
		t.Run(scenario, func(t *testing.T) {
			draining, other := testAccount("draining", 97), testAccount("other", 20)
			srv := newTestServer(t, []*Account{draining, other})
			entry := testModelEntry("gpt-test", "default")
			if scenario == "model" {
				entry = testModelEntry("another-model", "priority")
			}
			srv.catalog.replace([]string{draining.id(), other.id()}, map[string][]modelEntry{
				draining.id(): {entry}, other.id(): {testModelEntry("gpt-test", "priority")},
			}, "0.1.0")
			if got := srv.pickAccount("fresh-thread", nil, "gpt-test", "priority", nil, 0).account; got != other {
				t.Fatal("draining must not override model or service-tier eligibility")
			}
		})
	}
}
