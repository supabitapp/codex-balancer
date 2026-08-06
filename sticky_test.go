package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStickyBindingsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sticky.json")
	first, err := newSticky(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.bind("thread-1", "acct-a"); err != nil {
		t.Fatal(err)
	}

	second, err := newSticky(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.get("thread-1"); got != "acct-a" {
		t.Fatalf("binding after restart = %q, want acct-a", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("binding file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestTurnStateDoesNotRebindTheThread(t *testing.T) {
	var mu sync.Mutex
	var served []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served = append(served, r.Header.Get("chatgpt-account-id"))
		mu.Unlock()
		sse(w, r.Header.Get("chatgpt-account-id"))
	}))
	defer upstream.Close()

	s := testServer(t, upstream.URL, "acct-a", "acct-b")

	for _, turnState := range []string{"", "ts-first", "ts-second"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("session-id", "thread-1")
		if turnState != "" {
			req.Header.Set("x-codex-turn-state", turnState)
		}
		s.responses(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}

	if served[0] != served[1] || served[1] != served[2] {
		t.Fatalf("per-turn token moved the thread between accounts: %v", served)
	}
}

func TestPinnedThreadTakesThe429RatherThanAnotherAccount(t *testing.T) {
	var mu sync.Mutex
	var served []string
	limit := false

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("chatgpt-account-id")
		mu.Lock()
		served = append(served, id)
		refuse := limit
		mu.Unlock()
		if refuse {
			w.Header().Set("x-codex-primary-reset-at", "1785943171")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		sse(w, id)
	}))
	defer upstream.Close()

	s := testServer(t, upstream.URL, "acct-a", "acct-b")

	turn := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("session-id", "thread-1")
		s.responses(rec, req)
		return rec
	}

	turn()
	mu.Lock()
	limit = true
	mu.Unlock()
	rec := turn()

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream 429", rec.Code)
	}
	if got := rec.Header().Get("x-codex-primary-reset-at"); got != "1785943171" {
		t.Fatalf("reset header = %q, want it relayed so Codex can report the window", got)
	}
	if len(served) != 2 || served[0] != served[1] {
		t.Fatalf("thread was replayed onto another account: %v", served)
	}
}
