package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// CODEX_BALANCER_TEST_SDK points to an isolated install of @ai-sdk/openai
// 3.0.88 (with OpenCode's existing patch), ai 6.0.168 and zod 4.1.8.
// This exercises the actual pinned client adapter, not the OpenCode application.
func TestHTTPResponsesPinnedSDK(t *testing.T) {
	sdk := os.Getenv("CODEX_BALANCER_TEST_SDK")
	if sdk == "" {
		t.Skip("set CODEX_BALANCER_TEST_SDK to an isolated pinned SDK install")
	}
	for name, want := range map[string]string{"@ai-sdk/openai": "3.0.88", "ai": "6.0.168"} {
		data, err := os.ReadFile(filepath.Join(sdk, "node_modules", name, "package.json"))
		if err != nil {
			t.Fatal(err)
		}
		var pkg struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(data, &pkg); err != nil || pkg.Version != want {
			t.Fatalf("%s version=%s want=%s err=%v", name, pkg.Version, want, err)
		}
	}
	var requests atomic.Int64
	upstream := newHTTPUpstream(t, func(r *http.Request, c *websocket.Conn, data []byte) {
		requests.Add(1)
		fields, err := responseObject(data)
		if err != nil {
			t.Error(err)
			return
		}
		if string(fields["model"]) != `"gpt-6-astra"` || string(fields["store"]) != "false" || fields["stream"] != nil || fields["temperature"] != nil || fields["max_output_tokens"] != nil {
			t.Errorf("invalid translated SDK request: %s", data)
		}
		instructions, _ := responseString(fields["instructions"])
		if instructions == "" && !strings.Contains(string(fields["input"]), "ERROR_") {
			t.Error("SDK instructions were lost")
		}
		if r.Header.Get("Authorization") != "Bearer token-pool" {
			t.Error("wrong upstream auth")
		}
		input := string(fields["input"])
		if strings.Contains(input, "ERROR_BEFORE") {
			sendHTTPEvents(t, c, `{"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"local context limit"}}`)
			return
		}
		if strings.Contains(input, "ERROR_AFTER") {
			sendHTTPEvents(t, c, httpCreatedEvent,
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg-test","role":"assistant","content":[]}}`,
				httpTextDelta,
				`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"local limit"}}`)
			return
		}
		sendHTTPEvents(t, c, strings.ReplaceAll(httpCreatedEvent, "pool-model", "gpt-6-astra"))
		if strings.Contains(input, "TITLE_AUXILIARY") || strings.Contains(input, "TOOL_RESULT") {
			text := "second answer"
			if strings.Contains(input, "TITLE_AUXILIARY") {
				text = `{"title":"Local title"}`
				if !strings.Contains(string(fields["text"]), "json_schema") {
					t.Error("structured output settings lost")
				}
			} else if !strings.Contains(input, "ORDINARY_TURN") || !strings.Contains(input, "encrypted-sdk") {
				t.Error("second turn lost stateless history")
			}
			item, _ := json.Marshal(map[string]any{"type": "message", "id": "msg-final", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}})
			// Output is available only through item events: exercise production
			// reconstruction with the SDK's non-streaming response parser.
			sendHTTPEvents(t, c, `{"type":"response.output_item.done","output_index":0,"item":`+string(item)+`}`, `{"type":"response.completed","response":{"id":"resp-test","model":"gpt-6-astra","status":"completed","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}}`)
			return
		}
		sendHTTPEvents(t, c,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs-sdk","summary":[]}}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs-sdk","output_index":0,"summary_index":0,"delta":"thinking"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs-sdk","encrypted_content":"encrypted-sdk","summary":[{"type":"summary_text","text":"thinking"}]}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg-sdk","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg-sdk","output_index":1,"content_index":0,"delta":"hello"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg-sdk","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}}`,
			`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc-sdk","call_id":"call-sdk","name":"lookup","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc-sdk","output_index":2,"delta":"{\"value\":\"local\"}"}`,
			`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc-sdk","call_id":"call-sdk","name":"lookup","status":"completed","arguments":"{\"value\":\"local\"}"}}`,
			httpCompletedEvent,
		)
	})
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("pool", 0)})
	srv.admission = newAdmissionGate(4)
	key, err := generateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.pool.store.addAPIKey(storedAPIKey{Name: "sdk", Secret: key, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	srv.lookupAPIKey = srv.pool.store.apiKeyName
	dir, home := t.TempDir(), t.TempDir()
	if err := os.Symlink(filepath.Join(sdk, "node_modules"), filepath.Join(dir, "node_modules")); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../../scripts/responses-sdk-smoke.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "smoke.mjs"), script, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", filepath.Join(dir, "smoke.mjs"))
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home, "XDG_CACHE_HOME=" + home, "CODEX_BALANCER_TEST_URL=" + proxy.URL + "/v1", "CODEX_BALANCER_TEST_KEY=" + key}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned SDK: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
	assertHTTPClean(t, srv)
	if requests.Load() != 5 {
		t.Fatalf("requests=%d", requests.Load())
	}
	usage, err := srv.pool.store.apiKeyUsage()
	if err != nil || usage["sdk"].TotalTokens != 42 {
		t.Fatalf("usage=%v err=%v", usage, err)
	}
}
