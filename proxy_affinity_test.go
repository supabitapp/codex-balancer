package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestHTTPSoftSessionMovesFromSpentAccount(t *testing.T) {
	a := testAccount("account-a", 99)
	b := testAccount("account-b", 0)
	a.spent = true
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-b]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_b"}); got != "account-b" {
		t.Fatalf("response owner = %q, want account-b", got)
	}
}

func TestHTTPSoftSessionRetriesRateLimitOnAnotherAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		if account == "account-a" {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a account-b]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
}

func TestHTTPSoftSessionRetriesServerFailureOnAnotherAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		if account == "account-a" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a account-b]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPHardPreviousResponseNeverMoves(t *testing.T) {
	a := testAccount("account-a", 99)
	b := testAccount("account-b", 0)
	a.spent = true
	calls := 0
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeResponseCreated(w, "unexpected")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"previous_response_id":"resp_a","input":[]}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestHTTPUnknownHardAffinityPinsBeforeRateLimit(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "", "turn", `{"input":[]}`)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinityTurnState, value: "turn"}); got != "account-a" {
		t.Fatalf("turn owner = %q, want account-a", got)
	}

	response = serveHTTPResponse(t, server, "", "turn", `{"input":[]}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("retry status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("retry calls = %v", calls)
	}
}

func TestHTTPUnknownHardAffinityDoesNotFailOverAfterNetworkFailure(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	var mu sync.Mutex
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		mu.Lock()
		calls = append(calls, account)
		mu.Unlock()
		if account == "account-a" {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
		writeResponseCreated(w, "unexpected")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "", "turn", `{"input":[]}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	mu.Lock()
	gotCalls := fmt.Sprint(calls)
	mu.Unlock()
	if gotCalls != "[account-a]" {
		t.Fatalf("calls = %s", gotCalls)
	}
	if got := store.lookup(affinityRef{kind: affinityTurnState, value: "turn"}); got != "account-a" {
		t.Fatalf("turn owner = %q, want account-a", got)
	}
}

func TestHTTPPreviousResponseOverridesSoftSession(t *testing.T) {
	a := testAccount("account-a", 90)
	b := testAccount("account-b", 0)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_next")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"previous_response_id":"resp_a","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-a" {
		t.Fatalf("session owner = %q, want account-a", got)
	}
}

func TestHTTPFollowUpsKeepStableSessionStats(t *testing.T) {
	account := testAccount("account", 0)
	calls := 0
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeResponseCreated(w, fmt.Sprintf("resp_%d", calls))
	})
	defer closeServer()

	first := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	second := serveHTTPResponse(t, server, "session", "", `{"previous_response_id":"resp_1","input":[]}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}

	snapshot := server.stats.snapshot()
	if snapshot.Turns != 2 {
		t.Fatalf("turns = %d, want 2", snapshot.Turns)
	}
	if len(snapshot.Threads) != 1 {
		t.Fatalf("threads = %+v, want one session", snapshot.Threads)
	}
	thread := snapshot.Threads[0]
	if thread.Key != "session" || thread.Turns != 2 || thread.Via != transportHTTP {
		t.Fatalf("thread = %+v, want session with two HTTP turns", thread)
	}
}

func TestHTTPConflictingHardOwnersFailBeforeUpstream(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 0)
	calls := 0
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls++
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinityTurnState, value: "turn"}, "account-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp"}, "account-b"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "", "turn", `{"previous_response_id":"resp","input":[]}`)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestHTTPChunkedResponseRegistersHardOwner(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 0)
	var mu sync.Mutex
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		mu.Lock()
		calls = append(calls, account)
		call := len(calls)
		mu.Unlock()
		if call == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"type":"response.cre`)
			w.(http.Flusher).Flush()
			io.WriteString(w, `ated","response":{"id":"resp_a"}}`+"\n\n")
			return
		}
		writeResponseCreated(w, "resp_next")
	})
	defer closeServer()

	first := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	second := serveHTTPResponse(t, server, "other", "", `{"previous_response_id":"resp_a","input":[]}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(calls) != "[account-b account-b]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_a"}); got != "account-b" {
		t.Fatalf("response owner = %q, want account-b", got)
	}
}

func TestHTTPJSONResponseRegistersHardOwner(t *testing.T) {
	a := testAccount("account-a", 0)
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_json","object":"response","status":"completed"}`)
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_json"}); got != "account-a" {
		t.Fatalf("response owner = %q, want account-a", got)
	}
}

func TestHTTPMissingPreviousResponseFailsBeforeUpstream(t *testing.T) {
	a := testAccount("account-a", 0)
	calls := 0
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a}, func(w http.ResponseWriter, r *http.Request) {
		calls++
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"previous_response_id":"missing","input":[]}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func newAffinityHTTPServer(
	t *testing.T,
	accounts []*Account,
	handler http.HandlerFunc,
) (*server, *AffinityStore, func()) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	store, err := newAffinityStore(filepath.Join(t.TempDir(), "affinity.json"))
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &server{
		ctx:      context.Background(),
		pool:     &Pool{accounts: accounts},
		affinity: store,
		stats:    newStats(),
		upstream: upstream.URL,
		client:   newProxyClient(),
		log:      log,
	}
	return server, store, upstream.Close
}

func serveHTTPResponse(t *testing.T, server *server, session, turnState, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	if session != "" {
		request.Header.Set("session-id", session)
	}
	if turnState != "" {
		request.Header.Set("x-codex-turn-state", turnState)
	}
	response := httptest.NewRecorder()
	server.responses(response, request)
	return response
}

func writeResponseCreated(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":%q}}\n\n", id)
}
