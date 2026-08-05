package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const completedEvent = `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":1200,"output_tokens":340}}}

`

func TestSnifferReadsUsageFromACompletedEvent(t *testing.T) {
	var s sniffer
	s.feed([]byte(completedEvent))
	if s.usage.input != 1200 || s.usage.output != 340 {
		t.Fatalf("usage = %+v, want 1200/340", s.usage)
	}
}

func TestSnifferSurvivesEventsSplitAcrossReads(t *testing.T) {
	var s sniffer
	for i := 0; i < len(completedEvent); i++ {
		s.feed([]byte{completedEvent[i]})
	}
	if s.usage.input != 1200 || s.usage.output != 340 {
		t.Fatalf("usage = %+v; an event split across reads must still parse", s.usage)
	}
}

func TestSnifferIgnoresJunkAndCapsItsBuffer(t *testing.T) {
	var s sniffer
	s.feed([]byte("data: not json\n\nevent: ping\n\n"))
	s.feed([]byte(strings.Repeat("x", 2*maxSSEEvent)))
	s.feed([]byte(completedEvent))

	if s.usage.input != 1200 {
		t.Fatalf("usage = %+v, want the trailing event to still parse", s.usage)
	}
	if s.pending.Len() > maxSSEEvent {
		t.Fatalf("pending buffer grew to %d, want it capped", s.pending.Len())
	}
}

func TestRelayForwardsBytesUntouchedWhileCountingTokens(t *testing.T) {
	s := testServer(t, "http://unused", "acct-a")
	stream := "data: {\"type\":\"response.created\"}\n\n" + completedEvent
	rec := replayUpstream(t, s, stream)

	if rec.Body.String() != stream {
		t.Fatalf("relayed body differs from upstream:\n got %q\nwant %q", rec.Body, stream)
	}
	snap := s.stats.snapshot()
	if snap.InTokens != 1200 || snap.OutTokens != 340 {
		t.Fatalf("stats recorded %d/%d tokens, want 1200/340", snap.InTokens, snap.OutTokens)
	}
}

func TestWindowsReadBothRateLimitHeaders(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Unix()
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "42.5")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-at", itoa(reset))
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
