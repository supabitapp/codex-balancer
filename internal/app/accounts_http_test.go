package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestAccountLoginPageRendersEmbeddedTemplate(t *testing.T) {
	var page bytes.Buffer
	if err := accountLoginPage.Execute(&page, accountLoginPageData{
		VerificationURL: "https://example.com/device",
		UserCode:        "ABCD-EFGH",
		ExpiresIn:       15,
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"https://example.com/device", "ABCD-EFGH", "15 minutes"} {
		if !strings.Contains(page.String(), want) {
			t.Fatalf("rendered account page does not contain %q", want)
		}
	}
}
