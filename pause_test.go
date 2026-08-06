package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func servedBy(t *testing.T, served *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("chatgpt-account-id")
		mu.Lock()
		*served = append(*served, id)
		mu.Unlock()
		sse(w, id)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func turn(s *server, thread string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("session-id", thread)
	s.responses(rec, req)
	return rec
}

func TestPausedAccountTakesNoNewThreads(t *testing.T) {
	var served []string
	upstream := servedBy(t, &served)

	s := testServer(t, upstream.URL, "acct-a", "acct-b")
	if _, err := s.pool.togglePause(s.pool.find("acct-a")); err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		if rec := turn(s, fmt.Sprintf("thread-%d", i)); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
	}
	for _, id := range served {
		if id == "acct-a" {
			t.Fatalf("a paused account still served turns: %v", served)
		}
	}
}

func TestPausingStopsTheThreadsAlreadyPinnedToIt(t *testing.T) {
	var served []string
	upstream := servedBy(t, &served)

	s := testServer(t, upstream.URL, "acct-a", "acct-b")
	if rec := turn(s, "thread-1"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if _, err := s.pool.togglePause(s.pool.find(served[0])); err != nil {
		t.Fatal(err)
	}

	if rec := turn(s, "thread-1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; a pause must hold even for pinned threads", rec.Code)
	}
	if len(served) != 1 {
		t.Fatalf("the thread was replayed elsewhere: %v", served)
	}
}

func TestPauseSurvivesAReload(t *testing.T) {
	path := t.TempDir() + "/accounts.json"
	pool := &Pool{path: path}
	if err := pool.add(accountFromState(accountState{IDToken: jwtFor("acct-1"), RefreshToken: "RT"})); err != nil {
		t.Fatal(err)
	}
	if paused, err := pool.togglePause(pool.find("acct-1")); err != nil {
		t.Fatal(err)
	} else if !paused {
		t.Fatal("account was resumed instead of paused")
	}

	reloaded, err := loadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.accounts[0].paused() {
		t.Fatal("the pause was lost on reload, so a restart would spend the account again")
	}
	if got := reloaded.pick("", nil); got != nil {
		t.Fatalf("picked %s, want nothing while the only account is paused", got.id())
	}
}

func TestDashboardTogglesTheSelectedAccount(t *testing.T) {
	pool := &Pool{path: t.TempDir() + "/accounts.json"}
	for _, id := range []string{"acct-a", "acct-b"} {
		if err := pool.add(accountFor(id)); err != nil {
			t.Fatal(err)
		}
	}

	var board tea.Model = dashboard{pool: pool, stats: newStats()}
	board, _ = board.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	board, _ = board.Update(tea.KeyPressMsg{Code: tea.KeySpace})

	if pool.find("acct-a").paused() {
		t.Error("the cursor had moved off acct-a, so it must stay live")
	}
	if !pool.find("acct-b").paused() {
		t.Fatal("space did not pause the selected account")
	}

	reloaded, err := loadPool(pool.path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.find("acct-b").paused() {
		t.Error("the toggle never reached disk")
	}

	board.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	if pool.find("acct-b").paused() {
		t.Error("space is a toggle; the second press must resume the account")
	}
}

func TestDashboardSurvivesAnEmptyPool(t *testing.T) {
	var board tea.Model = dashboard{pool: &Pool{path: t.TempDir() + "/accounts.json"}, stats: newStats()}
	board, _ = board.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	board.Update(tea.KeyPressMsg{Code: tea.KeySpace})
}
