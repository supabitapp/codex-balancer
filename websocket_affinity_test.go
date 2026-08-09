package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestWebSocketSoftRateLimitReplaysOnAnotherAccount(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-a" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":   "error",
				"status": http.StatusTooManyRequests,
				"error":  map[string]any{"code": "rate_limit_exceeded"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_b"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_b"}); got != "account-b" {
		t.Fatalf("response owner = %q, want account-b", got)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketPreviousResponseSwitchesBeforeSend(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_next"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 90)
	b := testAccount("account-b", 0)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": "resp_a",
		"input":                []any{},
	})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketHardRateLimitDoesNotReplay(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "error",
			"status": http.StatusTooManyRequests,
			"error":  map[string]any{"code": "rate_limit_exceeded"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinityTurnState, value: "turn"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"X-Codex-Turn-State": {"turn"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "error" || event.Status != http.StatusTooManyRequests {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketSoftServerFailureReplaysOnAnotherAccount(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-a" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":   "error",
				"status": http.StatusBadGateway,
				"error":  map[string]any{"code": "upstream_error"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_b"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketSoftDisconnectReplaysOnAnotherAccount(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-a" {
			conn.CloseNow()
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_b"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketHardServerFailureDoesNotReplay(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "error",
			"status": http.StatusBadGateway,
			"error":  map[string]any{"code": "upstream_error"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinityTurnState, value: "turn"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"X-Codex-Turn-State": {"turn"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "error" || event.Status != http.StatusBadGateway {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketFailureAfterCreatedDoesNotReplay(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_a"},
		})
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "error",
			"status": http.StatusBadGateway,
			"error":  map[string]any{"code": "upstream_error"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	created := readWebSocketEvent(t, conn)
	failed := readWebSocketEvent(t, conn)
	if created.Type != "response.created" || failed.Type != "error" {
		t.Fatalf("events = %+v, %+v", created, failed)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketUnknownHardAffinityPinsBeforeRateLimit(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "error",
			"status": http.StatusTooManyRequests,
			"error":  map[string]any{"code": "rate_limit_exceeded"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"X-Codex-Turn-State": {"turn"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "error" || event.Status != http.StatusTooManyRequests {
		t.Fatalf("event = %+v", event)
	}
	if got := store.lookup(affinityRef{kind: affinityTurnState, value: "turn"}); got != "account-a" {
		t.Fatalf("turn owner = %q, want account-a", got)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketMissingPreviousResponseFailsBeforeSend(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		t.Error("unexpected upstream request")
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	proxy, _, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a})
	defer closeProxy()

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": "missing",
		"input":                []any{},
	})
	event := readWebSocketEvent(t, conn)
	if event.Type != "error" || event.Status != http.StatusServiceUnavailable {
		t.Fatalf("event = %+v", event)
	}
	if len(upstream.RequestAccounts()) != 0 {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketSoftHandshakeRateLimitUsesAnotherAccount(t *testing.T) {
	upstream := newAffinityWebSocketHandshakeUpstream(t, "account-a")
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.ConnectionAccounts()) != "[account-a account-b]" {
		t.Fatalf("connection accounts = %v", upstream.ConnectionAccounts())
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketHardHandshakeRateLimitFailsClosed(t *testing.T) {
	upstream := newAffinityWebSocketHandshakeUpstream(t, "account-a")
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinityTurnState, value: "turn"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Codex-Turn-State": {"turn"}},
	})
	if err == nil {
		t.Fatal("expected websocket dial failure")
	}
	if response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("response = %v, error = %v", response, err)
	}
	if fmt.Sprint(upstream.ConnectionAccounts()) != "[account-a]" {
		t.Fatalf("connection accounts = %v", upstream.ConnectionAccounts())
	}
}

type affinityWebSocketUpstream struct {
	*httptest.Server
	mu          sync.Mutex
	connections []string
	requests    []string
}

func newAffinityWebSocketUpstream(
	t *testing.T,
	respond func(string, *websocket.Conn, websocketEnvelope),
) *affinityWebSocketUpstream {
	t.Helper()
	upstream := &affinityWebSocketUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		account := r.Header.Get("chatgpt-account-id")
		upstream.mu.Lock()
		upstream.connections = append(upstream.connections, account)
		upstream.mu.Unlock()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request websocketEnvelope
			if err := json.Unmarshal(data, &request); err != nil {
				t.Error(err)
				return
			}
			if request.Type != "response.create" {
				continue
			}
			upstream.mu.Lock()
			upstream.requests = append(upstream.requests, account)
			upstream.mu.Unlock()
			respond(account, conn, request)
		}
	}))
	return upstream
}

func newAffinityWebSocketHandshakeUpstream(t *testing.T, rejected string) *affinityWebSocketUpstream {
	t.Helper()
	upstream := &affinityWebSocketUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		upstream.mu.Lock()
		upstream.connections = append(upstream.connections, account)
		upstream.mu.Unlock()
		if account == rejected {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request websocketEnvelope
			if err := json.Unmarshal(data, &request); err != nil {
				t.Error(err)
				return
			}
			if request.Type != "response.create" {
				continue
			}
			upstream.mu.Lock()
			upstream.requests = append(upstream.requests, account)
			upstream.mu.Unlock()
			writeWebSocketEvent(t, conn, map[string]any{
				"type":     "response.created",
				"response": map[string]any{"id": "response"},
			})
		}
	}))
	return upstream
}

func (u *affinityWebSocketUpstream) RequestAccounts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.requests...)
}

func (u *affinityWebSocketUpstream) ConnectionAccounts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.connections...)
}

func newAffinityProxyWebSocketServer(
	t *testing.T,
	upstream string,
	accounts []*Account,
) (*httptest.Server, *AffinityStore, func()) {
	t.Helper()
	server, store, closeUpstream := newAffinityHTTPServer(t, accounts, func(http.ResponseWriter, *http.Request) {})
	closeUpstream()
	server.upstream = upstream
	proxy := httptest.NewServer(server.routes())
	return proxy, store, proxy.Close
}

func dialAffinityWebSocket(t *testing.T, proxyURL string, headers http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxyURL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		if response != nil {
			t.Fatalf("dial: %v, status = %d", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	return conn
}

func writeWebSocketEvent(t *testing.T, conn *websocket.Conn, value any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, value); err != nil {
		t.Fatal(err)
	}
}

func readWebSocketEvent(t *testing.T, conn *websocket.Conn) websocketEnvelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var event websocketEnvelope
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatal(err)
	}
	return event
}
