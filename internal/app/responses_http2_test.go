package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Exercise real HTTP/2 expiry semantics without adding 30-second sleeps to the
// suite. Only positive write deadlines are shortened, not idle/read timeouts.
type shortHTTPWriteDeadline struct{ http.ResponseWriter }

func (w shortHTTPWriteDeadline) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w shortHTTPWriteDeadline) SetWriteDeadline(deadline time.Time) error {
	if deadline.After(time.Now()) {
		deadline = time.Now().Add(100 * time.Millisecond)
	}
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
}

func TestHTTPResponsesHTTP2GenerationIdleIsNotWriteTimeout(t *testing.T) {
	for _, phase := range []string{"first event", "between events", "json"} {
		t.Run(phase, func(t *testing.T) {
			upstream := newHTTPUpstream(t, func(r *http.Request, conn *websocket.Conn, _ []byte) {
				if phase != "first event" {
					sendHTTPEvents(t, conn, httpCreatedEvent)
				}
				// Deliberately exceed the test writer's deadline while no write is
				// active. This must remain governed by the 90s upstream idle limit.
				timer := time.NewTimer(250 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-r.Context().Done():
					return
				}
				sendHTTPEvents(t, conn, httpCompletedEvent)
			})
			srv := newTestServer(t, []*Account{testAccount("a", 0)})
			srv.upstream, srv.admission = upstream.URL, newAdmissionGate(1)
			handler := srv.routes()
			proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler.ServeHTTP(shortHTTPWriteDeadline{w}, r)
			}))
			proxy.EnableHTTP2 = true
			proxy.StartTLS()
			defer proxy.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			body := `{"model":"m","stream":true}`
			if phase == "json" {
				body = `{"model":"m"}`
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := proxy.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil || resp.ProtoMajor != 2 || resp.StatusCode != 200 || !strings.Contains(string(data), `"status":"completed"`) {
				t.Fatalf("protocol=%s status=%d body=%s error=%v", resp.Proto, resp.StatusCode, data, err)
			}
			assertHTTPClean(t, srv)
		})
	}
}
