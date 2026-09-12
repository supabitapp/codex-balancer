package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func waitHTTPSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for HTTP test synchronization")
	}
}

func TestHTTPResponsesOverlappingSessionOwnership(t *testing.T) {
	started := make(chan string, 2)
	accept := make(chan struct{})
	upstream := newHTTPUpstream(t, func(r *http.Request, c *websocket.Conn, data []byte) {
		started <- r.Header.Get("Chatgpt-Account-Id")
		<-accept
		sendHTTPEvents(t, c, httpCreatedEvent)
		// Each request gets its own identifier/output and usage, despite sharing
		// one provisional owner and overlapping before response.created.
		text := "first"
		if strings.Contains(string(data), "second") {
			text = "second"
		}
		sendHTTPEvents(t, c, fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"output":[{"type":"message","id":%q,"role":"assistant","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`, text, text, text))
	})
	a, b := testAccount("a", 0), testAccount("b", 20)
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
	srv.admission = newAdmissionGate(2)
	results := make(chan string, 2)
	request := func(text string) {
		resp := postResponse(t, proxy.URL, fmt.Sprintf(`{"model":"m","input":%q}`, text), http.Header{"Session-Id": {"same"}})
		results <- readHTTPBody(t, resp)
	}
	go request("first")
	if account := <-started; account != "a" {
		t.Fatal(account)
	}
	if owners, err := srv.pool.store.routeOwners("", "same"); err != nil || len(owners) != 0 {
		t.Fatalf("accepted at handshake: %v %v", owners, err)
	}
	b.mu.Lock()
	b.RoutingMode = routingModePriority
	b.mu.Unlock()
	go request("second")
	if account := <-started; account != "a" {
		t.Fatal("overlap ignored provisional owner:", account)
	}
	close(accept)
	one, two := <-results, <-results
	if one == two || !strings.Contains(one+two, `"id":"first"`) || !strings.Contains(one+two, `"id":"second"`) {
		t.Fatalf("mixed output %s %s", one, two)
	}
	assertHTTPClean(t, srv)
	if snapshot := srv.stats.snapshot(); snapshot.Turns != 2 || snapshot.MonthlyUsage.TotalTokens != 4 {
		t.Fatalf("duplicate/lost accounting: %+v", snapshot)
	}
	if owners, err := srv.pool.store.routeOwners("", "same"); err != nil || fmt.Sprint(owners) != "[a]" {
		t.Fatalf("owners=%v err=%v", owners, err)
	}
}

func TestHTTPResponsesAffinityAliasesAndAnonymous(t *testing.T) {
	for _, header := range []string{"Session_id", "Session-Id", "X-Codex-Session-Id", "X-Codex-Conversation-Id", "X-Session-Affinity", "X-Session-Id", ""} {
		t.Run(header, func(t *testing.T) {
			accounts := make(chan string, 2)
			upstream := newHTTPUpstream(t, func(r *http.Request, c *websocket.Conn, _ []byte) {
				accounts <- r.Header.Get("Chatgpt-Account-Id")
				sendHTTPEvents(t, c, httpCreatedEvent, httpCompletedEvent)
			})
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 0)})
			srv.admission = newAdmissionGate(1)
			headers := http.Header{"Chatgpt-Account-Id": {"shared-client-id"}, "Authorization": {"Bearer shared-key"}}
			if header != "" {
				headers.Set(header, "same")
			}
			for range 2 {
				resp := postResponse(t, proxy.URL, `{"model":"m"}`, headers)
				readHTTPBody(t, resp)
				assertHTTPClean(t, srv)
			}
			first, second := <-accounts, <-accounts
			if first != "a" || header != "" && second != "a" || header == "" && second != "b" {
				t.Fatalf("accounts=%s,%s", first, second)
			}
		})
	}
	headers := http.Header{"Session_id": {"strong"}, "Session-Id": {"weaker"}, "X-Session-Affinity": {"weak"}, "X-Session-Id": {"weakest"}, "Thread-Id": {"thread"}, "X-Client-Request-Id": {"request"}}
	if route := websocketRouteFrom(headers); route.session != "strong" || route.thread != "thread" {
		t.Fatalf("precedence=%+v", route)
	}
}

func TestHTTPResponsesModelTierRetentionAndTurnState(t *testing.T) {
	accounts := make(chan string, 10)
	upstream := newHTTPUpstream(t, func(r *http.Request, c *websocket.Conn, data []byte) {
		accounts <- r.Header.Get("Chatgpt-Account-Id")
		sendHTTPEvents(t, c, httpCreatedEvent, httpCompletedEvent)
	})
	a, b := testAccount("standard", 0), testAccount("fast", 20)
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
	srv.admission = newAdmissionGate(1)
	srv.catalog.replace([]string{a.id(), b.id()}, map[string][]modelEntry{a.id(): {testModelEntry("m")}, b.id(): {testModelEntry("m", "priority")}}, "0.1.0")
	headers := http.Header{"Session-Id": {"session"}}
	srv.fastMode.set(fastModeOn)
	resp := postResponse(t, proxy.URL, `{"model":"m","service_tier":"default"}`, headers)
	readHTTPBody(t, resp)
	assertHTTPClean(t, srv)
	if account := <-accounts; account != "fast" {
		t.Fatalf("override before selection: %s", account)
	}
	usage, err := srv.pool.store.usageEventsSince(time.Now().Add(-time.Hour))
	if err != nil || len(usage) != 1 || usage[0].ServiceTier != "priority" {
		t.Fatalf("usage=%v err=%v", usage, err)
	}
	srv.fastMode.set(fastModeOff)
	resp = postResponse(t, proxy.URL, `{"model":"m"}`, headers)
	readHTTPBody(t, resp)
	assertHTTPClean(t, srv)
	if account := <-accounts; account != "fast" {
		t.Fatal("healthy owner was not retained")
	}
	b.markSpent()
	headers.Set(codexTurnStateKey, "old-state")
	resp = postResponse(t, proxy.URL, `{"model":"m"}`, headers)
	if body := readHTTPBody(t, resp); resp.StatusCode != 409 {
		t.Fatalf("moved turn header: %s", body)
	}
	headers.Del(codexTurnStateKey)
	resp = postResponse(t, proxy.URL, `{"model":"m","client_metadata":{"x-codex-turn-state":"old-state"}}`, headers)
	if body := readHTTPBody(t, resp); resp.StatusCode != 409 {
		t.Fatalf("moved metadata: %d %s", resp.StatusCode, body)
	}
	resp = postResponse(t, proxy.URL, `{"model":"m","input":[{"type":"reasoning","encrypted_content":"portable","summary":[]}]}`, headers)
	readHTTPBody(t, resp)
	assertHTTPClean(t, srv)
	if account := <-accounts; account != "standard" {
		t.Fatal("portable request did not replace spent owner")
	}
	select {
	case account := <-accounts:
		t.Fatal("bound request reached", account)
	default:
	}
}

func TestHTTPResponsesInvalidationAndPolicyChanges(t *testing.T) {
	for _, phase := range []string{"handshake", "stream"} {
		for _, change := range []string{"fast mode", "pause", "remove", "signout"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				started, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var requests atomic.Int64
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(closed)
					if phase == "handshake" {
						close(started)
						<-release
					}
					c, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer c.CloseNow()
					if _, _, err := c.Read(r.Context()); err != nil {
						return
					}
					requests.Add(1)
					sendHTTPEvents(t, c, httpCreatedEvent, httpTextDelta)
					if phase == "stream" {
						close(started)
					}
					c.Read(r.Context())
				}))
				defer upstream.Close()
				a := testAccount("owner", 0)
				srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a})
				srv.admission = newAdmissionGate(1)
				result := make(chan string, 1)
				go func() {
					resp := postResponse(t, proxy.URL, `{"model":"m","stream":true}`, http.Header{"Session-Id": {"session"}})
					result <- readHTTPBody(t, resp)
				}()
				waitHTTPSignal(t, started)
				switch change {
				case "fast mode":
					srv.fastMode.set(fastModeOn)
				case "pause":
					a.mu.Lock()
					a.Paused = true
					a.mu.Unlock()
					srv.invalidateAccount(a.id(), routingReasonOwnerPaused)
				case "remove":
					if err := srv.pool.remove(a); err != nil {
						t.Fatal(err)
					}
					srv.invalidateAccount(a.id(), routingReasonOwnerRemoved)
				case "signout":
					a.mu.Lock()
					a.Reauth = "signed out"
					a.mu.Unlock()
					srv.invalidateAccount(a.id(), routingReasonOwnerSignedOut)
				}
				if phase == "handshake" {
					close(release)
				}
				body := <-result
				if !strings.Contains(body, `"error"`) || strings.Contains(body, "event: response.completed") {
					t.Fatalf("body=%s", body)
				}
				waitHTTPSignal(t, closed)
				assertHTTPClean(t, srv)
				if phase == "handshake" && requests.Load() != 0 {
					t.Fatal("inference sent after setup invalidation")
				}
				owners, err := srv.pool.store.routeOwners("", "session")
				if err != nil || fmt.Sprint(owners) != "[owner]" {
					t.Fatalf("owner boundary lost: %v %v", owners, err)
				}
			})
		}
	}
}

func TestHTTPResponsesCancellationAndShutdown(t *testing.T) {
	for _, phase := range []string{"body", "handshake", "first event", "stream", "slow reader"} {
		for _, source := range []string{"client", "server"} {
			t.Run(phase+"/"+source, func(t *testing.T) {
				started, closed := make(chan struct{}), make(chan struct{})
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(closed)
					if phase == "handshake" {
						close(started)
						<-r.Context().Done()
						return
					}
					c, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer c.CloseNow()
					if _, _, err := c.Read(r.Context()); err != nil {
						return
					}
					if phase == "stream" || phase == "slow reader" {
						sendHTTPEvents(t, c, httpCreatedEvent, httpTextDelta)
					}
					close(started)
					if phase == "slow reader" {
						data := []byte(`{"type":"response.output_text.delta","item_id":"m","delta":"` + strings.Repeat("x", 1<<20) + `"}`)
						for range 32 {
							if err := c.Write(r.Context(), websocket.MessageText, data); err != nil {
								return
							}
						}
					}
					c.Read(r.Context())
				}))
				defer upstream.Close()
				srv := newTestServer(t, []*Account{testAccount("a", 0)})
				runtime, cancelServer := context.WithCancel(context.Background())
				defer cancelServer()
				srv.ctx, srv.upstream, srv.admission = runtime, upstream.URL, newAdmissionGate(1)
				handler := srv.routes()
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if phase == "body" {
						close(started)
					}
					handler.ServeHTTP(w, r)
				}))
				defer proxy.Close()
				ctx, cancelClient := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancelClient()
				var body io.Reader = strings.NewReader(`{"model":"m","stream":true}`)
				if phase == "body" {
					reader, writer := io.Pipe()
					defer reader.Close()
					defer writer.Close()
					body = reader
					go func() { writer.Write([]byte(`{"model":`)); <-ctx.Done(); writer.Close() }()
				}
				req, err := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/v1/responses", body)
				if err != nil {
					t.Fatal(err)
				}
				clientDone := make(chan struct{})
				go func() {
					defer close(clientDone)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						return
					}
					defer resp.Body.Close()
					if phase == "slow reader" {
						<-ctx.Done()
						return
					}
					io.Copy(io.Discard, resp.Body)
				}()
				waitHTTPSignal(t, started)
				if source == "client" {
					cancelClient()
				} else {
					srv.admission.beginDrain()
					cancelServer()
				}
				assertHTTPClean(t, srv)
				cancelClient()
				waitHTTPSignal(t, clientDone)
				if phase != "body" {
					waitHTTPSignal(t, closed)
				}
			})
		}
	}
}

func TestHTTPResponsesHandshakeErrorsAndRefresh(t *testing.T) {
	for _, scenario := range []string{"server failure", "model missing", "rate limited", "refresh", "in-band refresh"} {
		t.Run(scenario, func(t *testing.T) {
			refreshCalls := useOAuthRefreshServer(t)
			var connections, requests atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connections.Add(1)
				w.Header().Set("Authorization", "secret-header")
				w.Header().Set("Chatgpt-Account-Id", "private-id")
				switch scenario {
				case "server failure":
					w.WriteHeader(502)
					io.WriteString(w, `{"error":{"code":"upstream_down","message":"token-a"}}`)
					return
				case "model missing":
					w.WriteHeader(404)
					io.WriteString(w, `{"error":{"code":"model_not_found","message":"missing"}}`)
					return
				case "rate limited":
					w.Header().Set("Retry-After", "30")
					w.WriteHeader(429)
					io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"limit"}}`)
					return
				case "refresh":
					if r.Header.Get("Authorization") != "Bearer refreshed-token" {
						w.WriteHeader(401)
						return
					}
				}
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer c.CloseNow()
				if _, _, err := c.Read(r.Context()); err != nil {
					return
				}
				requests.Add(1)
				if scenario == "in-band refresh" {
					sendHTTPEvents(t, c, `{"type":"error","status":401,"error":{"code":"unauthorized","message":"token-a"}}`)
				} else {
					sendHTTPEvents(t, c, httpCreatedEvent, httpCompletedEvent)
				}
				c.Read(r.Context())
			}))
			defer upstream.Close()
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
			srv.admission = newAdmissionGate(1)
			resp := postResponse(t, proxy.URL, `{"model":"m"}`, nil)
			body := readHTTPBody(t, resp)
			want := map[string]int{"server failure": 502, "model missing": 404, "rate limited": 429, "refresh": 200, "in-band refresh": 503}[scenario]
			if resp.StatusCode != want || strings.Contains(body, "token-a") || resp.Header.Get("Authorization") != "" || resp.Header.Get("Chatgpt-Account-Id") != "" || resp.Header.Get("Upgrade") != "" {
				t.Fatalf("status=%d headers=%v body=%s", resp.StatusCode, resp.Header, body)
			}
			assertHTTPClean(t, srv)
			if scenario == "refresh" || scenario == "in-band refresh" {
				if refreshCalls() != 1 || requests.Load() != 1 {
					t.Fatalf("refreshes=%d requests=%d", refreshCalls(), requests.Load())
				}
			} else if connections.Load() != 1 || requests.Load() != 0 {
				t.Fatal("handshake unexpectedly retried")
			}
		})
	}
}

func TestHTTPResponsesIdleTimeout(t *testing.T) {
	upstream := newHTTPUpstream(t, func(_ *http.Request, _ *websocket.Conn, _ []byte) {})
	srv := newTestServer(t, []*Account{testAccount("a", 0)})
	srv.upstream = upstream.URL
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	dial, failed, err := srv.dialResponsesWebSocket(req, websocketRoute{}, "m", "")
	if err != nil || failed != nil {
		t.Fatalf("dial=%v %v", err, failed)
	}
	writer := httptest.NewRecorder()
	peer := &httpResponsesDownstream{writer: writer, controller: http.NewResponseController(writer), request: []byte(`{"type":"response.create","model":"m"}`), stream: true}
	mode, changed := srv.fastMode.snapshot()
	relay := newResponsesWebSocketRelay(srv, peer, req, dial, websocketRoute{}, apiKeyIdentity{}, mode, changed)
	relay.idleTimeout = 5 * time.Millisecond
	relay.run()
	if writer.Code != 504 || !strings.Contains(writer.Body.String(), "upstream_timeout") {
		t.Fatalf("status=%d %s", writer.Code, writer.Body.String())
	}
	if srv.stats.snapshot().WSOpen != 0 {
		t.Fatal("timeout leaked socket")
	}
}
