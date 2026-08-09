package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAffinityFromRequest(t *testing.T) {
	tests := []struct {
		name               string
		headers            http.Header
		body               string
		preferred          affinityRef
		hard               []affinityRef
		requireUnambiguous bool
	}{
		{
			name:      "session header is soft",
			headers:   http.Header{"Session-Id": {"session"}},
			body:      `{"input":[]}`,
			preferred: affinityRef{kind: affinitySession, value: "session"},
		},
		{
			name:      "session header wins over prompt cache",
			headers:   http.Header{"Session-Id": {"session"}},
			body:      `{"prompt_cache_key":"cache","input":[]}`,
			preferred: affinityRef{kind: affinitySession, value: "session"},
		},
		{
			name:      "prompt cache is soft",
			body:      `{"prompt_cache_key":"cache","input":[]}`,
			preferred: affinityRef{kind: affinityPromptCache, value: "cache"},
		},
		{
			name:      "camel prompt cache is soft",
			body:      `{"promptCacheKey":"cache","input":[]}`,
			preferred: affinityRef{kind: affinityPromptCache, value: "cache"},
		},
		{
			name:    "turn state is hard",
			headers: http.Header{"X-Codex-Turn-State": {"turn"}, "Session-Id": {"session"}},
			body:    `{"input":[]}`,
			hard:    []affinityRef{{kind: affinityTurnState, value: "turn"}},
		},
		{
			name:      "previous response overrides soft session",
			headers:   http.Header{"Session-Id": {"session"}},
			body:      `{"previous_response_id":"resp_a","input":[]}`,
			preferred: affinityRef{kind: affinitySession, value: "session"},
			hard:      []affinityRef{{kind: affinityResponse, value: "resp_a"}},
		},
		{
			name:               "conversation requires one owner",
			body:               `{"conversation":"conv_a","input":[]}`,
			hard:               []affinityRef{{kind: affinityConversation, value: "conv_a"}},
			requireUnambiguous: true,
		},
		{
			name: "blank conversation has no affinity",
			body: `{"conversation":"  ","input":[]}`,
		},
		{
			name: "nested files are hard owner evidence",
			body: `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file_a"},{"type":"input_file","file_id":"file_b"}]}]}`,
			hard: []affinityRef{
				{kind: affinityFile, value: "file_a"},
				{kind: affinityFile, value: "file_b"},
			},
		},
		{
			name:      "accepted session header order",
			headers:   http.Header{"Session-Id": {"first"}, "X-Codex-Session-Id": {"second"}, "Thread-Id": {"third"}},
			body:      `{"input":[]}`,
			preferred: affinityRef{kind: affinitySession, value: "first"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := affinityFromRequest(test.headers, []byte(test.body))
			if err != nil {
				t.Fatal(err)
			}
			if got.preferred != test.preferred {
				t.Fatalf("preferred = %#v, want %#v", got.preferred, test.preferred)
			}
			if !sameAffinityRefs(got.hard, test.hard) {
				t.Fatalf("hard = %#v, want %#v", got.hard, test.hard)
			}
			if got.requireUnambiguous != test.requireUnambiguous {
				t.Fatalf("requireUnambiguous = %t, want %t", got.requireUnambiguous, test.requireUnambiguous)
			}
		})
	}
}

func TestAffinityFromRequestRejectsInvalidJSON(t *testing.T) {
	if _, err := affinityFromRequest(nil, []byte(`{"input":`)); err == nil {
		t.Fatal("expected invalid JSON error")
	}
}

func TestAffinityStatsKey(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		body    string
		want    string
	}{
		{
			name:    "session wins over hard references",
			headers: http.Header{"Session-Id": {"session"}, "X-Codex-Turn-State": {"turn"}},
			body:    `{"previous_response_id":"response","input":[]}`,
			want:    "session",
		},
		{
			name: "prompt cache wins over response",
			body: `{"prompt_cache_key":"cache","previous_response_id":"response","input":[]}`,
			want: "cache",
		},
		{
			name: "hard reference is the fallback",
			body: `{"previous_response_id":"response","input":[]}`,
			want: "response",
		},
		{
			name: "missing affinity stays empty",
			body: `{"input":[]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			affinity, err := affinityFromRequest(test.headers, []byte(test.body))
			if err != nil {
				t.Fatal(err)
			}
			if got := affinity.statsKey(test.headers); got != test.want {
				t.Fatalf("stats key = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAffinityStoreSeparatesKindsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "affinity.json")
	store, err := newAffinityStore(path)
	if err != nil {
		t.Fatal(err)
	}
	refs := map[affinityRef]string{
		{kind: affinitySession, value: "same"}:      "account-a",
		{kind: affinityTurnState, value: "same"}:    "account-b",
		{kind: affinityResponse, value: "response"}: "account-c",
	}
	for ref, account := range refs {
		if err := store.bind(ref, account); err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := newAffinityStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for ref, want := range refs {
		if got := reloaded.lookup(ref); got != want {
			t.Errorf("lookup(%#v) = %q, want %q", ref, got, want)
		}
	}
}

func TestAffinityStoreExpiresOnlySoftBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "affinity.json")
	store, err := newAffinityStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000, 0)
	store.now = func() time.Time { return now }
	soft := affinityRef{kind: affinitySession, value: "session"}
	hard := affinityRef{kind: affinityTurnState, value: "turn"}
	if err := store.bindAll([]affinityRef{soft, hard}, "account-a"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(affinityTTL + time.Second)
	if got := store.lookup(soft); got != "" {
		t.Fatalf("expired soft owner = %q", got)
	}
	if got := store.lookup(hard); got != "account-a" {
		t.Fatalf("hard owner = %q, want account-a", got)
	}
}

func TestAffinityStoreRejectsHardRebindAtomically(t *testing.T) {
	store, err := newAffinityStore(filepath.Join(t.TempDir(), "affinity.json"))
	if err != nil {
		t.Fatal(err)
	}
	session := affinityRef{kind: affinitySession, value: "session"}
	response := affinityRef{kind: affinityResponse, value: "response"}
	if err := store.bind(response, "account-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.bindAll([]affinityRef{session, response}, "account-b"); !errors.Is(err, errAffinityConflict) {
		t.Fatalf("error = %v, want affinity conflict", err)
	}
	if got := store.lookup(session); got != "" {
		t.Fatalf("session owner = %q, want empty", got)
	}
	if got := store.lookup(response); got != "account-a" {
		t.Fatalf("response owner = %q, want account-a", got)
	}
}

func TestAffinityStoreKeepsMemoryUnchangedWhenSaveFails(t *testing.T) {
	store, err := newAffinityStore(filepath.Join(t.TempDir(), "affinity.json"))
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(blocker, "affinity.json")
	session := affinityRef{kind: affinitySession, value: "session"}
	if err := store.bind(session, "account-a"); err == nil {
		t.Fatal("expected save error")
	}
	if got := store.lookup(session); got != "" {
		t.Fatalf("session owner = %q, want empty", got)
	}
}

func TestAffinityStoreSweepPersistsSoftExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "affinity.json")
	store, err := newAffinityStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000, 0)
	store.now = func() time.Time { return now }
	soft := affinityRef{kind: affinityPromptCache, value: "cache"}
	hard := affinityRef{kind: affinityResponse, value: "response"}
	if err := store.bindAll([]affinityRef{soft, hard}, "account-a"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(affinityTTL + time.Second)
	if err := store.sweep(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newAffinityStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.lookup(soft); got != "" {
		t.Fatalf("soft owner = %q, want empty", got)
	}
	if got := reloaded.lookup(hard); got != "account-a" {
		t.Fatalf("hard owner = %q, want account-a", got)
	}
}

func TestResolveAffinity(t *testing.T) {
	store, err := newAffinityStore(filepath.Join(t.TempDir(), "affinity.json"))
	if err != nil {
		t.Fatal(err)
	}
	pool := &Pool{accounts: []*Account{testAccount("account-a", 10), testAccount("account-b", 20)}}
	session := affinityRef{kind: affinitySession, value: "session"}
	turn := affinityRef{kind: affinityTurnState, value: "turn"}
	response := affinityRef{kind: affinityResponse, value: "response"}
	if err := store.bindAll([]affinityRef{session, turn}, "account-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(response, "account-b"); err != nil {
		t.Fatal(err)
	}

	t.Run("soft preference", func(t *testing.T) {
		got, err := store.resolve(requestAffinity{preferred: session}, pool)
		if err != nil || got.required != "" || got.preferred != "account-a" {
			t.Fatalf("resolution = %+v, error = %v", got, err)
		}
	})

	t.Run("hard owner", func(t *testing.T) {
		got, err := store.resolve(requestAffinity{preferred: session, hard: []affinityRef{response}}, pool)
		if err != nil || got.required != "account-b" {
			t.Fatalf("resolution = %+v, error = %v", got, err)
		}
	})

	t.Run("hard owner conflict", func(t *testing.T) {
		_, err := store.resolve(requestAffinity{hard: []affinityRef{turn, response}}, pool)
		if !errors.Is(err, errAffinityConflict) {
			t.Fatalf("error = %v, want affinity conflict", err)
		}
	})

	t.Run("unknown previous response", func(t *testing.T) {
		_, err := store.resolve(requestAffinity{hard: []affinityRef{{kind: affinityResponse, value: "missing"}}}, pool)
		if !errors.Is(err, errAffinityOwnerUnavailable) {
			t.Fatalf("error = %v, want owner unavailable", err)
		}
	})

	t.Run("unknown files remain opaque", func(t *testing.T) {
		got, err := store.resolve(requestAffinity{hard: []affinityRef{{kind: affinityFile, value: "missing"}}}, pool)
		if err != nil || got.required != "" || got.hard || len(got.bindings) != 0 {
			t.Fatalf("resolution = %+v, error = %v", got, err)
		}
	})

	t.Run("hard owner does not rewrite soft affinity", func(t *testing.T) {
		file := affinityRef{kind: affinityFile, value: "owned-file"}
		if err := store.bind(file, "account-b"); err != nil {
			t.Fatal(err)
		}
		got, err := store.resolve(requestAffinity{preferred: session, hard: []affinityRef{file}}, pool)
		if err != nil || got.required != "account-b" || !got.hard {
			t.Fatalf("resolution = %+v, error = %v", got, err)
		}
		if !sameAffinityRefs(got.bindings, []affinityRef{file}) {
			t.Fatalf("bindings = %#v, want only file owner", got.bindings)
		}
	})

	t.Run("partial file ownership fails", func(t *testing.T) {
		file := affinityRef{kind: affinityFile, value: "known"}
		if err := store.bind(file, "account-a"); err != nil {
			t.Fatal(err)
		}
		_, err := store.resolve(requestAffinity{hard: []affinityRef{file, {kind: affinityFile, value: "missing"}}}, pool)
		if !errors.Is(err, errAffinityOwnerUnavailable) {
			t.Fatalf("error = %v, want owner unavailable", err)
		}
	})

	t.Run("unknown conversation is ambiguous", func(t *testing.T) {
		_, err := store.resolve(requestAffinity{
			hard:               []affinityRef{{kind: affinityConversation, value: "conversation"}},
			requireUnambiguous: true,
		}, pool)
		if !errors.Is(err, errAffinityAmbiguous) {
			t.Fatalf("error = %v, want ambiguous owner", err)
		}
	})

	t.Run("another hard owner does not resolve an unknown conversation", func(t *testing.T) {
		file := affinityRef{kind: affinityFile, value: "conversation-file"}
		if err := store.bind(file, "account-a"); err != nil {
			t.Fatal(err)
		}
		_, err := store.resolve(requestAffinity{
			hard: []affinityRef{
				{kind: affinityConversation, value: "unknown-conversation"},
				file,
			},
			requireUnambiguous: true,
		}, pool)
		if !errors.Is(err, errAffinityAmbiguous) {
			t.Fatalf("error = %v, want ambiguous owner", err)
		}
	})

	t.Run("hard turn owner proves conversation ownership", func(t *testing.T) {
		conversation := affinityRef{kind: affinityConversation, value: "turn-conversation"}
		got, err := store.resolve(requestAffinity{
			hard:               []affinityRef{turn, conversation},
			requireUnambiguous: true,
		}, pool)
		if err != nil || got.required != "account-a" || !got.hard {
			t.Fatalf("resolution = %+v, error = %v", got, err)
		}
		if !sameAffinityRefs(got.bindings, []affinityRef{turn, conversation}) {
			t.Fatalf("bindings = %#v", got.bindings)
		}
	})

	t.Run("unknown conversation uses the only account", func(t *testing.T) {
		only := &Pool{accounts: []*Account{testAccount("account-a", 10)}}
		got, err := store.resolve(requestAffinity{
			hard:               []affinityRef{{kind: affinityConversation, value: "new-conversation"}},
			requireUnambiguous: true,
		}, only)
		if err != nil || got.required != "account-a" || !got.hard {
			t.Fatalf("resolution = %+v, error = %v", got, err)
		}
	})

	t.Run("removed hard owner is unavailable", func(t *testing.T) {
		removed := affinityRef{kind: affinityTurnState, value: "removed"}
		if err := store.bind(removed, "account-c"); err != nil {
			t.Fatal(err)
		}
		_, err := store.resolve(requestAffinity{hard: []affinityRef{removed}}, pool)
		if !errors.Is(err, errAffinityOwnerUnavailable) {
			t.Fatalf("error = %v, want owner unavailable", err)
		}
	})
}

func sameAffinityRefs(a, b []affinityRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
