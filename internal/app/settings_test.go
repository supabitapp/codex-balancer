package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSettingsCLIAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	for _, mode := range []string{"default", "on", "off", "default"} {
		if err := settingsCmd([]string{"set", "-state", path, "fast-mode", mode}); err != nil {
			t.Fatal(err)
		}
		store, err := openStateStore(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := store.raw.FastMode()
		store.Close()
		if err != nil || got != mode {
			t.Fatalf("stored = %q, %v; want %q", got, err, mode)
		}
	}
	for _, args := range [][]string{
		{"set", "fast-mode", "fast"}, {"set", "unknown", "on"}, {"get", "unknown"}, {"list", "extra"},
	} {
		args = append([]string{args[0], "-state", path}, args[1:]...)
		if err := settingsCmd(args); err == nil {
			t.Fatalf("accepted invalid command %v", args)
		}
	}
	for _, args := range [][]string{{"get", "-state", path, "fast-mode"}, {"list", "-state", path, "-json"}} {
		if err := settingsCmd(args); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSettingsMigrationPreservesRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.preserveRouteOwners(time.Now(), "owner", []string{"thread"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("DROP TABLE settings; PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mode, err := store.raw.FastMode()
	if err != nil || mode != "default" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	owners, err := store.routeOwners("thread", "")
	if err != nil || fmt.Sprint(owners) != "[owner]" {
		t.Fatalf("owners = %v, %v", owners, err)
	}
}

func TestFastModePreservesRequestFields(t *testing.T) {
	for _, test := range []struct{ requestedTier, fastTier string }{
		{"flex", "priority"},
		{"ultrafast", "ultrafast"},
		{" UltraFast ", " UltraFast "},
	} {
		t.Run(test.requestedTier, func(t *testing.T) {
			original := []byte(fmt.Sprintf(`{"type":"response.create","service_tier":%q,"input":[{"encrypted_content":"opaque"}],"unknown":{"number":9007199254740993},"previous_response_id":"resp-1","client_metadata":{"x-codex-turn-state":"state"}}`, test.requestedTier))
			for _, mode := range []fastMode{fastModeDefault, fastModeOn, fastModeOff} {
				data, tier, err := mode.override(original, test.requestedTier)
				if err != nil {
					t.Fatal(err)
				}
				want := map[fastMode]string{fastModeDefault: test.requestedTier, fastModeOn: test.fastTier, fastModeOff: "default"}[mode]
				if tier != want {
					t.Fatalf("%s tier = %s", mode, tier)
				}
				if tier == test.requestedTier && string(data) != string(original) {
					t.Fatalf("%s changed request bytes", mode)
				}
				var before, after map[string]json.RawMessage
				json.Unmarshal(original, &before)
				json.Unmarshal(data, &after)
				delete(before, "service_tier")
				delete(after, "service_tier")
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("override changed other fields: %s", data)
				}
			}
		})
	}
}

func TestFastModeReconnectPreservesOwners(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprintf("accepted_%t", accepted), func(t *testing.T) {
			tiers := make(chan string, 10)
			upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, request websocketEnvelope) {
				tiers <- request.ServiceTier
				writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
				writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
			})
			defer upstream.Close()
			a, b := testAccount("owner", 0), testAccount("other", 20)
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
			headers := codexWebSocketHeaders("session", "thread")
			conn, _ := dialWebSocket(t, proxy.URL, headers)
			defer func() { conn.CloseNow() }()
			request := map[string]any{"type": "response.create", "service_tier": "priority", "input": []any{}}
			if accepted {
				completeWebSocketTurn(t, conn, request)
				<-tiers
			}
			setTestAccountUsage(a, 90)
			setTestAccountUsage(b, 0)
			// Exercise every transition, all three effective tiers, and idempotence.
			for _, mode := range []fastMode{fastModeOn, fastModeOff, fastModeDefault} {
				srv.fastMode.set(mode)
				readCloseStatus(t, conn, websocket.StatusServiceRestart)
				conn.CloseNow()
				conn, _ = dialWebSocket(t, proxy.URL, headers)
				completeWebSocketTurn(t, conn, request)
				want := "priority"
				if mode == fastModeOff {
					want = "default"
				}
				if got := <-tiers; got != want {
					t.Fatalf("%s: tier=%s, want %s", mode, got, want)
				}
				snapshot := srv.stats.snapshot()
				if len(snapshot.Threads) != 1 || snapshot.Threads[0].ServiceTier != want {
					t.Fatalf("tracked tier differs from upstream: %+v", snapshot.Threads)
				}

				if srv.fastMode.set(mode) {
					t.Fatal("same setting triggered restart")
				}
				completeWebSocketTurn(t, conn, request)
				<-tiers
			}
			for _, account := range upstream.RequestAccounts() {
				if account != "owner" {
					t.Fatalf("lost ownership: %v", upstream.RequestAccounts())
				}
			}
		})
	}
}

func TestFastModeOverridesBeforeModelSelection(t *testing.T) {
	for _, test := range []struct {
		mode                             fastMode
		requestedTier, wantTier, account string
	}{
		{fastModeOn, "default", "priority", "fast"},
		{fastModeOn, "ultrafast", "ultrafast", "ultrafast"},
		{fastModeOff, "priority", "default", "standard"},
		{fastModeOff, "ultrafast", "default", "standard"},
	} {
		for _, via := range []transport{transportHTTP, transportWebSocket} {
			t.Run(fmt.Sprintf("%s/%s/%s", test.mode, test.requestedTier, via), func(t *testing.T) {
				upstream := newHTTPUpstream(t, func(r *http.Request, conn *testResponseStream, data []byte) {
					var request websocketEnvelope
					if err := json.Unmarshal(data, &request); err != nil {
						t.Error(err)
					}
					if account := r.Header.Get("Chatgpt-Account-Id"); account != test.account || request.ServiceTier != test.wantTier {
						t.Errorf("upstream account/tier = %s/%s, want %s/%s", account, request.ServiceTier, test.account, test.wantTier)
					}
					sendHTTPEvents(t, conn, httpCreatedEvent, httpCompletedEvent)
				})
				a, b, c := testAccount("standard", 0), testAccount("fast", 20), testAccount("ultrafast", 40)
				srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b, c})
				srv.catalog.replace([]string{a.id(), b.id(), c.id()}, map[string][]modelEntry{
					a.id(): {testModelEntry("gpt-sol")},
					b.id(): {testModelEntry("gpt-sol", "priority")},
					c.id(): {testModelEntry("gpt-sol", "priority", "ultrafast")},
				}, "0.1.0")
				srv.fastMode.set(test.mode)
				request := map[string]any{"type": "response.create", "model": "gpt-sol", "service_tier": test.requestedTier, "input": []any{}}
				if via == transportWebSocket {
					conn, _ := dialWebSocket(t, proxy.URL, nil)
					defer conn.CloseNow()
					completeWebSocketTurn(t, conn, request)
				} else {
					data, err := json.Marshal(request)
					if err != nil {
						t.Fatal(err)
					}
					response := postResponse(t, proxy.URL, string(data), nil)
					if body := readHTTPBody(t, response); response.StatusCode != http.StatusOK {
						t.Fatalf("status=%d body=%s", response.StatusCode, body)
					}
				}
				usage, err := srv.pool.store.usageEventsSince(time.Now().Add(-time.Hour))
				if err != nil || len(usage) != 1 || usage[0].ServiceTier != test.wantTier {
					t.Fatalf("usage=%v err=%v", usage, err)
				}
			})
		}
	}
}

func TestSettingsWatcherRestartsAllSockets(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 0)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.watchSettings(ctx) }()
	defer func() { cancel(); <-done }()
	var connections []*websocket.Conn
	for i := 0; i < 3; i++ {
		conn, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders(fmt.Sprint(i), fmt.Sprint(i)))
		defer conn.CloseNow()
		// Ensure the relay is running before the external setting write.
		completeWebSocketTurn(t, conn, map[string]any{"type": "response.create", "input": []any{}})
		connections = append(connections, conn)
	}
	if err := settingsCmd([]string{"set", "-state", srv.pool.store.path, "fast-mode", "on"}); err != nil {
		t.Fatal(err)
	}
	for _, conn := range connections {
		readCloseStatus(t, conn, websocket.StatusServiceRestart)
	}
	if mode, _ := srv.fastMode.snapshot(); mode != fastModeOn {
		t.Fatalf("mode=%s", mode)
	}
}

func TestFastModeChangeDuringHandshake(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
	}))
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("owner", 0)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		conn *websocket.Conn
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: codexWebSocketHeaders("session", "thread")})
		resultCh <- result{conn, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("handshake did not start")
	}
	srv.fastMode.set(fastModeOn)
	close(release)
	resultValue := <-resultCh
	if resultValue.err != nil {
		t.Fatal(resultValue.err)
	}
	defer resultValue.conn.CloseNow()
	readCloseStatus(t, resultValue.conn, websocket.StatusServiceRestart)
	owners, err := srv.pool.store.routeOwners("thread", "session")
	if err != nil || fmt.Sprint(owners) != "[owner]" {
		t.Fatalf("owners=%v, %v", owners, err)
	}
}
