package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func useOAuthRefreshServer(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"refreshed-token","refresh_token":"refreshed-refresh"}`)
	}))
	previous := oauthEndpoint
	oauthEndpoint = oauth.URL
	t.Cleanup(func() {
		oauthEndpoint = previous
		oauth.Close()
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

func newTestServer(t *testing.T, accounts []*Account) *server {
	t.Helper()
	state, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(state)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	for _, account := range accounts {
		if err := pool.add(account); err != nil {
			state.Close()
			t.Fatal(err)
		}
	}
	stats, err := newPersistentStats(state, testPriceSnapshot(t), nil)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	return &server{
		ctx:          context.Background(),
		pool:         pool,
		catalog:      newModelCatalog(),
		stats:        stats,
		upstream:     "http://127.0.0.1:1",
		client:       newProxyClient(),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		retryBackoff: func(int) time.Duration { return 0 },
	}
}

type websocketTestUpstream struct {
	*httptest.Server
	mu          sync.Mutex
	connections []string
	requests    []string
}

func newWebSocketUpstream(t *testing.T, respond func(string, *websocket.Conn, websocketEnvelope)) *websocketTestUpstream {
	t.Helper()
	upstream := &websocketTestUpstream{}
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

func newWebSocketHandshakeUpstream(t *testing.T, status int, rejected ...string) *websocketTestUpstream {
	t.Helper()
	upstream := &websocketTestUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		upstream.mu.Lock()
		upstream.connections = append(upstream.connections, account)
		upstream.mu.Unlock()
		if slices.Contains(rejected, account) {
			w.WriteHeader(status)
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

func (u *websocketTestUpstream) RequestAccounts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.requests...)
}

func (u *websocketTestUpstream) ConnectionAccounts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.connections...)
}

func newWebSocketProxy(t *testing.T, upstream string, accounts []*Account) (*server, *httptest.Server) {
	t.Helper()
	server := newTestServer(t, accounts)
	server.upstream = upstream
	proxy := httptest.NewServer(server.routes())
	t.Cleanup(proxy.Close)
	return server, proxy
}

func dialWebSocket(t *testing.T, proxyURL string, headers http.Header) (*websocket.Conn, *http.Response) {
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
	return conn, response
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

func completeWebSocketTurn(t *testing.T, conn *websocket.Conn, request map[string]any) {
	t.Helper()
	writeWebSocketEvent(t, conn, request)
	if event := readWebSocketEvent(t, conn); event.Type != "response.created" {
		t.Fatalf("first turn event = %q, want response.created before accepting the route", event.Type)
	}
	if event := readWebSocketEvent(t, conn); event.Type != "response.completed" {
		t.Fatalf("final turn event = %q, want response.completed before the next test step", event.Type)
	}
}

func codexWebSocketHeaders(session, thread string) http.Header {
	return http.Header{"Session-Id": {session}, "Thread-Id": {thread}}
}
