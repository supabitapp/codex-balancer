package app

import (
	"context"
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
	source := testAccount("account-a", 0).persisted()
	client := &http.Client{Transport: accountSettingsRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: http.NoBody}, nil
	})}
	_, err := connectAccount(context.Background(), client, tokenResponse{
		IDToken:      source.IDToken,
		AccessToken:  source.AccessToken,
		RefreshToken: source.RefreshToken,
	})
	if err == nil || !strings.Contains(err.Error(), "account settings returned 403 Forbidden") {
		t.Fatalf("error = %v", err)
	}
}
