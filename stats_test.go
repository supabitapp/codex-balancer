package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

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

func TestRequestIPUsesLastForwardedAddress(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.RemoteAddr = "10.0.0.1:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.7, 203.0.113.9")
	if got := requestIP(request); got != "203.0.113.9" {
		t.Fatalf("requestIP() = %q, want 203.0.113.9", got)
	}
}

func TestRequestIPFallsBackToRemoteAddress(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.RemoteAddr = "[2001:db8::1]:1234"
	if got := requestIP(request); got != "2001:db8::1" {
		t.Fatalf("requestIP() = %q, want 2001:db8::1", got)
	}
}

func TestMaskIPHidesHost(t *testing.T) {
	for ip, want := range map[string]string{
		"203.0.113.42":       "203.0.113.***",
		"::ffff:192.0.2.128": "192.0.2.***",
		"2001:db8:1:2:3::4":  "2001:db8:1:2:****",
		"invalid":            "",
		"":                   "",
	} {
		if got := maskIP(ip); got != want {
			t.Errorf("maskIP(%q) = %q, want %q", ip, got, want)
		}
	}
}

func TestAPIEstimateResetsAtMonthBoundary(t *testing.T) {
	stats := newStats()
	previousMonth := time.Date(2026, time.July, 31, 23, 59, 0, 0, time.UTC)
	currentMonth := previousMonth.Add(time.Minute)
	stats.apiMonth = calendarMonth(previousMonth)
	usage := responseUsage{InputTokens: 1_000}
	stats.applyUsageAt(previousMonth, "unknown", "default", usage)
	stats.applyUsageAt(currentMonth, "gpt-5.6-sol", "default", usage)
	want, _ := estimateAPIPrice("gpt-5.6-sol", "default", usage)
	if stats.apiCostNanoDollars != want || stats.unpricedResponses != 0 {
		t.Fatalf("API estimate = %d with %d unpriced, want %d with none", stats.apiCostNanoDollars, stats.unpricedResponses, want)
	}
}
