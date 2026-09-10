package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestRedeemDoesNotChangeAccountSettings(t *testing.T) {
	for _, plan := range []string{"free", "go", "plus", "pro", "prolite", "team", "", "unknown", "self_serve_business_unknown", "business", "enterprise", "self_serve_business_prolite", " Business ", "ENTERPRISE", " SELF_SERVE_BUSINESS_PROLITE "} {
		t.Run(plan, func(t *testing.T) {
			source := testAccountWithPlan("account-a", 0, plan).persisted()
			tokens, err := json.Marshal(tokenResponse{
				IDToken:      source.IDToken,
				AccessToken:  source.AccessToken,
				RefreshToken: source.RefreshToken,
			})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Method != http.MethodPost || request.URL.String() != oauthEndpoint {
					t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(tokens)))}, nil
			})}
			account, err := redeem(context.Background(), client, url.Values{"code": {"code"}}, "verifier")
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("requests = %d, want only the token exchange", calls)
			}
			state := account.persisted()
			if state.IDToken != source.IDToken || state.AccessToken != source.AccessToken || state.RefreshToken != source.RefreshToken || state.LastRefresh.IsZero() {
				t.Fatal("connected account did not retain credentials and refresh time")
			}
		})
	}
}

func TestConnectAccountRejectsIncompleteCredentials(t *testing.T) {
	for _, plan := range []string{"pro", "business", "enterprise", "self_serve_business_prolite"} {
		for _, missing := range []string{"access token", "account ID"} {
			t.Run(plan+"/"+missing, func(t *testing.T) {
				source := testAccountWithPlan("account-a", 0, plan).persisted()
				if missing == "access token" {
					source.AccessToken = ""
				} else {
					source.IDToken = testAccountWithPlan("", 0, plan).persisted().IDToken
				}
				account, err := connectAccount(tokenResponse{
					IDToken:      source.IDToken,
					AccessToken:  source.AccessToken,
					RefreshToken: source.RefreshToken,
				})
				if err == nil || err.Error() != "account credentials are incomplete" || account != nil {
					t.Fatalf("error = %v, account returned = %t", err, account != nil)
				}
			})
		}
	}
}
