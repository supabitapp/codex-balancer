package app

import (
	"sort"
	"strings"
	"time"
)

const (
	nanoCreditsPerCredit = 1_000_000_000
	routedCreditHistory  = 8 * 24 * time.Hour
)

type creditMultiplier struct {
	numerator   int64
	denominator int64
}

type creditRates struct {
	model     string
	input     int64
	cached    int64
	output    int64
	fast      creditMultiplier
	ultrafast creditMultiplier
}

var (
	includedFast      = creditMultiplier{5, 2}
	includedUltrafast = creditMultiplier{8, 1}
)

var codexCreditRates = []creditRates{
	{model: "gpt-5.6-sol", input: 100_000, cached: 10_000, output: 500_000, fast: includedFast},
	{model: "gpt-5.6-terra", input: 50_000, cached: 5_000, output: 300_000, fast: includedFast},
	{model: "gpt-5.6-luna", input: 5_000, cached: 500, output: 30_000, fast: includedFast},
	{model: "gpt-5.5", input: 125_000, cached: 12_500, output: 750_000, fast: includedFast},
	{model: "gpt-5.4-mini", input: 18_750, cached: 1_875, output: 113_000, fast: creditMultiplier{2, 1}},
	{model: "gpt-5.4", input: 62_500, cached: 6_250, output: 375_000, fast: creditMultiplier{2, 1}},
	{model: "daybreak-blue", input: 100_000, cached: 10_000, output: 500_000},
	{model: "daybreak-red", input: 312_500, cached: 31_250, output: 1_875_000},
	{model: "gpt-6-astra", input: 250_000, cached: 25_000, output: 1_250_000, fast: includedFast, ultrafast: includedUltrafast},
	{model: "gpt-6.1-sol", input: 50_000, cached: 2_500, output: 250_000, fast: includedFast, ultrafast: includedUltrafast},
	{model: "gpt-6.1-terra", input: 25_000, cached: 1_250, output: 125_000, fast: includedFast},
	{model: "gpt-6-sol", input: 50_000, cached: 5_000, output: 250_000, fast: includedFast},
	{model: "gpt-6-luna", input: 2_500, cached: 250, output: 12_500, fast: includedFast},
}

type routedCreditPoint struct {
	at            time.Time
	nanoCredits   int64
	unpriced      int64
	totalCredits  int64
	totalUnpriced int64
}

func estimateCodexCredits(model, serviceTier string, usage responseUsage) (int64, bool) {
	rates, known := codexRatesForModel(model)
	if !known {
		return 0, false
	}
	multiplier := creditMultiplier{1, 1}
	switch serviceTierPriceMode(serviceTier) {
	case priceModeFast:
		multiplier = rates.fast
	case priceModeUltrafast:
		multiplier = rates.ultrafast
	}
	if multiplier.denominator == 0 {
		return 0, false
	}
	cached := max(usage.InputDetails.CachedTokens, 0)
	input := max(usage.InputTokens-cached, 0)
	credits := input*rates.input + cached*rates.cached + usage.OutputTokens*rates.output
	return credits * multiplier.numerator / multiplier.denominator, true
}

func codexRatesForModel(model string) (creditRates, bool) {
	if model == "gpt-5.6" {
		return codexCreditRates[0], true
	}
	for _, rates := range codexCreditRates {
		if model == rates.model || strings.HasPrefix(model, rates.model+"-") {
			return rates, true
		}
	}
	return creditRates{}, false
}

func (a *accountStats) addRoutedCredits(at time.Time, nanoCredits int64, known bool) {
	point := routedCreditPoint{at: at, nanoCredits: nanoCredits}
	if !known {
		point.unpriced = 1
	}
	index := sort.Search(len(a.routedCredits), func(index int) bool {
		return a.routedCredits[index].at.After(at)
	})
	a.routedCredits = append(a.routedCredits, routedCreditPoint{})
	copy(a.routedCredits[index+1:], a.routedCredits[index:])
	a.routedCredits[index] = point
	var credits, unpriced int64
	if index > 0 {
		credits = a.routedCredits[index-1].totalCredits
		unpriced = a.routedCredits[index-1].totalUnpriced
	}
	for current := index; current < len(a.routedCredits); current++ {
		credits += a.routedCredits[current].nanoCredits
		unpriced += a.routedCredits[current].unpriced
		a.routedCredits[current].totalCredits = credits
		a.routedCredits[current].totalUnpriced = unpriced
	}
	a.pruneRoutedCredits(a.routedCredits[len(a.routedCredits)-1].at.Add(-routedCreditHistory))
}

func (a *accountStats) pruneRoutedCredits(cutoff time.Time) {
	index := sort.Search(len(a.routedCredits), func(index int) bool {
		return !a.routedCredits[index].at.Before(cutoff)
	})
	if index < 1_024 || index*2 < len(a.routedCredits) {
		return
	}
	index--
	copy(a.routedCredits, a.routedCredits[index:])
	a.routedCredits = a.routedCredits[:len(a.routedCredits)-index]
}

func (a *accountStats) routedCreditsSince(start time.Time) (float64, time.Time, bool) {
	if len(a.routedCredits) == 0 || start.IsZero() {
		return 0, time.Time{}, false
	}
	index := sort.Search(len(a.routedCredits), func(index int) bool {
		return !a.routedCredits[index].at.Before(start)
	})
	last := a.routedCredits[len(a.routedCredits)-1]
	var credits, unpriced int64
	if index > 0 {
		before := a.routedCredits[index-1]
		credits = last.totalCredits - before.totalCredits
		unpriced = last.totalUnpriced - before.totalUnpriced
	} else {
		first := a.routedCredits[0]
		credits = last.totalCredits - first.totalCredits + first.nanoCredits
		unpriced = last.totalUnpriced - first.totalUnpriced + first.unpriced
	}
	if unpriced > 0 {
		return 0, time.Time{}, false
	}
	since := start
	if index == 0 && a.routedCredits[0].at.After(start) {
		since = a.routedCredits[0].at
	}
	return float64(credits) / nanoCreditsPerCredit, since, true
}
