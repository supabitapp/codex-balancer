package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestDashboardWebSocketStreamsPublicSnapshot(t *testing.T) {
	stats := newStats()
	stats.routed("019fe5c2private", "unused", serviceTierFast, transportWebSocket)
	stats.failedOver("unused", "upstream unavailable")
	tokenPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"alice@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"unused","chatgpt_plan_type":"pro"}}`))
	account := accountFromState(accountState{IDToken: "x." + tokenPayload + ".x"})
	server := &server{pool: &Pool{accounts: []*Account{account}}, stats: stats, key: "secret"}
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/dashboard/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var payload json.RawMessage
	if err := wsjson.Read(ctx, conn, &payload); err != nil {
		t.Fatal(err)
	}
	fields := jsonFields(t, payload,
		"uptime_seconds", "turns", "websocket_turns", "open_websockets",
		"failovers", "rate_limits", "average_ttfb_ms", "accounts", "threads", "events",
	)
	var accounts []json.RawMessage
	if err := json.Unmarshal(fields["accounts"], &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	jsonFields(t, accounts[0],
		"name", "plan", "status", "weekly_remaining_percent", "banked_resets",
		"reset_at", "turns", "open_websockets", "rate_limits", "activity",
	)
	var threads []json.RawMessage
	if err := json.Unmarshal(fields["threads"], &threads); err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 {
		t.Fatalf("threads = %d, want 1", len(threads))
	}
	jsonFields(t, threads[0], "key_prefix", "account", "service_tier", "turns", "last", "via")
	var events []json.RawMessage
	if err := json.Unmarshal(fields["events"], &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	jsonFields(t, events[0], "at", "kind", "account", "detail")
	var response dashboardResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	if response.Turns != 1 || response.WebSocketTurns != 1 || response.Failovers != 1 || response.Accounts[0].Name != "a***e@***.com" || response.Accounts[0].Plan != "pro" || response.Accounts[0].Status != accountChecking || response.Accounts[0].Activity[0] != 1 || response.Threads[0].KeyPrefix != "019fe5c2" || response.Threads[0].Account != "a***e@***.com" || response.Events[0].Account != "a***e@***.com" {
		t.Fatalf("unexpected dashboard response: %+v", response)
	}
}

func jsonFields(t *testing.T, payload json.RawMessage, expected ...string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != len(expected) {
		t.Fatalf("fields = %v, want %v", fields, expected)
	}
	for _, field := range expected {
		if fields[field] == nil {
			t.Fatalf("missing public field %q", field)
		}
	}
	return fields
}

func TestDashboardWebSocketRejectsWhenFull(t *testing.T) {
	server := &server{pool: &Pool{}, stats: newStats()}
	server.dashboardConnections.Store(dashboardMaxConnections)
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/dashboard/ws", nil)
	if conn != nil {
		conn.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response = %v, error = %v", response, err)
	}
	response.Body.Close()
}
