package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAccountEndpointCompletesDeviceLogin(t *testing.T) {
	approved := make(chan struct{})
	var approve sync.Once
	release := func() { approve.Do(func() { close(approved) }) }
	var issuer string
	var starts atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			starts.Add(1)
			json.NewEncoder(w).Encode(deviceAuthorizationResponse{
				ID:       "DEVICE",
				Code:     "CODE-123",
				Interval: "0",
			})
		case "/api/accounts/deviceauth/token":
			<-approved
			json.NewEncoder(w).Encode(deviceTokenResponse{
				AuthorizationCode: "AUTHORIZATION",
				CodeVerifier:      "VERIFIER",
			})
		case "/oauth/token":
			r.ParseForm()
			want := url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {"AUTHORIZATION"},
				"redirect_uri":  {issuer + "/deviceauth/callback"},
				"client_id":     {oauthClientID},
				"code_verifier": {"VERIFIER"},
			}
			if r.PostForm.Encode() != want.Encode() {
				t.Errorf("token exchange = %v, want %v", r.PostForm, want)
			}
			json.NewEncoder(w).Encode(tokenResponse{
				IDToken:      jwtForEmail("khoi.nguyen@example.com", "acct-new"),
				AccessToken:  "AT-private",
				RefreshToken: "RT-private",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	defer release()
	issuer = provider.URL

	s := testServer(t, "http://unused")
	s.authIssuer = issuer
	s.client = provider.Client()
	s.key = "secret"
	handler := s.routes()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/accounts", nil))
	if unauthorized.Code != http.StatusUnauthorized || starts.Load() != 0 {
		t.Fatalf("unauthorized request returned %d after %d provider calls", unauthorized.Code, starts.Load())
	}

	started := httptest.NewRecorder()
	startRequest := httptest.NewRequest(http.MethodPost, "/accounts", nil)
	startRequest.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(started, startRequest)
	if started.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202: %s", started.Code, started.Body)
	}
	var pending accountLoginResponse
	if err := json.Unmarshal(started.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" || pending.UserCode != "CODE-123" {
		t.Fatalf("pending login = %+v", pending)
	}
	if pending.VerificationURL != issuer+"/codex/device" || pending.ExpiresAt.IsZero() {
		t.Fatalf("pending login = %+v", pending)
	}
	if started.Header().Get("Location") != "" {
		t.Fatalf("removed status endpoint returned location %q", started.Header().Get("Location"))
	}

	duplicate := httptest.NewRecorder()
	duplicateRequest := httptest.NewRequest(http.MethodPost, "/accounts", nil)
	duplicateRequest.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(duplicate, duplicateRequest)
	if duplicate.Code != http.StatusConflict || starts.Load() != 1 {
		t.Fatalf("duplicate request returned %d after %d provider calls", duplicate.Code, starts.Load())
	}

	removed := httptest.NewRecorder()
	handler.ServeHTTP(removed, httptest.NewRequest(http.MethodGet, "/accounts/login-id", nil))
	if removed.Code != http.StatusNotFound {
		t.Fatalf("removed status endpoint = %d, want 404", removed.Code)
	}

	release()
	waitFor(t, func() bool {
		return s.pool.find("acct-new") != nil
	})

	if s.pool.count() != 1 || s.pool.find("acct-new") == nil {
		t.Fatalf("pool accounts = %v", s.pool.all())
	}
	reloaded, err := loadPool(s.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.find("acct-new") == nil {
		t.Fatal("added account was not saved")
	}
}
