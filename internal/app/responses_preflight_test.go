package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"
)

func TestHTTPResponsesModelPreflightKeepsVettedHeaders(t *testing.T) {
	var srv *server
	requests := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("Chatgpt-Account-Id")
		if r.Header.Get("X-Private") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Forwarded-For") != "" {
			t.Error("unvetted headers reached a model preflight connection")
		}
		if account == "a" {
			// The catalog can refresh while a handshake is in flight. Force the
			// existing relay's safe first-turn model preflight onto account B.
			srv.catalog.replace([]string{"a", "b"}, map[string][]modelEntry{
				"a": {testModelEntry("different")}, "b": {testModelEntry("m")},
			}, "0.1.0")
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		requests <- account
		sendHTTPEvents(t, conn, httpCreatedEvent, httpCompletedEvent)
		conn.Read(r.Context())
	}))
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 20)})
	srv.admission = newAdmissionGate(1)
	resp := postResponse(t, proxy.URL, `{"model":"m"}`, http.Header{
		"Session-Id": {"session"}, "Thread-Id": {"thread"},
		"X-Private": {"private"}, "Cookie": {"private"}, "X-Forwarded-For": {"192.0.2.1"},
	})
	body := readHTTPBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	assertHTTPClean(t, srv)
	if got := <-requests; got != "b" {
		t.Fatalf("inference account=%s", got)
	}
	select {
	case account := <-requests:
		t.Fatal("unexpected inference on", account)
	default:
	}
	srv.stats.mu.Lock()
	ip := srv.stats.threads["thread"].clientIP
	srv.stats.mu.Unlock()
	if ip != "192.0.2.1" {
		t.Fatalf("stats client IP=%s", ip)
	}
}
