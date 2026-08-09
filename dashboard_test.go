package main

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestDashboardPageConnectsHTMXWebSocket(t *testing.T) {
	server := &server{pool: &Pool{}, stats: newStats()}
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("status = %s, CSP = %q", response.Status, response.Header.Get("Content-Security-Policy"))
	}
	for _, expected := range []string{
		`src="/dashboard/assets/dashboard.js"`,
		`src="/dashboard/assets/htmx-2.0.10.min.js"`,
		`src="/dashboard/assets/ws-2.0.4.min.js"`,
		`hx-ext="ws"`,
		`ws-connect="/dashboard/ws"`,
		`id="dashboard"`,
		`nothing routed yet`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q", expected)
		}
	}
}

func TestDashboardScriptsAreServedFromBinary(t *testing.T) {
	server := &server{pool: &Pool{}, stats: newStats()}
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	for _, asset := range []struct {
		path string
		min  int
	}{
		{"/dashboard/assets/dashboard.js", 500},
		{"/dashboard/assets/htmx-2.0.10.min.js", 1_000},
		{"/dashboard/assets/ws-2.0.4.min.js", 1_000},
	} {
		response, err := http.Get(httpServer.URL + asset.path)
		if err != nil {
			t.Fatal(err)
		}
		payload, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/javascript") || len(payload) < asset.min {
			t.Fatalf("%s returned status %s, content type %q, length %d", asset.path, response.Status, response.Header.Get("Content-Type"), len(payload))
		}
		if response.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("%s cache control = %q", asset.path, response.Header.Get("Cache-Control"))
		}
	}
}

func TestDashboardWebSocketStreamsEscapedHTML(t *testing.T) {
	stats := newStats()
	stats.routed("019fe5c2private", "unused", serviceTierFast, transportWebSocket)
	stats.recordUsage("gpt-5.6-sol", "default", responseUsage{OutputTokens: 1_000_000})
	stats.failedOver("unused", "<script>upstream unavailable</script>")
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
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("message type = %v", messageType)
	}
	body := string(payload)
	for _, expected := range []string{
		`hx-swap-oob="outerHTML"`,
		`a***e@***.com`,
		`<td class="dim">pro</td>`,
		`019fe5c2`,
		`<td class="status">◌ checking</td>`,
		`<td>WS</td>`,
		`class="fast-icon"`,
		`aria-label="Fast"`,
		`API estimate`,
		`$30.00`,
		`<td>failover</td>`,
		`&lt;script&gt;upstream unavailable&lt;/script&gt;`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard update missing %q:\n%s", expected, body)
		}
	}
	for _, private := range []string{"alice@example.com", "019fe5c2private", "<script>"} {
		if strings.Contains(body, private) {
			t.Fatalf("dashboard update exposed %q", private)
		}
	}
}

func TestDashboardBankedResetHoverShowsExpirations(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(4 * time.Minute)
	laterExpiresAt := now.Add(2 * time.Hour)
	account := testAccount("account-a", 20)
	account.adoptResetCredits(2, []resetCredit{
		{
			ID:          "private-credit-id",
			ResetType:   "codex_rate_limits",
			Status:      "available",
			ExpiresAt:   &expiresAt,
			Title:       "Full <reset>",
			Description: "Weekly & five-hour windows",
		},
		{
			ID:          "second-private-credit-id",
			ResetType:   "codex_rate_limits",
			Status:      "available",
			ExpiresAt:   &laterExpiresAt,
			Title:       "Another reset",
			Description: "More detail",
		},
	})
	server := &server{pool: &Pool{accounts: []*Account{account}}, stats: newStats()}

	stats := server.currentStats(now)
	if len(stats.Accounts) != 1 || len(stats.Accounts[0].ResetCredits) != 2 {
		t.Fatalf("reset credits = %+v", stats.Accounts)
	}
	view := server.currentDashboard(now)
	wantInfo := "Expires in 4m at 2026-08-09 12:04 UTC\nExpires in 2h00m at 2026-08-09 14:00 UTC"
	if len(view.Accounts) != 1 || view.Accounts[0].BankedInfo != wantInfo {
		t.Fatalf("banked info = %q", view.Accounts[0].BankedInfo)
	}
	payload, err := renderDashboard("dashboard", view)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, expected := range []string{`class="banked-info"`, `data-info="Expires in 4m at 2026-08-09 12:04 UTC`, `Expires in 2h00m at 2026-08-09 14:00 UTC`, `aria-describedby="banked-tooltip"`, `tabindex="0"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q:\n%s", expected, body)
		}
	}
	for _, absent := range []string{"private-credit-id", "second-private-credit-id", "Full &lt;reset&gt;", "Weekly &amp; five-hour windows", "Another reset", "More detail"} {
		if strings.Contains(body, absent) {
			t.Fatalf("dashboard exposed %q", absent)
		}
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
