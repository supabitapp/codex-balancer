package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccountLoginPageRendersEmbeddedTemplate(t *testing.T) {
	var page bytes.Buffer
	if err := accountLoginPage.Execute(&page, accountLoginPageData{
		VerificationURL: "https://example.com/device",
		UserCode:        "ABCD-EFGH",
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{waterCSSURL, dashboardAssetURL("accounts.css"), dashboardAssetURL("accounts.js"), "https://example.com/device", "ABCD-EFGH", "data-copy-code"} {
		if !strings.Contains(page.String(), want) {
			t.Fatalf("rendered account page does not contain %q", want)
		}
	}
	if strings.Contains(page.String(), "expires") {
		t.Fatal("rendered account page contains expiry copy")
	}
}

func TestCompleteAccountLoginTrainingPolicy(t *testing.T) {
	for _, plan := range []string{"business", "enterprise", "self_serve_business_prolite", "pro", "team", ""} {
		t.Run(plan, func(t *testing.T) {
			store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			pool, err := loadPool(store)
			if err != nil {
				t.Fatal(err)
			}
			source := testAccountWithPlan("account-a", 0, plan).persisted()
			tokens, err := json.Marshal(tokenResponse{
				IDToken:      source.IDToken,
				AccessToken:  source.AccessToken,
				RefreshToken: source.RefreshToken,
			})
			if err != nil {
				t.Fatal(err)
			}
			issuer := "https://auth.example.com"
			settingsCalls := 0
			client := &http.Client{Transport: accountSettingsRoundTrip(func(request *http.Request) (*http.Response, error) {
				var body string
				switch request.URL.Scheme + "://" + request.URL.Host + request.URL.Path {
				case issuer + "/api/accounts/deviceauth/token":
					body = `{"authorization_code":"code","code_verifier":"verifier"}`
				case issuer + "/oauth/token":
					body = string(tokens)
				case accountSettingsEndpoint:
					settingsCalls++
					return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: http.NoBody}, nil
				default:
					t.Fatalf("unexpected request: %s", request.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			device := deviceAuthorization{authID: "auth-id", userCode: "ABCD-EFGH"}
			s := server{
				pool: pool, client: client, stats: newStatsWithPrices(priceSnapshot{}),
				logins: accountLoginStore{active: &accountLogin{device: device}},
			}
			s.completeAccountLogin(context.Background(), issuer, device)
			response := httptest.NewRecorder()
			s.accountLoginStatus(response, httptest.NewRequest(http.MethodGet, "/accounts/status", nil))
			reloaded, err := loadPool(store)
			if err != nil {
				t.Fatal(err)
			}
			account := reloaded.find("account-a")
			if plan != "business" && plan != "enterprise" && plan != "self_serve_business_prolite" {
				if response.Code != http.StatusGone || settingsCalls != 1 || account != nil {
					t.Fatalf("status = %d, settings calls = %d, account saved = %t", response.Code, settingsCalls, account != nil)
				}
				want := fmt.Sprintf("disable training: account settings returned 403 Forbidden (ID token plan: %q)", plan)
				if len(s.stats.events) != 1 || s.stats.events[0].Kind != "account login failed" || s.stats.events[0].Detail != want || s.stats.events[0].Account != "" {
					t.Fatalf("events = %+v, want one login failure with only status and plan", s.stats.events)
				}
				return
			}
			if response.Code != http.StatusOK || settingsCalls != 0 || account == nil {
				t.Fatalf("status = %d, settings calls = %d, account saved = %t", response.Code, settingsCalls, account != nil)
			}
			wantStatus := accountNotRouted
			if plan == "self_serve_business_prolite" {
				wantStatus = accountChecking
			}
			if got := account.status(time.Now()); got != wantStatus {
				t.Fatalf("account status = %s, want %s", got, wantStatus)
			}
		})
	}
}

func TestAccountLoginStatus(t *testing.T) {
	login := accountLogin{device: deviceAuthorization{authID: "auth-id"}}
	tests := []struct {
		name      string
		active    *accountLogin
		completed bool
		wantCode  int
	}{
		{name: "active", active: &login, wantCode: http.StatusNoContent},
		{name: "completed", completed: true, wantCode: http.StatusOK},
		{name: "failed", wantCode: http.StatusGone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := server{logins: accountLoginStore{active: test.active, completed: test.completed}}
			response := httptest.NewRecorder()
			s.accountLoginStatus(response, httptest.NewRequest(http.MethodGet, "/accounts/status", nil))
			if response.Code != test.wantCode {
				t.Fatalf("status code = %d, want %d", response.Code, test.wantCode)
			}
		})
	}
}
