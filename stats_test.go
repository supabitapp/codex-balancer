package main

import (
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
