package main

import (
	"path/filepath"
	"testing"
)

func TestCompactionRotationDefaultsOffAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rotation, err := newCompactionRotation(store)
	if err != nil {
		t.Fatal(err)
	}
	if rotation.isEnabled() {
		t.Fatal("rotation enabled by default")
	}
	if enabled, err := rotation.toggle(); err != nil || !enabled {
		t.Fatalf("toggle = %t, %v", enabled, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reloaded, err := newCompactionRotation(reopened)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.isEnabled() {
		t.Fatal("rotation setting was not persisted")
	}
}

func TestCompactionRotationWaitsForNewUnanchoredTurn(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rotation, err := newCompactionRotation(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotation.toggle(); err != nil {
		t.Fatal(err)
	}
	compaction := turnMetadata{RequestKind: "compaction", TurnID: "turn-a"}
	rotation.arm("thread", "account-a", compaction)
	if rotation.shouldReconnect("thread", "account-a", compaction, false, true) {
		t.Fatal("same turn triggered reconnect")
	}
	if rotation.shouldReconnect("thread", "account-a", turnMetadata{TurnID: "turn-b"}, true, true) {
		t.Fatal("hard affinity triggered reconnect")
	}
	if !rotation.shouldReconnect("thread", "account-a", turnMetadata{TurnID: "turn-b"}, false, true) {
		t.Fatal("new unanchored turn did not trigger reconnect")
	}
	if skip, ok := rotation.handshakeSkip("thread", false); !ok || !skip["account-a"] {
		t.Fatalf("handshake skip = %v, %t", skip, ok)
	}
	if source := rotation.routeSource("thread", "account-b", turnMetadata{TurnID: "turn-b"}, false); source != "account-a" {
		t.Fatal("reconnected turn did not route to the new account")
	}
}
