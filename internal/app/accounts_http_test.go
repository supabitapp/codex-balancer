package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestAccountLoginStatus(t *testing.T) {
	login := accountLogin{device: deviceAuthorization{authID: "auth-id"}}
	tests := []struct {
		name     string
		store    accountLoginStore
		wantCode int
	}{
		{name: "active", store: accountLoginStore{active: &login}, wantCode: http.StatusNoContent},
		{name: "completed", store: accountLoginStore{completed: true}, wantCode: http.StatusOK},
		{name: "failed", store: accountLoginStore{}, wantCode: http.StatusGone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := server{logins: test.store}
			response := httptest.NewRecorder()
			s.accountLoginStatus(response, httptest.NewRequest(http.MethodGet, "/accounts/status", nil))
			if response.Code != test.wantCode {
				t.Fatalf("status code = %d, want %d", response.Code, test.wantCode)
			}
		})
	}
}
