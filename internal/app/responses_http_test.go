package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const httpCreatedEvent = `{"type":"response.created","response":{"id":"resp-test","object":"response","created_at":1,"model":"pool-model","status":"in_progress","output":[]}}`
const httpCompletedEvent = `{"type":"response.completed","response":{"id":"resp-test","object":"response","created_at":1,"model":"pool-model","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}}`
const httpTextDelta = `{"type":"response.output_text.delta","item_id":"msg-test","output_index":0,"content_index":0,"delta":"hello"}`

func newHTTPUpstream(t *testing.T, respond func(*http.Request, *websocket.Conn, []byte)) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(maxHTTPResponseBody + 1024)
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return // canceled or invalidated setup must not send inference
		}
		respond(r, conn, data)
		conn.Read(r.Context()) // deliberately stay open after a terminal event
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func sendHTTPEvents(t *testing.T, conn *websocket.Conn, events ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, event := range events {
		if err := conn.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
			t.Error(err)
			return
		}
	}
}

func postResponse(t *testing.T, url, body string, headers http.Header) *http.Response {
	t.Helper()
	return postResponseTimeout(t, url, body, headers, 5*time.Second)
}

func postResponseTimeout(t *testing.T, url, body string, headers http.Header, timeout time.Duration) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readHTTPBody(t *testing.T, response *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertHTTPClean(t *testing.T, srv *server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.admission.wait(ctx); err != nil {
		t.Fatal("admission was not released:", err)
	}
	srv.routeClaims.mu.Lock()
	claims := len(srv.routeClaims.byID)
	srv.routeClaims.mu.Unlock()
	srv.activeWebSockets.mu.Lock()
	active := len(srv.activeWebSockets.byAccount)
	srv.activeWebSockets.mu.Unlock()
	snapshot := srv.stats.snapshot()
	if claims != 0 || active != 0 || snapshot.WSOpen != 0 || len(snapshot.Threads) != 0 {
		t.Fatalf("leaked claims=%d active=%d sockets=%d threads=%d", claims, active, snapshot.WSOpen, len(snapshot.Threads))
	}
}

func TestHTTPResponsesRequestGuards(t *testing.T) {
	var connections atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connections.Add(1) // count attempted handshakes, not just inference frames
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("pool", 0)})
	srv.admission = newAdmissionGate(1)
	tests := []struct {
		name, body, encoding, contentType string
		status                            int
	}{
		{"empty", "", "", "", 400}, {"null", "null", "", "", 400},
		{"array", "[]", "", "", 400}, {"invalid", "{", "", "", 400},
		{"invalid UTF-8", "{\"model\":\"m\",\"input\":\"\xff\"}", "", "", 400},
		{"trailing", `{"model":"m"} {}`, "", "", 400},
		{"model missing", `{}`, "", "", 400}, {"model blank", `{"model":" "}`, "", "", 400},
		{"model type", `{"model":5}`, "", "", 400}, {"model null", `{"model":null}`, "", "", 400},
		{"input type", `{"model":"m","input":4}`, "", "", 400},
		{"input null", `{"model":"m","input":null}`, "", "", 400},
		{"item type", `{"model":"m","input":["hi"]}`, "", "", 400},
		{"stream type", `{"model":"m","stream":"true"}`, "", "", 400},
		{"stream null", `{"model":"m","stream":null}`, "", "", 400},
		{"instructions type", `{"model":"m","instructions":[]}`, "", "", 400},
		{"duplicate", `{"model":"m","model":"n"}`, "", "", 400},
		{"case collision", `{"model":"m","Model":"n"}`, "", "", 400},
		{"store", `{"model":"m","store":true}`, "", "", 400},
		{"store type", `{"model":"m","store":"false"}`, "", "", 400},
		{"background", `{"model":"m","background":true}`, "", "", 400},
		{"background type", `{"model":"m","background":null}`, "", "", 400},
		{"previous", `{"model":"m","previous_response_id":"r"}`, "", "", 400},
		{"conversation", `{"model":"m","conversation":{"id":"c"}}`, "", "", 400},
		{"reference", `{"model":"m","input":[{"type":"item_reference","id":"i"}]}`, "", "", 400},
		{"implicit reference", `{"model":"m","input":[{"id":"i"}]}`, "", "", 400},
		{"type", `{"model":"m","type":"response.cancel"}`, "", "", 400},
		{"generate", `{"model":"m","generate":false}`, "", "", 400},
		{"unsupported stream controls", `{"model":"m","stream_options":{}}`, "", "", 400},
		{"max type", `{"model":"m","max_output_tokens":"4"}`, "", "", 400},
		{"max fractional", `{"model":"m","max_output_tokens":4.2}`, "", "", 400},
		{"temperature range", `{"model":"m","temperature":-1}`, "", "", 400},
		{"top p range", `{"model":"m","top_p":2}`, "", "", 400},
		{"instruction image", `{"model":"m","input":[{"role":"system","content":[{"type":"input_image","image_url":"data:..."}]}]}`, "", "", 400},
		{"instruction extra semantics", `{"model":"m","input":[{"role":"system","content":[{"type":"input_text","text":"hi","prompt_cache_breakpoint":{}}]}]}`, "", "", 400},
		{"metadata type", `{"model":"m","client_metadata":{"x":{}}}`, "", "", 400},
		{"gzip", `{"model":"m"}`, "gzip", "", 415},
		{"media type", `{"model":"m"}`, "", "text/plain", 415},
		{"size", strings.Repeat(" ", maxHTTPResponseBody+1), "", "", 413},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp := postResponse(t, proxy.URL, test.body, http.Header{"Content-Encoding": {test.encoding}, "Content-Type": {test.contentType}})
			body := readHTTPBody(t, resp)
			if resp.StatusCode != test.status || !strings.Contains(body, `"error"`) {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			assertHTTPClean(t, srv)
		})
	}
	if connections.Load() != 0 {
		t.Fatal("rejected requests opened inference connections")
	}
}

func TestHTTPResponsesAuthenticationAndAdmission(t *testing.T) {
	var connections atomic.Int64
	upstream := newHTTPUpstream(t, func(r *http.Request, conn *websocket.Conn, _ []byte) {
		connections.Add(1)
		if r.Header.Get("Chatgpt-Account-Id") != "pool" || r.Header.Get("Authorization") != "Bearer token-pool" {
			t.Errorf("client identity affected upstream credentials: %v", r.Header)
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Secret") != "" {
			t.Error("unvetted client headers reached upstream")
		}
		sendHTTPEvents(t, conn, httpCreatedEvent, httpCompletedEvent)
	})
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("pool", 0)})
	srv.admission = newAdmissionGate(1)
	key, err := generateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string]string{"jwt": key, "legacy": "cb_legacy", "uuid": "old-opaque"} {
		if err := srv.pool.store.addAPIKey(storedAPIKey{Name: name, Secret: secret, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	srv.lookupAPIKey = srv.pool.store.apiKeyName
	for _, secret := range []string{key, "cb_legacy", "old-opaque", "", "wrong", key + "forged"} {
		resp := postResponse(t, proxy.URL, `{"model":"pool-model","input":"hello"}`, http.Header{
			"Authorization": {"Bearer " + secret}, "Chatgpt-Account-Id": {"not-pool"}, "Cookie": {"private"}, "X-Secret": {"private"},
		})
		readHTTPBody(t, resp)
		want := 401
		if secret == key || secret == "cb_legacy" || secret == "old-opaque" {
			want = 200
		}
		if resp.StatusCode != want {
			t.Fatalf("status=%d want=%d", resp.StatusCode, want)
		}
		assertHTTPClean(t, srv)
	}
	if _, err := srv.pool.store.revokeAPIKey("jwt", time.Now()); err != nil {
		t.Fatal(err)
	}
	resp := postResponse(t, proxy.URL, `{"model":"m"}`, http.Header{"Authorization": {"Bearer " + key}})
	readHTTPBody(t, resp)
	if resp.StatusCode != 401 {
		t.Fatalf("revoked status=%d", resp.StatusCode)
	}
	srv.lookupAPIKey = nil
	resp = postResponse(t, proxy.URL, `{"model":"pool-model"}`, nil)
	readHTTPBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatal("no-auth failed")
	}
	assertHTTPClean(t, srv)
	if !srv.admission.acquire() {
		t.Fatal("acquire")
	}
	resp = postResponse(t, proxy.URL, `{"model":"m"}`, nil)
	readHTTPBody(t, resp)
	srv.admission.release()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "1" {
		t.Fatal("capacity rejection failed")
	}
	srv.admission.beginDrain()
	resp = postResponse(t, proxy.URL, `{"model":"m"}`, nil)
	readHTTPBody(t, resp)
	if resp.StatusCode != 503 {
		t.Fatal("drain admitted a request")
	}
	if connections.Load() != 4 {
		t.Fatalf("connections=%d", connections.Load())
	}
	usage, err := srv.pool.store.apiKeyUsage()
	if err != nil || usage["jwt"].TotalTokens != 14 || usage["legacy"].TotalTokens != 14 || usage["uuid"].TotalTokens != 14 {
		t.Fatalf("usage=%v err=%v", usage, err)
	}
}

func TestHTTPResponsesLosslessTranslation(t *testing.T) {
	const body = `{"model":"exact-model","stream":false,"store":false,"background":false,"previous_response_id":null,"instructions":"explicit","input":[{"role":"system","content":"first"},{"type":"message","role":"developer","content":[{"type":"input_text","text":"second"},{"type":"input_text","text":"third"}]},{"role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]},{"role":"developer","content":"later"},{"type":"function_call","call_id":"call","name":"run","arguments":"{}"},{"type":"function_call_output","call_id":"call","output":"result"},{"type":"custom_tool_call","call_id":"custom","name":"patch","input":"patch text"},{"type":"custom_tool_call_output","call_id":"custom","output":"done"},{"type":"reasoning","id":"rs","encrypted_content":"opaque","summary":[]}],"tools":[{"type":"function","name":"run","parameters":{"type":"object"}},{"type":"custom","name":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}],"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}},"verbosity":"low"},"reasoning":{"effort":"high","summary":"auto"},"include":["reasoning.encrypted_content"],"service_tier":"priority","prompt_cache_key":"cache","client_metadata":{"trace":"client"},"metadata":{"number":"9007199254740993"},"unknown":{"number":9007199254740993,"decimal":0.123456789012345678901},"temperature":0.3,"top_p":0.9,"max_output_tokens":32768}`
	captured := make(chan []byte, 1)
	upstream := newHTTPUpstream(t, func(_ *http.Request, conn *websocket.Conn, data []byte) {
		captured <- data
		sendHTTPEvents(t, conn, httpCreatedEvent, httpCompletedEvent)
	})
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(1)
	resp := postResponse(t, proxy.URL, body, nil)
	if got := readHTTPBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("status=%d %s", resp.StatusCode, got)
	}
	before, _ := responseObject([]byte(body))
	after, _ := responseObject(<-captured)
	if got, _ := responseString(after["instructions"]); got != "explicit\n\nfirst\n\nsecond\n\nthird" {
		t.Fatalf("instructions=%q", got)
	}
	var original, translated []json.RawMessage
	json.Unmarshal(before["input"], &original)
	json.Unmarshal(after["input"], &translated)
	if !reflect.DeepEqual(original[2:], translated) {
		t.Fatalf("input changed: %s", after["input"])
	}
	for _, key := range []string{"model", "tools", "text", "reasoning", "include", "service_tier", "prompt_cache_key", "client_metadata", "metadata", "unknown"} {
		if string(before[key]) != string(after[key]) {
			t.Errorf("%s changed: %s", key, after[key])
		}
	}
	for _, key := range []string{"stream", "background", "previous_response_id", "temperature", "top_p", "max_output_tokens"} {
		if after[key] != nil {
			t.Errorf("HTTP-only field %s forwarded", key)
		}
	}
	if string(after["type"]) != `"response.create"` || string(after["store"]) != "false" {
		t.Fatal("invalid upstream controls")
	}
	assertHTTPClean(t, srv)
}

func TestHTTPResponsesStringAndEmptyInput(t *testing.T) {
	for _, body := range []string{`{"model":"m","input":"hello"}`, `{"model":"m","input":[]}`, `{"model":"m","instructions":"only"}`, `{"model":"m"}`} {
		data, stream, err := translateHTTPResponse([]byte(body))
		if err != nil || stream {
			t.Fatalf("translate: %s %v", data, err)
		}
		fields, _ := responseObject(data)
		if _, ok := responseString(fields["instructions"]); !ok {
			t.Fatal("no instructions string")
		}
		if fields["input"] == nil {
			t.Fatal("no input array")
		}
		if strings.Contains(body, "hello") && string(fields["input"]) != `[{"content":[{"text":"hello","type":"input_text"}],"role":"user"}]` {
			t.Fatalf("string input=%s", fields["input"])
		}
	}
}

func TestHTTPResponsesSSEIsIncrementalAndTerminal(t *testing.T) {
	finish := make(chan struct{})
	upstream := newHTTPUpstream(t, func(_ *http.Request, conn *websocket.Conn, _ []byte) {
		sendHTTPEvents(t, conn, httpCreatedEvent, httpTextDelta)
		<-finish
		sendHTTPEvents(t, conn,
			`{"type":"response.function_call_arguments.delta","item_id":"tool","output_index":1,"delta":"{}"}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs","output_index":2,"summary_index":0,"delta":"thought"}`,
			`{"type":"response.future:event","identifier":"kept","index":9007199254740993,"headers":{"authorization":"hidden"}}`,
			httpCompletedEvent,
		)
	})
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(1)
	resp := postResponse(t, proxy.URL, `{"model":"pool-model","stream":true,"input":"hi"}`, nil)
	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers=%v", resp.Header)
	}
	scanner := bufio.NewScanner(resp.Body)
	events := []string{}
	for scanner.Scan() {
		line := scanner.Text()
		events = append(events, line)
		if strings.Contains(line, `"delta":"hello"`) {
			break
		}
	}
	if scanner.Err() != nil || !strings.Contains(strings.Join(events, "\n"), `"delta":"hello"`) {
		close(finish)
		t.Fatal("delta not delivered before completion")
	}
	close(finish)
	for scanner.Scan() {
		events = append(events, scanner.Text())
	}
	if scanner.Err() != nil {
		t.Fatal(scanner.Err())
	}
	text := strings.Join(events, "\n")
	for _, want := range []string{"event: response.output_text.delta", "event: response.function_call_arguments.delta", "event: response.reasoning_summary_text.delta", "response.future:event", "9007199254740993"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Count(text, "data: [DONE]") != 1 || strings.Count(text, "event: response.completed") != 1 || strings.Contains(text, "hidden") {
		t.Fatalf("stream=%s", text)
	}
	assertHTTPClean(t, srv)
	if usage := srv.stats.snapshot().MonthlyUsage.TotalTokens; usage != 14 {
		t.Fatalf("usage=%d", usage)
	}
}

func TestHTTPResponsesJSONOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		events []string
		want   string
		status int
	}{
		{"terminal authoritative", []string{`{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[]}]}],"extra":9007199254740993,"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`}, `"extra":9007199254740993`, 200},
		{"reconstruct tools by index", []string{
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc","call_id":"c","name":"run","arguments":"{}"}}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs","encrypted_content":"opaque","summary":[]}}`,
			`{"type":"response.completed","response":{"output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		}, `"output":[{"type":"reasoning"`, 200},
		{"empty", []string{httpCompletedEvent}, `"output":[]`, 200},
		{"incomplete", []string{`{"type":"response.incomplete","response":{"id":"r","status":"incomplete","output":[{"type":"custom_tool_call","call_id":"c","name":"patch","input":"partial"}],"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`}, `"status":"incomplete"`, 200},
		{"legacy done", []string{strings.Replace(httpCompletedEvent, "response.completed", "response.done", 1)}, `"status":"completed"`, 200},
		{"unfinished item", []string{`{"type":"response.output_item.added","output_index":0,"item":{"id":"m","type":"message","content":[]}}`, `{"type":"response.completed","response":{"id":"r"}}`}, "invalid_upstream_output", 502},
		{"unassembled delta", []string{httpTextDelta, `{"type":"response.completed","response":{"id":"r"}}`}, "invalid_upstream_output", 502},
		{"invalid index", []string{`{"type":"response.output_item.done","output_index":-1,"item":{}}`}, "invalid_upstream_output", 502},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := newHTTPUpstream(t, func(_ *http.Request, c *websocket.Conn, _ []byte) {
				sendHTTPEvents(t, c, append([]string{httpCreatedEvent}, test.events...)...)
			})
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
			srv.admission = newAdmissionGate(1)
			resp := postResponse(t, proxy.URL, `{"model":"pool-model"}`, nil)
			body := readHTTPBody(t, resp)
			if resp.StatusCode != test.status || !strings.Contains(body, test.want) || resp.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			if strings.Contains(body, `"type":"response.completed"`) {
				t.Fatal("returned event wrapper")
			}
			assertHTTPClean(t, srv)
			if test.name == "incomplete" && srv.stats.snapshot().MonthlyUsage.TotalTokens != 3 {
				t.Fatal("lost incomplete usage")
			}
		})
	}
}

func TestHTTPResponsesInBandErrors(t *testing.T) {
	for _, test := range []struct {
		code   string
		status int
		kind   string
	}{
		{"context_length_exceeded", 400, "response.failed"}, {"model_not_found", 404, "error"},
		{"rate_limit_exceeded", 429, "error"}, {"usage_limit_reached", 429, "error"},
		{"server_is_overloaded", 503, "response.failed"}, {"slow_down", 503, "error"},
		{"websocket_connection_limit_reached", 503, "error"},
	} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/committed_%t", test.code, committed), func(t *testing.T) {
				var requests atomic.Int64
				upstream := newHTTPUpstream(t, func(_ *http.Request, c *websocket.Conn, _ []byte) {
					requests.Add(1)
					if committed {
						sendHTTPEvents(t, c, httpCreatedEvent, httpTextDelta)
					}
					event := fmt.Sprintf(`{"type":"error","error":{"code":%q,"message":"failure"},"headers":{"authorization":"private","retry-after":"20"}}`, test.code)
					if test.kind == "response.failed" {
						event = fmt.Sprintf(`{"type":"response.failed","response":{"status":"failed","error":{"code":%q,"message":"failure"},"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`, test.code)
					}
					sendHTTPEvents(t, c, event)
				})
				a, b := testAccount("a", 0), testAccount("b", 20)
				srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
				srv.admission = newAdmissionGate(1)
				resp := postResponse(t, proxy.URL, `{"model":"pool-model","stream":true}`, http.Header{"Session-Id": {"session"}})
				body := readHTTPBody(t, resp)
				want := test.status
				if committed {
					want = 200
				}
				if resp.StatusCode != want || !strings.Contains(body, test.code) || strings.Contains(body, "private") {
					t.Fatalf("status=%d body=%s", resp.StatusCode, body)
				}
				if committed && (strings.Count(body, "data: [DONE]") != 1 || strings.Contains(body, "event: response.completed")) {
					t.Fatalf("false/duplicate terminal: %s", body)
				}
				assertHTTPClean(t, srv)
				if requests.Load() != 1 {
					t.Fatalf("silently replayed %d requests", requests.Load())
				}
				if test.code == "websocket_connection_limit_reached" || test.code == "server_is_overloaded" || test.code == "slow_down" {
					if !a.routingCandidate().cooldown.IsZero() || a.routingCandidate().spent {
						t.Fatal("connection/capacity failure penalized account")
					}
				}
				if test.code == "usage_limit_reached" && !a.routingCandidate().spent {
					t.Fatal("usage failure not applied")
				}
				if test.code == "server_is_overloaded" || test.code == "slow_down" || test.code == "usage_limit_reached" {
					owners, err := srv.pool.store.routeOwners("", "session")
					if err != nil || fmt.Sprint(owners) != "[a]" {
						t.Fatalf("owner barrier=%v err=%v", owners, err)
					}
				}
			})
		}
	}
}

func TestHTTPResponsesTransportFailures(t *testing.T) {
	for _, scenario := range []string{"malformed", "binary", "eof", "terminal missing response", "conflicting status"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", scenario, stream), func(t *testing.T) {
				upstream := newHTTPUpstream(t, func(_ *http.Request, c *websocket.Conn, _ []byte) {
					if stream {
						sendHTTPEvents(t, c, httpCreatedEvent, httpTextDelta)
					}
					switch scenario {
					case "malformed":
						sendHTTPEvents(t, c, `{`)
					case "binary":
						c.Write(context.Background(), websocket.MessageBinary, []byte(`{}`))
					case "eof":
						c.CloseNow()
					case "terminal missing response":
						sendHTTPEvents(t, c, `{"type":"response.completed"}`)
					case "conflicting status":
						sendHTTPEvents(t, c, `{"type":"response.completed","response":{"status":"failed"}}`)
					}
				})
				srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
				srv.admission = newAdmissionGate(1)
				resp := postResponse(t, proxy.URL, fmt.Sprintf(`{"model":"m","stream":%t}`, stream), nil)
				body := readHTTPBody(t, resp)
				want := 502
				if stream {
					want = 200
				}
				if resp.StatusCode != want || !strings.Contains(body, `"error"`) || strings.Contains(body, "event: response.completed") {
					t.Fatalf("status=%d body=%s", resp.StatusCode, body)
				}
				assertHTTPClean(t, srv)
			})
		}
	}
}
