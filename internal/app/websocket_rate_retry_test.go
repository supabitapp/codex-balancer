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

func TestWebSocketRateLimitPreservesBoundFirstTurn(t *testing.T) {
	for _, kind := range []string{"error", "response.failed"} {
		for _, source := range []string{"request header", "metadata", "upstream header"} {
			t.Run(kind+"/"+source, func(t *testing.T) {
				upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
					if kind == "response.failed" {
						writeWebSocketEvent(t, conn, map[string]any{"type": kind, "response": map[string]any{"status": "failed", "error": map[string]any{"code": "rate_limit_exceeded"}}})
					} else {
						writeWebSocketEvent(t, conn, map[string]any{"type": kind, "status": 429, "error": map[string]any{"code": "rate_limit_exceeded"}})
					}
				})
				defer upstream.Close()
				if source == "upstream header" {
					handler := upstream.Config.Handler
					upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set(codexTurnStateKey, "state-a")
						handler.ServeHTTP(w, r)
					})
				}
				srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 20)})
				headers := codexWebSocketHeaders("session", "thread")
				turn := map[string]any{"type": "response.create", "model": "m", "input": []any{}}
				if source == "request header" {
					headers.Set(codexTurnStateKey, "state-a")
				}
				if source == "metadata" {
					turn["client_metadata"] = map[string]string{codexTurnStateKey: "state-a"}
				}
				conn, _ := dialWebSocket(t, proxy.URL, headers)
				writeWebSocketEvent(t, conn, turn)
				if kind == "error" {
					readWebSocketFailure(t, conn, "rate_limit_exceeded")
				} else {
					event := readWebSocketEvent(t, conn)
					if event.Type != kind {
						t.Fatalf("event = %v, want original response.failed", event)
					}
				}
				readCloseStatus(t, conn, websocket.StatusServiceRestart)
				conn.CloseNow()
				owners, err := srv.pool.store.routeOwners("thread", "session")
				if err != nil || fmt.Sprint(owners) != "[a]" {
					t.Errorf("bound owner = %v, err=%v", owners, err)
				}
				if source == "upstream header" {
					headers.Set(codexTurnStateKey, "state-a")
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				retry, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers})
				if retry != nil {
					retry.CloseNow()
				}
				if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
					t.Errorf("bound retry = %v / %v, want503", resp, err)
				}
				if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[a]" {
					t.Errorf("opened accounts %s, want only a", got)
				}
			})
		}
	}
}

func TestRateLimitRewritePreservesTerminalErrors(t *testing.T) {
	for _, test := range []struct {
		name, code, frame string
		bound             bool
	}{
		{"flex unavailable", "flex_unavailable", `{"type":"error","status":429,"error":{"code":"flex_unavailable","message":"flex unavailable"}}`, false},
		{"quota wins over rate code", "rate_limit_exceeded", `{"type":"error","status":429,"error":{"type":"usage_limit_reached","code":"rate_limit_exceeded","message":"quota exhausted"}}`, false},
		{"anonymous bound", "rate_limit_exceeded", `{"type":"error","status":429,"error":{"code":"rate_limit_exceeded","message":"slow down"}}`, true},
	} {
		for _, transport := range []string{"http", "websocket"} {
			t.Run(test.name+"/"+transport, func(t *testing.T) {
				upstream := newHTTPUpstream(t, func(_ *http.Request, stream *testResponseStream, _ []byte) {
					sendHTTPEvents(t, stream, test.frame)
				})
				_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 20)})
				headers := http.Header{}
				if test.bound {
					headers.Set(codexTurnStateKey, "state-a")
				}
				if transport == "http" {
					response := postResponse(t, proxy.URL, `{"model":"m"}`, headers)
					body := readHTTPBody(t, response)
					if response.StatusCode != 429 || !strings.Contains(body, test.code) {
						t.Fatalf("terminal HTTP error changed: %d %s", response.StatusCode, body)
					}
					return
				}
				conn, _ := dialWebSocket(t, proxy.URL, headers)
				defer conn.CloseNow()
				writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "model": "m"})
				failure := readWebSocketFailure(t, conn, test.code)
				if failure.Status != 429 || failure.Retryable {
					t.Fatalf("terminal WebSocket error changed: %+v", failure)
				}
				readCloseStatus(t, conn, websocket.StatusServiceRestart)
			})
		}
	}
}
