package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWebSocketPinsAccountAcrossTurnsAndCompaction(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "response_" + account},
		})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	account := testAccount("account-a", 0)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{account, testAccount("account-b", 20)})
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	defer conn.CloseNow()

	for _, metadata := range []turnMetadata{
		{RequestKind: "turn", ThreadID: "thread", TurnID: "one"},
		{RequestKind: "compaction", ThreadID: "thread", TurnID: "compact"},
		{RequestKind: "turn", ThreadID: "thread", TurnID: "two"},
	} {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":            "response.create",
			"client_metadata": map[string]string{codexTurnMetadataKey: encodeTurnMetadata(metadata)},
			"input":           []any{},
		})
		readWebSocketEvent(t, conn)
		readWebSocketEvent(t, conn)
	}
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-a account-a account-a]" {
		t.Fatalf("request accounts = %s", got)
	}
	snapshot := server.stats.snapshot()
	if len(snapshot.Threads) != 1 || snapshot.Threads[0].Compactions != 1 || snapshot.Threads[0].Metadata.TurnID != "two" {
		t.Fatalf("thread snapshot = %+v", snapshot.Threads)
	}
}

func TestWebSocketSocketsChooseIndependentAccounts(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "response"},
		})
	})
	defer upstream.Close()
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("account-a", 0), testAccount("account-b", 0)})
	first, _ := dialWebSocket(t, proxy.URL, nil)
	defer first.CloseNow()
	second, _ := dialWebSocket(t, proxy.URL, nil)
	defer second.CloseNow()
	writeWebSocketEvent(t, first, map[string]any{"type": "response.create", "input": []any{}})
	writeWebSocketEvent(t, second, map[string]any{"type": "response.create", "input": []any{}})
	readWebSocketEvent(t, first)
	readWebSocketEvent(t, second)
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a account-b]" {
		t.Fatalf("connection accounts = %s", got)
	}
	snapshot := server.stats.snapshot()
	if snapshot.Accounts["account-a"].WSOpen != 1 || snapshot.Accounts["account-b"].WSOpen != 1 {
		t.Fatalf("open sockets = %+v", snapshot.Accounts)
	}
	first.CloseNow()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot = server.stats.snapshot()
		if snapshot.Accounts["account-a"].WSOpen == 0 && snapshot.Accounts["account-b"].WSOpen == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("open sockets after close = %+v", snapshot.Accounts)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWebSocketUsageDoesNotMovePinnedSockets(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type": "response.created",
			"headers": map[string]any{
				"x-codex-primary-used-percent":   "99",
				"x-codex-primary-window-minutes": "300",
			},
		})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("account-a", 0), testAccount("account-b", 0)})
	first, _ := dialWebSocket(t, proxy.URL, nil)
	defer first.CloseNow()
	second, _ := dialWebSocket(t, proxy.URL, nil)
	defer second.CloseNow()
	waitForWebSocketCounts(t, server, map[string]int64{"account-a": 1, "account-b": 1})

	for _, conn := range []*websocket.Conn{second, first} {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
		readWebSocketEvent(t, conn)
		readWebSocketEvent(t, conn)
	}
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-b account-a]" {
		t.Fatalf("request accounts = %s", got)
	}
}

func TestWebSocketRejectionClosesWithoutReplayAndReconnects(t *testing.T) {
	requests := 0
	upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		requests++
		if requests == 1 {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":    "error",
				"status":  http.StatusTooManyRequests,
				"headers": map[string]any{"retry-after": "30"},
				"error":   map[string]any{"type": "invalid_request_error", "code": "rate_limit_exceeded"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, testAccount("account-b", 20)})
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	readCloseStatus(t, conn, websocket.StatusServiceRestart)
	conn.CloseNow()

	reconnected, _ := dialWebSocket(t, proxy.URL, nil)
	defer reconnected.CloseNow()
	writeWebSocketEvent(t, reconnected, map[string]any{"type": "response.create", "input": []any{}})
	readWebSocketEvent(t, reconnected)
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-a account-b]" {
		t.Fatalf("request accounts = %s", got)
	}
	if got := server.stats.snapshot().Limited; got != 1 {
		t.Fatalf("limited = %d, want 1", got)
	}
}

func TestWebSocketConnectionLimitCoolsAccountDown(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-full" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":  "error",
				"error": map[string]any{"code": "websocket_connection_limit_reached"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
	})
	defer upstream.Close()
	healthy := testAccount("account-healthy", 10)
	full := testAccount("account-full", 0)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{healthy, full})
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	readCloseStatus(t, conn, websocket.StatusServiceRestart)
	conn.CloseNow()
	if _, _, cooldown, _ := full.health(); !cooldown.After(time.Now()) {
		t.Fatalf("cooldown = %s, want future", cooldown)
	}
	reconnected, _ := dialWebSocket(t, proxy.URL, nil)
	defer reconnected.CloseNow()
	writeWebSocketEvent(t, reconnected, map[string]any{"type": "response.create", "input": []any{}})
	readWebSocketEvent(t, reconnected)
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-full account-healthy]" {
		t.Fatalf("connection accounts = %s", got)
	}
}

func TestWebSocketCredentialAndUsageRejectionsRestart(t *testing.T) {
	tests := []struct {
		name    string
		event   map[string]any
		limited int64
		spent   bool
	}{
		{
			name:  "credential",
			event: map[string]any{"type": "error", "status": http.StatusUnauthorized, "error": map[string]any{"code": "unauthorized"}},
		},
		{
			name:    "usage",
			event:   map[string]any{"type": "error", "error": map[string]any{"code": "usage_limit_reached"}},
			limited: 1,
			spent:   true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
				writeWebSocketEvent(t, conn, test.event)
			})
			defer upstream.Close()
			account := testAccount("account-a", 0)
			server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{account})
			conn, _ := dialWebSocket(t, proxy.URL, nil)
			writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
			readCloseStatus(t, conn, websocket.StatusServiceRestart)
			conn.CloseNow()
			candidate := account.routingCandidate()
			if candidate.spent != test.spent || server.stats.snapshot().Limited != test.limited {
				t.Fatalf("candidate = %+v, limited = %d", candidate, server.stats.snapshot().Limited)
			}
			if test.name == "credential" && candidate.cooldown.IsZero() {
				t.Fatal("credential rejection did not cool account")
			}
			if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-a]" {
				t.Fatalf("request accounts = %s", got)
			}
		})
	}
}

func TestWebSocketHandshakeFailsOverBeforeAccept(t *testing.T) {
	upstream := newWebSocketHandshakeUpstream(t, http.StatusTooManyRequests, "account-a")
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("account-a", 0), testAccount("account-b", 20)})
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	readWebSocketEvent(t, conn)
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a account-b]" {
		t.Fatalf("connection accounts = %s", got)
	}
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-b]" {
		t.Fatalf("request accounts = %s", got)
	}
}

func TestWebSocketHandshakeFailsOverToEveryAccount(t *testing.T) {
	upstream := newWebSocketHandshakeUpstream(t, http.StatusTooManyRequests, "account-a", "account-b", "account-c")
	defer upstream.Close()
	accounts := []*Account{
		testAccount("account-a", 0),
		testAccount("account-b", 10),
		testAccount("account-c", 20),
		testAccount("account-d", 30),
	}
	_, proxy := newWebSocketProxy(t, upstream.URL, accounts)
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	readWebSocketEvent(t, conn)
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a account-b account-c account-d]" {
		t.Fatalf("connection accounts = %s", got)
	}
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-d]" {
		t.Fatalf("request accounts = %s", got)
	}
}

func TestWebSocketHandshakeServerFailurePassesThroughWithoutFailover(t *testing.T) {
	upstream := newWebSocketHandshakeUpstream(t, http.StatusBadGateway, "account-a")
	defer upstream.Close()
	a := testAccount("account-a", 0)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, testAccount("account-b", 20)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", nil)
	if err == nil {
		t.Fatal("dial succeeded")
	}
	if response == nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("response = %+v", response)
	}
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a account-a account-a account-a]" {
		t.Fatalf("connection accounts = %s", got)
	}
	_, _, cooldown, _ := a.health()
	if !cooldown.IsZero() {
		t.Fatalf("cooldown = %s", cooldown)
	}
}

func TestWebSocketUnreachableUpstreamDoesNotPenalizeAccounts(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	_, proxy := newWebSocketProxy(t, "http://127.0.0.1:1", []*Account{a, b})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", nil)
	if err == nil {
		t.Fatal("dial succeeded")
	}
	if response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response = %+v", response)
	}
	for _, account := range []*Account{a, b} {
		_, _, cooldown, _ := account.health()
		if !cooldown.IsZero() {
			t.Fatalf("cooldown = %s", cooldown)
		}
	}
}

func TestWebSocketGenericErrorsPassThroughAndStayOnSocket(t *testing.T) {
	errors := []map[string]any{
		{"type": "error", "status": http.StatusBadRequest, "error": map[string]any{"type": "invalid_request_error", "message": "bad input"}},
		{"type": "error", "status": http.StatusBadRequest, "error": map[string]any{"code": "invalid_encrypted_content"}},
		{"type": "error", "status": http.StatusNotFound, "error": map[string]any{"code": "previous_response_not_found"}},
		{"type": "error", "status": http.StatusBadGateway, "error": map[string]any{"code": "upstream_error"}},
	}
	requests := 0
	upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if requests < len(errors) {
			writeWebSocketEvent(t, conn, errors[requests])
			requests++
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
	})
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("account-a", 0)})
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	defer conn.CloseNow()
	for index, want := range []string{"invalid_request_error", "invalid_encrypted_content", "previous_response_not_found", "upstream_error"} {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
		event := readWebSocketEvent(t, conn)
		if event.Type != "error" || !websocketErrorIs(event, want) {
			t.Fatalf("error event %d = %+v", index, event)
		}
	}
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	if readWebSocketEvent(t, conn).Type != "response.created" {
		t.Fatal("second turn did not stay on socket")
	}
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a]" {
		t.Fatalf("connection accounts = %s", got)
	}
}

func TestWebSocketUpstreamTransportLossRestartsDownstream(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		conn.CloseNow()
	})
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("account-a", 0)})
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	readCloseStatus(t, conn, websocket.StatusServiceRestart)
}

func TestWebSocketTracksUsageHeadersAndMetadata(t *testing.T) {
	var mu sync.Mutex
	serviceTiers := []string{}
	upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		mu.Lock()
		serviceTiers = append(serviceTiers, request.ServiceTier)
		mu.Unlock()
		writeWebSocketEvent(t, conn, map[string]any{
			"type": "response.created",
			"headers": map[string]any{
				"x-codex-primary-used-percent":   "97",
				"x-codex-primary-window-minutes": "300",
			},
			"response": map[string]any{"id": "response"},
		})
		writeWebSocketEvent(t, conn, map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"model":        "gpt-5.6-sol",
				"service_tier": "priority",
				"usage":        map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14},
			},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 96)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a})
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{
		"type":            "response.create",
		"model":           "gpt-5.6-sol",
		"service_tier":    "default",
		"client_metadata": map[string]string{codexTurnMetadataKey: encodeTurnMetadata(turnMetadata{ThreadID: "thread", TurnID: "turn", RequestKind: "compaction"})},
		"input":           []any{},
	})
	readWebSocketEvent(t, conn)
	readWebSocketEvent(t, conn)
	mu.Lock()
	gotTier := append([]string(nil), serviceTiers...)
	mu.Unlock()
	if fmt.Sprint(gotTier) != "[default]" {
		t.Fatalf("service tiers = %v", gotTier)
	}
	primary, _, _, _ := a.health()
	if primary.usedPercent != 97 {
		t.Fatalf("primary usage = %v", primary.usedPercent)
	}
	snapshot := server.stats.snapshot()
	if snapshot.MonthlyUsage.InputTokens != 10 || len(snapshot.Threads) != 1 || snapshot.Threads[0].Metadata.TurnID != "turn" || snapshot.Threads[0].Compactions != 1 {
		t.Fatalf("stats = %+v", snapshot)
	}
}

func TestWebSocketCanceledHandshakeDoesNotPenalizeAccounts(t *testing.T) {
	server := newTestServer(t, []*Account{testAccount("account-a", 0)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil).WithContext(ctx)
	_, _, err := server.dialResponsesWebSocket(request, "session")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	_, _, cooldown, _ := server.pool.all()[0].health()
	if !cooldown.IsZero() {
		t.Fatalf("cooldown = %s", cooldown)
	}
}

func readCloseStatus(t *testing.T, conn *websocket.Conn, want websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != want {
		t.Fatalf("close status = %d, error = %v, want %d", got, err, want)
	}
}

func waitForWebSocketCounts(t *testing.T, server *server, want map[string]int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot := server.stats.snapshot()
		matched := true
		for account, count := range want {
			matched = matched && snapshot.Accounts[account].WSOpen == count
		}
		if matched {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("open sockets = %+v, want %v", snapshot.Accounts, want)
		}
		time.Sleep(time.Millisecond)
	}
}
