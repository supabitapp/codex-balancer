package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWebSocketUsageRetryRequiresFullReplayBeforeAcceptingReplacement(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprintf("prior_accepted_route_%t", accepted), func(t *testing.T) {
			upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, event websocketEnvelope) {
				if account == "account-a" && event.Generate == nil {
					writeWebSocketEvent(t, conn, map[string]any{"type": "error", "error": map[string]any{"code": "usage_limit_reached"}})
					return
				}
				writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
				writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
			})
			defer upstream.Close()
			a, b := testAccount("account-a", 0), testAccount("account-b", 20)
			server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
			headers := codexWebSocketHeaders("session", "thread")
			first, _ := dialWebSocket(t, proxy.URL, headers)
			defer first.CloseNow()
			if accepted {
				completeWebSocketTurn(t, first, map[string]any{"type": "response.create", "generate": false, "input": []any{}})
			}
			writeWebSocketEvent(t, first, map[string]any{"type": "response.create", "previous_response_id": "old-response", "input": []any{}})
			readCloseStatus(t, first, websocket.StatusServiceRestart)
			if !a.routingCandidate().spent || server.stats.snapshot().Limited != 1 {
				t.Fatal("usage rejection did not mark the owner spent")
			}
			// Even an unaccepted initial request leaves an owner barrier. A client
			// that fails to discard its response ID cannot leak it to account B.
			invalid, _ := dialWebSocket(t, proxy.URL, headers)
			writeWebSocketEvent(t, invalid, map[string]any{"type": "response.create", "previous_response_id": "old-response", "input": []any{}})
			readCloseStatus(t, invalid, websocket.StatusTryAgainLater)
			invalid.CloseNow()
			for _, account := range upstream.RequestAccounts() {
				if account != "account-a" {
					t.Fatal("replacement received an account-bound request")
				}
			}
			replay, _ := dialWebSocket(t, proxy.URL, headers)
			completeWebSocketTurn(t, replay, map[string]any{"type": "response.create", "input": []any{
				map[string]any{"type": "reasoning", "encrypted_content": "preserved-history"},
			}})
			replay.CloseNow()
			owners, err := server.pool.store.routeOwners("thread", "session")
			if err != nil || fmt.Sprint(owners) != "[account-b]" {
				t.Fatalf("owners = %v, error = %v", owners, err)
			}
		})
	}
}

func TestWebSocketUsageRetryPreservesTerminalErrorWhenUnsafe(t *testing.T) {
	for _, scenario := range []string{"no capacity", "paused", "cooling", "unknown quota", "model", "tier", "metadata token", "header token", "upstream token", "anonymous", "pipelined", "accepted response"} {
		t.Run(scenario, func(t *testing.T) {
			requests := 0
			upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
				requests++
				if scenario == "pipelined" && requests == 1 {
					return
				}
				if scenario == "accepted response" {
					writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
				}
				writeWebSocketEvent(t, conn, map[string]any{"type": "error", "error": map[string]any{"code": "usage_limit_reached"}})
			})
			defer upstream.Close()
			if scenario == "upstream token" {
				original := upstream.Config.Handler
				upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set(codexTurnStateKey, "old-state")
					original.ServeHTTP(w, r)
				})
			}
			a, b := testAccount("account-a", 0), testAccount("account-b", 20)
			switch scenario {
			case "no capacity":
				b.markSpent()
			case "paused":
				b.Paused = true
			case "cooling":
				b.cooldown = time.Now().Add(time.Hour)
			case "unknown quota":
				b.primary = window{}
				b.secondary = window{}
			}
			server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
			model, tier := "gpt-test", "priority"
			other := testModelEntry(model, tier)
			if scenario == "model" {
				other = testModelEntry("another-model", tier)
			}
			if scenario == "tier" {
				other = testModelEntry(model, "default")
			}
			server.catalog.replace([]string{a.id(), b.id()}, map[string][]modelEntry{
				a.id(): {testModelEntry(model, tier)}, b.id(): {other},
			}, "0.1.0")
			headers := codexWebSocketHeaders("session", "thread")
			if scenario == "anonymous" {
				headers = http.Header{}
			}
			metadata := map[string]string{}
			if scenario == "header token" {
				headers.Set(codexTurnStateKey, "old-state")
			}
			if scenario == "metadata token" {
				metadata[codexTurnStateKey] = "old-state"
			}
			conn, _ := dialWebSocket(t, proxy.URL, headers)
			defer conn.CloseNow()
			writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "model": model, "service_tier": tier, "client_metadata": metadata, "input": []any{}})
			if scenario == "pipelined" {
				writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
			}
			if scenario == "accepted response" {
				readWebSocketEvent(t, conn)
			}
			if event := readWebSocketEvent(t, conn); !websocketErrorIs(event, "usage_limit_reached") {
				t.Fatalf("terminal event = %+v", event)
			}
			readCloseStatus(t, conn, websocket.StatusServiceRestart)
			want := "[account-a]"
			if scenario == "pipelined" {
				want = "[account-a account-a]"
			}
			if got := fmt.Sprint(upstream.RequestAccounts()); got != want {
				t.Fatalf("requests = %s", got)
			}
		})
	}
}

func TestWebSocketReplacementRejectsTurnStateHeaderBeforeUpstreamHandshake(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	a, b := testAccount("account-a", 0), testAccount("account-b", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
	headers := codexWebSocketHeaders("session", "thread")
	first, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, first, map[string]any{"type": "response.create", "input": []any{}})
	first.CloseNow()
	a.markSpent()
	headers.Set(codexTurnStateKey, "old-state")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers})
	if err == nil {
		conn.CloseNow()
		t.Fatal("account-bound handshake succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response = %v", resp)
	}
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a]" {
		t.Fatalf("connections = %s", got)
	}
}
