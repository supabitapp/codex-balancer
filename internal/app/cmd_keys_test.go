package app

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestGenerateAPIKey(t *testing.T) {
	first, err := generateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("generated duplicate API keys")
	}
	if !strings.HasPrefix(first, "cb_") {
		t.Fatalf("key = %q, want cb_ prefix", first)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(first, "cb_"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 32 {
		t.Fatalf("decoded key length = %d, want 32", len(raw))
	}
}

func TestLegacyAPIKeyIsImportedOnlyIntoUnusedStore(t *testing.T) {
	store, err := openStateStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := importLegacyAPIKey(store, "old-secret"); err != nil {
		t.Fatal(err)
	}
	if err := importLegacyAPIKey(store, "replacement-secret"); err != nil {
		t.Fatal(err)
	}
	if valid, err := store.validAPIKey("old-secret"); err != nil || !valid {
		t.Fatalf("old key valid = %t, error = %v", valid, err)
	}
	if valid, err := store.validAPIKey("replacement-secret"); err != nil || valid {
		t.Fatalf("replacement key valid = %t, error = %v", valid, err)
	}
	if revoked, err := store.revokeAPIKey("legacy", time.Now()); err != nil || !revoked {
		t.Fatalf("revoke = %t, error = %v", revoked, err)
	}
	if err := importLegacyAPIKey(store, "old-secret"); err != nil {
		t.Fatal(err)
	}
	if count, err := store.activeAPIKeyCount(); err != nil || count != 0 {
		t.Fatalf("active keys = %d, error = %v", count, err)
	}
}
