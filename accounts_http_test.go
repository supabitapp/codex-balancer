package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

	started := httptest.NewRecorder()
	handler.ServeHTTP(started, httptest.NewRequest(http.MethodGet, "/accounts", nil))
	if started.Code != http.StatusOK {
		t.Fatalf("start status = %d, want 200: %s", started.Code, started.Body)
	}
	if got := started.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	for _, want := range []string{issuer + "/codex/device", "CODE-123", "15 minutes", "href=\"/stats\""} {
		if !strings.Contains(started.Body.String(), want) {
			t.Fatalf("account page does not contain %q: %s", want, started.Body)
		}
	}
	if starts.Load() != 1 {
		t.Fatalf("provider starts = %d, want 1", starts.Load())
	}

	duplicate := httptest.NewRecorder()
	handler.ServeHTTP(duplicate, httptest.NewRequest(http.MethodGet, "/accounts", nil))
	if duplicate.Code != http.StatusConflict || starts.Load() != 1 {
		t.Fatalf("duplicate request returned %d after %d provider calls", duplicate.Code, starts.Load())
	}
	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/accounts", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("removed POST endpoint = %d, want 405", post.Code)
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
