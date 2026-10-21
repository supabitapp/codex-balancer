package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexAppServerSearchWithoutModelMetadata(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	extract := func(source, before, after string) string {
		t.Helper()
		_, config, found := strings.Cut(source, before)
		if !found {
			t.Fatal("Codex setup is missing")
		}
		config, _, found = strings.Cut(config, after)
		if !found {
			t.Fatal("Codex setup has no end")
		}
		return strings.ReplaceAll(strings.TrimSpace(config), "\n  ", "\n")
	}
	setup := extract(string(readme), "```toml\n", "\n```")
	help := extract(serverHelp, "Merge these settings into ~/.codex/config.toml, keeping existing table entries:\n\n", "\n\nProvision")
	if setup != help {
		t.Fatalf("README and CLI help configure Codex differently:\nREADME:\n%s\nCLI help:\n%s", setup, help)
	}
	if os.Getenv("CODEX_BALANCER_TEST_CODEX") == "" {
		t.Skip("set CODEX_BALANCER_TEST_CODEX to run the app-server integration test")
	}
	for _, websockets := range []bool{false, true} {
		for _, mode := range []string{"live", "cached"} {
			t.Run(fmt.Sprintf("websockets=%t/%s", websockets, mode), func(t *testing.T) {
				testCodexSearchWithoutModelMetadata(t, setup, websockets, mode)
			})
		}
	}
}

func testCodexSearchWithoutModelMetadata(t *testing.T, setup string, websockets bool, mode string) {
	t.Helper()
	var searches, modelRequests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			modelRequests.Add(1)
			http.Error(w, "model discovery unavailable", http.StatusServiceUnavailable)
			return
		case "/alpha/search":
			if r.Header.Get("Authorization") != "Bearer token-a" || r.Header.Get("Chatgpt-Account-Id") != "a" {
				t.Error("standalone search did not use pool credentials")
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || !strings.Contains(string(body), "balancer regression") {
				t.Errorf("search body=%s error=%v", body, err)
			}
			searches.Add(1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"output":"BALANCER_SEARCH_RESULT","results":[]}`)
			return
		case "/responses":
			if websockets != (r.Method == http.MethodGet) {
				t.Errorf("generation transport: websockets=%t method=%s", websockets, r.Method)
			}
		default:
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		stream, err := acceptResponseTestStream(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		stream.SetReadLimit(maxWebSocketMessage)
		defer func() {
			if stream.conn != nil {
				stream.conn.CloseNow()
			}
		}()
		for {
			_, body, err := stream.Read(r.Context())
			if err != nil {
				return
			}
			var request struct {
				Generate *bool `json:"generate"`
				Tools    []struct {
					Type string `json:"type"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Error(err)
				return
			}
			for _, tool := range request.Tools {
				if tool.Type == "web_search" {
					sendHTTPEvents(t, stream, `{"type":"error","status":400,"error":{"message":"Hosted tool 'web_search' requires authorization and metering that are not supported by rustponsesapi.","type":"invalid_request_error","param":"tools","code":"unsupported_parameter"}}`)
					return
				}
			}
			if request.Generate != nil && !*request.Generate || strings.Contains(string(body), "BALANCER_SEARCH_RESULT") {
				sendHTTPEvents(t, stream, httpCreatedEvent, httpCompletedEvent)
			} else {
				sendHTTPEvents(t, stream, httpCreatedEvent,
					`{"type":"response.output_item.done","item":{"type":"function_call","id":"search-1","call_id":"search-1","name":"run","namespace":"web","arguments":"{\"search_query\":[{\"q\":\"balancer regression\"}]}"}}`,
					httpCompletedEvent)
			}
			if stream.conn == nil {
				return
			}
		}
	}))
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	key, err := generateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.pool.store.addAPIKey(storedAPIKey{Name: "codex", Secret: key, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	srv.lookupAPIKey = srv.pool.store.apiKeyName
	t.Setenv("CODEX_BALANCER_API_KEY", key)
	turn, events := runCodexAppServerTurn(t, setup, "search for balancer regression",
		`model="gpt-metadata-test"`,
		fmt.Sprintf("web_search=%q", mode),
		fmt.Sprintf("model_providers.balancer.base_url=%q", proxy.URL+"/v1"),
		fmt.Sprintf("model_providers.balancer.model_catalog_url=%q", proxy.URL+"/v1/models"),
		"model_providers.balancer.requires_openai_auth=false",
		fmt.Sprintf("model_providers.balancer.supports_websockets=%t", websockets),
		"model_providers.balancer.request_max_retries=0")
	if turn["status"] != "completed" || searches.Load() != 1 || modelRequests.Load() == 0 {
		t.Fatalf("searches=%d model requests=%d turn=%v events=%s", searches.Load(), modelRequests.Load(), turn, events)
	}
	if !strings.Contains(events, "Defaulting to fallback metadata") {
		t.Fatalf("test did not exercise missing model metadata: %s", events)
	}
}
