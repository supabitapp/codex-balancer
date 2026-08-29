package app

import (
	"testing"
	"time"
)

func TestCodexCreditEstimateUsesModelTokenRates(t *testing.T) {
	usage := responseUsage{InputTokens: 1_000_000, OutputTokens: 100_000}
	usage.InputDetails.CachedTokens = 800_000
	for _, test := range []struct {
		model       string
		serviceTier string
		want        int64
		known       bool
	}{
		{model: "gpt-5.6-sol", want: 78 * nanoCreditsPerCredit, known: true},
		{model: "gpt-5.6-sol-2026-08-01", serviceTier: "priority", want: 195 * nanoCreditsPerCredit, known: true},
		{model: "gpt-5.6-terra", want: 44 * nanoCreditsPerCredit, known: true},
		{model: "gpt-5.6-luna", want: 4_400_000_000, known: true},
		{model: "gpt-5.5", want: 110 * nanoCreditsPerCredit, known: true},
		{model: "gpt-5.4-mini-2026-08-01", want: 16_550_000_000, known: true},
		{model: "gpt-5.4", serviceTier: "fast", want: 110 * nanoCreditsPerCredit, known: true},
		{model: "unknown"},
	} {
		got, known := estimateCodexCredits(test.model, test.serviceTier, usage)
		if got != test.want || known != test.known {
			t.Errorf("%s/%s credits = %d, %t, want %d, %t", test.model, test.serviceTier, got, known, test.want, test.known)
		}
	}
}

func TestCodexCreditEstimateMatchesProAnalyticsSample(t *testing.T) {
	usage := responseUsage{InputTokens: 209_280_000, OutputTokens: 916_000}
	usage.InputDetails.CachedTokens = 200_770_000
	got, known := estimateCodexCredits("gpt-5.6-sol", "default", usage)
	if !known || got != 3_316_700_000_000 {
		t.Fatalf("credits = %d, %t, want 3316.7", got, known)
	}
}

func TestRoutedCreditsUseCycleAndRejectUnknownModels(t *testing.T) {
	stats := newStatsWithPrices(priceSnapshot{})
	start := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	usage := responseUsage{InputTokens: 1_000_000}
	stats.applyUsageAt(start.Add(-time.Hour), "", "account", "gpt-5.6-sol", "", "default", usage)
	stats.applyUsageAt(start.Add(time.Hour), "", "account", "gpt-5.6-sol", "", "default", usage)

	credits, since, known := stats.routedCreditsSince("account", start)
	if !known || credits != 100 || !since.Equal(start) {
		t.Fatalf("credits = %v since %v, %t, want 100 since reset", credits, since, known)
	}
	credits, since, known = stats.routedCreditsSince("account", start.Add(2*time.Hour))
	if !known || credits != 0 || !since.Equal(start.Add(2*time.Hour)) {
		t.Fatalf("credits after usage = %v since %v, %t, want zero", credits, since, known)
	}

	stats.applyUsageAt(start.Add(3*time.Hour), "", "account", "unknown", "", "default", usage)
	if credits, since, known = stats.routedCreditsSince("account", start); known {
		t.Fatalf("unpriced credits = %v since %v, true", credits, since)
	}
}

func TestRoutedCreditsReportFirstTrackedResponse(t *testing.T) {
	stats := newStatsWithPrices(priceSnapshot{})
	start := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	tracked := start.Add(3 * time.Hour)
	stats.applyUsageAt(tracked, "", "account", "gpt-5.6-luna", "", "default", responseUsage{InputTokens: 1_000_000})

	credits, since, known := stats.routedCreditsSince("account", start)
	if !known || credits != 5 || !since.Equal(tracked) {
		t.Fatalf("credits = %v since %v, %t, want 5 since first response", credits, since, known)
	}
}

func TestRoutedCreditsPruneOldResponsesWithoutChangingCycleTotal(t *testing.T) {
	var account accountStats
	start := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	old := start.Add(-10 * 24 * time.Hour)
	for index := range 1_100 {
		account.addRoutedCredits(old.Add(time.Duration(index)*time.Second), nanoCreditsPerCredit, true)
	}
	account.addRoutedCredits(start.Add(time.Hour), 2*nanoCreditsPerCredit, true)

	credits, since, known := account.routedCreditsSince(start)
	if !known || credits != 2 || !since.Equal(start) {
		t.Fatalf("credits = %v since %v, %t, want 2 since reset", credits, since, known)
	}
}

func BenchmarkCodexCreditEstimate(b *testing.B) {
	usage := responseUsage{InputTokens: 1_000_000, OutputTokens: 100_000}
	usage.InputDetails.CachedTokens = 800_000
	b.ReportAllocs()
	for range b.N {
		estimateCodexCredits("gpt-5.6-sol", "priority", usage)
	}
}

func BenchmarkRoutedCreditsSince100KResponses(b *testing.B) {
	stats := newStatsWithPrices(priceSnapshot{})
	account := stats.account("account")
	start := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	for index := range 100_000 {
		account.addRoutedCredits(start.Add(time.Duration(index)*time.Second), nanoCreditsPerCredit, true)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		stats.routedCreditsSince("account", start.Add(12*time.Hour))
	}
}
