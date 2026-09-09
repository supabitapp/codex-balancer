package app

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAdminIssuesPiCompatibleAPIKeys(t *testing.T) {
	srv := newTestServer(t, nil)
	enableTestAdmin(t, srv)
	handler := srv.routes()
	cookie, csrf := loginTestAdmin(t, handler)
	response := adminRequest(handler, http.MethodPost, "/admin/keys/add", url.Values{
		"csrf": {csrf}, "name": {"pi"},
	}, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("key creation status = %d", response.Code)
	}
	keys, err := srv.pool.store.readAPIKeys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("stored keys = %d, error = %v", len(keys), err)
	}
	testAPIKeyAccountID(t, keys[0].Secret)
	if !strings.Contains(response.Body.String(), keys[0].Secret) {
		t.Fatal("creation response omitted the new key")
	}
	page := adminRequest(handler, http.MethodGet, "/admin", nil, cookie)
	if strings.Contains(page.Body.String(), keys[0].Secret) {
		t.Fatal("key list exposed the existing secret")
	}
}

func TestJWTAPIKeysRequireAnExactStoredMatch(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	key, err := generateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	id := testAPIKeyAccountID(t, key)
	secrets := map[string]string{
		"jwt":    key,
		"legacy": "cb_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		"uuid":   "237aed98-b74a-49ca-8c61-a32707e5827c",
	}
	for name, secret := range secrets {
		if err := store.addAPIKey(storedAPIKey{Name: name, Secret: secret, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store = reopened
	srv := &server{lookupAPIKey: store.apiKeyName}
	handler := srv.routes()
	assertStatus := func(t *testing.T, secret string, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		request.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("status = %d, want %d", response.Code, want)
		}
	}
	for name, secret := range secrets {
		assertStatus(t, secret, http.StatusOK)
		request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		request.Header.Set("Authorization", "Bearer "+secret)
		identity, valid := srv.authorizeAPIKey(request)
		if !valid || identity.name != name || identity.suffix != tokenSuffix(secret) {
			t.Fatalf("key identity = %+v, valid = %t, want %s", identity, valid, name)
		}
	}

	parts := strings.Split(key, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	tamperedPayload := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(payload), id, "another-account", 1)))
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	signature[0] ^= 1
	unsignedHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	unprovisioned, err := generateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	for name, forged := range map[string]string{
		"payload changed":   parts[0] + "." + tamperedPayload + "." + parts[2],
		"signature changed": parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(signature),
		"unsigned JWT":      unsignedHeader + "." + parts[1] + ".",
		"signature removed": parts[0] + "." + parts[1] + ".",
		"header changed":    unsignedHeader + "." + parts[1] + "." + parts[2],
		"unprovisioned JWT": unprovisioned,
		"synthetic ID only": id,
	} {
		t.Run(name, func(t *testing.T) { assertStatus(t, forged, http.StatusUnauthorized) })
	}

	if err := store.recordUsage(storedUsage{
		At: time.Now(), APIKeyName: "jwt", Model: "gpt-5.5",
		Usage: responseUsage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10},
	}); err != nil {
		t.Fatal(err)
	}
	usage, err := store.apiKeyUsage()
	if err != nil || usage["jwt"].TotalTokens != 10 {
		t.Fatalf("JWT key usage = %+v, error = %v", usage, err)
	}
	if revoked, err := store.revokeAPIKey("jwt", time.Now()); err != nil || !revoked {
		t.Fatalf("revoke = %t, error = %v", revoked, err)
	}
	assertStatus(t, key, http.StatusUnauthorized)
	assertStatus(t, secrets["legacy"], http.StatusOK)
	assertStatus(t, secrets["uuid"], http.StatusOK)
}
