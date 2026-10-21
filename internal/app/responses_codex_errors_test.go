package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestCodexAppServerSurfacesUpstreamFailures(t *testing.T) {
	for _, transport := range []string{"websocket", "http"} {
		scenarios := []string{"policy", "capacity", "authentication"}
		if transport == "http" {
			scenarios = append(scenarios, "message too big")
		}
		for _, scenario := range scenarios {
			t.Run(transport+"/"+scenario, func(t *testing.T) {
				testCodexAppServerUpstreamFailure(t, transport == "websocket", scenario)
			})
		}
	}
}

func TestCodexAppServerRetriesHTTPCapacity(t *testing.T) {
	if os.Getenv("CODEX_BALANCER_TEST_CODEX") == "" {
		t.Skip("set CODEX_BALANCER_TEST_CODEX to run the app-server integration test")
	}
	for _, code := range []string{"server_is_overloaded", "slow_down"} {
		for _, test := range []struct {
			name      string
			payload   string
			committed bool
			status    int
			failures  int64
		}{
			{"flat", `{"type":"error","code":%q,"message":"original capacity detail","param":"model"}`, true, 200, 2},
			{"nested", `{"type":"error","status":503,"error":{"code":%q,"type":"server_error","message":"original capacity detail","param":"model"}}`, true, 200, 2},
			{"failed", `{"type":"response.failed","response":{"error":{"code":%q,"message":"original capacity detail"}}}`, true, 200, 2},
			{"uncommitted failed", `{"type":"response.failed","response":{"error":{"code":%q,"message":"original capacity detail"}}}`, false, 200, 2},
			{"HTTP rejection", `{"error":{"code":%q,"message":"original capacity detail"}}`, false, 503, 3},
		} {
			t.Run(code+"/"+test.name, func(t *testing.T) {
				var attempts atomic.Int64
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempt := attempts.Add(1)
					io.Copy(io.Discard, r.Body)
					if attempt <= test.failures && test.status != 200 {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(test.status)
						fmt.Fprintf(w, test.payload, code)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if attempt <= test.failures {
						if test.committed {
							fmt.Fprintf(w, "data: %s\n\n", httpCreatedEvent)
						}
						fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(test.payload, code))
						return
					}
					fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", httpCreatedEvent, httpCompletedEvent)
				}))
				defer upstream.Close()
				_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
				turn, events := runCodexErrorTurn(t, proxy.URL, false)
				if turn["status"] != "completed" || attempts.Load() != test.failures+1 || strings.Contains(events, "Selected model is at capacity") {
					t.Fatalf("attempts=%d turn=%v events=%s", attempts.Load(), turn, events)
				}
				if test.committed && !strings.Contains(events, "original capacity detail") {
					t.Fatalf("retry diagnostics lost original error: %s", events)
				}
			})
		}
	}
}

func TestCodexAppServerWebSocketSizeFailureFallsBackToHTTP(t *testing.T) {
	if os.Getenv("CODEX_BALANCER_TEST_CODEX") == "" {
		t.Skip("set CODEX_BALANCER_TEST_CODEX to run the app-server integration test")
	}
	for _, httpStatus := range []int{http.StatusOK, http.StatusRequestEntityTooLarge} {
		t.Run(fmt.Sprint(httpStatus), func(t *testing.T) {
			var websocketAttempts, httpAttempts atomic.Int64
			upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, request websocketEnvelope) {
				if request.Generate != nil && !*request.Generate {
					sendHTTPEvents(t, conn, httpCreatedEvent, httpCompletedEvent)
					return
				}
				websocketAttempts.Add(1)
				conn.Close(websocket.StatusMessageTooBig, "request exceeds upstream limit")
			})
			defer upstream.Close()
			httpUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					upstream.Config.Handler.ServeHTTP(w, r)
					return
				}
				httpAttempts.Add(1)
				if r.Method != http.MethodPost || r.Header.Get("Upgrade") != "" || r.Header.Get("Authorization") != "Bearer token-a" {
					t.Errorf("unexpected upstream HTTP method, upgrade, or authentication")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				var request map[string]json.RawMessage
				if err := json.Unmarshal(body, &request); err != nil {
					t.Error(err)
				}
				if !strings.Contains(string(request["input"]), "test upstream errors") || request["type"] != nil || request["previous_response_id"] != nil {
					t.Errorf("HTTP fallback did not send a full-history Responses request")
				}
				if httpStatus == http.StatusRequestEntityTooLarge {
					w.WriteHeader(httpStatus)
					io.WriteString(w, `{"error":{"code":"request_too_large","message":"request exceeds upstream HTTP limit"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", httpCreatedEvent, httpCompletedEvent)
			}))
			defer httpUpstream.Close()
			_, proxy := newWebSocketProxy(t, httpUpstream.URL, []*Account{testAccount("a", 0)})
			turn, events := runCodexErrorTurn(t, proxy.URL, true)
			wantStatus := "completed"
			if httpStatus == http.StatusRequestEntityTooLarge {
				wantStatus = "failed"
			}
			if turn["status"] != wantStatus || websocketAttempts.Load() != 3 || httpAttempts.Load() != 1 || !strings.Contains(events, "Falling back from WebSockets to HTTPS transport") {
				t.Fatalf("websocket attempts=%d HTTP attempts=%d turn=%v events=%s", websocketAttempts.Load(), httpAttempts.Load(), turn, events)
			}
			if httpStatus == http.StatusRequestEntityTooLarge && !strings.Contains(events, "request_too_large") {
				t.Fatalf("HTTP size rejection lost its code: %s", events)
			}
		})
	}
}

func testCodexAppServerUpstreamFailure(t *testing.T, websockets bool, scenario string) {
	if os.Getenv("CODEX_BALANCER_TEST_CODEX") == "" {
		t.Skip("set CODEX_BALANCER_TEST_CODEX to run the app-server integration test")
	}
	var attempts atomic.Int64
	if scenario == "authentication" {
		useOAuthRefreshServer(t)
	}
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, request websocketEnvelope) {
		if request.Generate != nil && !*request.Generate {
			sendHTTPEvents(t, conn, httpCreatedEvent, httpCompletedEvent)
			return
		}
		attempt := attempts.Add(1)
		switch scenario {
		case "policy":
			conn.Close(websocket.StatusPolicyViolation, "upstream policy detail")
			return
		case "capacity":
			if attempt <= 2 {
				sendHTTPEvents(t, conn, `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"original capacity detail"}}}`)
				return
			}
		case "authentication":
			if attempt == 1 {
				sendHTTPEvents(t, conn, `{"type":"error","status":401,"error":{"code":"unauthorized","message":"original authentication detail"}}`)
				return
			}
		}
		sendHTTPEvents(t, conn, httpCreatedEvent, httpCompletedEvent)
	})
	defer upstream.Close()
	upstreamURL := upstream.URL
	if !websockets || scenario == "policy" {
		httpUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if websockets && r.Method == http.MethodGet {
				upstream.Config.Handler.ServeHTTP(w, r)
				return
			}
			if r.Method != http.MethodPost || r.Header.Get("Upgrade") != "" {
				t.Errorf("HTTP client reached upstream as %s", r.Method)
			}
			io.Copy(io.Discard, r.Body)
			attempt := attempts.Add(1)
			switch scenario {
			case "message too big":
				w.WriteHeader(413)
				io.WriteString(w, `{"error":{"code":"request_too_large","message":"request exceeds upstream limit"}}`)
				return
			case "policy":
				w.WriteHeader(400)
				io.WriteString(w, `{"error":{"code":"invalid_request","message":"http fallback detail"}}`)
				return
			case "authentication":
				if attempt == 1 {
					w.WriteHeader(401)
					io.WriteString(w, `{"error":{"code":"unauthorized","message":"original authentication detail"}}`)
					return
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if scenario == "capacity" && attempt <= 2 {
				io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"original capacity detail\"}}}\n\n")
				return
			}
			fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", httpCreatedEvent, httpCompletedEvent)
		}))
		defer httpUpstream.Close()
		upstreamURL = httpUpstream.URL
	}
	_, proxy := newWebSocketProxy(t, upstreamURL, []*Account{testAccount("a", 0)})
	turn, events := runCodexErrorTurn(t, proxy.URL, websockets)
	if scenario == "policy" && websockets {
		if turn["status"] != "failed" || attempts.Load() != 4 || !strings.Contains(events, "upstream policy detail") || !strings.Contains(events, "Falling back from WebSockets to HTTPS transport") || strings.Contains(events, "websocket closed by server") {
			t.Fatalf("attempts=%d turn=%v events=%s", attempts.Load(), turn, events)
		}
	} else if scenario == "message too big" || scenario == "policy" {
		want := "request_too_large"
		switch scenario {
		case "policy":
			want = "http fallback detail"
		}
		if turn["status"] != "failed" || attempts.Load() != 1 || !strings.Contains(events, want) || strings.Contains(events, "websocket closed by server") {
			t.Fatalf("attempts=%d turn=%v events=%s", attempts.Load(), turn, events)
		}
	} else {
		want := int64(3)
		if scenario == "authentication" {
			want = 2
		}
		if turn["status"] != "completed" || attempts.Load() != want {
			t.Fatalf("attempts=%d turn=%v events=%s", attempts.Load(), turn, events)
		}
		if scenario == "capacity" && !strings.Contains(events, "original capacity detail") {
			t.Fatalf("retry diagnostics lost original error: %s", events)
		}
	}
}

func runCodexErrorTurn(t *testing.T, url string, websockets bool) (map[string]any, string) {
	t.Helper()
	config := fmt.Sprintf(`model = "gpt-5.4"
model_provider = "balancer"
[model_providers.balancer]
name = "OpenAI"
base_url = %q
experimental_bearer_token = "test-key"
supports_websockets = %t
requires_openai_auth = false
request_max_retries = 2
stream_max_retries = 2
`, url+"/v1", websockets)
	return runCodexAppServerTurn(t, config, "test upstream errors")
}

func runCodexAppServerTurn(t *testing.T, config, prompt string, overrides ...string) (map[string]any, string) {
	t.Helper()
	binary := os.Getenv("CODEX_BALANCER_TEST_CODEX")
	home, cwd := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"app-server", "-c", "features.plugins=false", "-c", "features.unbounded_connection_retries=false"}
	for _, override := range overrides {
		args = append(args, "-c", override)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	cmd.Dir = cwd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr testLogBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() }()
	encoder := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var events strings.Builder
	send := func(id int, method string, params any) {
		t.Helper()
		if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(method string, id float64) map[string]any {
		t.Helper()
		for scanner.Scan() {
			data := scanner.Bytes()
			events.Write(data)
			events.WriteByte('\n')
			var event map[string]any
			if err := json.Unmarshal(data, &event); err != nil {
				t.Fatal(err)
			}
			if event["error"] != nil {
				t.Fatalf("RPC error: %v", event)
			}
			if method != "" && event["method"] == method || id != 0 && event["id"] == id {
				return event
			}
		}
		t.Fatalf("app-server ended: %v; stderr: %s; events: %s", scanner.Err(), stderr.String(), events.String())
		return nil
	}
	send(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "balancer_test", "version": "1"}})
	read("", 1)
	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	send(2, "thread/start", map[string]any{"cwd": cwd, "approvalPolicy": "never", "sandbox": "read-only"})
	started := read("", 2)
	thread := started["result"].(map[string]any)["thread"].(map[string]any)["id"]
	send(3, "turn/start", map[string]any{"threadId": thread, "input": []any{map[string]string{"type": "text", "text": prompt}}})
	completed := read("turn/completed", 0)
	return completed["params"].(map[string]any)["turn"].(map[string]any), events.String()
}
