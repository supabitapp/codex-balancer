package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
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
			if !reflect.DeepEqual(record[name], value) {
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

func forbidLogMessage(t *testing.T, records []map[string]any, message string) {
	t.Helper()
	for _, record := range records {
		if record["msg"] == message {
			t.Fatalf("unexpected log %q in %v", message, records)
		}
	}
}

func TestCompactionRotationWaitsForNewUnanchoredTurn(t *testing.T) {
	logs := &testLogBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rotation := newCompactionRotation(log)
	compaction := turnMetadata{RequestKind: "compaction", ThreadID: "logical-thread", TurnID: "turn-a"}
	rotation.arm("session", "account-a", compaction)
	if rotation.shouldReconnect("session", "account-a", turnMetadata{RequestKind: "memory"}, requestAffinity{}, false, 0, "account-b") {
		t.Fatal("memory request triggered reconnect")
	}
	if rotation.shouldReconnect("session", "account-a", turnMetadata{RequestKind: "turn", ThreadID: "other-thread", TurnID: "turn-b", SubagentKind: "thread_spawn"}, requestAffinity{}, false, 0, "account-b") {
		t.Fatal("other logical thread triggered reconnect")
	}
	if rotation.shouldReconnect("session", "account-a", compaction, requestAffinity{}, false, 0, "account-b") {
		t.Fatal("same turn triggered reconnect")
	}
	next := turnMetadata{RequestKind: "turn", ThreadID: "logical-thread", TurnID: "turn-b"}
	hard := requestAffinity{
		hard:             []affinityRef{{kind: affinityResponse, value: "response"}},
		compactionReplay: true,
	}
	if rotation.shouldReconnect("session", "account-a", next, hard, true, 0, "account-b") {
		t.Fatal("hard affinity triggered reconnect")
	}
	if !rotation.shouldReconnect("session", "account-a", next, requestAffinity{}, false, 0, "account-b") {
		t.Fatal("new unanchored turn did not trigger reconnect")
	}
	if skip, _, ok := rotation.handshakeSkip("session", false); !ok || !skip["account-a"] {
		t.Fatalf("handshake skip = %v, %t", skip, ok)
	}
	if source := rotation.routeSource("session", "account-b", next, requestAffinity{}, false); source != "account-a" {
		t.Fatal("reconnected turn did not route to the new account")
	}
	rotation.finish("logical-thread", "rotated", "account-b")
	records := logs.records(t)
	requireLogRecord(t, records, "compaction rotation armed", map[string]any{
		"session":         "session",
		"thread":          "logical-thread",
		"source_account":  "account-a",
		"compaction_turn": "turn-a",
	})
	for _, decision := range []string{"wait_same_turn", "wait_hard_affinity", "restart"} {
		requireLogRecord(t, records, "compaction rotation decision", map[string]any{
			"decision":       decision,
			"session":        "session",
			"thread":         "logical-thread",
			"source_account": "account-a",
		})
	}
	requireLogRecord(t, records, "compaction rotation decision", map[string]any{
		"decision":            "wait_hard_affinity",
		"hard_affinity_kinds": []any{"response"},
		"compaction_replay":   true,
	})
	requireLogRecord(t, records, "compaction rotation handshake", map[string]any{
		"decision":       "exclude_source",
		"session":        "session",
		"thread":         "logical-thread",
		"source_account": "account-a",
	})
	requireLogRecord(t, records, "compaction rotation request routed", map[string]any{
		"session":         "session",
		"thread":          "logical-thread",
		"source_account":  "account-a",
		"current_account": "account-b",
		"request_turn":    "turn-b",
	})
	requireLogRecord(t, records, "compaction rotation finished", map[string]any{
		"outcome":        "rotated",
		"session":        "session",
		"thread":         "logical-thread",
		"source_account": "account-a",
		"account":        "account-b",
	})
}

func TestCompactionRotationKeepsFreshRouteSource(t *testing.T) {
	logs := &testLogBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rotation := newCompactionRotation(log)
	compaction := turnMetadata{RequestKind: "compaction", ThreadID: "logical-thread", TurnID: "turn-a"}
	rotation.arm("session", "account-a", compaction)
	next := turnMetadata{RequestKind: "turn", ThreadID: "logical-thread", TurnID: "turn-b"}
	if rotation.shouldReconnect("session", "account-a", next, requestAffinity{}, false, 0, "account-a") {
		t.Fatal("fresh route source triggered reconnect")
	}
	if _, _, ok := rotation.handshakeSkip("session", false); ok {
		t.Fatal("source decision left a pending reconnect")
	}
	requireLogRecord(t, logs.records(t), "compaction rotation decision", map[string]any{
		"decision":       "cancel_source_selected",
		"session":        "session",
		"thread":         "logical-thread",
		"source_account": "account-a",
		"fresh_account":  "account-a",
	})
}

func TestCompactionRotationReportsPendingSession(t *testing.T) {
	rotation := newCompactionRotation(slog.New(slog.DiscardHandler))
	metadata := turnMetadata{RequestKind: "compaction", ThreadID: "thread", TurnID: "turn"}
	rotation.arm("session", "account-a", metadata)
	if !rotation.hasSession("session") {
		t.Fatal("pending session not found")
	}
	rotation.finish("thread", "done", "account-b")
	if rotation.hasSession("session") {
		t.Fatal("finished session remains pending")
	}
}
