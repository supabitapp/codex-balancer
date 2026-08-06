package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDeviceLoginPollsAndExchangesTheCode(t *testing.T) {
	var issuer string
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			var request struct {
				ClientID string `json:"client_id"`
			}
			json.NewDecoder(r.Body).Decode(&request)
			if request.ClientID != oauthClientID {
				t.Errorf("client id = %q", request.ClientID)
			}
			json.NewEncoder(w).Encode(map[string]string{
				"device_auth_id": "DEVICE",
				"user_code":      "CODE-123",
				"interval":       "0",
			})
		case "/api/accounts/deviceauth/token":
			var request deviceTokenRequest
			json.NewDecoder(r.Body).Decode(&request)
			if request.ID != "DEVICE" || request.Code != "CODE-123" {
				t.Errorf("poll request = %+v", request)
			}
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{
				"authorization_code": "AUTHORIZATION",
				"code_challenge":     "CHALLENGE",
				"code_verifier":      "VERIFIER",
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
				IDToken:      jwtFor("acct-1"),
				AccessToken:  "AT",
				RefreshToken: "RT",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL

	var output strings.Builder
	account, err := loginWithDeviceCode(t.Context(), server.Client(), &output, issuer)
	if err != nil {
		t.Fatal(err)
	}
	if polls != 2 {
		t.Fatalf("polled %d times, want 2", polls)
	}
	if !strings.Contains(output.String(), issuer+"/codex/device") || !strings.Contains(output.String(), "CODE-123") {
		t.Fatalf("prompt = %q", output.String())
	}
	if account.id() != "acct-1" || account.RefreshToken != "RT" {
		t.Fatalf("account = %+v", account)
	}
	if account.LastRefresh.IsZero() {
		t.Fatal("LastRefresh not stamped")
	}
}

func TestDeviceLoginReportsWhenDisabled(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, err := loginWithDeviceCode(t.Context(), server.Client(), &strings.Builder{}, server.URL)
	if err == nil || !strings.Contains(err.Error(), "device code login is not enabled") {
		t.Fatalf("err = %v", err)
	}
}
