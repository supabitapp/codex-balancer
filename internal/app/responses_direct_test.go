package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPResponsesDirectJSON(t *testing.T) {
	const request = `{"model":"m","stream":false,"input":"hello","stream_options":{},"unknown":9007199254740993}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.Path != "/responses" || r.Header.Get("Upgrade") != "" || string(data) != request {
			t.Errorf("unexpected upstream request: %s %s %s", r.Method, r.URL.Path, data)
		}
		if r.Header.Get("X-Codex-Hop") != "" || r.Header.Get("Authorization") != "Bearer token-a" || r.Header.Get("Chatgpt-Account-Id") != "a" {
			t.Errorf("incorrect upstream headers: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "token-a")
		io.WriteString(w, `{"id":"r","object":"response","status":"completed","model":"m","output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5},"unknown":9007199254740993}`)
	}))
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(1)
	response := postResponse(t, proxy.URL, request, http.Header{"Connection": {"X-Codex-Hop"}, "X-Codex-Hop": {"private"}})
	body := readHTTPBody(t, response)
	if response.StatusCode != 200 || !strings.Contains(body, `"unknown":9007199254740993`) || response.Header.Get("X-Request-Id") != "[redacted]" {
		t.Fatalf("status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
	}
	assertHTTPClean(t, srv)
	if got := srv.stats.snapshot(); got.MonthlyUsage.TotalTokens != 5 || got.Turns != 1 || got.WSOpen != 0 {
		t.Fatalf("accounting=%+v", got)
	}
}

func TestHTTPResponsesCountsRequestsWithWebSocketOnlyFields(t *testing.T) {
	const payload = `{"model":"m","generate":false,"type":"response.create"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if string(data) != payload {
			t.Errorf("request changed: %s", data)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"r","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	}))
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(1)
	response := postResponse(t, proxy.URL, payload, nil)
	body := readHTTPBody(t, response)
	assertHTTPClean(t, srv)
	if got := srv.stats.snapshot(); response.StatusCode != 200 || got.Turns != 1 || got.MonthlyUsage.TotalTokens != 5 {
		t.Fatalf("status=%d turns=%d tokens=%d body=%s", response.StatusCode, got.Turns, got.MonthlyUsage.TotalTokens, body)
	}
}

func TestHTTPResponsesRecordsDecodedJSONUsageAfterInvalidation(t *testing.T) {
	account := testAccount("a", 0)
	srv := newTestServer(t, []*Account{account})
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	writer := httptest.NewRecorder()
	peer := &httpResponsesDownstream{writer: writer, controller: http.NewResponseController(writer), ctx: request.Context()}
	accounting := &responseAccounting{server: srv, request: request, ctx: request.Context(), account: &responseAccount{account: account}, liveThreads: map[string]struct{}{}}
	accounting.startTurn(websocketEnvelope{Model: "m"})
	body := &invalidateOnEOFReader{Reader: strings.NewReader(`{"id":"r","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`), invalidate: func() {
		account.mu.Lock()
		account.Paused = true
		account.mu.Unlock()
	}}
	err := srv.deliverHTTPJSON(peer, accounting, body)
	if got := srv.stats.snapshot(); !errors.Is(err, errHTTPAccountUnavailable) || got.MonthlyUsage.TotalTokens != 5 || got.Turns != 0 {
		t.Fatalf("error=%v tokens=%d turns=%d", err, got.MonthlyUsage.TotalTokens, got.Turns)
	}
}

type invalidateOnEOFReader struct {
	io.Reader
	invalidate func()
}

func (r *invalidateOnEOFReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		r.invalidate()
	}
	return n, err
}

func TestHTTPResponsesNeverRedispatches(t *testing.T) {
	for _, scenario := range []string{"rate", "server", "redirect", "too large", "disconnect", "invalidate"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int64
			var srv *server
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.Copy(io.Discard, r.Body)
				switch scenario {
				case "rate":
					w.WriteHeader(429)
					io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
				case "server":
					w.WriteHeader(503)
					io.WriteString(w, `{"error":{"code":"server_error"}}`)
				case "redirect":
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(307)
				case "too large":
					w.WriteHeader(413)
					io.WriteString(w, `{"error":{"code":"too_large","message":"large body"}}`)
				case "disconnect":
					conn, _, _ := http.NewResponseController(w).Hijack()
					conn.Close()
				case "invalidate":
					srv.invalidateAccount("a", routingReasonOwnerPaused)
					<-r.Context().Done()
				}
			}))
			defer upstream.Close()
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 20)})
			srv.admission = newAdmissionGate(1)
			response := postResponse(t, proxy.URL, `{"model":"m"}`, http.Header{"Session-Id": {"session"}})
			body := readHTTPBody(t, response)
			want := map[string]int{"rate": 503, "server": 503, "redirect": 502, "too large": 400, "disconnect": 502, "invalidate": 503}[scenario]
			if response.StatusCode != want || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, calls.Load(), body)
			}
			if scenario == "too large" && !strings.Contains(body, "request_too_large") {
				t.Fatal(body)
			}
			assertHTTPClean(t, srv)
		})
	}
}

func TestHTTPResponsesLargeDirectRequest(t *testing.T) {
	const size = 70 << 20
	payload := `{"model":"m","input":"` + strings.Repeat("x", size) + `"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Upgrade") != "" {
			t.Errorf("method=%s upgrade=%s", r.Method, r.Header.Get("Upgrade"))
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != payload {
			t.Errorf("large payload mismatch: length=%d error=%v", len(data), err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", httpCreatedEvent, httpCompletedEvent)
	}))
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(1)
	response := postResponseTimeout(t, proxy.URL, payload, nil, 90*time.Second)
	if body := readHTTPBody(t, response); response.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	assertHTTPClean(t, srv)
}

func TestResponseSSEFraming(t *testing.T) {
	payload := `{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 70<<10) + `"}`
	input := ": keepalive\r\nevent: response.created\r\ndata: {\r\ndata: \"type\":\"response.created\"}\r\n\r\ndata: " + payload + "\n\ndata: " + httpCompletedEvent + "\n\n"
	var events [][]byte
	err := readResponseSSE(&fragmentedResponseReader{data: []byte(input)}, func(data []byte) error {
		if !json.Valid(data) {
			t.Fatalf("invalid event: %s", data)
		}
		events = append(events, bytes.Clone(data))
		if bytes.Contains(data, []byte("response.completed")) {
			return errResponseFinished
		}
		return nil
	})
	if !errors.Is(err, errResponseFinished) || len(events) != 3 || string(events[1]) != payload {
		t.Fatalf("events=%d error=%v", len(events), err)
	}
	if err := readResponseSSE(strings.NewReader("data: [DONE]\n\n"), func([]byte) error { t.Fatal("delivered DONE as JSON"); return nil }); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
}

type fragmentedResponseReader struct{ data []byte }

func (r *fragmentedResponseReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), 13)], r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestHTTPResponsesIdleCancellation(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	reader, writer := io.Pipe()
	defer writer.Close()
	timer := time.AfterFunc(10*time.Millisecond, func() { cancel(errHTTPUpstreamTimeout); reader.Close() })
	defer timer.Stop()
	err := readResponseSSE(&httpActivityReader{ReadCloser: reader, timer: timer}, func([]byte) error { return nil })
	if failure := httpTransportFailure(ctx, err); failure.Status != 504 || failure.Code != "upstream_timeout" {
		t.Fatalf("failure=%+v", failure)
	}
}

func TestHTTPResponsesNormalStreamDoesNotLogRejection(t *testing.T) {
	upstream := newHTTPUpstream(t, func(_ *http.Request, stream *testResponseStream, _ []byte) {
		sendHTTPEvents(t, stream, httpCreatedEvent, httpTextDelta, httpCompletedEvent)
	})
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(1)
	logs := captureTestLogs(srv)
	response := postResponse(t, proxy.URL, `{"model":"m","stream":true}`, nil)
	readHTTPBody(t, response)
	assertHTTPClean(t, srv)
	if strings.Contains(logs.String(), "account rejected") || response.StatusCode != 200 {
		t.Fatalf("status=%d logs=%s", response.StatusCode, logs.String())
	}
}

func TestHTTPResponsesSSEWithoutContentType(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header()["Content-Type"] = nil
		fmt.Fprintf(w, "event: response.created\ndata: %s\n\nevent: response.completed\ndata: %s\n\n", httpCreatedEvent, httpCompletedEvent)
	}))
	defer upstream.Close()
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
			srv.admission = newAdmissionGate(1)
			response := postResponse(t, proxy.URL, fmt.Sprintf(`{"model":"m","stream":%t}`, stream), nil)
			body := readHTTPBody(t, response)
			assertHTTPClean(t, srv)
			if response.StatusCode != 200 || !strings.Contains(body, "completed") || srv.stats.snapshot().MonthlyUsage.TotalTokens != 14 {
				t.Fatalf("status=%d body=%s", response.StatusCode, body)
			}
		})
	}
}

func TestHTTPResponsesRateLimitReleasesUnacceptedOwner(t *testing.T) {
	var attempts atomic.Int64
	upstream := newHTTPUpstream(t, func(r *http.Request, stream *testResponseStream, _ []byte) {
		if attempts.Add(1) == 1 {
			sendHTTPEvents(t, stream, `{"type":"error","status":429,"error":{"code":"rate_limit_exceeded"}}`)
			return
		}
		if r.Header.Get("Chatgpt-Account-Id") != "b" {
			t.Error("retry did not choose available account")
		}
		sendHTTPEvents(t, stream, httpCreatedEvent, httpCompletedEvent)
	})
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 20)})
	srv.admission = newAdmissionGate(1)
	for _, status := range []int{503, 200} {
		response := postResponse(t, proxy.URL, `{"model":"m"}`, http.Header{"Session-Id": {"same"}})
		body := readHTTPBody(t, response)
		assertHTTPClean(t, srv)
		if response.StatusCode != status {
			t.Fatalf("status=%d want=%d body=%s", response.StatusCode, status, body)
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d", attempts.Load())
	}
}

func TestHTTPResponsesStaleUnauthorizedDoesNotRefreshAgain(t *testing.T) {
	refreshes := useOAuthRefreshServer(t)
	started := make(chan int, 2)
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		index := int(attempts.Add(1)) - 1
		if index >= 2 {
			t.Error("inference replayed")
			return
		}
		if r.Header.Get("Authorization") != "Bearer token-a" {
			t.Error("request did not capture initial token")
		}
		started <- index
		<-release[index]
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"code":"unauthorized"}}`)
	}))
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(2)
	completed := make(chan struct{}, 2)
	for range 2 {
		go func() {
			response := postResponse(t, proxy.URL, `{"model":"m"}`, nil)
			readHTTPBody(t, response)
			if response.StatusCode != 503 {
				t.Errorf("status=%d", response.StatusCode)
			}
			completed <- struct{}{}
		}()
	}
	<-started
	<-started
	close(release[0])
	<-completed
	close(release[1])
	<-completed
	assertHTTPClean(t, srv)
	if refreshes() != 1 || attempts.Load() != 2 {
		t.Fatalf("refreshes=%d attempts=%d", refreshes(), attempts.Load())
	}
}

func TestHTTPResponsesRejectionTelemetryAfterDispatch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(503)
		io.WriteString(w, `{"error":{"code":"server_error"}}`)
	}))
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(1)
	logs, _, _ := observeTestServer(t, srv)
	response := postResponse(t, proxy.URL, `{"model":"m"}`, nil)
	readHTTPBody(t, response)
	assertHTTPClean(t, srv)
	found := false
	for _, record := range observationRecords(t, logs) {
		if record["stage"] == "setup_failed" {
			t.Fatal("post-dispatch failure reported as setup failure")
		}
		if record["stage"] == "upstream_rejected" && record["inference_sent"] == true {
			found = true
		}
	}
	if !found || response.Header.Get("Retry-After") != "17" {
		t.Fatalf("found=%t headers=%v", found, response.Header)
	}
}

func TestHTTPResponsesInvalidOutputClassification(t *testing.T) {
	upstream := newHTTPUpstream(t, func(_ *http.Request, stream *testResponseStream, _ []byte) {
		sendHTTPEvents(t, stream, `{"type":"response.completed","response":{"status":"failed"}}`)
	})
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(1)
	logs, _, _ := observeTestServer(t, srv)
	response := postResponse(t, proxy.URL, `{"model":"m"}`, nil)
	body := readHTTPBody(t, response)
	assertHTTPClean(t, srv)
	if response.StatusCode != 502 || !strings.Contains(body, "invalid_upstream_response") || !strings.Contains(logs.String(), `"stage":"upstream_failed"`) {
		t.Fatalf("status=%d body=%s logs=%s", response.StatusCode, body, logs.String())
	}
}
