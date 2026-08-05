package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

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
