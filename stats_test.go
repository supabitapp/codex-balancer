package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWindowsReadBothRateLimitHeaders(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Unix()
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "42.5")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-at", strconv.FormatInt(reset, 10))
	h.Set("x-codex-secondary-primary-used-percent", "91")

	a := &Account{IDToken: jwtFor("acct-a")}
	a.observe(h)

	primary, secondary, _, _ := a.health()
	if primary.usedPercent != 42.5 || primary.minutes != 300 || primary.resetsAt.Unix() != reset {
		t.Fatalf("primary = %+v", primary)
	}
	if secondary.usedPercent != 91 {
		t.Fatalf("secondary = %+v", secondary)
	}
	if got := a.pressure(); got != 91 {
		t.Fatalf("pressure = %v, want the fuller of the two windows", got)
	}
}

func TestWindowNameFollowsTheReportedWindow(t *testing.T) {
	for minutes, want := range map[int]string{300: "5h", 10080: "7d", 90: "90m", 0: ""} {
		if got := windowName(minutes); got != want {
			t.Errorf("windowName(%d) = %q, want %q", minutes, got, want)
		}
	}
}

func TestActivityRingShiftsWithTime(t *testing.T) {
	s := newStats()
	s.routed("thread", "acct-a")
	s.routed("thread", "acct-a")

	s.mu.Lock()
	s.accounts["acct-a"].bucket -= 3
	s.mu.Unlock()
	s.routed("thread", "acct-a")

	got := s.snapshot().Accounts["acct-a"].Activity
	if got[0] != 1 {
		t.Fatalf("newest bucket = %d, want the fresh turn", got[0])
	}
	if got[3] != 2 {
		t.Fatalf("activity = %v, want the older pair shifted three slots back", got[:6])
	}
}

func TestRelayForwardsBytesUntouched(t *testing.T) {
	stream := "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}))
	defer upstream.Close()

	s := testServer(t, upstream.URL, "acct-a")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	s.responses(rec, req)

	if rec.Body.String() != stream {
		t.Fatalf("relayed body differs from upstream:\n got %q\nwant %q", rec.Body, stream)
	}
	if s.stats.snapshot().TTFB <= 0 {
		t.Fatal("time to first byte not recorded")
	}
}
