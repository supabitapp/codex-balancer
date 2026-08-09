package main

import "testing"

func TestEstimateAPIPriceUsesCachedAndWrittenTokenRates(t *testing.T) {
	usage := responseUsage{InputTokens: 1_000_000, OutputTokens: 20_000}
	usage.InputDetails.CachedTokens = 100_000
	usage.InputDetails.CacheWriteTokens = 50_000

	got, known := estimateAPIPrice("gpt-5.6-sol", "default", usage)
	if !known || got != 10_125_000_000 {
		t.Fatalf("estimate = %d, %t, want 10125000000, true", got, known)
	}
}

func TestEstimateAPIPriceUsesFastLongContextRates(t *testing.T) {
	usage := responseUsage{InputTokens: 300_000, OutputTokens: 10_000}
	usage.InputDetails.CachedTokens = 100_000
	usage.InputDetails.CacheWriteTokens = 50_000

	got, known := estimateAPIPrice("gpt-5.6-terra-2026-08-01", serviceTierFast, usage)
	if !known || got != 2_140_000_000 {
		t.Fatalf("estimate = %d, %t, want 2140000000, true", got, known)
	}
}

func TestEstimateAPIPriceRejectsUnknownModels(t *testing.T) {
	if _, known := estimateAPIPrice("unknown", "default", responseUsage{InputTokens: 1}); known {
		t.Fatal("unknown model has a price")
	}
}
