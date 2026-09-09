package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type accountSettingsRoundTrip func(*http.Request) (*http.Response, error)

func (roundTrip accountSettingsRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestConnectAccountDisablesTraining(t *testing.T) {
	source := testAccount("account-a", 0).persisted()
	calls := 0
	client := &http.Client{Transport: accountSettingsRoundTrip(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", request.Method)
		}
		if request.URL.Scheme+"://"+request.URL.Host+request.URL.Path != accountSettingsEndpoint {
			t.Errorf("endpoint = %s", request.URL)
		}
		if request.URL.Query().Get("feature") != "training_allowed" || request.URL.Query().Get("value") != "false" {
			t.Errorf("query = %s", request.URL.RawQuery)
		}
		if request.Header.Get("Authorization") != "Bearer token-account-a" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		if request.Header.Get("chatgpt-account-id") != "account-a" {
			t.Errorf("account = %q", request.Header.Get("chatgpt-account-id"))
		}
		return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content", Body: http.NoBody}, nil
	})}
	account, err := connectAccount(context.Background(), client, tokenResponse{
		IDToken:      source.IDToken,
		AccessToken:  source.AccessToken,
		RefreshToken: source.RefreshToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || account.id() != "account-a" {
		t.Fatalf("calls = %d, account = %q", calls, account.id())
	}
}

func TestConnectAccountRejectsTrainingSettingFailure(t *testing.T) {
	for _, plan := range []string{"free", "go", "plus", "pro", "prolite", "team", "", "unknown"} {
		t.Run(plan, func(t *testing.T) {
			source := testAccountWithPlan("account-a", 0, plan).persisted()
			calls := 0
			client := &http.Client{Transport: accountSettingsRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Status:     "403 Forbidden",
					Body:       io.NopCloser(strings.NewReader("sensitive upstream response")),
				}, nil
			})}
			account, err := connectAccount(context.Background(), client, tokenResponse{
				IDToken:      source.IDToken,
				AccessToken:  source.AccessToken,
				RefreshToken: source.RefreshToken,
			})
			// Only the status and plan belong in this diagnostic, not credentials,
			// account identifiers, or the upstream response body.
			want := fmt.Sprintf("disable training: account settings returned 403 Forbidden (ID token plan: %q)", plan)
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %s", err, want)
			}
			if calls != 1 || account != nil {
				t.Fatalf("calls = %d, account returned = %t", calls, account != nil)
			}
		})
	}
}

func TestConnectAccountSkipsTrainingSettingForManagedWorkspaces(t *testing.T) {
	for _, plan := range []string{"business", "enterprise", " Business ", "ENTERPRISE"} {
		t.Run(plan, func(t *testing.T) {
			source := testAccountWithPlan("workspace", 0, plan).persisted()
			client := &http.Client{Transport: accountSettingsRoundTrip(func(*http.Request) (*http.Response, error) {
				t.Error("managed workspace must not request the personal training setting")
				return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: http.NoBody}, nil
			})}
			account, err := connectAccount(context.Background(), client, tokenResponse{
				IDToken:      source.IDToken,
				AccessToken:  source.AccessToken,
				RefreshToken: source.RefreshToken,
			})
			if err != nil {
				t.Fatal(err)
			}
			state := account.persisted()
			if state.IDToken != source.IDToken || state.AccessToken != source.AccessToken || state.RefreshToken != source.RefreshToken || state.LastRefresh.IsZero() {
				t.Fatal("connected account did not retain credentials and refresh time")
			}
			if account.routingCandidate().routingEnabled() {
				t.Fatal("managed workspace must remain excluded from routing")
			}
		})
	}
}

func TestConnectAccountRejectsIncompleteCredentials(t *testing.T) {
	for _, plan := range []string{"pro", "business", "enterprise"} {
		for _, missing := range []string{"access token", "account ID"} {
			t.Run(plan+"/"+missing, func(t *testing.T) {
				source := testAccountWithPlan("account-a", 0, plan).persisted()
				if missing == "access token" {
					source.AccessToken = ""
				} else {
					source.IDToken = testAccountWithPlan("", 0, plan).persisted().IDToken
				}
				client := &http.Client{Transport: accountSettingsRoundTrip(func(*http.Request) (*http.Response, error) {
					t.Error("incomplete credentials must not make a settings request")
					return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content", Body: http.NoBody}, nil
				})}
				account, err := connectAccount(context.Background(), client, tokenResponse{
					IDToken:      source.IDToken,
					AccessToken:  source.AccessToken,
					RefreshToken: source.RefreshToken,
				})
				if err == nil || !strings.Contains(err.Error(), "account credentials are incomplete") || account != nil {
					t.Fatalf("error = %v, account returned = %t", err, account != nil)
				}
			})
		}
	}
}
