package app

import "testing"

func TestStatsThreadKeyUsesCodexThreadWithoutChangingRouteFallback(t *testing.T) {
	if got := statsThreadKey("session", turnMetadata{ThreadID: "codex-thread"}); got != "codex-thread" {
		t.Fatalf("metadata stats key = %q, want codex-thread", got)
	}
	if got := statsThreadKey("session", turnMetadata{}); got != "session" {
		t.Fatalf("fallback stats key = %q, want session", got)
	}
}
