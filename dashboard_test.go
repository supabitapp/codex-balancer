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
	stats.routed("019fe5c2", "account", serviceTierFast, transportWebSocket)
	tokenPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"a@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"unused","chatgpt_plan_type":"pro"}}`))
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{
		"uptime_seconds":   true,
		"turns":            true,
		"websocket_turns":  true,
		"open_websockets":  true,
		"threads":          true,
		"failovers":        true,
		"rate_limits":      true,
		"average_ttfb_ms":  true,
		"account_statuses": true,
		"activity":         true,
	}
	for field := range fields {
		if !expected[field] {
			t.Fatalf("unexpected public field %q", field)
		}
	}
	for field := range expected {
		if _, ok := fields[field]; !ok {
			t.Fatalf("missing public field %q", field)
		}
	}
	var response dashboardResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	if response.Turns != 1 || response.WebSocketTurns != 1 || response.Threads != 1 || response.AccountStatuses[accountChecking] != 1 || response.Activity[0] != 1 {
		t.Fatalf("unexpected dashboard response: %+v", response)
	}
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
