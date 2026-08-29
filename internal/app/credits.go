package app

import (
	"sort"
	"strings"
	"time"
)

const (
	nanoCreditsPerCredit = 1_000_000_000
	routedCreditHistory  = 8 * 24 * time.Hour
	usdPerCodexCredit    = 0.04
)

type creditRates struct {
	model           string
	input           int64
	cached          int64
	output          int64
	fastNumerator   int64
	fastDenominator int64
}

var codexCreditRates20260829 = []creditRates{
	{model: "gpt-5.6-sol", input: 100_000, cached: 10_000, output: 500_000, fastNumerator: 5, fastDenominator: 2},
	{model: "gpt-5.6-terra", input: 50_000, cached: 5_000, output: 300_000, fastNumerator: 5, fastDenominator: 2},
	{model: "gpt-5.6-luna", input: 5_000, cached: 500, output: 30_000, fastNumerator: 5, fastDenominator: 2},
	{model: "gpt-5.5", input: 125_000, cached: 12_500, output: 750_000, fastNumerator: 5, fastDenominator: 2},
	{model: "gpt-5.4-mini", input: 18_750, cached: 1_875, output: 113_000, fastNumerator: 2, fastDenominator: 1},
	{model: "gpt-5.4", input: 62_500, cached: 6_250, output: 375_000, fastNumerator: 2, fastDenominator: 1},
	{model: "daybreak-blue", input: 100_000, cached: 10_000, output: 500_000},
	{model: "daybreak-red", input: 312_500, cached: 31_250, output: 1_875_000},
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
	if isFastServiceTier(serviceTier) {
		if rates.fastDenominator == 0 {
			return 0, false
		}
		rates.input = rates.input * rates.fastNumerator / rates.fastDenominator
		rates.cached = rates.cached * rates.fastNumerator / rates.fastDenominator
		rates.output = rates.output * rates.fastNumerator / rates.fastDenominator
	}
	cached := max(usage.InputDetails.CachedTokens, 0)
	input := max(usage.InputTokens-cached, 0)
	return input*rates.input + cached*rates.cached + usage.OutputTokens*rates.output, true
}

func codexRatesForModel(model string) (creditRates, bool) {
	if model == "gpt-5.6" {
		return codexCreditRates20260829[0], true
	}
	for _, rates := range codexCreditRates20260829 {
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
