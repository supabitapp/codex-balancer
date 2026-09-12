package app

import (
	"bytes"
	"encoding/json"
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

func TestHTTPResponsesFailedHandshakeStatuses(t *testing.T) {
	for _, status := range []int{200, 204, 302, 400, 500} {
		for _, body := range []string{"not a websocket", `{malformed`, ""} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/%q/stream_%t", status, body, stream), func(t *testing.T) {
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.WriteHeader(status)
						io.WriteString(w, body)
					}))
					defer upstream.Close()
					srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
					srv.admission = newAdmissionGate(1)
					resp := postResponse(t, proxy.URL, fmt.Sprintf(`{"model":"m","stream":%t}`, stream), nil)
					got := readHTTPBody(t, resp)
					want := status
					if want < 400 {
						want = 502
					}
					if resp.StatusCode != want || !strings.Contains(got, `"error"`) || resp.Header.Get("Content-Type") != "application/json" {
						t.Fatalf("status=%d body=%s", resp.StatusCode, got)
					}
					assertHTTPClean(t, srv)
					if srv.stats.snapshot().Turns != 0 {
						t.Fatal("failed handshake counted acceptance")
					}
				})
			}
		}
	}
}

func TestHTTPResponsesForbiddenUsageLimit(t *testing.T) {
	for _, field := range []string{"code", "type"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", field, stream), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Retry-After", "17")
					w.Header().Set("X-Codex-Rate-Limit-Reached-Type", "workspace_limit") // never call reset-credit APIs
					w.WriteHeader(403)
					fmt.Fprintf(w, `{"error":{%q:"usage_limit_reached","message":"quota exhausted"}}`, field)
				}))
				defer upstream.Close()
				srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
				srv.admission = newAdmissionGate(1)
				resp := postResponse(t, proxy.URL, fmt.Sprintf(`{"model":"m","stream":%t}`, stream), nil)
				body := readHTTPBody(t, resp)
				if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "17" || !strings.Contains(body, `"code":"usage_limit_reached"`) || !strings.Contains(body, "quota exhausted") {
					t.Fatalf("status=%d headers=%v body=%s", resp.StatusCode, resp.Header, body)
				}
				assertHTTPClean(t, srv)
				if snapshot := srv.stats.snapshot(); snapshot.Limited != 1 || snapshot.Turns != 0 || snapshot.MonthlyUsage.TotalTokens != 0 {
					t.Fatalf("accounting=%+v", snapshot)
				}
			})
		}
	}
}

func TestResponsesFailedModelPreflight(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body, code string
		timeout    bool
	}{
		{"rate", 429, `{"error":{"code":"rate_limit_exceeded","message":"local rate limit"}}`, "rate_limit_exceeded", false},
		{"usage", 403, `{"error":{"type":"usage_limit_reached","message":"local quota"}}`, "usage_limit_reached", false},
		{"model", 404, `{"error":{"code":"model_not_found","message":"local model unavailable"}}`, "model_not_found", false},
		{"server", 502, `{"error":{"code":"server_error","message":"local server failure"}}`, "server_error", false},
		{"non-upgrade", 200, "not a websocket", "upstream_rejected", false},
		{"timeout", 504, "", "upstream_timeout", true},
	} {
		for _, mode := range []string{"json", "sse", "websocket"} {
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				var srv *server
				var inference atomic.Int64
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Chatgpt-Account-Id") == "a" {
						srv.catalog.replace([]string{"a", "b"}, map[string][]modelEntry{"a": {testModelEntry("other")}, "b": {testModelEntry("m")}}, "0.1.0")
						conn, err := websocket.Accept(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer conn.CloseNow()
						if _, _, err := conn.Read(r.Context()); err == nil {
							inference.Add(1)
						}
						return
					}
					if test.timeout {
						<-r.Context().Done()
						return
					}
					w.Header().Set("Retry-After", "17")
					w.Header().Set("X-Codex-Rate-Limit-Reached-Type", "workspace_limit")
					w.WriteHeader(test.status)
					io.WriteString(w, test.body)
				}))
				defer upstream.Close()
				srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 20)})
				srv.admission = newAdmissionGate(1)
				if test.timeout {
					srv.client.Timeout = time.Second
				}
				headers := http.Header{"Session-Id": {"session"}}
				if mode == "websocket" {
					conn, _ := dialWebSocket(t, proxy.URL, headers)
					defer conn.CloseNow()
					writeWebSocketEvent(t, conn, map[string]string{"type": "response.create", "model": "m"})
					readCloseStatus(t, conn, websocket.StatusTryAgainLater)
				} else {
					resp := postResponse(t, proxy.URL, fmt.Sprintf(`{"model":"m","stream":%t}`, mode == "sse"), headers)
					body := readHTTPBody(t, resp)
					want := test.status
					if want == 403 {
						want = 429
					}
					if want < 400 {
						want = 502
					}
					if resp.StatusCode != want || !strings.Contains(body, test.code) || !test.timeout && resp.Header.Get("Retry-After") != "17" {
						t.Fatalf("status=%d retry=%s body=%s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
					}
				}
				// Also covers the inherited double-selection claim-reference leak.
				assertHTTPClean(t, srv)
				if inference.Load() != 0 || srv.stats.snapshot().Turns != 0 {
					t.Fatal("failed preflight transmitted inference")
				}
			})
		}
	}
}

func TestHTTPResponsesEscapedInstructionType(t *testing.T) {
	plain := `{"model":"m","input":[{"role":"system","content":[{"type":"input_text","text":"Follow instructions"}]}]}`
	escaped := strings.Replace(plain, "input_text", `input_\u0074ext`, 1)
	one, _, err := translateHTTPResponse([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := translateHTTPResponse([]byte(escaped))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, two) {
		t.Fatalf("equivalent JSON changed normalization: %s != %s", one, two)
	}
}

func TestHTTPResponsesDecodedCredentialRedaction(t *testing.T) {
	for _, mode := range []string{"json", "sse output", "sse failed", "sse nested error"} {
		t.Run(mode, func(t *testing.T) {
			upstream := newHTTPUpstream(t, func(_ *http.Request, conn *websocket.Conn, _ []byte) {
				sendHTTPEvents(t, conn, httpCreatedEvent)
				switch mode {
				case "sse failed":
					sendHTTPEvents(t, conn, `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"token-\u0061","extra":{"nested":["\u0074oken-a","safe"]}}}}`)
				case "sse nested error":
					sendHTTPEvents(t, conn, `{"type":"error","error":{"code":"server_error","message":"prefix token-\u0061 suffix","nested":{"message":"\u0074oken-a"}},"number":9007199254740993}`)
				default:
					sendHTTPEvents(t, conn, `{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"token-\u0061 and \u0074oken-a"}]}],"number":9007199254740993,"usage":{"input_tokens":1,"output_tokens":1}}}`)
				}
			})
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
			srv.admission = newAdmissionGate(1)
			resp := postResponse(t, proxy.URL, fmt.Sprintf(`{"model":"m","stream":%t}`, mode != "json"), nil)
			body := readHTTPBody(t, resp)
			payloads := []string{body}
			if mode != "json" {
				payloads = nil
				for _, line := range strings.Split(body, "\n") {
					if strings.HasPrefix(line, "data: {") {
						payloads = append(payloads, strings.TrimPrefix(line, "data: "))
					}
				}
			}
			for _, payload := range payloads {
				decoder := json.NewDecoder(strings.NewReader(payload))
				decoder.UseNumber()
				var value any
				if err := decoder.Decode(&value); err != nil {
					t.Fatal(err)
				}
				canonical, _ := json.Marshal(value)
				if strings.Contains(string(canonical), "token-a") {
					t.Fatalf("decoded credential leaked: %s", canonical)
				}
			}
			if !strings.Contains(body, "[redacted]") {
				t.Fatalf("missing redaction: %s", body)
			}
			if mode != "sse failed" && !strings.Contains(body, "9007199254740993") {
				t.Fatal("lost JSON precision")
			}
			assertHTTPClean(t, srv)
		})
	}
}

func TestResponseRedactionPreservesNonStringBytes(t *testing.T) {
	const input = `{"token-\u0061":["token-a","token-\u0061","\"token-\u0061\""],"n":9007199254740993,"f":-0.001230000000000000000005e+17,"safe":"line\nquote\"slash\\"}`
	const want = `{"[redacted]":["[redacted]","[redacted]","\"[redacted]\""],"n":9007199254740993,"f":-0.001230000000000000000005e+17,"safe":"line\nquote\"slash\\"}`
	peer := &httpResponsesDownstream{secrets: []string{"token-a"}}
	if got := string(peer.redact([]byte(input))); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}
