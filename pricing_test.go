package main

import (
	"testing"
	"time"
)

const testModelsDevCatalog = `{
	"openai": {
		"models": {
			"gpt-5.6-sol": {
				"cost": {
					"input": 5,
					"output": 30,
					"cache_read": 0.5,
					"cache_write": 6.25,
					"tiers": [{
						"input": 10,
						"output": 45,
						"cache_read": 1,
						"cache_write": 12.5,
						"tier": {"type": "context", "size": 272000}
					}]
				},
				"experimental": {
					"modes": {
						"fast": {
							"cost": {
								"input": 10,
								"output": 60,
								"cache_read": 1,
								"cache_write": 12.5,
								"tiers": [{
									"input": 20,
									"output": 90,
									"cache_read": 2,
									"cache_write": 25,
									"tier": {"type": "context", "size": 272000}
								}]
							}
						}
					}
				}
			},
			"gpt-5.4": {
				"cost": {
					"input": 2.5,
					"output": 15,
					"cache_read": 0.25,
					"tiers": [{
						"input": 5,
						"output": 22.5,
						"cache_read": 0.5,
						"tier": {"type": "context", "size": 272000}
					}]
				},
				"experimental": {
					"modes": {
						"fast": {
							"cost": {"input": 5, "output": 30, "cache_read": 0.5}
						}
					}
				}
			},
			"gpt-5.4-mini": {
				"cost": {"input": 0.75, "output": 4.5, "cache_read": 0.075}
			}
		}
	}
}`

func testPriceSnapshot(t *testing.T) priceSnapshot {
	t.Helper()
	snapshot, _, err := parseModelsDevPriceCatalog([]byte(testModelsDevCatalog), time.Date(2026, time.August, 11, 15, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestEstimateAPIPriceUsesCachedAndWrittenTokenRates(t *testing.T) {
	usage := responseUsage{InputTokens: 1_000_000, OutputTokens: 20_000}
	usage.InputDetails.CachedTokens = 100_000
	usage.InputDetails.CacheWriteTokens = 50_000

	got, known := testPriceSnapshot(t).estimate("gpt-5.6-sol", "default", usage)
	if !known || got != 10_125_000_000 {
		t.Fatalf("estimate = %d, %t, want 10125000000, true", got, known)
	}
}

func TestEstimateAPIPriceUsesFastLongContextRates(t *testing.T) {
	usage := responseUsage{InputTokens: 300_000, OutputTokens: 10_000}
	usage.InputDetails.CachedTokens = 100_000
	usage.InputDetails.CacheWriteTokens = 50_000

	got, known := testPriceSnapshot(t).estimate("gpt-5.6-sol-2026-08-01", serviceTierFast, usage)
	if !known || got != 5_350_000_000 {
		t.Fatalf("estimate = %d, %t, want 5350000000, true", got, known)
	}
}

func TestEstimateAPIPriceUsesFastBaseRateWithoutFastTiers(t *testing.T) {
	usage := responseUsage{InputTokens: 300_000, OutputTokens: 10_000}

	got, known := testPriceSnapshot(t).estimate("gpt-5.4", serviceTierFast, usage)
	if !known || got != 1_800_000_000 {
		t.Fatalf("estimate = %d, %t, want 1800000000, true", got, known)
	}
}

func TestEstimateAPIPriceUsesInputRateForCacheWritesWithoutSeparateRate(t *testing.T) {
	usage := responseUsage{InputTokens: 1_000}
	usage.InputDetails.CachedTokens = 200
	usage.InputDetails.CacheWriteTokens = 100

	got, known := testPriceSnapshot(t).estimate("gpt-5.4", "default", usage)
	if !known || got != 2_050_000 {
		t.Fatalf("estimate = %d, %t, want 2050000, true", got, known)
	}
}

func TestEstimateAPIPriceUsesLongestModelMatch(t *testing.T) {
	usage := responseUsage{InputTokens: 1_000, OutputTokens: 100}

	got, known := testPriceSnapshot(t).estimate("gpt-5.4-mini-2026-08-01", "default", usage)
	if !known || got != 1_200_000 {
		t.Fatalf("estimate = %d, %t, want 1200000, true", got, known)
	}
}

func TestEstimateAPIPriceRejectsUnknownModels(t *testing.T) {
	if _, known := testPriceSnapshot(t).estimate("unknown", "default", responseUsage{InputTokens: 1}); known {
		t.Fatal("unknown model has a price")
	}
}

func TestParseModelsDevPriceCatalogRejectsInvalidRates(t *testing.T) {
	_, _, err := parseModelsDevPriceCatalog([]byte(`{"openai":{"models":{"gpt":{"cost":{"input":-1,"output":1}}}}}`), time.Now())
	if err == nil {
		t.Fatal("invalid price catalog accepted")
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
