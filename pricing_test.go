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

func TestFormatTokenCount(t *testing.T) {
	tests := map[int64]string{
		0:             "0",
		999:           "999",
		1_000:         "1K",
		1_250:         "1.3K",
		999_949:       "999.9K",
		999_950:       "1M",
		1_293_911:     "1.3M",
		407_278_819:   "407.3M",
		2_000_000_000: "2B",
		3_140_000_000: "3.14B",
		3_100_000_000: "3.1B",
	}
	for tokens, want := range tests {
		if got := formatTokenCount(tokens); got != want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tokens, got, want)
		}
	}
}
