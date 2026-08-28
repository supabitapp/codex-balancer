package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestResponsesRejectsHTTP(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	response := httptest.NewRecorder()
	new(server).routes().ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestServerAcceptsMultipleDatabaseAPIKeysAndSeesChanges(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for index, secret := range []string{"secret-a", "secret-b"} {
		if err := store.addAPIKey(storedAPIKey{Name: fmt.Sprintf("client-%d", index), Secret: secret, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	server := &server{lookupAPIKey: store.apiKeyName}
	assertStatus := func(secret string, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if secret != "" {
			request.Header.Set("Authorization", "Bearer "+secret)
		}
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("key %q status = %d, want %d", secret, response.Code, want)
		}
	}
	assertStatus("secret-a", http.StatusOK)
	assertStatus("secret-b", http.StatusOK)
	assertStatus("wrong", http.StatusUnauthorized)
	assertStatus("", http.StatusUnauthorized)

	if err := store.addAPIKey(storedAPIKey{Name: "client-2", Secret: "secret-c", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	assertStatus("secret-c", http.StatusOK)
	if revoked, err := store.revokeAPIKey("client-0", time.Now()); err != nil || !revoked {
		t.Fatalf("revoke = %t, error = %v", revoked, err)
	}
	assertStatus("secret-a", http.StatusUnauthorized)
}

func TestResetHeaderAcceptsHTTPDate(t *testing.T) {
	want := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	headers := http.Header{"Retry-After": {want.Format(http.TimeFormat)}}
	if got := resetHeader(headers); !got.Equal(want) {
		t.Fatalf("reset = %s, want %s", got, want)
	}
}

func TestUpstreamRetryBackoffTotalsFiveSeconds(t *testing.T) {
	var total time.Duration
	for retry := 1; retry <= maxUpstreamRetries; retry++ {
		total += upstreamRetryBackoff(retry)
	}
	if remaining := upstreamRetryBudget - total; remaining < 0 || remaining > time.Nanosecond {
		t.Fatalf("backoff total = %s, want %s", total, upstreamRetryBudget)
	}
}
