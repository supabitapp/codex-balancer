package app

import (
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWebSocketStopsRoutingAfterFreeDowngrade(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprintf("accepted=%t", accepted), func(t *testing.T) {
			received := make(chan struct{}, 1)
			upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, _ websocketEnvelope) {
				if account == "owner" && !accepted {
					received <- struct{}{}
					return
				}
				writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
				writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
			})
			defer upstream.Close()
			owner := testAccount("owner", 0)
			paid := testAccount("paid", 20)
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, paid})
			headers := codexWebSocketHeaders("session", "thread")
			conn, _ := dialWebSocket(t, proxy.URL, headers)
			defer conn.CloseNow()
			turn := map[string]any{"type": "response.create", "input": []any{}}
			if accepted {
				completeWebSocketTurn(t, conn, turn)
			} else {
				writeWebSocketEvent(t, conn, turn)
				select {
				case <-received:
				case <-time.After(5 * time.Second):
					t.Fatal("original owner did not receive the initial turn")
				}
			}

			owner.adopt(time.Now(), "free", window{}, window{}, nil, nil, nil)
			writeWebSocketEvent(t, conn, turn)
			readCloseStatus(t, conn, websocket.StatusServiceRestart)
			if got := fmt.Sprint(upstream.RequestAccounts()); got != "[owner]" {
				t.Fatalf("requests = %s, want no inference after the free downgrade", got)
			}
			if owners, err := srv.pool.store.routeOwners("thread", "session"); err != nil || fmt.Sprint(owners) != "[owner]" {
				t.Fatalf("owners = %v, error = %v, want the original owner preserved before replay", owners, err)
			}

			reconnected, _ := dialWebSocket(t, proxy.URL, headers)
			defer reconnected.CloseNow()
			completeWebSocketTurn(t, reconnected, turn)
			if got := fmt.Sprint(upstream.RequestAccounts()); got != "[owner paid]" {
				t.Fatalf("requests = %s, want the paid account to serve the replay", got)
			}
			if owners, err := srv.pool.store.routeOwners("thread", "session"); err != nil || fmt.Sprint(owners) != "[paid]" {
				t.Fatalf("owners = %v, error = %v, want paid ownership", owners, err)
			}
		})
	}
}
