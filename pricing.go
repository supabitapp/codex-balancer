package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const longContextTokens = 272_000

type responseUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	InputDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

func (u responseUsage) empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.InputDetails.CachedTokens == 0 && u.InputDetails.CacheWriteTokens == 0
}

type responsePayload struct {
	ID          string        `json:"id"`
	Model       string        `json:"model"`
	ServiceTier string        `json:"service_tier"`
	Usage       responseUsage `json:"usage"`
}

type tokenRates struct {
	input      int64
	cached     int64
	cacheWrite int64
	output     int64
}

type modelRates struct {
	model        string
	standard     tokenRates
	standardLong tokenRates
	fast         tokenRates
	fastLong     tokenRates
}

var apiModelRates = []modelRates{
	{
		model:        "gpt-5.6-sol",
		standard:     tokenRates{input: 5_000, cached: 500, cacheWrite: 6_250, output: 30_000},
		standardLong: tokenRates{input: 10_000, cached: 1_000, cacheWrite: 12_500, output: 45_000},
		fast:         tokenRates{input: 10_000, cached: 1_000, cacheWrite: 12_500, output: 60_000},
		fastLong:     tokenRates{input: 20_000, cached: 2_000, cacheWrite: 25_000, output: 90_000},
	},
	{
		model:        "gpt-5.6-terra",
		standard:     tokenRates{input: 2_000, cached: 200, cacheWrite: 2_500, output: 12_000},
		standardLong: tokenRates{input: 4_000, cached: 400, cacheWrite: 5_000, output: 18_000},
		fast:         tokenRates{input: 4_000, cached: 400, cacheWrite: 5_000, output: 24_000},
		fastLong:     tokenRates{input: 8_000, cached: 800, cacheWrite: 10_000, output: 36_000},
	},
	{
		model:        "gpt-5.6-luna",
		standard:     tokenRates{input: 200, cached: 20, cacheWrite: 250, output: 1_200},
		standardLong: tokenRates{input: 400, cached: 40, cacheWrite: 500, output: 1_800},
		fast:         tokenRates{input: 400, cached: 40, cacheWrite: 500, output: 2_400},
		fastLong:     tokenRates{input: 800, cached: 80, cacheWrite: 1_000, output: 3_600},
	},
}

func estimateAPIPrice(model, serviceTier string, usage responseUsage) (int64, bool) {
	var prices *modelRates
	for i := range apiModelRates {
		if model == apiModelRates[i].model || strings.HasPrefix(model, apiModelRates[i].model+"-") {
			prices = &apiModelRates[i]
			break
		}
	}
	if prices == nil {
		return 0, false
	}

	rates := prices.standard
	if usage.InputTokens >= longContextTokens {
		rates = prices.standardLong
	}
	if isFastServiceTier(serviceTier) {
		rates = prices.fast
		if usage.InputTokens >= longContextTokens {
			rates = prices.fastLong
		}
	}

	cached := usage.InputDetails.CachedTokens
	cacheWrite := usage.InputDetails.CacheWriteTokens
	input := max(usage.InputTokens-cached-cacheWrite, 0)
	return input*rates.input + cached*rates.cached + cacheWrite*rates.cacheWrite + usage.OutputTokens*rates.output, true
}

func isFastServiceTier(serviceTier string) bool {
	return serviceTier == serviceTierFast || serviceTier == "fast"
}

func formatAPIPrice(nanoDollars int64, unpricedResponses int64) string {
	if unpricedResponses > 0 {
		return "--"
	}
	if nanoDollars == 0 {
		return "$0.00"
	}
	dollars := float64(nanoDollars) / 1_000_000_000
	if dollars < 0.01 {
		return fmt.Sprintf("$%.4f", dollars)
	}
	if dollars < 1 {
		return fmt.Sprintf("$%.3f", dollars)
	}
	return fmt.Sprintf("$%.2f", dollars)
}

func formatTokenCount(tokens int64) string {
	units := [...]string{"", "K", "M", "B", "T"}
	value := float64(tokens)
	unit := 0
	for unit < len(units)-1 && (value <= -1_000 || value >= 1_000) {
		value /= 1_000
		unit++
	}
	if unit == 0 {
		return strconv.FormatInt(tokens, 10)
	}
	value = math.Round(value*10) / 10
	if unit < len(units)-1 && (value <= -1_000 || value >= 1_000) {
		value /= 1_000
		unit++
	}
	return strings.TrimSuffix(strconv.FormatFloat(value, 'f', 1, 64), ".0") + units[unit]
}
