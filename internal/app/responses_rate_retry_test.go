package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPRateLimitPreservesNonportableOwner(t *testing.T) {
	for _, source := range []string{"header", "metadata", "response ID", "upstream header"} {
		for _, format := range []string{"http error", "SSE error", "SSE failed"} {
			t.Run(source+"/"+format, func(t *testing.T) {
				var calls atomic.Int64
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					io.Copy(io.Discard, r.Body)
					if source == "upstream header" {
						w.Header().Set(codexTurnStateKey, "state-a")
					}
					w.Header().Set("Retry-After", "17")
					switch format {
					case "http error":
						w.WriteHeader(429)
						io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
					case "SSE error":
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: "+`{"type":"error","status":429,"error":{"code":"rate_limit_exceeded"}}`+"\n\n")
					case "SSE failed":
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: "+`{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded"}}}`+"\n\n")
					}
				}))
				defer upstream.Close()
				a, b := testAccount("a", 0), testAccount("b", 20)
				srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
				headers := http.Header{"Session-Id": {"same"}}
				payload := `{"model":"m","stream":true}`
				switch source {
				case "header":
					headers.Set(codexTurnStateKey, "state-a")
				case "metadata":
					payload = `{"model":"m","stream":true,"client_metadata":{"x-codex-turn-state":"state-a"}}`
				case "response ID":
					payload = `{"model":"m","stream":true,"previous_response_id":"response-a"}`
				}
				response := postResponse(t, proxy.URL, payload, headers)
				body := readHTTPBody(t, response)
				want := 503
				if format == "SSE failed" {
					want = 200 // Preserve the original terminal SSE event.
				}
				if response.StatusCode != want || !strings.Contains(body, "rate_limit_exceeded") || response.Header.Get("Retry-After") != "17" {
					t.Fatalf("status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
				}
				owners, err := srv.pool.store.routeOwners("", "same")
				if err != nil || fmt.Sprint(owners) != "[a]" {
					t.Fatalf("owners=%v err=%v, want provisional boundary on a", owners, err)
				}
				if err := srv.pool.setPaused(a, true); err != nil {
					t.Fatal(err)
				}
				if source == "upstream header" {
					headers.Set(codexTurnStateKey, "state-a")
				}
				retry := postResponse(t, proxy.URL, payload, headers)
				body = readHTTPBody(t, retry)
				if retry.StatusCode != 400 || !strings.Contains(body, "account_bound_request") || calls.Load() != 1 {
					t.Fatalf("bound retry status=%d calls=%d body=%s", retry.StatusCode, calls.Load(), body)
				}
			})
		}
	}
}

func TestHTTPAnonymousBoundRateLimitStaysTerminal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
	}))
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 0)})
	response := postResponse(t, proxy.URL, `{"model":"m","previous_response_id":"response-a"}`, nil)
	body := readHTTPBody(t, response)
	if response.StatusCode != 429 || !strings.Contains(body, "rate_limit_exceeded") {
		t.Fatalf("anonymous bound error became automatically retryable: %d %s", response.StatusCode, body)
	}
}
