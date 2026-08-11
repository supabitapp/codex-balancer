package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestDecodeRequestBodyRejectsInvalidEncoding(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		body     []byte
	}{
		{name: "malformed zstd", encoding: "zstd", body: []byte("not-zstd")},
		{name: "unsupported encoding", encoding: "gzip", body: []byte("body")},
		{name: "stacked encoding", encoding: "zstd, gzip", body: []byte("body")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{"Content-Encoding": {test.encoding}}
			if _, err := decodeRequestBody(headers, test.body); err == nil {
				t.Fatal("decode succeeded")
			}
		})
	}
}

func TestDecodeRequestBodyAcceptsCaseInsensitiveZstd(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	want := []byte(`{"input":[]}`)
	encoded := encoder.EncodeAll(want, nil)
	got, err := decodeRequestBody(http.Header{"Content-Encoding": {" ZSTD "}}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded body = %q, want %q", got, want)
	}
}

func TestDecodeRequestBodyRejectsExpandedBodyOverLimit(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	encoded := encoder.EncodeAll(bytes.Repeat([]byte{'x'}, maxRequestBody+1), nil)
	if _, err := decodeRequestBody(http.Header{"Content-Encoding": {"zstd"}}, encoded); err == nil {
		t.Fatal("oversized body decoded")
	}
}

func TestResponsesDistinguishesReadFailureFromOversize(t *testing.T) {
	t.Run("read failure", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		request.Body = failingRequestBody{}
		response := httptest.NewRecorder()
		new(server).responses(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		request.Body = &repeatedRequestBody{remaining: maxRequestBody + 1}
		response := httptest.NewRecorder()
		new(server).responses(response, request)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
		}
	})
}

func TestHTTPResponseEndingBeforeBodyRetriesAnotherAccount(t *testing.T) {
	accounts := []*Account{testAccount("account-a", 10), testAccount("account-b", 20)}
	var mu sync.Mutex
	var calls []string
	server, _, closeServer := newAffinityHTTPServer(t, accounts, func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("chatgpt-account-id")
		mu.Lock()
		calls = append(calls, id)
		mu.Unlock()
		if id == "account-a" {
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(http.StatusOK)
			return
		}
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "resp_b") {
		t.Fatalf("body = %q", response.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(calls) != "[account-a account-b]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPHardResponseEndingBeforeBodyFailsClosed(t *testing.T) {
	accounts := []*Account{testAccount("account-a", 10), testAccount("account-b", 20)}
	server, store, closeServer := newAffinityHTTPServer(t, accounts, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinityTurnState, value: "turn"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "turn", `{"input":[]}`)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadGateway, response.Body.String())
	}
}

func TestHTTPResponseFailureAfterBodyDoesNotReplay(t *testing.T) {
	accounts := []*Account{testAccount("account-a", 10), testAccount("account-b", 20)}
	calls := 0
	server, _, closeServer := newAffinityHTTPServer(t, accounts, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_a\"}}\n\n"
		w.Header().Set("Content-Length", fmt.Sprint(len(body)+10))
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body)
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestHTTPEmptyClientErrorDoesNotReplay(t *testing.T) {
	accounts := []*Account{testAccount("account-a", 10), testAccount("account-b", 20)}
	calls := 0
	server, _, closeServer := newAffinityHTTPServer(t, accounts, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestHTTPUpstreamCompressionStillRegistersResponseOwner(t *testing.T) {
	account := testAccount("account-a", 10)
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); got != "gzip" {
			t.Errorf("Accept-Encoding = %q, want gzip", got)
		}
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		writeResponseCreated(writerResponse{Writer: writer, header: w.Header()}, "resp_gzip")
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	})
	defer closeServer()

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":[]}`))
	request.Header.Set("Accept-Encoding", "zstd")
	response := httptest.NewRecorder()
	server.responses(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_gzip"}); got != account.id() {
		t.Fatalf("response owner = %q, want %q", got, account.id())
	}
}

func TestResponseOwnerInspectorBoundsMalformedEvents(t *testing.T) {
	store, err := newAffinityStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	inspector := responseOwnerInspector{
		store:   store,
		stats:   newStats(),
		account: "account-a",
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	chunk := bytes.Repeat([]byte{'x'}, 32<<10)
	for range maxResponseEventLine/(32<<10) + 2 {
		inspector.write(chunk)
	}
	inspector.write([]byte("\n\n"))
	inspector.write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_after_large\"}}\n\n"))
	inspector.finish()

	if len(inspector.buffer) > maxResponseEventLine || len(inspector.event) > maxResponseEventLine {
		t.Fatalf("inspector retained %d buffer bytes and %d event bytes", len(inspector.buffer), len(inspector.event))
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_after_large"}); got != "account-a" {
		t.Fatalf("response owner = %q, want account-a", got)
	}

	for range maxResponseEventLine/(32<<10) + 2 {
		inspector.write([]byte("data: "))
		inspector.write(chunk)
		inspector.write([]byte("\n"))
	}
	inspector.write([]byte("\n"))
	inspector.write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_after_event\"}}\n\n"))
	inspector.finish()
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_after_event"}); got != "account-a" {
		t.Fatalf("response owner = %q, want account-a", got)
	}
}

func TestHTTPDownstreamWriteFailureClosesUpstreamBody(t *testing.T) {
	store, err := newAffinityStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	body := &trackedResponseBody{Reader: strings.NewReader("data: {}\n\n")}
	server := &server{
		affinity: store,
		stats:    newStats(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	server.relay(failingResponseWriter{header: http.Header{}}, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       body,
	}, time.Now(), "thread", "thread", "account-a", responseRequestData{}, turnMetadata{}, false)
	if !body.closed {
		t.Fatal("upstream body remained open")
	}
}

func TestResetHeaderAcceptsHTTPDate(t *testing.T) {
	want := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	headers := http.Header{"Retry-After": {want.Format(http.TimeFormat)}}
	if got := resetHeader(headers); !got.Equal(want) {
		t.Fatalf("reset = %s, want %s", got, want)
	}
}

func TestUpstreamRetryBackoffTotalsFiveSeconds(t *testing.T) {
	var total time.Duration
	for retry := 1; retry <= maxUpstreamRetries; retry++ {
		total += upstreamRetryBackoff(retry)
	}
	if remaining := upstreamRetryBudget - total; remaining < 0 || remaining > time.Nanosecond {
		t.Fatalf("backoff total = %s, want %s", total, upstreamRetryBudget)
	}
}

type failingRequestBody struct{}

func (failingRequestBody) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func (failingRequestBody) Close() error {
	return nil
}

type repeatedRequestBody struct {
	remaining int64
}

func (b *repeatedRequestBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), b.remaining)
	for index := range n {
		p[index] = 'x'
	}
	b.remaining -= n
	return int(n), nil
}

func (b *repeatedRequestBody) Close() error {
	return nil
}

type writerResponse struct {
	io.Writer
	header http.Header
}

func (w writerResponse) Header() http.Header {
	return w.header
}

func (writerResponse) WriteHeader(int) {}

type trackedResponseBody struct {
	io.Reader
	closed bool
}

func (b *trackedResponseBody) Close() error {
	b.closed = true
	return nil
}

type failingResponseWriter struct {
	header http.Header
}

func (w failingResponseWriter) Header() http.Header {
	return w.header
}

func (failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func (failingResponseWriter) WriteHeader(int) {}
