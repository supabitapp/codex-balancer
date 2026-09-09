package app

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestGenerateAPIKey(t *testing.T) {
	keys, ids := map[string]bool{}, map[string]bool{}
	for range 100 {
		key, err := generateAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		id := testAPIKeyAccountID(t, key)
		if keys[key] || ids[id] {
			t.Fatal("generated duplicate API key or synthetic account ID")
		}
		keys[key], ids[id] = true, true
	}
}

func testAPIKeyAccountID(t *testing.T, key string) string {
	t.Helper()
	parts := strings.Split(key, ".")
	if len(parts) != 3 {
		t.Fatal("API key must have three JWT segments")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var header struct{ Alg, Typ string }
	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Alg != "HS256" || header.Typ != "JWT" {
		t.Fatalf("invalid JWT header: %+v, %v", header, err)
	}
	// Unpatched pi uses atob(), not a base64url-aware decoder, for this segment.
	payloadJSON, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("payload is not compatible with pi's atob decoder: %v", err)
	}
	var payload struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatal(err)
	}
	id := payload.Auth.AccountID
	rawID, err := hex.DecodeString(strings.TrimPrefix(id, "cb_"))
	if !strings.HasPrefix(id, "cb_") || err != nil || len(rawID) != 16 {
		t.Fatalf("invalid synthetic account ID %q", id)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 32 {
		t.Fatalf("invalid credential signature: length = %d, error = %v", len(signature), err)
	}
	return id
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
