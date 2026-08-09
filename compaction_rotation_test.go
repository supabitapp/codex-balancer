package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
)

type testLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *testLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}

func (b *testLogBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	decoder := json.NewDecoder(bytes.NewReader(b.Bytes()))
	var records []map[string]any
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func requireLogRecord(t *testing.T, records []map[string]any, message string, fields map[string]any) {
	t.Helper()
	for _, record := range records {
		if record["msg"] != message {
			continue
		}
		matched := true
		for name, value := range fields {
			if record[name] != value {
				matched = false
				break
			}
		}
		if matched {
			return
		}
	}
	t.Fatalf("missing log %q with fields %v in %v", message, fields, records)
}

func TestCompactionRotationDefaultsOffAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rotation, err := newCompactionRotation(store, slog.New(slog.DiscardHandler))
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
	reloaded, err := newCompactionRotation(reopened, slog.New(slog.DiscardHandler))
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
	logs := &testLogBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rotation, err := newCompactionRotation(store, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotation.toggle(); err != nil {
		t.Fatal(err)
	}
	compaction := turnMetadata{RequestKind: "compaction", TurnID: "turn-a"}
	rotation.arm("thread", "account-a", compaction)
	if rotation.shouldReconnect("thread", "account-a", compaction, false, 0, true) {
		t.Fatal("same turn triggered reconnect")
	}
	if rotation.shouldReconnect("thread", "account-a", turnMetadata{TurnID: "turn-b"}, true, 0, true) {
		t.Fatal("hard affinity triggered reconnect")
	}
	if !rotation.shouldReconnect("thread", "account-a", turnMetadata{TurnID: "turn-b"}, false, 0, true) {
		t.Fatal("new unanchored turn did not trigger reconnect")
	}
	if skip, _, ok := rotation.handshakeSkip("thread", false); !ok || !skip["account-a"] {
		t.Fatalf("handshake skip = %v, %t", skip, ok)
	}
	if source := rotation.routeSource("thread", "account-b", turnMetadata{TurnID: "turn-b"}, false); source != "account-a" {
		t.Fatal("reconnected turn did not route to the new account")
	}
	rotation.finish("thread", "rotated", "account-b")
	records := logs.records(t)
	requireLogRecord(t, records, "compaction rotation armed", map[string]any{
		"thread":          "thread",
		"source_account":  "account-a",
		"compaction_turn": "turn-a",
	})
	for _, decision := range []string{"wait_same_turn", "wait_hard_affinity", "restart"} {
		requireLogRecord(t, records, "compaction rotation decision", map[string]any{
			"decision":       decision,
			"thread":         "thread",
			"source_account": "account-a",
		})
	}
	requireLogRecord(t, records, "compaction rotation handshake", map[string]any{
		"decision":       "exclude_source",
		"thread":         "thread",
		"source_account": "account-a",
	})
	requireLogRecord(t, records, "compaction rotation request routed", map[string]any{
		"thread":         "thread",
		"source_account": "account-a",
		"target_account": "account-b",
		"request_turn":   "turn-b",
	})
	requireLogRecord(t, records, "compaction rotation finished", map[string]any{
		"outcome":        "rotated",
		"thread":         "thread",
		"source_account": "account-a",
		"account":        "account-b",
	})
}
