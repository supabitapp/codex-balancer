package app

import (
	"maps"
	"sync"
	"time"
)

const apiKeyLedgerBatch = 10_000

type apiKeyTotals struct {
	month        responseUsage
	monthCost    usageCost
	lifetimeCost usageCost
	lastUsed     time.Time
}

type apiKeyLedger struct {
	mu         sync.Mutex
	monthStart time.Time
	pricedAt   time.Time
	lastID     int64
	keys       map[string]apiKeyTotals
}

func (l *apiKeyLedger) totals(store *StateStore, prices priceSnapshot, now time.Time) (map[string]apiKeyTotals, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	monthStart := calendarMonthStart(now)
	if l.keys == nil || !l.monthStart.Equal(monthStart) || !l.pricedAt.Equal(prices.fetchedAt) {
		l.monthStart, l.pricedAt, l.lastID, l.keys = monthStart, prices.fetchedAt, 0, make(map[string]apiKeyTotals)
	}
	for {
		rows, err := store.raw.APIKeyUsageAfter(l.lastID, apiKeyLedgerBatch)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			usage := responseUsageFromState(row.Usage)
			cost, known := prices.estimate(row.Model, row.ServiceTier, usage)
			key := l.keys[row.APIKeyName]
			key.lifetimeCost.add(cost, known)
			if !row.At.Before(l.monthStart) {
				key.month.add(usage)
				key.monthCost.add(cost, known)
			}
			if row.At.After(key.lastUsed) {
				key.lastUsed = row.At
			}
			l.keys[row.APIKeyName] = key
			l.lastID = row.ID
		}
		if len(rows) < apiKeyLedgerBatch {
			return maps.Clone(l.keys), nil
		}
	}
}
