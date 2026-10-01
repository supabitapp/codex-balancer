package app

import (
	"math"
	"time"
)

func (c routingCandidate) weeklyExhausted() bool {
	weekly := longestWindow(c.primary, c.secondary)
	return weekly.known() && weekly.minutes >= 7*24*60 && weekly.usedPercent >= 100 && !math.IsInf(weekly.usedPercent, 0)
}

func (c routingCandidate) creditAvailable(now time.Time) bool {
	return c.routingEnabled() && !c.paused && c.reauth == "" && now.After(c.cooldown) &&
		!spendLimitReached(c.spendControl) && !c.creditsRejected && c.weeklyExhausted() && c.credits.spendable()
}

// A usage rejection can mean the upstream cannot spend this account's credits.
// Wait for a fresh usage snapshot before trying its credit balance again.
func (a *Account) rejectCredits() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.creditsRejected = true
}
