package main

import "testing"

func TestMaskEmailHidesLocalPartAndDomain(t *testing.T) {
	for email, want := range map[string]string{
		"khoi@example.com":     "k***i@***.com",
		"khoi@example.net":     "k***i@***.net",
		"khoi@mail.example.uk": "k***i@***.uk",
		"ab@localhost":         "a***@***",
		"a@example.com":        "***@***.com",
		"not-an-email":         "***",
		"":                     "",
	} {
		if got := maskEmail(email); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", email, got, want)
		}
	}
}
