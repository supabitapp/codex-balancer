package app

import (
	"context"
	"encoding/json"
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

func TestWebSocketRouteKeepsSessionAndThreadIdentitySeparate(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		want    websocketRoute
	}{
		{
			name: "thread-id wins over request id because it states the conversation identity",
			headers: http.Header{
				"Session-Id":          {"session"},
				"Thread-Id":           {"thread"},
				"X-Client-Request-Id": {"request"},
			},
			want: websocketRoute{session: "session", thread: "thread"},
		},
		{
			name: "request id recovers thread identity for clients without thread-id",
			headers: http.Header{
				"Session-Id":          {"session"},
				"X-Client-Request-Id": {"thread"},
			},
			want: websocketRoute{session: "session", thread: "thread"},
		},
		{
			name:    "underscore session header keeps older Codex sessions stable",
			headers: http.Header{"Session_id": {"session"}, "Thread-Id": {"thread"}},
			want:    websocketRoute{session: "session", thread: "thread"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := websocketRouteFrom(test.headers); got != test.want {
				t.Fatalf("route = %+v, want separate identities %+v", got, test.want)
			}
		})
	}
}

func TestWebSocketReconnectRetainsAcceptedThreadOwner(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	fresh := testAccount("account-fresh", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
	headers := http.Header{"Session-Id": {"session"}, "Thread-Id": {"thread"}}

	first, _ := dialWebSocket(t, proxy.URL, headers)
	writeWebSocketEvent(t, first, map[string]any{"type": "response.create", "input": []any{}})
	readWebSocketEvent(t, first)
	readWebSocketEvent(t, first)
	first.CloseNow()
	setTestAccountUsage(owner, 90)
	setTestAccountUsage(fresh, 0)
	fresh.RoutingMode = routingModePriority

	second, _ := dialWebSocket(t, proxy.URL, headers)
	defer second.CloseNow()
	writeWebSocketEvent(t, second, map[string]any{"type": "response.create", "input": []any{}})
	readWebSocketEvent(t, second)
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner account-owner]" {
		t.Fatalf("request accounts = %s, want reconnect to retain the accepted thread owner", got)
	}
}

func TestWebSocketReconnectUsesHandshakeThreadWhenMetadataNamesAStatsThread(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	fresh := testAccount("account-fresh", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
	headers := codexWebSocketHeaders("", "route-thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, first, map[string]any{
		"type": "response.create",
		"client_metadata": map[string]string{
			codexTurnMetadataKey: encodeTurnMetadata(turnMetadata{ThreadID: "stats-thread"}),
		},
		"input": []any{},
	})
	first.CloseNow()
	setTestAccountUsage(owner, 90)
	setTestAccountUsage(fresh, 0)
	fresh.RoutingMode = routingModePriority

	second, _ := dialWebSocket(t, proxy.URL, headers)
	defer second.CloseNow()
	completeWebSocketTurn(t, second, map[string]any{"type": "response.create", "input": []any{}})
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner account-owner]" {
		t.Fatalf("request accounts = %s, want the handshake thread to retain its owner when metadata uses a dashboard thread key", got)
	}
}

func TestWebSocketSpentOwnerRejectsPreviousResponseBeforeAccountMove(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	fresh := testAccount("account-fresh", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
	headers := http.Header{"Session-Id": {"session"}, "Thread-Id": {"thread"}}

	first, _ := dialWebSocket(t, proxy.URL, headers)
	writeWebSocketEvent(t, first, map[string]any{"type": "response.create", "input": []any{}})
	readWebSocketEvent(t, first)
	readWebSocketEvent(t, first)
	first.CloseNow()
	owner.markSpent()

	second, _ := dialWebSocket(t, proxy.URL, headers)
	defer second.CloseNow()
	writeWebSocketEvent(t, second, map[string]any{
		"type":                 "response.create",
		"previous_response_id": "response-owner",
		"input":                []any{},
	})
	readCloseStatus(t, second, websocket.StatusTryAgainLater)
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner]" {
		t.Fatalf("request accounts = %s, want account-bound response state withheld from the fresh account", got)
	}
}

func TestWebSocketPortableMoveAllowsNewSocketResponseChainAfterAcceptance(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "response-" + account},
		})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	fresh := testAccount("account-fresh", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
	headers := http.Header{"Session-Id": {"session"}, "Thread-Id": {"thread"}}

	first, _ := dialWebSocket(t, proxy.URL, headers)
	writeWebSocketEvent(t, first, map[string]any{"type": "response.create", "input": []any{}})
	readWebSocketEvent(t, first)
	readWebSocketEvent(t, first)
	first.CloseNow()
	owner.markSpent()

	second, _ := dialWebSocket(t, proxy.URL, headers)
	defer second.CloseNow()
	writeWebSocketEvent(t, second, map[string]any{"type": "response.create", "input": []any{}})
	readWebSocketEvent(t, second)
	readWebSocketEvent(t, second)
	writeWebSocketEvent(t, second, map[string]any{
		"type":                 "response.create",
		"previous_response_id": "response-account-fresh",
		"input":                []any{},
	})
	readWebSocketEvent(t, second)
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner account-fresh account-fresh]" {
		t.Fatalf("request accounts = %s, want accepted move to establish the new socket response chain", got)
	}
}

func TestWebSocketOverlappingUnacceptedConnectionsShareProvisionalAccount(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, _ *websocket.Conn, _ websocketEnvelope) {})
	defer upstream.Close()
	firstAccount := testAccount("account-a", 10)
	otherAccount := testAccount("account-b", 10)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{firstAccount, otherAccount})
	headers := codexWebSocketHeaders("session", "thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	defer first.CloseNow()
	if got := upstream.ConnectionAccounts(); fmt.Sprint(got) != "[account-a]" {
		t.Fatalf("first connection accounts = %v, want account-a", got)
	}

	otherAccount.RoutingMode = routingModePriority
	second, _ := dialWebSocket(t, proxy.URL, headers)
	defer second.CloseNow()
	if got := upstream.ConnectionAccounts(); fmt.Sprint(got) != "[account-a account-a]" {
		t.Fatalf("connection accounts = %v, want the provisional account shared before response.created", got)
	}
}

func TestWebSocketOverlappingSiblingThreadsShareTheSessionClaim(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, _ *websocket.Conn, _ websocketEnvelope) {})
	defer upstream.Close()
	firstAccount := testAccount("account-a", 10)
	otherAccount := testAccount("account-b", 10)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{firstAccount, otherAccount})

	root, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "root"))
	defer root.CloseNow()
	otherAccount.RoutingMode = routingModePriority
	child, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "child"))
	defer child.CloseNow()
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a account-a]" {
		t.Fatalf("connection accounts = %s, want sibling threads to share the provisional session account", got)
	}
}

func TestWebSocketClosingEveryUnacceptedConnectionReleasesTheClaim(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, _ *websocket.Conn, _ websocketEnvelope) {})
	defer upstream.Close()
	firstAccount := testAccount("account-a", 10)
	otherAccount := testAccount("account-b", 10)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{firstAccount, otherAccount})
	headers := codexWebSocketHeaders("session", "thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	waitForWebSocketCounts(t, server, map[string]int64{firstAccount.id(): 1})
	if err := first.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatal(err)
	}
	waitForWebSocketCounts(t, server, map[string]int64{firstAccount.id(): 0})
	otherAccount.RoutingMode = routingModePriority

	second, _ := dialWebSocket(t, proxy.URL, headers)
	defer second.CloseNow()
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a account-b]" {
		t.Fatalf("connection accounts = %s, want a fresh account after the unaccepted claim was released", got)
	}
}

func TestWebSocketResponseCreatedCommitsTheProvisionalClaim(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("account-a", 10)})
	conn, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "thread"))
	defer conn.CloseNow()

	server.routeClaims.mu.Lock()
	before := len(server.routeClaims.byID)
	server.routeClaims.mu.Unlock()
	if before != 1 {
		t.Fatalf("claims before response.created = %d, want 1", before)
	}
	completeWebSocketTurn(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	server.routeClaims.mu.Lock()
	after := len(server.routeClaims.byID)
	server.routeClaims.mu.Unlock()
	if after != 0 {
		t.Fatalf("claims after response.created = %d, want the durable route to replace the claim", after)
	}
}

func TestWebSocketRouteAcceptanceRejectsAnInvalidatedClaim(t *testing.T) {
	account := testAccount("account-a", 10)
	server := newTestServer(t, []*Account{account})
	route := websocketRoute{session: "session", thread: "thread"}
	selection := server.routeClaims.selectAccount(route, durableRouteOwners{}, func([]string) routingDecision {
		return routingDecision{account: account}
	})
	dial := &websocketDial{account: account, claim: selection.claim}

	account.mu.Lock()
	account.Paused = true
	account.mu.Unlock()
	server.invalidateAccount(account.id(), routingReasonOwnerPaused)
	accepted := server.acceptWebSocketRoute(dial, storedRoute{
		At:      time.Now(),
		Session: route.session,
		Thread:  route.thread,
		Account: account.id(),
	})
	if accepted.allowed || accepted.persisted {
		t.Fatalf("acceptance = %+v, want an invalidated claim rejected", accepted)
	}
	owners, err := server.pool.store.routeOwners(route.thread, route.session)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(owners) != "[account-a]" {
		t.Fatalf("owners = %v, want the invalidation barrier", owners)
	}
}

func TestWebSocketAccountInvalidationPreservesAMoveBarrierUntilReplacementAcceptance(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	account := testAccount("account-a", 10)
	replacement := testAccount("account-b", 20)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{account, replacement})
	headers := codexWebSocketHeaders("session", "thread")
	conn, _ := dialWebSocket(t, proxy.URL, headers)
	waitForWebSocketCounts(t, server, map[string]int64{account.id(): 1})

	account.mu.Lock()
	account.Paused = true
	account.mu.Unlock()
	server.invalidateAccount(account.id(), routingReasonOwnerSignedOut)
	readCloseStatus(t, conn, websocket.StatusServiceRestart)
	waitForWebSocketCounts(t, server, map[string]int64{account.id(): 0})
	server.routeClaims.mu.Lock()
	claims := len(server.routeClaims.byID)
	server.routeClaims.mu.Unlock()
	if claims != 0 {
		t.Fatalf("claims after invalidation = %d, want 0", claims)
	}
	owners, err := server.pool.store.routeOwners("thread", "session")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(owners) != "[account-a]" {
		t.Fatalf("owners after invalidation = %v, want the account-a move barrier", owners)
	}

	bound, _ := dialWebSocket(t, proxy.URL, headers)
	writeWebSocketEvent(t, bound, map[string]any{
		"type":            "response.create",
		"client_metadata": map[string]string{codexTurnStateKey: "account-a-state"},
		"input":           []any{},
	})
	readCloseStatus(t, bound, websocket.StatusTryAgainLater)
	bound.CloseNow()
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[]" {
		t.Fatalf("request accounts after account-bound reconnect = %s, want no cross-account request", got)
	}

	portable, _ := dialWebSocket(t, proxy.URL, headers)
	defer portable.CloseNow()
	completeWebSocketTurn(t, portable, map[string]any{"type": "response.create", "input": []any{}})
	owners, err = server.pool.store.routeOwners("thread", "session")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(owners) != "[account-b]" {
		t.Fatalf("owners after replacement acceptance = %v, want account-b", owners)
	}
}

func TestWebSocketChildThreadInheritsTheAcceptedSessionOwner(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	fresh := testAccount("account-fresh", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})

	root, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "root"))
	completeWebSocketTurn(t, root, map[string]any{"type": "response.create", "input": []any{}})
	root.CloseNow()
	setTestAccountUsage(owner, 90)
	setTestAccountUsage(fresh, 0)
	fresh.RoutingMode = routingModePriority

	child, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "child"))
	defer child.CloseNow()
	completeWebSocketTurn(t, child, map[string]any{"type": "response.create", "input": []any{}})
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner account-owner]" {
		t.Fatalf("request accounts = %s, want the new child to inherit its session cache owner", got)
	}
}

func TestWebSocketThreadOwnerBeatsANewerSiblingSessionOwner(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	threadOwner := testAccount("account-thread", 0)
	sessionOwner := testAccount("account-session", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{threadOwner, sessionOwner})

	root, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "root"))
	completeWebSocketTurn(t, root, map[string]any{"type": "response.create", "input": []any{}})
	root.CloseNow()
	threadOwner.markSpent()
	child, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "child"))
	completeWebSocketTurn(t, child, map[string]any{"type": "response.create", "input": []any{}})
	child.CloseNow()

	setTestAccountSpent(threadOwner, false)
	setTestAccountUsage(threadOwner, 90)
	setTestAccountUsage(sessionOwner, 0)
	sessionOwner.RoutingMode = routingModePriority
	reconnected, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "root"))
	defer reconnected.CloseNow()
	completeWebSocketTurn(t, reconnected, map[string]any{"type": "response.create", "input": []any{}})
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-thread account-session account-thread]" {
		t.Fatalf("request accounts = %s, want the root thread owner ahead of its session's newer sibling owner", got)
	}
}

func TestWebSocketAcceptedMoveDoesNotBounceWhenTheOldOwnerRecovers(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	oldOwner := testAccount("account-old", 0)
	newOwner := testAccount("account-new", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{oldOwner, newOwner})
	headers := codexWebSocketHeaders("session", "thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, first, map[string]any{"type": "response.create", "input": []any{}})
	first.CloseNow()
	oldOwner.markSpent()
	second, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, second, map[string]any{"type": "response.create", "input": []any{}})
	second.CloseNow()

	setTestAccountSpent(oldOwner, false)
	setTestAccountUsage(oldOwner, 0)
	oldOwner.RoutingMode = routingModePriority
	setTestAccountUsage(newOwner, 90)
	third, _ := dialWebSocket(t, proxy.URL, headers)
	defer third.CloseNow()
	completeWebSocketTurn(t, third, map[string]any{"type": "response.create", "input": []any{}})
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-old account-new account-new]" {
		t.Fatalf("request accounts = %s, want the latest accepted owner so old quota recovery cannot cause a cache bounce", got)
	}
}

func TestWebSocketAcceptedMoveLogsTheDurableSwitchReason(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	oldOwner := testAccount("account-old", 0)
	newOwner := testAccount("account-new", 20)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{oldOwner, newOwner})
	logs := captureTestLogs(server)
	headers := codexWebSocketHeaders("session", "thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, first, map[string]any{"type": "response.create", "input": []any{}})
	first.CloseNow()
	oldOwner.markSpent()

	second, _ := dialWebSocket(t, proxy.URL, headers)
	third, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, second, map[string]any{"type": "response.create", "input": []any{}})
	completeWebSocketTurn(t, third, map[string]any{"type": "response.create", "input": []any{}})
	second.CloseNow()
	third.CloseNow()
	output := logs.String()
	if got := strings.Count(output, `"msg":"websocket account switch accepted"`); got != 1 {
		t.Fatalf("accepted switch log count = %d, want 1 for joined sockets:\n%s", got, output)
	}
	for _, want := range []string{
		`"msg":"websocket account switch accepted"`,
		`"from_account":"account-old"`,
		`"to_account":"account-new"`,
		`"routing_reason":"owner_spent"`,
		`"route_persisted":true`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("accepted switch logs do not contain %s:\n%s", want, output)
		}
	}
}

func TestWebSocketCoolingOwnerReturnsRetryWithoutSpillingTheSession(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	fresh := testAccount("account-fresh", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
	headers := codexWebSocketHeaders("session", "thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, first, map[string]any{"type": "response.create", "input": []any{}})
	first.CloseNow()
	owner.rateLimited(http.Header{"Retry-After": {"60"}}, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers})
	if err == nil {
		t.Fatal("reconnect succeeded, want retry while the accepted owner cools down")
	}
	if response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("reconnect response = %+v, want 503 so the client retries without rebinding", response)
	}
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-owner]" {
		t.Fatalf("connection accounts = %s, want no spill connection while the owner may recover", got)
	}
}

func TestWebSocketTemporaryRefreshFailureDoesNotSpillAnAcceptedRoute(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	fresh := testAccount("account-fresh", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
	headers := codexWebSocketHeaders("session", "thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, first, map[string]any{"type": "response.create", "input": []any{}})
	first.CloseNow()
	owner.mu.Lock()
	owner.LastRefresh = time.Now().Add(-tokenRefreshFallback - time.Hour)
	owner.mu.Unlock()
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer oauth.Close()
	previousOAuthEndpoint := oauthEndpoint
	oauthEndpoint = oauth.URL
	defer func() { oauthEndpoint = previousOAuthEndpoint }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers})
	if err == nil {
		t.Fatal("reconnect succeeded, want retry after a temporary refresh failure on the accepted owner")
	}
	if response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("reconnect response = %+v, want 503 without replacing an owner that may refresh later", response)
	}
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-owner]" {
		t.Fatalf("connection accounts = %s, want no fresh-account handshake after a temporary owner refresh failure", got)
	}
}

func TestWebSocketSpentOwnerRejectsTurnStateBeforeAccountMove(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	fresh := testAccount("account-fresh", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
	headers := codexWebSocketHeaders("session", "thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, first, map[string]any{"type": "response.create", "input": []any{}})
	first.CloseNow()
	owner.markSpent()

	second, _ := dialWebSocket(t, proxy.URL, headers)
	defer second.CloseNow()
	writeWebSocketEvent(t, second, map[string]any{
		"type":            "response.create",
		"client_metadata": map[string]string{codexTurnStateKey: "state-from-owner"},
		"input":           []any{},
	})
	readCloseStatus(t, second, websocket.StatusTryAgainLater)
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner]" {
		t.Fatalf("request accounts = %s, want per-turn state withheld from a different account", got)
	}
}

func TestWebSocketFailedTurnDoesNotBindAnAccountBeforeResponseCreated(t *testing.T) {
	var mu sync.Mutex
	firstRequest := true
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		mu.Lock()
		reject := firstRequest
		firstRequest = false
		mu.Unlock()
		if reject {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":   "error",
				"status": http.StatusBadRequest,
				"error":  map[string]any{"code": "bad_request"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	failed := testAccount("account-failed", 0)
	fresh := testAccount("account-fresh", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{failed, fresh})
	headers := codexWebSocketHeaders("session", "thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	writeWebSocketEvent(t, first, map[string]any{"type": "response.create", "input": []any{}})
	if event := readWebSocketEvent(t, first); event.Type != "error" {
		t.Fatalf("failed turn event = %q, want error before any route becomes accepted", event.Type)
	}
	first.CloseNow()
	setTestAccountUsage(failed, 90)
	setTestAccountUsage(fresh, 0)
	fresh.RoutingMode = routingModePriority

	second, _ := dialWebSocket(t, proxy.URL, headers)
	defer second.CloseNow()
	completeWebSocketTurn(t, second, map[string]any{"type": "response.create", "input": []any{}})
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-failed account-fresh]" {
		t.Fatalf("request accounts = %s, want fresh placement because failure before response.created proves no owner", got)
	}
}

func TestWebSocketAcceptedWarmupBindsTheRouteWithoutCountingUsage(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	fresh := testAccount("account-fresh", 20)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
	headers := codexWebSocketHeaders("session", "thread")

	warmup, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, warmup, map[string]any{"type": "response.create", "generate": false, "input": []any{}})
	warmup.CloseNow()
	if got := server.stats.snapshot().Turns; got != 0 {
		t.Fatalf("turns after warmup = %d, want route acceptance without usage inflation", got)
	}
	setTestAccountUsage(owner, 90)
	setTestAccountUsage(fresh, 0)
	fresh.RoutingMode = routingModePriority

	turn, _ := dialWebSocket(t, proxy.URL, headers)
	defer turn.CloseNow()
	completeWebSocketTurn(t, turn, map[string]any{"type": "response.create", "input": []any{}})
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner account-owner]" {
		t.Fatalf("request accounts = %s, want response.created warmup to retain the cache owner", got)
	}
	if got := server.stats.snapshot().Turns; got != 1 {
		t.Fatalf("turns after real request = %d, want only generated work counted", got)
	}
}

func TestWebSocketPortableReplayMovesFromAnIncompatibleOwnerAndRebinds(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	oldOwner := testAccount("account-old", 0)
	newOwner := testAccount("account-new", 20)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{oldOwner, newOwner})
	headers := codexWebSocketHeaders("session", "thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, first, map[string]any{"type": "response.create", "input": []any{}})
	first.CloseNow()
	server.catalog.replace(
		[]string{oldOwner.id(), newOwner.id()},
		map[string][]modelEntry{
			oldOwner.id(): {testModelEntry("gpt-terra")},
			newOwner.id(): {testModelEntry("gpt-sol")},
		},
		"0.1.0",
	)

	second, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, second, map[string]any{"type": "response.create", "model": "gpt-sol", "input": []any{}})
	second.CloseNow()
	oldOwner.RoutingMode = routingModePriority
	third, _ := dialWebSocket(t, proxy.URL, headers)
	defer third.CloseNow()
	completeWebSocketTurn(t, third, map[string]any{"type": "response.create", "model": "gpt-sol", "input": []any{}})
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-old account-new account-new]" {
		t.Fatalf("request accounts = %s, want a full replay to move once and retain the compatible owner", got)
	}
}

func TestWebSocketRequestPortableRecognizesAccountBoundState(t *testing.T) {
	tests := []struct {
		name    string
		request map[string]any
		want    bool
	}{
		{name: "ordinary full replay", request: map[string]any{"input": []any{map[string]any{"type": "message", "content": "hello"}}}, want: true},
		{name: "previous response", request: map[string]any{"previous_response_id": "response"}},
		{name: "turn state", request: map[string]any{"client_metadata": map[string]string{codexTurnStateKey: "state"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(test.request)
			if err != nil {
				t.Fatal(err)
			}
			var event websocketEnvelope
			if err := json.Unmarshal(data, &event); err != nil {
				t.Fatal(err)
			}
			if got := websocketRequestPortable(event); got != test.want {
				t.Fatalf("portable = %t, want %t", got, test.want)
			}
		})
	}
}

func TestWebSocketAccountBoundFrameDoesNotMoveForModelCompatibility(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	compatible := testAccount("account-compatible", 20)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, compatible})
	headers := codexWebSocketHeaders("session", "thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, first, map[string]any{"type": "response.create", "input": []any{}})
	first.CloseNow()
	server.catalog.replace(
		[]string{owner.id(), compatible.id()},
		map[string][]modelEntry{
			owner.id():      {testModelEntry("gpt-terra")},
			compatible.id(): {testModelEntry("gpt-sol")},
		},
		"0.1.0",
	)

	second, _ := dialWebSocket(t, proxy.URL, headers)
	defer second.CloseNow()
	writeWebSocketEvent(t, second, map[string]any{
		"type":                 "response.create",
		"model":                "gpt-sol",
		"previous_response_id": "response-owner",
		"input":                []any{},
	})
	readCloseStatus(t, second, websocket.StatusTryAgainLater)
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner]" {
		t.Fatalf("request accounts = %s, want account-bound state rejected before a model-driven switch", got)
	}
}

func TestWebSocketFirstKeyedTurnTransfersItsProvisionalClaimForTheModel(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
	server.catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-terra")},
			b.id(): {testModelEntry("gpt-sol")},
		},
		"0.1.0",
	)
	conn, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "thread"))
	defer conn.CloseNow()

	completeWebSocketTurn(t, conn, map[string]any{"type": "response.create", "model": "gpt-sol", "input": []any{}})
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a account-b]" {
		t.Fatalf("connection accounts = %s, want model preflight to switch to account-b", got)
	}
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-b]" {
		t.Fatalf("request accounts = %s, want only account-b to receive the turn", got)
	}
	server.routeClaims.mu.Lock()
	claims := len(server.routeClaims.byID)
	server.routeClaims.mu.Unlock()
	if claims != 0 {
		t.Fatalf("claims after acceptance = %d, want the transferred claim committed", claims)
	}
}

func TestWebSocketModelClaimTransferStopsAnOldAccountPeerBeforeItsTurn(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
	server.catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-terra")},
			b.id(): {testModelEntry("gpt-sol")},
		},
		"0.1.0",
	)
	headers := codexWebSocketHeaders("session", "thread")
	first, _ := dialWebSocket(t, proxy.URL, headers)
	peer, _ := dialWebSocket(t, proxy.URL, headers)
	defer first.CloseNow()
	defer peer.CloseNow()

	completeWebSocketTurn(t, first, map[string]any{"type": "response.create", "model": "gpt-sol", "input": []any{}})
	writeWebSocketEvent(t, peer, map[string]any{"type": "response.create", "model": "gpt-sol", "input": []any{}})
	readCloseStatus(t, peer, websocket.StatusServiceRestart)
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-b]" {
		t.Fatalf("request accounts = %s, want the old account peer stopped before dispatch", got)
	}
}

func TestWebSocketExhaustedOwnerMovesTheNextEncryptedFullReplayAndRebinds(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, _ websocketEnvelope) {
		mu.Lock()
		requests++
		request := requests
		mu.Unlock()
		if request == 2 {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":  "error",
				"error": map[string]any{"code": "usage_limit_reached"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "response-" + account},
		})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	fresh := testAccount("account-fresh", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
	headers := codexWebSocketHeaders("session", "thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, first, map[string]any{"type": "response.create", "input": []any{}})
	first.CloseNow()
	exhausted, _ := dialWebSocket(t, proxy.URL, headers)
	writeWebSocketEvent(t, exhausted, map[string]any{"type": "response.create", "input": []any{}})
	if event := readWebSocketEvent(t, exhausted); event.Type != "error" || !websocketErrorIs(event, "usage_limit_reached") {
		t.Fatalf("usage-limit event = %+v", event)
	}
	readCloseStatus(t, exhausted, websocket.StatusServiceRestart)
	exhausted.CloseNow()

	replayed, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, replayed, map[string]any{
		"type": "response.create",
		"input": []any{
			map[string]any{"type": "reasoning", "encrypted_content": "encrypted-history"},
		},
	})
	replayed.CloseNow()
	setTestAccountSpent(owner, false)
	owner.RoutingMode = routingModePriority
	reconnected, _ := dialWebSocket(t, proxy.URL, headers)
	defer reconnected.CloseNow()
	completeWebSocketTurn(t, reconnected, map[string]any{"type": "response.create", "input": []any{}})
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner account-owner account-fresh account-fresh]" {
		t.Fatalf("request accounts = %s, want exhaustion to move only a full replay and acceptance to rebind away from the old owner", got)
	}
}

func TestWebSocketKnownSpentAccountRestartsBeforeSendingANewTurn(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	fresh := testAccount("account-fresh", 20)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
	headers := codexWebSocketHeaders("session", "thread")

	conn, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	owner.markSpent()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	readCloseStatus(t, conn, websocket.StatusServiceRestart)
	conn.CloseNow()
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner]" {
		t.Fatalf("request accounts before reconnect = %s, want no doomed second request", got)
	}

	reconnected, _ := dialWebSocket(t, proxy.URL, headers)
	defer reconnected.CloseNow()
	completeWebSocketTurn(t, reconnected, map[string]any{"type": "response.create", "input": []any{}})
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner account-fresh]" {
		t.Fatalf("request accounts after reconnect = %s, want the fresh account", got)
	}
}

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
	// Wait for the first socket to pin its upstream before starting the second.
	readWebSocketEvent(t, first)
	writeWebSocketEvent(t, second, map[string]any{"type": "response.create", "input": []any{}})
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

func TestWebSocketFirstTurnRoutesByModel(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
	server.catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-terra")},
			b.id(): {testModelEntry("gpt-sol")},
		},
		"0.1.0",
	)
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	defer conn.CloseNow()

	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "model": "gpt-sol", "input": []any{}})
	readWebSocketEvent(t, conn)
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a account-b]" {
		t.Fatalf("connection accounts = %s", got)
	}
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-b]" {
		t.Fatalf("request accounts = %s", got)
	}
}

func TestWebSocketFirstTurnRoutesByServiceTier(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
	server.catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-sol")},
			b.id(): {testModelEntry("gpt-sol", "priority")},
		},
		"0.1.0",
	)
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	defer conn.CloseNow()

	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "model": "gpt-sol", "service_tier": "fast", "input": []any{}})
	readWebSocketEvent(t, conn)
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-b]" {
		t.Fatalf("request accounts = %s", got)
	}
}

func TestWebSocketRestartsBeforeUnsupportedLaterModel(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
	server.catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-terra")},
			b.id(): {testModelEntry("gpt-sol")},
		},
		"0.1.0",
	)
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	defer conn.CloseNow()

	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "model": "gpt-sol", "input": []any{}})
	readWebSocketEvent(t, conn)
	readWebSocketEvent(t, conn)
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "model": "gpt-terra", "input": []any{}})
	readCloseStatus(t, conn, websocket.StatusServiceRestart)
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-b]" {
		t.Fatalf("request accounts = %s", got)
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

func TestWebSocketRateLimitPassesThroughBeforeReconnect(t *testing.T) {
	requests := 0
	const rateLimitEvent = `{"type":"error","status":429,"headers":{"retry-after":"30"},"error":{"type":"invalid_request_error","code":"rate_limit_exceeded"},"upstream_only":{"kept":true}}`
	upstream := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		requests++
		if requests == 1 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := conn.Write(ctx, websocket.MessageText, []byte(rateLimitEvent)); err != nil {
				t.Error(err)
			}
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, testAccount("account-b", 20)})
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	kind, data, err := conn.Read(ctx)
	cancel()
	if err != nil || kind != websocket.MessageText || string(data) != rateLimitEvent {
		t.Fatalf("rate-limit frame = %q/%q, error = %v, want the original upstream frame", kind, data, err)
	}
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

func TestWebSocketConnectionLimitReconnectsSameAccountWithoutCooldown(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		mu.Lock()
		requests++
		request := requests
		mu.Unlock()
		switch request {
		case 1:
			writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
			writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
		case 2:
			writeWebSocketEvent(t, conn, map[string]any{
				"type":  "error",
				"error": map[string]any{"code": "websocket_connection_limit_reached"},
			})
		default:
			writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		}
	})
	defer upstream.Close()
	account := testAccount("account-a", 0)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{account, testAccount("account-b", 20)})
	headers := codexWebSocketHeaders("session", "thread")
	conn, _ := dialWebSocket(t, proxy.URL, headers)
	completeWebSocketTurn(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	if event := readWebSocketEvent(t, conn); event.Type != "error" || !websocketErrorIs(event, "websocket_connection_limit_reached") {
		t.Fatalf("connection-limit event = %+v", event)
	}
	conn.CloseNow()
	if _, _, cooldown, _ := account.health(); !cooldown.IsZero() {
		t.Fatalf("cooldown = %s, want zero", cooldown)
	}
	reconnected, _ := dialWebSocket(t, proxy.URL, headers)
	defer reconnected.CloseNow()
	writeWebSocketEvent(t, reconnected, map[string]any{"type": "response.create", "input": []any{}})
	readWebSocketEvent(t, reconnected)
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a account-a]" {
		t.Fatalf("connection accounts = %s", got)
	}
}

func TestWebSocketInBandUnauthorizedRefreshesARejectedTokenOnceWithoutForwarding(t *testing.T) {
	refreshCalls := useOAuthRefreshServer(t)
	unauthorized := map[string]any{"type": "error", "status": http.StatusUnauthorized, "error": map[string]any{"code": "unauthorized"}}
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, unauthorized)
	})
	defer upstream.Close()
	account := testAccount("account-a", 0)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{account})
	first, _ := dialWebSocket(t, proxy.URL, nil)
	second, _ := dialWebSocket(t, proxy.URL, nil)
	defer first.CloseNow()
	defer second.CloseNow()

	writeWebSocketEvent(t, first, map[string]any{"type": "response.create", "input": []any{}})
	writeWebSocketEvent(t, second, map[string]any{"type": "response.create", "input": []any{}})
	readCloseStatus(t, first, websocket.StatusServiceRestart)
	readCloseStatus(t, second, websocket.StatusServiceRestart)
	if calls := refreshCalls(); calls != 1 {
		t.Fatalf("refresh calls = %d, want one refresh for the rejected token revision", calls)
	}
	if account.persisted().AccessToken != "refreshed-token" {
		t.Fatalf("access token = %q, want refreshed-token", account.persisted().AccessToken)
	}
}

func TestWebSocketUsageRejectionPassesThroughBeforeRestart(t *testing.T) {
	const code = "usage_limit_reached"
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "error", "error": map[string]any{"code": code}})
	})
	defer upstream.Close()
	account := testAccount("account-a", 0)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{account})
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	if event := readWebSocketEvent(t, conn); event.Type != "error" || !websocketErrorIs(event, code) {
		t.Fatalf("rejection event = %+v", event)
	}
	readCloseStatus(t, conn, websocket.StatusServiceRestart)
	conn.CloseNow()
	if candidate := account.routingCandidate(); !candidate.spent || server.stats.snapshot().Limited != 1 {
		t.Fatalf("candidate = %+v, limited = %d", candidate, server.stats.snapshot().Limited)
	}
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-a]" {
		t.Fatalf("request accounts = %s", got)
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
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a]" {
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

func TestWebSocketCapacityFailureRestartsOnRetainedAccount(t *testing.T) {
	for _, code := range []string{"server_is_overloaded", "slow_down"} {
		t.Run(code, func(t *testing.T) {
			var mu sync.Mutex
			requests := 0
			upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
				mu.Lock()
				requests++
				capacityFailure := requests == 1
				mu.Unlock()
				writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
				if capacityFailure {
					writeWebSocketEvent(t, conn, map[string]any{
						"type": "response.failed",
						"response": map[string]any{
							"error": map[string]any{"code": code},
						},
					})
					return
				}
				writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
			})
			defer upstream.Close()
			owner := testAccount("account-owner", 0)
			fresh := testAccount("account-fresh", 20)
			_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
			headers := codexWebSocketHeaders("session", "thread")

			first, _ := dialWebSocket(t, proxy.URL, headers)
			writeWebSocketEvent(t, first, map[string]any{"type": "response.create", "input": []any{}})
			if event := readWebSocketEvent(t, first); event.Type != "response.created" {
				t.Fatalf("first event = %q, want response.created", event.Type)
			}
			readCloseStatus(t, first, websocket.StatusServiceRestart)
			first.CloseNow()

			setTestAccountUsage(owner, 90)
			setTestAccountUsage(fresh, 0)
			fresh.RoutingMode = routingModePriority
			retried, _ := dialWebSocket(t, proxy.URL, headers)
			defer retried.CloseNow()
			completeWebSocketTurn(t, retried, map[string]any{"type": "response.create", "input": []any{}})

			if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner account-owner]" {
				t.Fatalf("request accounts = %s, want capacity retry on retained account", got)
			}
		})
	}
}

func TestWebSocketCapacityErrorRetainsProvisionalAccount(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		mu.Lock()
		requests++
		capacityFailure := requests == 1
		mu.Unlock()
		if capacityFailure {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":   "error",
				"status": http.StatusServiceUnavailable,
				"error":  map[string]any{"code": "server_is_overloaded"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	owner := testAccount("account-owner", 0)
	fresh := testAccount("account-fresh", 20)
	server, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
	headers := codexWebSocketHeaders("session", "thread")

	first, _ := dialWebSocket(t, proxy.URL, headers)
	writeWebSocketEvent(t, first, map[string]any{"type": "response.create", "input": []any{}})
	readCloseStatus(t, first, websocket.StatusServiceRestart)
	first.CloseNow()
	owners, err := server.pool.store.routeOwners("thread", "session")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(owners); got != "[account-owner]" {
		t.Fatalf("persisted owners = %s, want provisional capacity owner", got)
	}

	setTestAccountUsage(owner, 90)
	setTestAccountUsage(fresh, 0)
	fresh.RoutingMode = routingModePriority
	retried, _ := dialWebSocket(t, proxy.URL, headers)
	defer retried.CloseNow()
	completeWebSocketTurn(t, retried, map[string]any{"type": "response.create", "input": []any{}})

	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-owner account-owner]" {
		t.Fatalf("request accounts = %s, want capacity retry on provisional account", got)
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
	if err := server.stats.store.addAPIKey(storedAPIKey{Name: "my-laptop", Secret: "secret", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	server.lookupAPIKey = func(presented string) (string, bool, error) {
		return "my-laptop", presented == "secret", nil
	}
	conn, _ := dialWebSocket(t, proxy.URL, http.Header{"Authorization": {"Bearer secret"}})
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
	if snapshot.MonthlyUsage.InputTokens != 10 || len(snapshot.Threads) != 1 || snapshot.Threads[0].Metadata.TurnID != "turn" || snapshot.Threads[0].Compactions != 1 || snapshot.Threads[0].APIKeySuffix != "ret" {
		t.Fatalf("stats = %+v", snapshot)
	}
	usage, err := server.stats.store.apiKeyUsage()
	if err != nil {
		t.Fatal(err)
	}
	if usage["my-laptop"].InputTokens != 10 || usage["my-laptop"].OutputTokens != 4 {
		t.Fatalf("API key usage = %+v", usage)
	}
}

func TestWebSocketCanceledHandshakeDoesNotPenalizeAccounts(t *testing.T) {
	server := newTestServer(t, []*Account{testAccount("account-a", 0)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil).WithContext(ctx)
	_, _, err := server.dialResponsesWebSocket(request, websocketRoute{session: "session"}, "", "")
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
