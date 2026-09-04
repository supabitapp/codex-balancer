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

	"github.com/coder/websocket"
)

// Run with CODEX_BALANCER_TEST_CODEX=/path/to/codex. Uses an isolated home,
// synthetic credentials, and a local upstream; no real inference or login.
func TestCodexAppServerUsageLimitReplaysFullHistory(t *testing.T) {
	for _, token := range []bool{false, true} {
		t.Run(fmt.Sprintf("turn_state_%t", token), func(t *testing.T) { testCodexUsageReplay(t, token) })
	}
}

func testCodexUsageReplay(t *testing.T, token bool) {
	binary := os.Getenv("CODEX_BALANCER_TEST_CODEX")
	if binary == "" {
		t.Skip("set CODEX_BALANCER_TEST_CODEX to run the app-server integration test")
	}
	var mu sync.Mutex
	var rejected, replay, firstReplacement map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token && r.Header.Get("chatgpt-account-id") == "account-a" {
			w.Header().Set(codexTurnStateKey, "account-a-token")
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(maxWebSocketMessage)
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
			if request["type"] != "response.create" {
				continue
			}
			warmup := request["generate"] == false
			if !warmup && account == "account-a" && strings.Contains(string(data), "SECOND_TURN") {
				mu.Lock()
				rejected = request
				mu.Unlock()
				writeWebSocketEvent(t, conn, map[string]any{"type": "error", "status": 429, "error": map[string]any{"type": "usage_limit_reached", "code": "usage_limit_reached"}})
				continue
			}
			if account == "account-b" {
				mu.Lock()
				if firstReplacement == nil {
					firstReplacement = request
				}
				if !warmup {
					replay = request
				}
				mu.Unlock()
			}
			id := "resp-" + account
			writeWebSocketEvent(t, conn, map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
			if !warmup {
				writeWebSocketEvent(t, conn, map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "reasoning", "id": "reasoning-" + account, "summary": []any{}, "encrypted_content": "preserved-encrypted-history"}})
				writeWebSocketEvent(t, conn, map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "id": "message-" + account, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "FIRST_ANSWER"}}}})
			}
			writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
		}
	}))
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("account-a", 0), testAccount("account-b", 20)})
	home, cwd := t.TempDir(), t.TempDir()
	config := fmt.Sprintf(`model = "gpt-5.4"
model_provider = "balancer"
[model_providers.balancer]
name = "OpenAI"
base_url = %q
experimental_bearer_token = "synthetic-client-key"
supports_websockets = true
requires_openai_auth = false
`, proxy.URL+"/v1")
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
	if token {
		prompts = append(prompts, "RESUME_TURN")
	}
	for i, text := range prompts {
		if token && i == 2 {
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
		if token && i == 1 {
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
	if rejected["previous_response_id"] == nil || rejected["previous_response_id"] == "" {
		t.Fatal("test did not exercise an incremental request")
	}
	if firstReplacement["previous_response_id"] != nil && firstReplacement["previous_response_id"] != "" {
		t.Fatal("old response ID reached replacement's first request")
	}
	if id := replay["previous_response_id"]; id != nil && id != "" && (id != "resp-account-b" || firstReplacement["generate"] != false) {
		t.Fatal("replay references a response outside the replacement socket")
	}
	metadata, _ := replay["client_metadata"].(map[string]any)
	if metadata[codexTurnStateKey] != nil && metadata[codexTurnStateKey] != "" {
		t.Fatal("old turn state reached replacement")
	}
	input, _ := json.Marshal(replay["input"])
	for _, part := range []string{"FIRST_TURN", "FIRST_ANSWER", "SECOND_TURN", "preserved-encrypted-history"} {
		if !strings.Contains(string(input), part) {
			t.Errorf("replay omitted %s", part)
		}
	}
}
