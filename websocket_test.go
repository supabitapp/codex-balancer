package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestResponsesWebSocketRelaysTracksAndHonorsPause(t *testing.T) {
	requests := make(chan string, 3)
	headers := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-codex-turn-state", "turn-upstream")
		w.Header().Set("openai-model", "gpt-test")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		headers <- r.Header.Clone()
		for i := 0; ; i++ {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			kind, data, err := conn.Read(ctx)
			cancel()
			if err != nil {
				return
			}
			requests <- string(data)
			ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
			err = conn.Write(ctx, kind, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":"resp-%d"}}`, i)))
			if err == nil {
				err = conn.Write(ctx, kind, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-%d"}}`, i)))
			}
			cancel()
			if err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	s := testServer(t, upstream.URL, "acct-a")
	s.key = "proxy-key"
	proxy := httptest.NewServer(s.routes())
	defer proxy.Close()

	requestHeaders := http.Header{}
	requestHeaders.Set("Authorization", "Bearer proxy-key")
	requestHeaders.Set("session-id", "thread-1")
	requestHeaders.Set("originator", "codex_cli_rs")
	requestHeaders.Set("Cookie", "private=downstream")
	requestHeaders.Set("OpenAI-Beta", "responses=experimental, another=1")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	client, response, err := websocket.Dial(ctx, websocketTestURL(proxy.URL)+"/v1/responses", &websocket.DialOptions{
		HTTPHeader: requestHeaders,
	})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()

	if got := response.Header.Get("x-codex-turn-state"); got != "turn-upstream" {
		t.Fatalf("turn state = %q, want turn-upstream", got)
	}
	if got := response.Header.Get("openai-model"); got != "gpt-test" {
		t.Fatalf("model = %q, want gpt-test", got)
	}

	gotHeaders := <-headers
	if got := gotHeaders.Get("Authorization"); got != "Bearer token-acct-a" {
		t.Fatalf("upstream authorization = %q", got)
	}
	if got := gotHeaders.Get("chatgpt-account-id"); got != "acct-a" {
		t.Fatalf("upstream account = %q", got)
	}
	if got := gotHeaders.Get("Cookie"); got != "" {
		t.Fatalf("upstream cookie = %q", got)
	}
	if got := gotHeaders.Get("originator"); got != "codex_cli_rs" {
		t.Fatalf("upstream originator = %q", got)
	}
	beta := gotHeaders.Get("OpenAI-Beta")
	if strings.Contains(beta, "responses=experimental") || !strings.Contains(beta, responsesWebSocketBeta) || !strings.Contains(beta, "another=1") {
		t.Fatalf("upstream beta header = %q", beta)
	}

	waitFor(t, func() bool { return s.stats.snapshot().WSOpen == 1 })
	websocketExchange(t, client, `{"type":"response.create","generate":false}`)
	websocketExchange(t, client, `{"type":"response.create","input":[],"service_tier":"priority"}`)

	snap := s.stats.snapshot()
	if snap.Turns != 1 || snap.WSTurns != 1 || snap.WSOpen != 1 {
		t.Fatalf("websocket stats = turns %d, ws turns %d, open %d", snap.Turns, snap.WSTurns, snap.WSOpen)
	}
	if snap.Accounts["acct-a"].WSOpen != 1 {
		t.Fatalf("account websocket count = %d", snap.Accounts["acct-a"].WSOpen)
	}
	if len(snap.Threads) != 1 || snap.Threads[0].Via != transportWebSocket || snap.Threads[0].ServiceTier != serviceTierFast {
		t.Fatalf("thread stats = %+v", snap.Threads)
	}
	if snap.TTFB <= 0 {
		t.Fatal("websocket time to first byte not recorded")
	}
	if got := s.sticky.get("thread-1"); got != "acct-a" {
		t.Fatalf("sticky account = %q, want acct-a", got)
	}

	if _, err := s.pool.togglePause(s.pool.find("acct-a")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
	if err := client.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","input":[]}`)); err != nil {
		cancel()
		t.Fatal(err)
	}
	_, _, err = client.Read(ctx)
	cancel()
	if err == nil {
		t.Fatal("paused account left the websocket open")
	}
	waitFor(t, func() bool { return s.stats.snapshot().WSOpen == 0 })

	if got := len(requests); got != 2 {
		t.Fatalf("upstream requests = %d, want warmup and one turn", got)
	}
	if warmup := <-requests; !strings.Contains(warmup, `"generate":false`) {
		t.Fatalf("warmup request = %s", warmup)
	}
	<-requests
}

func TestResponsesWebSocketRejectsBadKeyBeforeUpstream(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
	}))
	defer upstream.Close()

	s := testServer(t, upstream.URL, "acct-a")
	s.key = "proxy-key"
	proxy := httptest.NewServer(s.routes())
	defer proxy.Close()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer wrong")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	conn, response, err := websocket.Dial(ctx, websocketTestURL(proxy.URL)+"/v1/responses", &websocket.DialOptions{
		HTTPHeader: headers,
	})
	cancel()
	if conn != nil {
		conn.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dial error = %v, response = %v", err, response)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("unauthorized handshake made %d upstream calls", upstreamCalls.Load())
	}
}

func TestResponsesWebSocketFailsOverBeforeUpgrade(t *testing.T) {
	var mu sync.Mutex
	var accounts []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("chatgpt-account-id")
		mu.Lock()
		accounts = append(accounts, id)
		mu.Unlock()
		if id == "acct-a" {
			w.Header().Set("x-codex-primary-used-percent", "100")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		conn.Read(ctx)
	}))
	defer upstream.Close()

	s := testServer(t, upstream.URL, "acct-a", "acct-b")
	proxy := httptest.NewServer(s.routes())
	defer proxy.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	conn, _, err := websocket.Dial(ctx, websocketTestURL(proxy.URL)+"/v1/responses", nil)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	conn.CloseNow()

	mu.Lock()
	got := append([]string(nil), accounts...)
	mu.Unlock()
	if fmt.Sprint(got) != "[acct-a acct-b]" {
		t.Fatalf("upstream accounts = %v", got)
	}
	snap := s.stats.snapshot()
	if snap.Failures != 1 || snap.Limited != 1 {
		t.Fatalf("failovers = %d, limits = %d", snap.Failures, snap.Limited)
	}
}

func websocketExchange(t *testing.T, conn *websocket.Conn, request string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(request)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, err := conn.Read(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func websocketTestURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition not met")
		case <-ticker.C:
		}
	}
}
