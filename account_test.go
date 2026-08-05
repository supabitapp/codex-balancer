package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func withTokenEndpoint(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	original := oauthEndpoint
	oauthEndpoint = srv.URL
	t.Cleanup(func() {
		oauthEndpoint = original
		srv.Close()
	})
	return srv
}

func TestRefreshRotatesTheTokenPair(t *testing.T) {
	withTokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.GrantType != "refresh_token" || req.ClientID != oauthClientID || req.RefreshToken != "RT-old" {
			t.Errorf("unexpected refresh request %+v", req)
		}
		json.NewEncoder(w).Encode(refreshResponse{
			IDToken:      jwtFor("acct-1"),
			AccessToken:  "AT-new",
			RefreshToken: "RT-new",
		})
	})

	a := &Account{IDToken: jwtFor("acct-1"), AccessToken: "AT-old", RefreshToken: "RT-old"}
	if err := a.refresh(context.Background(), http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	if a.AccessToken != "AT-new" {
		t.Fatalf("access token = %q, want AT-new", a.AccessToken)
	}
	if a.RefreshToken != "RT-new" {
		t.Fatalf("refresh token = %q; a rotated token must replace the old one", a.RefreshToken)
	}
	if a.LastRefresh.IsZero() {
		t.Fatal("LastRefresh not stamped, so the account looks stale forever")
	}
}

func TestConcurrentRefreshExchangesOnce(t *testing.T) {
	var calls atomic.Int32
	withTokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		json.NewEncoder(w).Encode(refreshResponse{AccessToken: "AT-new", RefreshToken: "RT-new"})
	})

	a := &Account{IDToken: jwtFor("acct-1"), RefreshToken: "RT-old"}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.refresh(context.Background(), http.DefaultClient)
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("exchanged %d times; the refresh token is single use and must not race", got)
	}
}

func TestPermanentFailureRetiresTheAccount(t *testing.T) {
	withTokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"token already used"}`))
	})

	a := &Account{IDToken: jwtFor("acct-1"), RefreshToken: "RT-old"}
	err := a.refresh(context.Background(), http.DefaultClient)
	if err == nil {
		t.Fatal("expected an error")
	}
	if a.available(time.Now()) {
		t.Fatal("account still selectable after invalid_grant; it needs reauth")
	}
	if err := a.refresh(context.Background(), http.DefaultClient); err == nil ||
		!strings.Contains(err.Error(), "needs reauth") {
		t.Fatalf("retired account should refuse further refreshes, got %v", err)
	}
}

func TestTransientFailureKeepsTheAccount(t *testing.T) {
	withTokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	a := &Account{IDToken: jwtFor("acct-1"), RefreshToken: "RT-old"}
	if err := a.refresh(context.Background(), http.DefaultClient); err == nil {
		t.Fatal("expected an error")
	}
	if !a.available(time.Now()) {
		t.Fatal("a 502 from the token endpoint must not retire the account")
	}
}

func TestUnauthorizedTurnRefreshesThenRetriesSameAccount(t *testing.T) {
	withTokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(refreshResponse{AccessToken: "AT-new", RefreshToken: "RT-new"})
	})

	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		seen = append(seen, token)
		if token == "AT-old" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sse(w, r.Header.Get("chatgpt-account-id"))
	}))
	defer upstream.Close()

	s := testServer(t, upstream.URL, "acct-a")
	s.pool.accounts[0].AccessToken = "AT-old"
	s.pool.accounts[0].RefreshToken = "RT-old"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	s.responses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if len(seen) != 2 || seen[0] != "AT-old" || seen[1] != "AT-new" {
		t.Fatalf("upstream saw %v, want the stale token then the refreshed one", seen)
	}
}

func TestPoolRoundTripsThroughDisk(t *testing.T) {
	path := t.TempDir() + "/accounts.json"
	pool := &Pool{path: path}
	if err := pool.add(&Account{IDToken: jwtFor("acct-1"), AccessToken: "AT", RefreshToken: "RT"}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.accounts) != 1 {
		t.Fatalf("loaded %d accounts, want 1", len(reloaded.accounts))
	}
	if got := reloaded.accounts[0]; got.ID() != "acct-1" || got.RefreshToken != "RT" {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

func TestWaitersSeeTheLeadersRefreshFailure(t *testing.T) {
	withTokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"refresh_token_invalidated"}`))
	})

	a := &Account{IDToken: jwtFor("acct-1"), RefreshToken: "RT-old"}
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = a.refresh(context.Background(), http.DefaultClient)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d saw success; a waiter that reports nil retries with a stale token", i)
		}
	}
}
