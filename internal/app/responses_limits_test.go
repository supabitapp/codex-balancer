package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestHTTPResponsesOutputLimit(t *testing.T) {
	upstream := newHTTPUpstream(t, func(_ *http.Request, c *websocket.Conn, _ []byte) {
		item := strings.Repeat("x", 9<<20)
		for index := 0; index < 2; index++ {
			event := fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":{"type":"custom_tool_call","call_id":"c","name":"large","input":%q}}`, index, item)
			sendHTTPEvents(t, c, event)
		}
	})
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(1)
	// Race instrumentation scans two large JSON frames several times; retain
	// the real 16 MiB production limit rather than shrinking the test fixture.
	resp := postResponseTimeout(t, proxy.URL, `{"model":"m"}`, nil, 90*time.Second)
	body := readHTTPBody(t, resp)
	if resp.StatusCode != 502 || !strings.Contains(body, "retained output exceeds") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	assertHTTPClean(t, srv)
}

func TestHTTPResponsesDuplicateTerminalDoesNotDoubleCount(t *testing.T) {
	upstream := newHTTPUpstream(t, func(_ *http.Request, c *websocket.Conn, _ []byte) {
		sendHTTPEvents(t, c, httpCreatedEvent, httpCreatedEvent, httpCompletedEvent)
		// The adapter may already have closed the socket. Either way the second
		// terminal must never reach accounting or the downstream.
		c.Write(context.Background(), websocket.MessageText, []byte(httpCompletedEvent))
	})
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(1)
	resp := postResponse(t, proxy.URL, `{"model":"m","stream":true}`, nil)
	body := readHTTPBody(t, resp)
	if strings.Count(body, "event: response.completed") != 1 || strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("body=%s", body)
	}
	assertHTTPClean(t, srv)
	if snapshot := srv.stats.snapshot(); snapshot.Turns != 1 || snapshot.MonthlyUsage.TotalTokens != 14 {
		t.Fatalf("duplicate accounting: %+v", snapshot)
	}
}

func TestHTTPResponsesGracefulDrain(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	upstream := newHTTPUpstream(t, func(_ *http.Request, c *websocket.Conn, _ []byte) {
		close(started)
		<-finish
		sendHTTPEvents(t, c, httpCreatedEvent, httpCompletedEvent)
	})
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	runtime, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.ctx, srv.admission = runtime, newAdmissionGate(1)
	result := make(chan string, 1)
	go func() { result <- readHTTPBody(t, postResponse(t, proxy.URL, `{"model":"m"}`, nil)) }()
	waitHTTPSignal(t, started)
	srv.admission.beginDrain()
	drained := make(chan struct{})
	go func() {
		if err := drainServer(&http.Server{}, srv, cancel); err != nil {
			t.Error(err)
		}
		close(drained)
	}()
	select {
	case <-runtime.Done():
		t.Fatal("drain canceled active response early")
	default:
	}
	resp := postResponse(t, proxy.URL, `{"model":"m"}`, nil)
	readHTTPBody(t, resp)
	if resp.StatusCode != 503 {
		t.Fatal("admitted during drain")
	}
	close(finish)
	if body := <-result; !strings.Contains(body, `"status":"completed"`) {
		t.Fatal(body)
	}
	waitHTTPSignal(t, drained)
	assertHTTPClean(t, srv)
}

func TestHTTPResponsesRefreshCancellation(t *testing.T) {
	started, closed := make(chan struct{}), make(chan struct{})
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(closed)
	}))
	defer oauth.Close()
	previous := oauthEndpoint
	oauthEndpoint = oauth.URL
	defer func() { oauthEndpoint = previous }()
	a := testAccount("a", 0)
	a.LastRefresh = time.Now().Add(-tokenRefreshFallback - time.Hour)
	srv, proxy := newWebSocketProxy(t, "http://127.0.0.1:1", []*Account{a})
	srv.admission = newAdmissionGate(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/v1/responses", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}()
	waitHTTPSignal(t, started)
	cancel()
	waitHTTPSignal(t, closed)
	waitHTTPSignal(t, done)
	assertHTTPClean(t, srv)
}

func TestHTTPResponsesHandshakeTimeout(t *testing.T) {
	closed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done(); close(closed) }))
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(1)
	srv.client.Timeout = 10 * time.Millisecond
	resp := postResponse(t, proxy.URL, `{"model":"m","stream":true}`, nil)
	body := readHTTPBody(t, resp)
	if resp.StatusCode != 504 || !strings.Contains(body, "upstream_timeout") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	waitHTTPSignal(t, closed)
	assertHTTPClean(t, srv)
}
