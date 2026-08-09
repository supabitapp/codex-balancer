package main

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestDashboardWebSocketAuthenticatesAndStreams(t *testing.T) {
	stats := newStats()
	stats.routed("019fe5c2", "account", serviceTierFast, transportWebSocket)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"a@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"unused","chatgpt_plan_type":"pro"}}`))
	account := accountFromState(accountState{IDToken: "x." + payload + ".x"})
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
	if err := wsjson.Write(ctx, conn, dashboardAuth{Password: "secret"}); err != nil {
		t.Fatal(err)
	}
	var response dashboardResponse
	if err := wsjson.Read(ctx, conn, &response); err != nil {
		t.Fatal(err)
	}
	if response.Stats.Turns != 1 || len(response.Stats.Accounts) != 1 || response.Stats.Accounts[0].Activity == nil || len(response.Threads) != 1 || response.Threads[0].Key != "019fe5c2" || response.Events == nil {
		t.Fatalf("unexpected dashboard response: %+v", response)
	}
}

func TestDashboardWebSocketRejectsInvalidPassword(t *testing.T) {
	server := &server{pool: &Pool{}, stats: newStats(), key: "secret"}
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/dashboard/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(ctx, conn, dashboardAuth{Password: "wrong"}); err != nil {
		t.Fatal(err)
	}
	var response dashboardResponse
	err = wsjson.Read(ctx, conn, &response)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %d, want %d: %v", websocket.CloseStatus(err), websocket.StatusPolicyViolation, err)
	}
}
