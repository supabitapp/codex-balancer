package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	if fmt.Sprint(calls) != "[account-a account-a account-a account-a account-b]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPServerFailureRetriesSameAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	calls := 0
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeResponseCreated(w, "resp_a")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if events := server.stats.snapshot().Events; len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
}

func TestHTTPCanceledRequestDoesNotFailOver(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	started := make(chan struct{})
	release := make(chan struct{})
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})
	defer closeServer()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt","input":[]}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		server.responses(httptest.NewRecorder(), request)
		close(done)
	}()

	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled request did not return")
	}

	requireNoFailedAccounts(t, server, a, b)
}

func TestHTTPUnsupportedModelRetriesAnotherAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		if account == "account-a" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":"invalid_request_error","message":"The 'gpt-route' model is not supported when using Codex with a ChatGPT account."}}`)
			return
		}
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-route","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a account-b]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPUnsupportedModelPreservesRejectionWithoutReplacement(t *testing.T) {
	a := testAccount("account-a", 0)
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"invalid_request_error","message":"The 'gpt-route' model is not supported when using Codex with a ChatGPT account."}}`)
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-route","input":[]}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "The 'gpt-route' model is not supported") {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestHTTPModelCatalogSkipsUnsupportedSoftOwner(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()
	server.catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-5.6-terra")},
			b.id(): {testModelEntry("gpt-5.6-sol")},
		},
		"0.1.0",
	)
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, a.id()); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-sol","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-b]" {
		t.Fatalf("calls = %v, want account-b", calls)
	}
}

func TestHTTPModelCatalogFiltersPriorityTier(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()
	server.catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-5.6-sol")},
			b.id(): {testModelEntry("gpt-5.6-sol", "priority")},
		},
		"0.1.0",
	)

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-sol","service_tier":"priority","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-b]" {
		t.Fatalf("calls = %v, want account-b", calls)
	}
}

func TestHTTPModelCatalogDoesNotMoveHardOwner(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "unexpected")
	})
	defer closeServer()
	server.catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-5.6-terra")},
			b.id(): {testModelEntry("gpt-5.6-sol")},
		},
		"0.1.0",
	)
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, a.id()); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-sol","previous_response_id":"resp_a","input":[]}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %v, want none", calls)
	}
}

func TestHTTPUnsupportedModelRetriesOnlyOnce(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	c := testAccount("account-c", 40)
	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a, b, c}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		if account == c.id() {
			writeResponseCreated(w, "unexpected")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"invalid_request_error","message":"The 'gpt-route' model is not supported when using Codex with a ChatGPT account."}}`)
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-route","input":[]}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a account-b]" {
		t.Fatalf("calls = %v, want one replacement", calls)
	}
}

func TestHTTPUnsupportedModelRequiresInvalidRequestCode(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"other_error","message":"The 'gpt-route' model is not supported when using Codex with a ChatGPT account."}}`)
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-route","input":[]}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("calls = %v, want one attempt", calls)
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

func TestHTTPResponseTurnStateBindsOwner(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Turn-State", "turn")
		writeResponseCreated(w, "resp_a")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := store.lookup(affinityRef{kind: affinityTurnState, value: "turn"}); got != "account-a" {
		t.Fatalf("turn owner = %q, want account-a", got)
	}
}

func TestHTTPUnknownHardAffinityFailsBeforeUpstream(t *testing.T) {
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
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinityTurnState, value: "turn"}); got != "" {
		t.Fatalf("turn owner = %q, want none", got)
	}
}

func TestHTTPHardAffinityDoesNotFailOverAfterNetworkFailure(t *testing.T) {
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
	if err := store.bind(affinityRef{kind: affinityTurnState, value: "turn"}, "account-a"); err != nil {
		t.Fatal(err)
	}
	lastUsed := time.Now().Add(-2 * time.Hour).UnixNano()
	if _, err := store.store.db.Exec(
		`UPDATE bindings SET last_used_at_ns = ? WHERE kind = ? AND value = ?`,
		lastUsed,
		affinityTurnState,
		"turn",
	); err != nil {
		t.Fatal(err)
	}

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
	var storedLastUsed int64
	if err := store.store.db.QueryRow(
		`SELECT last_used_at_ns FROM bindings WHERE kind = ? AND value = ?`,
		affinityTurnState,
		"turn",
	).Scan(&storedLastUsed); err != nil {
		t.Fatal(err)
	}
	if storedLastUsed != lastUsed {
		t.Fatalf("last used = %d, want %d", storedLastUsed, lastUsed)
	}
}

func TestHTTPPreviousResponseOverridesSoftSessionWithoutRebinding(t *testing.T) {
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
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
}

func TestHTTPPreviousResponseOverridesPromptCacheWithoutRebinding(t *testing.T) {
	a := testAccount("account-a", 90)
	b := testAccount("account-b", 0)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_next")
	})
	defer closeServer()
	cache := affinityRef{kind: affinityPromptCache, value: "cache"}
	if err := store.bind(cache, "account-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "", "", `{"prompt_cache_key":"cache","previous_response_id":"resp_a","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(cache); got != "account-b" {
		t.Fatalf("cache owner = %q, want account-b", got)
	}
}

func TestHTTPOpaqueFileCanFailOver(t *testing.T) {
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

	response := serveHTTPResponse(t, server, "session", "", `{"input":[{"type":"input_file","file_id":"file_unknown"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a account-a account-a account-a account-b]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinityFile, value: "file_unknown"}); got != "" {
		t.Fatalf("file owner = %q, want empty", got)
	}
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
}

func TestHTTPKnownFileOverridesSoftSessionWithoutRebinding(t *testing.T) {
	a := testAccount("account-a", 90)
	b := testAccount("account-b", 0)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_next")
	})
	defer closeServer()
	session := affinityRef{kind: affinitySession, value: "session"}
	file := affinityRef{kind: affinityFile, value: "file_owned"}
	if err := store.bind(session, "account-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(file, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"input":[{"type":"input_file","file_id":"file_owned"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(session); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
}

func TestHTTPPartialFileOwnershipFailsBeforeUpstream(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 0)
	calls := 0
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(http.ResponseWriter, *http.Request) {
		calls++
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinityFile, value: "file_known"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "", "", `{"input":[{"type":"input_file","file_id":"file_known"},{"type":"input_file","file_id":"file_unknown"}]}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestHTTPZstdBodyPreservesAffinityAndForwardedBytes(t *testing.T) {
	a := testAccount("account-a", 90)
	b := testAccount("account-b", 0)
	body := []byte(`{"previous_response_id":"resp_a","input":[]}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(body, nil)
	encoder.Close()
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if !bytes.Equal(got, compressed) {
			t.Errorf("forwarded body changed")
			return
		}
		if got := r.Header.Get("Content-Encoding"); got != "zstd" {
			t.Errorf("content encoding = %q, want zstd", got)
			return
		}
		writeResponseCreated(w, "resp_next")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressed))
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set("session-id", "session")
	response := httptest.NewRecorder()
	server.responses(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPCompletedFollowUpsLeaveNoLiveThreads(t *testing.T) {
	account := testAccount("account", 0)
	calls := 0
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeResponseCreated(w, fmt.Sprintf("resp_%d", calls))
	})
	defer closeServer()

	first := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-sol","reasoning":{"effort":"medium"},"input":[]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	second := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-terra","reasoning":{"effort":"xhigh"},"previous_response_id":"resp_1","input":[]}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}

	snapshot := server.stats.snapshot()
	if snapshot.Turns != 2 {
		t.Fatalf("turns = %d, want 2", snapshot.Turns)
	}
	if len(snapshot.Threads) != 0 {
		t.Fatalf("completed HTTP threads = %+v", snapshot.Threads)
	}
}

func TestHTTPThreadLivesOnlyWhileResponseIsOpen(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseResponse := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseResponse()
	account := testAccount("account", 0)
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		writeResponseCreated(w, "resp")
		w.(http.Flusher).Flush()
		close(started)
		<-release
		io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	})
	defer closeServer()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-sol","input":[]}`)
	}()
	<-started
	deadline := time.Now().Add(time.Second)
	for {
		threads := server.stats.snapshot().Threads
		if len(threads) == 1 {
			if threads[0].Key != "session" || threads[0].Via != transportHTTP {
				t.Fatalf("live HTTP thread = %+v", threads[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("HTTP thread did not become live")
		}
		time.Sleep(time.Millisecond)
	}

	releaseResponse()
	response := <-done
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if threads := server.stats.snapshot().Threads; len(threads) != 0 {
		t.Fatalf("completed HTTP threads = %+v", threads)
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

func TestHTTPCarriageReturnStreamRegistersHardOwner(t *testing.T) {
	a := testAccount("account-a", 0)
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cr\"}}\r\r")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_cr\"}}\r\r")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_cr"}); got != "account-a" {
		t.Fatalf("response owner = %q, want account-a", got)
	}
}

func TestHTTPMultilineStreamRegistersHardOwner(t *testing.T) {
	a := testAccount("account-a", 0)
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.created\",\r")
		w.(http.Flusher).Flush()
		io.WriteString(w, "\ndata: \"response\":{\"id\":\"resp_multiline\"}}\r\n\r\n")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_multiline"}); got != "account-a" {
		t.Fatalf("response owner = %q, want account-a", got)
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

func TestHTTPCompletedResponseTracksAPIEstimate(t *testing.T) {
	account := testAccount("account-a", 0)
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":800},"output_tokens":100}}}`+"\n\n")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-sol","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	snapshot := server.stats.snapshot()
	if snapshot.APICostNanoDollars != 4_400_000 || snapshot.UnpricedResponses != 0 {
		t.Fatalf("API estimate = %d with %d unpriced, want 4400000 with none", snapshot.APICostNanoDollars, snapshot.UnpricedResponses)
	}
	if len(snapshot.Threads) != 0 {
		t.Fatalf("completed HTTP threads = %+v", snapshot.Threads)
	}
}

func TestHTTPV2CompactionTracksMetadataAndUsage(t *testing.T) {
	account := testAccount("account-a", 0)
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"model":"gpt-5.6-sol","usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":100},"output_tokens":100,"total_tokens":1100,"output_tokens_details":{"reasoning_tokens":60}}}}`+"\n\n")
	})
	defer closeServer()
	logs := &testLogBuffer{}
	server.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	metadata := turnMetadata{RequestKind: "compaction", ThreadID: "codex-thread", TurnID: "codex-turn", WindowID: "codex-window:1", SubagentKind: "compact"}
	body, err := json.Marshal(map[string]any{
		"model":           "gpt-5.6-sol",
		"client_metadata": map[string]string{codexTurnMetadataKey: encodeTurnMetadata(metadata)},
		"input":           []any{map[string]any{"type": "compaction_trigger"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := serveHTTPResponse(t, server, "session", "", string(body))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if threads := server.stats.snapshot().Threads; len(threads) != 0 {
		t.Fatalf("completed HTTP threads = %+v", threads)
	}
	requireLogRecord(t, logs.records(t), "response usage", map[string]any{
		"transport":          "http",
		"thread":             "session",
		"turn":               "codex-turn",
		"request_kind":       "compaction",
		"account":            "account-a",
		"rotation_source":    "",
		"compaction_replay":  false,
		"model":              "gpt-5.6-sol",
		"service_tier":       "",
		"input_tokens":       float64(1_000),
		"cached_tokens":      float64(800),
		"cache_write_tokens": float64(100),
		"output_tokens":      float64(100),
		"reasoning_tokens":   float64(60),
	})
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
	state, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	pool, err := loadPool(state)
	if err != nil {
		state.Close()
		upstream.Close()
		t.Fatal(err)
	}
	for _, account := range accounts {
		if err := pool.add(account); err != nil {
			state.Close()
			upstream.Close()
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { state.Close() })
	store := &AffinityStore{store: state}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &server{
		ctx:          context.Background(),
		pool:         pool,
		catalog:      newModelCatalog(),
		affinity:     store,
		stats:        newStats(),
		upstream:     upstream.URL,
		client:       newProxyClient(),
		log:          log,
		retryBackoff: func(int) time.Duration { return 0 },
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

func requireNoFailedAccounts(t *testing.T, server *server, accounts ...*Account) {
	t.Helper()
	if events := server.stats.snapshot().Events; len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
	for _, account := range accounts {
		_, _, cooldown, _ := account.health()
		if !cooldown.IsZero() {
			t.Fatalf("account %s cooldown = %s, want none", account.id(), cooldown)
		}
	}
}
