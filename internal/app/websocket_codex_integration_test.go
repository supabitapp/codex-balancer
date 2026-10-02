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
	"sync"
	"testing"
	"time"
)

// Run with CODEX_BALANCER_TEST_CODEX=/path/to/codex. Uses an isolated home,
// synthetic credentials, and a local upstream; no real inference or login.
func TestCodexAppServerUsageLimitReplaysFullHistory(t *testing.T) {
	for _, token := range []bool{false, true} {
		t.Run(fmt.Sprintf("turn_state_%t", token), func(t *testing.T) { testCodexReconnectReplay(t, token, "usage") })
	}
}

func TestCodexAppServerFastModeReplaysFullHistory(t *testing.T) {
	testCodexReconnectReplay(t, false, "fast")
}

func testCodexReconnectReplay(t *testing.T, token bool, scenario string) {
	forceFast := scenario == "fast"
	transientRateLimit := strings.HasPrefix(scenario, "rate-")
	httpOnly := scenario == "rate-http"
	firstTurn := scenario == "rate-first-turn"
	coldResume := token && scenario == "usage"
	binary := os.Getenv("CODEX_BALANCER_TEST_CODEX")
	if binary == "" {
		t.Skip("set CODEX_BALANCER_TEST_CODEX to run the app-server integration test")
	}
	var mu sync.Mutex
	var rejected, replay, firstReplacement map[string]any
	var replayAccount, firstReplacementAccount string
	var rejectedAt, firstReplacementAt time.Time
	var enableFast func()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token && r.Header.Get("chatgpt-account-id") == "account-a" {
			w.Header().Set(codexTurnStateKey, "account-a-token")
		}
		conn, err := acceptResponseTestStream(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() {
			if conn.conn != nil {
				conn.CloseNow()
			}
		}()
		conn.SetReadLimit(maxWebSocketMessage)
		writeEvent := func(value any) {
			data, err := json.Marshal(value)
			if err != nil {
				t.Error(err)
				return
			}
			sendHTTPEvents(t, conn, string(data))
		}
		account := r.Header.Get("chatgpt-account-id")
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request map[string]any
			if err := json.Unmarshal(data, &request); err != nil {
				t.Error(err)
				return
			}
			if r.Method == http.MethodGet && request["type"] != "response.create" {
				continue
			}
			warmup := request["generate"] == false
			mu.Lock()
			alreadyRejected := rejected != nil
			mu.Unlock()
			if account == "account-a" && (firstTurn || !warmup && strings.Contains(string(data), "SECOND_TURN")) && (!(forceFast || transientRateLimit) || !alreadyRejected) {
				mu.Lock()
				rejected = request
				rejectedAt = time.Now()
				mu.Unlock()
				if forceFast {
					enableFast()
				} else if transientRateLimit {
					if r.Method == http.MethodPost {
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("Retry-After", "1")
						w.WriteHeader(429)
						io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"try later"}}`)
						return
					}
					writeEvent(map[string]any{"type": "error", "status": 429, "error": map[string]any{"type": "rate_limit_error", "code": "rate_limit_exceeded"}})
				} else {
					writeEvent(map[string]any{"type": "error", "status": 429, "error": map[string]any{"type": "usage_limit_reached", "code": "usage_limit_reached"}})
				}
				continue
			}
			if account == "account-b" || (forceFast || transientRateLimit) && alreadyRejected {
				if forceFast && (account != "account-a" || request["service_tier"] != "priority") {
					t.Errorf("fast mode replay account/tier = %s/%v", account, request["service_tier"])
				}
				mu.Lock()
				if firstReplacement == nil {
					firstReplacement = request
					firstReplacementAccount = account
					firstReplacementAt = time.Now()
				}
				if !warmup {
					replay = request
					replayAccount = account
				}
				mu.Unlock()
			}
			id := "resp-" + account
			writeEvent(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
			if !warmup {
				writeEvent(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "reasoning", "id": "reasoning-" + account, "summary": []any{}, "encrypted_content": "preserved-encrypted-history"}})
				writeEvent(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "id": "message-" + account, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "FIRST_ANSWER"}}}})
			}
			writeEvent(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
		}
	}))
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("account-a", 0), testAccount("account-b", 20)})
	key, err := generateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.pool.store.addAPIKey(storedAPIKey{Name: "codex", Secret: key, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	srv.lookupAPIKey = srv.pool.store.apiKeyName
	enableFast = func() { srv.fastMode.set(fastModeOn) }
	home, cwd := t.TempDir(), t.TempDir()
	config := fmt.Sprintf(`model = "gpt-5.4"
model_provider = "balancer"
[features]
# Avoid background marketplace clones racing temporary-home cleanup.
plugins = false
[model_providers.balancer]
name = "OpenAI"
base_url = %q
experimental_bearer_token = %q
supports_websockets = %t
requires_openai_auth = false
`, proxy.URL+"/v1", key, !httpOnly)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	var stdin io.WriteCloser
	var encoder *json.Encoder
	var scanner *bufio.Scanner
	var stderr testLogBuffer
	startClient := func() {
		t.Helper()
		cmd = exec.CommandContext(ctx, binary, "app-server")
		cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
		cmd.Dir = cwd
		var err error
		stdin, err = cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		encoder = json.NewEncoder(stdin)
		scanner = bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 8<<20)
	}
	stopClient := func() { stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }
	startClient()
	defer func() { stopClient() }()
	send := func(value any) {
		t.Helper()
		if err := encoder.Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	readUntil := func(match func(map[string]any) bool) map[string]any {
		t.Helper()
		for scanner.Scan() {
			var event map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if event["error"] != nil {
				t.Fatalf("RPC error: %v", event)
			}
			if match(event) {
				return event
			}
		}
		t.Fatalf("app-server ended: %v; stderr: %s", scanner.Err(), stderr.String())
		return nil
	}
	initialize := func() {
		send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "balancer_test", "version": "1"}}})
		readUntil(func(e map[string]any) bool { return e["id"] == float64(1) })
		send(map[string]any{"method": "initialized", "params": map[string]any{}})
	}
	initialize()
	send(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": cwd, "approvalPolicy": "never", "sandbox": "read-only"}})
	started := readUntil(func(e map[string]any) bool { return e["id"] == float64(2) })
	thread := started["result"].(map[string]any)["thread"].(map[string]any)["id"]
	prompts := []string{"FIRST_TURN", "SECOND_TURN"}
	if coldResume {
		prompts = append(prompts, "RESUME_TURN")
	}
	for i, text := range prompts {
		if coldResume && i == 2 {
			// A cold resume discards the old process's turn state, like returning
			// after logout/login. Account B supplies the upstream credentials.
			stopClient()
			startClient()
			initialize()
			send(map[string]any{"id": 20, "method": "thread/resume", "params": map[string]any{"threadId": thread, "cwd": cwd}})
			readUntil(func(e map[string]any) bool { return e["id"] == float64(20) })
		}
		send(map[string]any{"id": 3 + i, "method": "turn/start", "params": map[string]any{"threadId": thread, "input": []any{map[string]any{"type": "text", "text": text}}}})
		completed := readUntil(func(e map[string]any) bool { return e["method"] == "turn/completed" })
		turn := completed["params"].(map[string]any)["turn"].(map[string]any)
		if coldResume && i == 1 {
			if turn["status"] != "failed" {
				t.Fatalf("account-bound turn should fail: %v", turn)
			}
			mu.Lock()
			moved := replay != nil
			mu.Unlock()
			if moved {
				t.Fatal("account-bound turn was moved")
			}
			continue
		}
		if turn["status"] != "completed" {
			t.Fatalf("turn failed: %v; stderr: %s", turn, stderr.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if rejected == nil || replay == nil {
		t.Fatalf("missing rejection or replay: rejected=%v replay=%v", rejected != nil, replay != nil)
	}
	if !httpOnly && !firstTurn && (rejected["previous_response_id"] == nil || rejected["previous_response_id"] == "") {
		t.Fatal("test did not exercise an incremental request")
	}
	if firstReplacement["previous_response_id"] != nil && firstReplacement["previous_response_id"] != "" {
		t.Fatal("old response ID reached replacement's first request")
	}
	replayResponseID := "resp-account-b"
	if forceFast || transientRateLimit {
		replayResponseID = "resp-account-a"
	}
	if id := replay["previous_response_id"]; id != nil && id != "" && (id != replayResponseID || !firstTurn && firstReplacement["generate"] != false) {
		t.Fatal("replay references a response outside the replacement socket")
	}
	metadata, _ := replay["client_metadata"].(map[string]any)
	if !firstTurn && metadata[codexTurnStateKey] != nil && metadata[codexTurnStateKey] != "" {
		t.Fatal("old turn state reached replacement")
	}
	if transientRateLimit {
		if replayAccount != "account-a" || firstReplacementAccount != "account-a" {
			t.Fatalf("rate retry moved accounts: first=%s replay=%s", firstReplacementAccount, replayAccount)
		}
		if firstReplacementAt.Sub(rejectedAt) < minCooldown-100*time.Millisecond {
			t.Fatal("client retried inference before the cooldown expired")
		}
	}
	input, _ := json.Marshal(replay["input"])
	parts := []string{"FIRST_TURN", "FIRST_ANSWER", "SECOND_TURN", "preserved-encrypted-history"}
	if firstTurn {
		// The rejection predates any accepted history. After recovering, the
		// second turn can reference the first turn on the new socket.
		parts = []string{"SECOND_TURN"}
	}
	for _, part := range parts {
		if !strings.Contains(string(input), part) {
			t.Errorf("replay omitted %s", part)
		}
	}
}

func TestCodexAppServerTransientRateLimitRecovers(t *testing.T) {
	for _, transport := range []string{"websocket", "http"} {
		t.Run(transport, func(t *testing.T) { testCodexReconnectReplay(t, false, "rate-"+transport) })
	}
}

func TestCodexAppServerFirstTurnRateLimitRetainsOwner(t *testing.T) {
	testCodexReconnectReplay(t, true, "rate-first-turn")
}
