package app

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

type responseUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	InputDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u responseUsage) empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 && u.InputDetails.CachedTokens == 0 && u.InputDetails.CacheWriteTokens == 0 && u.OutputDetails.ReasoningTokens == 0
}

func (u *responseUsage) add(other responseUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.TotalTokens += other.TotalTokens
	u.InputDetails.CachedTokens += other.InputDetails.CachedTokens
	u.InputDetails.CacheWriteTokens += other.InputDetails.CacheWriteTokens
	u.OutputDetails.ReasoningTokens += other.OutputDetails.ReasoningTokens
}

func (u responseUsage) contextTokens() int64 {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.InputTokens + u.OutputTokens
}

func (u responseUsage) nonCachedInput() int64 {
	return max(u.InputTokens-max(u.InputDetails.CachedTokens, 0), 0)
}

type responsePayload struct {
	ID          string        `json:"id"`
	Model       string        `json:"model"`
	Status      string        `json:"status"`
	ServiceTier string        `json:"service_tier"`
	Usage       responseUsage `json:"usage"`
}

type tokenRates struct {
	input      int64
	cached     int64
	cacheWrite int64
	output     int64
}

type priceTier struct {
	minInput int64
	rates    tokenRates
}

type modelPrice struct {
	standard []priceTier
	fast     []priceTier
}

type priceSnapshot struct {
	models    map[string]modelPrice
	modelIDs  []string
	fetchedAt time.Time
}

func newPriceSnapshot(models map[string]modelPrice, fetchedAt time.Time) priceSnapshot {
	modelIDs := make([]string, 0, len(models))
	for model := range models {
		modelIDs = append(modelIDs, model)
	}
	slices.SortFunc(modelIDs, func(left, right string) int {
		if len(left) > len(right) {
			return -1
		}
		if len(left) < len(right) {
			return 1
		}
		return strings.Compare(left, right)
	})
	return priceSnapshot{models: models, modelIDs: modelIDs, fetchedAt: fetchedAt}
}

func (s priceSnapshot) estimate(model, serviceTier string, usage responseUsage) (int64, bool) {
	prices, known := s.models[model]
	if !known {
		for _, candidate := range s.modelIDs {
			if strings.HasPrefix(model, candidate+"-") {
				prices = s.models[candidate]
				known = true
				break
			}
		}
	}
	if !known {
		return 0, false
	}

	tiers := prices.standard
	if isFastServiceTier(serviceTier) {
		tiers = prices.fast
	}
	if len(tiers) == 0 {
		return 0, false
	}
	rates := tiers[0].rates
	for _, tier := range tiers[1:] {
		if usage.InputTokens < tier.minInput {
			break
		}
		rates = tier.rates
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
	precision := 1
	if units[unit] == "B" {
		precision = 2
	}
	scale := math.Pow10(precision)
	value = math.Round(value*scale) / scale
	if unit < len(units)-1 && (value <= -1_000 || value >= 1_000) {
		value /= 1_000
		unit++
		precision = 1
		if units[unit] == "B" {
			precision = 2
		}
	}
	formatted := strings.TrimRight(strconv.FormatFloat(value, 'f', precision, 64), "0")
	return strings.TrimSuffix(formatted, ".") + units[unit]
}
