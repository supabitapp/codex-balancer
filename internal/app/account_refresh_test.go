package app

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func accessTokenExpiringAt(expiresAt time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(expiresAt.Unix(), 10) + `}`))
	return "x." + payload + ".x"
}

func TestAccountRefreshDueUsesAccessTokenExpiry(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		accessToken string
		lastRefresh time.Time
		want        bool
	}{
		{name: "outside lead", accessToken: accessTokenExpiringAt(now.Add(tokenRefreshLead + time.Second)), lastRefresh: now.Add(-tokenRefreshFallback), want: false},
		{name: "at lead", accessToken: accessTokenExpiringAt(now.Add(tokenRefreshLead)), lastRefresh: now, want: true},
		{name: "expired", accessToken: accessTokenExpiringAt(now.Add(-time.Second)), lastRefresh: now, want: true},
		{name: "fallback fresh", accessToken: "opaque", lastRefresh: now.Add(-tokenRefreshFallback), want: false},
		{name: "fallback old", accessToken: "opaque", lastRefresh: now.Add(-tokenRefreshFallback - time.Second), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := testAccount("account-a", 10)
			account.AccessToken = test.accessToken
			account.LastRefresh = test.lastRefresh
			if got := account.refreshDue(now); got != test.want {
				t.Fatalf("refresh due = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAccountRefreshPublishesTokensAfterPersistence(t *testing.T) {
	refreshCalls := useOAuthRefreshServer(t)
	account := testAccount("account-a", 10)
	original := account.persisted()
	persistCalls := 0
	err := account.refresh(context.Background(), http.DefaultClient, func(next accountState) (accountState, error) {
		persistCalls++
		if got := account.persisted(); got != original {
			t.Fatalf("account changed before persistence: %+v", got)
		}
		if next.AccessToken != "refreshed-token" || next.RefreshToken != "refreshed-refresh" {
			t.Fatalf("persisted tokens = %q, %q", next.AccessToken, next.RefreshToken)
		}
		return next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshCalls() != 1 || persistCalls != 1 {
		t.Fatalf("refresh calls = %d, persist calls = %d", refreshCalls(), persistCalls)
	}
	if got := account.persisted(); got.AccessToken != "refreshed-token" || got.RefreshToken != "refreshed-refresh" {
		t.Fatalf("account tokens = %q, %q", got.AccessToken, got.RefreshToken)
	}
}

func TestAccountRefreshLeavesTokensUnchangedWhenPersistenceFails(t *testing.T) {
	useOAuthRefreshServer(t)
	account := testAccount("account-a", 10)
	original := account.persisted()
	wantErr := errors.New("save failed")
	err := account.refresh(context.Background(), http.DefaultClient, func(accountState) (accountState, error) {
		return accountState{}, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if got := account.persisted(); got != original {
		t.Fatalf("account changed after failed persistence: %+v", got)
	}
}

func TestAccountRefreshCoalescesExchangeAndPersistence(t *testing.T) {
	refreshCalls := useOAuthRefreshServer(t)
	account := testAccount("account-a", 10)
	persistEntered := make(chan struct{})
	releasePersist := make(chan struct{})
	persistCalls := 0
	persist := func(next accountState) (accountState, error) {
		persistCalls++
		close(persistEntered)
		<-releasePersist
		return next, nil
	}
	first := make(chan error, 1)
	go func() {
		first <- account.refresh(context.Background(), http.DefaultClient, persist)
	}()
	<-persistEntered
	second := make(chan error, 1)
	go func() {
		second <- account.refresh(context.Background(), http.DefaultClient, persist)
	}()
	select {
	case err := <-second:
		t.Fatalf("waiting refresh returned early: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(releasePersist)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if refreshCalls() != 1 || persistCalls != 1 {
		t.Fatalf("refresh calls = %d, persist calls = %d", refreshCalls(), persistCalls)
	}
}

func TestAccountRefreshIgnoresFailureAfterCredentialsChange(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "invalid_grant")
	}))
	previous := oauthEndpoint
	oauthEndpoint = oauth.URL
	t.Cleanup(func() {
		oauthEndpoint = previous
		oauth.Close()
	})

	account := testAccount("account-a", 10)
	result := make(chan error, 1)
	go func() {
		result <- account.refresh(context.Background(), http.DefaultClient, nil)
	}()
	<-entered
	next := account.persisted()
	next.AccessToken = "external-access"
	next.RefreshToken = "external-refresh"
	account.applyPersisted(next)
	close(release)

	if err := <-result; err != nil {
		t.Fatal(err)
	}
	state := account.persisted()
	if state.AccessToken != "external-access" || state.RefreshToken != "external-refresh" {
		t.Fatalf("tokens = %q, %q", state.AccessToken, state.RefreshToken)
	}
	if _, _, _, dead := account.health(); dead != "" {
		t.Fatalf("account marked dead: %s", dead)
	}
}

func TestServerRefreshPersistsRotatedTokenForRestart(t *testing.T) {
	useOAuthRefreshServer(t)
	account := testAccount("account-a", 10)
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	stored := accountFromState(account.persisted())
	stored.Paused = true
	if err := pool.add(stored); err != nil {
		t.Fatal(err)
	}
	server := &server{
		pool:   pool,
		client: http.DefaultClient,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if !server.refreshed(account, account.id()) {
		t.Fatal("refresh failed")
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reloaded, err := reopened.readAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded) != 1 {
		t.Fatalf("accounts = %d, want 1", len(reloaded))
	}
	state := reloaded[0].persisted()
	if state.AccessToken != "refreshed-token" || state.RefreshToken != "refreshed-refresh" {
		t.Fatalf("reloaded tokens = %q, %q", state.AccessToken, state.RefreshToken)
	}
	if !state.Paused || !account.paused() {
		t.Fatal("refresh overwrote persisted pause state")
	}
}

func TestServerRefreshFailsWhenRotatedTokenCannotPersist(t *testing.T) {
	useOAuthRefreshServer(t)
	account := testAccount("account-a", 10)
	original := account.persisted()
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.add(account); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	server := &server{
		pool:   pool,
		client: http.DefaultClient,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if server.refreshed(account, account.id()) {
		t.Fatal("refresh succeeded")
	}
	if got := account.persisted(); got != original {
		t.Fatalf("account changed after failed persistence: %+v", got)
	}
}

func TestPermanentRefreshFailurePersistsUntilCredentialsChange(t *testing.T) {
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "invalid_grant")
	}))
	previous := oauthEndpoint
	oauthEndpoint = oauth.URL
	t.Cleanup(func() {
		oauthEndpoint = previous
		oauth.Close()
	})

	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.add(testAccount("account-a", 10)); err != nil {
		t.Fatal(err)
	}
	server := &server{
		pool:   pool,
		client: http.DefaultClient,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if server.refreshed(pool.find("account-a"), "account-a") {
		t.Fatal("permanent refresh failure succeeded")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reloaded, err := loadPool(reopened)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.find("account-a").needsReauth() {
		t.Fatal("reauth state did not survive restart")
	}
	replacement := testAccount("account-a", 10)
	replacement.AccessToken = "new-access-token"
	replacement.RefreshToken = "new-refresh-token"
	if err := reloaded.add(replacement); err != nil {
		t.Fatal(err)
	}
	if reloaded.find("account-a").needsReauth() {
		t.Fatal("new credentials retained reauth state")
	}
}
