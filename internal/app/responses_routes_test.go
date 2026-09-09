package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

var testResponsePaths = []string{"/v1/responses", "/codex/responses", "/v1/codex/responses"}

func TestResponsesRoutesGuards(t *testing.T) {
	for _, path := range testResponsePaths {
		t.Run(path, func(t *testing.T) {
			for _, test := range []struct {
				name   string
				method string
				key    string
				gate   *admissionGate
				want   int
			}{
				{name: "POST is not supported", method: http.MethodPost, key: "balancer-key", want: http.StatusMethodNotAllowed},
				{name: "GET requires upgrade", method: http.MethodGet, key: "balancer-key", want: http.StatusMethodNotAllowed},
				{name: "missing key", method: http.MethodGet, want: http.StatusUnauthorized},
				{name: "invalid key", method: http.MethodGet, key: "wrong", want: http.StatusUnauthorized},
				{name: "at capacity", method: http.MethodGet, key: "balancer-key", gate: newAdmissionGate(0), want: http.StatusServiceUnavailable},
				{name: "draining", method: http.MethodGet, key: "balancer-key", gate: &admissionGate{limit: 1, draining: true}, want: http.StatusServiceUnavailable},
			} {
				t.Run(test.name, func(t *testing.T) {
					srv := &server{
						admission: test.gate,
						lookupAPIKey: func(key string) (string, bool, error) {
							return "pi", key == "balancer-key", nil
						},
					}
					request := httptest.NewRequest(test.method, path, nil)
					if test.key != "" {
						request.Header.Set("Authorization", "Bearer "+test.key)
					}
					response := httptest.NewRecorder()
					srv.routes().ServeHTTP(response, request)
					if response.Code != test.want {
						t.Fatalf("status = %d, want %d", response.Code, test.want)
					}
					if test.want == http.StatusServiceUnavailable &&
						response.Header().Get("Retry-After") != "1" {
						t.Fatalf("Retry-After = %q, want 1", response.Header().Get("Retry-After"))
					}
				})
			}
		})
	}
}

func TestResponsesRoutesProxyWebSocket(t *testing.T) {
	for _, path := range testResponsePaths {
		t.Run(path, func(t *testing.T) {
			account := testAccount("pool-account", 0)
			key, err := generateAPIKey()
			if err != nil {
				t.Fatal(err)
			}
			clientAccountID := testAPIKeyAccountID(t, key)
			upstream := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/responses" {
						t.Errorf("upstream path = %q, want /responses", r.URL.Path)
					}
					for name, want := range map[string]string{
						"Authorization":       "Bearer " + account.AccessToken,
						"Chatgpt-Account-Id":  account.id(),
						"Originator":          "pi",
						"X-Client-Request-Id": "pi-session",
					} {
						if got := r.Header.Get(name); got != want {
							t.Errorf("upstream %s = %q, want %q", name, got, want)
						}
					}
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					request := readWebSocketEvent(t, conn)
					if request.Type != "response.create" || request.Model != "gpt-5.5" {
						t.Errorf("upstream request = %+v", request)
					}
					writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
					writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
					// Keep the upstream open until the client closes the connection.
					conn.Read(r.Context())
				}),
			)
			defer upstream.Close()
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{account})
			if err := srv.pool.store.addAPIKey(storedAPIKey{Name: "pi", Secret: key, CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			srv.lookupAPIKey = srv.pool.store.apiKeyName
			headers := http.Header{
				"Authorization":       {"Bearer " + key},
				"Chatgpt-Account-Id":  {clientAccountID},
				"Originator":          {"pi"},
				"X-Client-Request-Id": {"pi-session"},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, response, err := websocket.Dial(
				ctx,
				"ws"+strings.TrimPrefix(proxy.URL, "http")+path,
				&websocket.DialOptions{HTTPHeader: headers},
			)
			if err != nil {
				t.Fatalf("dial: %v, response = %+v", err, response)
			}
			defer conn.CloseNow()
			if response.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("status = %d, want 101", response.StatusCode)
			}
			completeWebSocketTurn(
				t,
				conn,
				map[string]any{"type": "response.create", "model": "gpt-5.5", "input": []any{}},
			)
		})
	}
}
