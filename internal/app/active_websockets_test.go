package app

import (
	"fmt"
	"testing"
)

func TestActiveWebSocketRegistryClosesOnlyTheInvalidatedAccount(t *testing.T) {
	registry := activeWebSocketRegistry{}
	closed := []string{}
	registry.add("account-a", func(reason string) { closed = append(closed, "a1:"+reason) })
	registry.add("account-a", func(reason string) { closed = append(closed, "a2:"+reason) })
	registry.add("account-b", func(reason string) { closed = append(closed, "b:"+reason) })

	if got := registry.closeAccount("account-a", "owner_removed"); got != 2 {
		t.Fatalf("closed sockets = %d, want 2", got)
	}
	if got := fmt.Sprint(closed); got != "[a1:owner_removed a2:owner_removed]" && got != "[a2:owner_removed a1:owner_removed]" {
		t.Fatalf("callbacks = %s, want both account-a callbacks", got)
	}
	if got := registry.closeAccount("account-a", "again"); got != 0 {
		t.Fatalf("closed sockets on repeat = %d, want 0", got)
	}
	if got := registry.closeAccount("account-b", "owner_paused"); got != 1 {
		t.Fatalf("closed account-b sockets = %d, want 1", got)
	}
}

func TestActiveWebSocketRegistryFollowsAccountSwitchesAndRemoval(t *testing.T) {
	registry := activeWebSocketRegistry{}
	closed := []string{}
	id := registry.add("account-a", func(reason string) { closed = append(closed, reason) })
	registry.move(id, "account-a", "account-b")
	if got := registry.closeAccount("account-a", "old"); got != 0 {
		t.Fatalf("old account closed %d sockets after move, want 0", got)
	}
	if got := registry.closeAccount("account-b", "new"); got != 1 || fmt.Sprint(closed) != "[new]" {
		t.Fatalf("new account close = %d, callbacks = %v", got, closed)
	}

	id = registry.add("account-c", func(reason string) { closed = append(closed, reason) })
	registry.remove(id, "account-c")
	if got := registry.closeAccount("account-c", "removed"); got != 0 {
		t.Fatalf("explicitly removed socket was closed %d times, want 0", got)
	}
}
