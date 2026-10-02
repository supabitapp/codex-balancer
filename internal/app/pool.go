package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	minCooldown       = 5 * time.Second
	maxCooldown       = time.Hour
	resetPriorityLead = 24 * time.Hour
)

type Pool struct {
	store     *StateStore
	storageMu contextMutex
	mu        sync.RWMutex
	accounts  []*Account
}

type routingCandidate struct {
	account         *Account
	id              string
	plan            string
	paused          bool
	reauth          string
	cooldown        time.Time
	primary         window
	secondary       window
	resetCredits    resetCreditState
	spendControl    *spendControlPayload
	credits         *creditsPayload
	creditsRejected bool
	spent           bool
	pressure        float64
	lastUsed        time.Time
	mode            routingMode
}

type routingDecision struct {
	account        *Account
	blocked        string
	priorOwner     string
	reason         routingReason
	candidates     []routingCandidate
	now            time.Time
	creditFallback bool
	retryAt        time.Time
}

func (d routingDecision) moved() bool {
	return d.priorOwner != "" && d.account != nil && d.priorOwner != d.account.id()
}

type routingPriority struct {
	expiresAt        time.Time
	remainingPercent float64
}

func indexOf(accounts []*Account, id string) int {
	return slices.IndexFunc(accounts, func(a *Account) bool { return a.id() == id })
}

func (p *Pool) find(id string) *Account {
	accounts := p.all()
	if i := indexOf(accounts, id); i >= 0 {
		return accounts[i]
	}
	return nil
}

func (p *Pool) add(a *Account) error {
	id := a.id()
	if id == "" {
		return errors.New("credentials carry no chatgpt_account_id")
	}
	return p.mutate(func(accounts []*Account) ([]*Account, error) {
		if i := indexOf(accounts, id); i >= 0 {
			accounts[i] = a
			return accounts, nil
		}
		return append(accounts, a), nil
	})
}

func (p *Pool) resolve(query string) (*Account, error) {
	if a := p.find(query); a != nil {
		return a, nil
	}
	var matched []*Account
	for _, a := range p.all() {
		if strings.EqualFold(a.email(), query) {
			matched = append(matched, a)
		}
	}
	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return nil, fmt.Errorf("no account %q", query)
	default:
		return nil, fmt.Errorf("%q matches %d accounts; name one by id, which `accounts list -json` prints", query, len(matched))
	}
}

func (p *Pool) remove(a *Account) error {
	id := a.id()
	return p.mutate(func(accounts []*Account) ([]*Account, error) {
		i := indexOf(accounts, id)
		if i < 0 {
			return nil, fmt.Errorf("no account %q", id)
		}
		return slices.Delete(accounts, i, i+1), nil
	})
}

func (p *Pool) togglePause(a *Account) (bool, error) {
	return p.updatePause(a, func(paused bool) bool { return !paused })
}

func (p *Pool) setPaused(a *Account, paused bool) error {
	_, err := p.updatePause(a, func(bool) bool { return paused })
	return err
}

func (p *Pool) updatePause(a *Account, update func(bool) bool) (bool, error) {
	id := a.id()
	paused := false
	err := p.mutate(func(accounts []*Account) ([]*Account, error) {
		i := indexOf(accounts, id)
		if i < 0 {
			return nil, fmt.Errorf("no account %q", id)
		}
		state := accounts[i].persisted()
		state.Paused = update(state.Paused)
		paused = state.Paused
		accounts[i] = accountFromState(state)
		return accounts, nil
	})
	return paused, err
}

func (p *Pool) cycleRoutingMode(a *Account) (routingMode, error) {
	return p.updateRoutingMode(a, routingMode.next)
}

func (p *Pool) setRoutingMode(a *Account, mode routingMode) error {
	if mode != routingModeNormal && mode != routingModePriority && mode != routingModePaused {
		return fmt.Errorf("unknown routing mode %q; use normal, priority, or paused", mode)
	}
	_, err := p.updateRoutingMode(a, func(routingMode) routingMode { return mode })
	return err
}

func (p *Pool) updateRoutingMode(a *Account, update func(routingMode) routingMode) (routingMode, error) {
	id := a.id()
	mode := routingModeNormal
	err := p.mutate(func(accounts []*Account) ([]*Account, error) {
		i := indexOf(accounts, id)
		if i < 0 {
			return nil, fmt.Errorf("no account %q", id)
		}
		state := accounts[i].persisted()
		mode = update(state.effectiveRoutingMode())
		state.Paused = mode == routingModePaused
		// Keep the routing preference while paused so Resume can restore it.
		if !state.Paused {
			state.RoutingMode = mode
		}
		accounts[i] = accountFromState(state)
		return accounts, nil
	})
	return mode, err
}

func (p *Pool) persistAccountState(state accountState) (accountState, error) {
	return p.persistAccountStateContext(context.Background(), state)
}

func (p *Pool) persistAccountStateContext(ctx context.Context, state accountState) (accountState, error) {
	id := claimsFromToken(state.IDToken).Auth.AccountID
	var persisted accountState
	err := p.mutateContext(ctx, func(accounts []*Account) ([]*Account, error) {
		i := indexOf(accounts, id)
		if i < 0 {
			return nil, fmt.Errorf("no account %q", id)
		}
		current := accounts[i].persisted()
		if current.LastRefresh.After(state.LastRefresh) {
			persisted = current
			return accounts, nil
		}
		current.IDToken = state.IDToken
		current.AccessToken = state.AccessToken
		current.RefreshToken = state.RefreshToken
		current.LastRefresh = state.LastRefresh
		current.Reauth = state.Reauth
		persisted = current
		accounts[i] = accountFromState(current)
		return accounts, nil
	})
	return persisted, err
}

func (p *Pool) route(owners []string, skip map[string]bool) routingDecision {
	now := time.Now()
	decision := routingDecision{now: now}
	if len(owners) > 0 {
		decision.priorOwner = owners[0]
	}
	for _, account := range p.all() {
		decision.candidates = append(decision.candidates, account.routingCandidate())
	}
	for _, owner := range owners {
		for i := range decision.candidates {
			candidate := &decision.candidates[i]
			if candidate.id != owner || !candidate.routingEnabled() || candidate.paused || candidate.reauth != "" || candidate.spent || candidate.weeklyExhausted() {
				continue
			}
			if !candidate.quotaKnown() || !now.After(candidate.cooldown) {
				decision.blocked = owner
				decision.retryAt = decision.cooldownRecovery(nil)
				return decision
			}
			if !skip[candidate.id] {
				decision.account = candidate.account
				return decision
			}
		}
	}
	decision.account = selectFreshAccount(decision.candidates, now, func(candidate routingCandidate) bool {
		return !skip[candidate.id] && candidate.available(now)
	})
	if decision.account != nil {
		return decision
	}
	// Use included quota throughout the pool before charging an account's credits.
	for _, owner := range owners {
		for i := range decision.candidates {
			candidate := &decision.candidates[i]
			if candidate.id == owner && !skip[candidate.id] && candidate.creditAvailable(now) {
				decision.account = candidate.account
				decision.creditFallback = true
				return decision
			}
		}
	}
	decision.account = selectFreshAccount(decision.candidates, now, func(candidate routingCandidate) bool {
		return !skip[candidate.id] && candidate.creditAvailable(now)
	})
	decision.creditFallback = decision.account != nil
	decision.retryAt = decision.cooldownRecovery(nil)
	return decision
}

func selectFreshAccount(candidates []routingCandidate, now time.Time, eligible func(routingCandidate) bool) *Account {
	var least *routingCandidate
	for i := range candidates {
		candidate := &candidates[i]
		if !eligible(*candidate) {
			continue
		}
		if least == nil || cmp.Or(candidate.comparePriority(*least, now), cmp.Compare(candidate.pressure, least.pressure)) < 0 {
			least = candidate
		}
	}
	if least == nil {
		return nil
	}
	// Anchor the tolerance to the cohort minimum. Pairwise pressure ties can
	// form cycles and make selection depend on the order of the account list.
	best := least
	for i := range candidates {
		candidate := &candidates[i]
		if !eligible(*candidate) || candidate.comparePriority(*least, now) != 0 || candidate.pressure > least.pressure+1 {
			continue
		}
		if cmp.Or(candidate.lastUsed.Compare(best.lastUsed), cmp.Compare(candidate.id, best.id)) < 0 {
			best = candidate
		}
	}
	return best.account
}

func (a *Account) routingCandidate() routingCandidate {
	a.mu.Lock()
	defer a.mu.Unlock()
	plan := a.planType
	if plan == "" {
		plan = claimsFromToken(a.IDToken).Auth.Plan
	}
	return routingCandidate{
		account:   a,
		id:        claimsFromToken(a.IDToken).Auth.AccountID,
		plan:      plan,
		paused:    a.Paused,
		reauth:    a.Reauth,
		cooldown:  a.cooldown,
		primary:   a.primary,
		secondary: a.secondary,
		resetCredits: resetCreditState{
			fetchedAt: a.resetCredits.fetchedAt,
			known:     a.resetCredits.known,
			count:     a.resetCredits.count,
			details:   append([]resetCredit(nil), a.resetCredits.details...),
		},
		spendControl:    cloneSpendControl(a.spendControl),
		credits:         cloneCredits(a.credits),
		creditsRejected: a.creditsRejected,
		spent:           a.spent || spendLimitReached(a.spendControl),
		pressure:        a.pressure(),
		lastUsed:        a.lastUsed,
		mode:            a.accountState.effectiveRoutingMode(),
	}
}

func (c routingCandidate) available(now time.Time) bool {
	return c.routingEnabled() && !c.paused && c.reauth == "" && !c.spent && !c.weeklyExhausted() && c.quotaKnown() && now.After(c.cooldown)
}

func (c routingCandidate) status(now time.Time) accountStatus {
	if !c.paused && c.reauth == "" && !c.routingEnabled() {
		return accountNotRouted
	}
	if c.creditAvailable(now) {
		return accountCredits
	}
	status := accountStatusAt(c.paused, c.reauth, c.cooldown, c.spent || c.weeklyExhausted(), c.quotaKnown(), now)
	if status == accountLive {
		if c.mode == routingModePriority {
			return accountPriority
		}
		if _, ok := c.routingPriority(now); ok {
			return accountPriority
		}
	}
	return status
}

func (c routingCandidate) routingEnabled() bool {
	return routablePlan(c.plan)
}

func (c routingCandidate) quotaKnown() bool {
	return c.primary.known() || c.secondary.known()
}

func (c routingCandidate) routingPriority(now time.Time) (routingPriority, bool) {
	remaining, known := remainingPercent(longestWindow(c.primary, c.secondary))
	if !known {
		return routingPriority{}, false
	}
	credit, ok := expiringResetCredit(c.resetCredits.details, now)
	if !ok {
		return routingPriority{}, false
	}
	return routingPriority{
		expiresAt:        *credit.ExpiresAt,
		remainingPercent: remaining,
	}, true
}

func (c routingCandidate) comparePriority(other routingCandidate, now time.Time) int {
	manualPriority := c.mode == routingModePriority
	otherManualPriority := other.mode == routingModePriority
	if manualPriority != otherManualPriority {
		if manualPriority {
			return -1
		}
		return 1
	}
	priority, prioritized := c.routingPriority(now)
	otherPriority, otherPrioritized := other.routingPriority(now)
	if prioritized != otherPrioritized {
		if prioritized {
			return -1
		}
		return 1
	}
	if prioritized && !priority.expiresAt.Equal(otherPriority.expiresAt) {
		return priority.expiresAt.Compare(otherPriority.expiresAt)
	}
	return 0
}

func (p *Pool) sorted() []*Account {
	out := p.all()
	slices.Reverse(out)
	return out
}

func (p *Pool) all() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.accounts)
}

func (p *Pool) count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

func readWindow(h http.Header, prefix string) window {
	used, err := strconv.ParseFloat(h.Get(prefix+"-used-percent"), 64)
	if err != nil {
		return window{}
	}
	w := window{usedPercent: used, seenAt: time.Now()}
	if minutes, err := strconv.Atoi(h.Get(prefix + "-window-minutes")); err == nil {
		w.minutes = minutes
	}
	if secs, err := strconv.ParseInt(h.Get(prefix+"-reset-at"), 10, 64); err == nil {
		w.resetsAt = time.Unix(secs, 0)
	}
	return w
}

func (a *Account) observe(h http.Header) {
	primary := readWindow(h, "x-codex-primary")
	secondary := readWindow(h, "x-codex-secondary")

	a.mu.Lock()
	defer a.mu.Unlock()
	if primary.known() {
		a.primary = primary
	}
	if secondary.known() {
		a.secondary = secondary
	}
}

func (a *Account) accepted(at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if at.After(a.lastUsed) {
		a.lastUsed = at
	}
}

func (a *Account) rateLimited(h http.Header) {
	now := time.Now()
	until := now.Add(minCooldown)
	if retryAfter := retryAfterHeader(h); retryAfter.After(until) {
		until = retryAfter
	}
	if limit := now.Add(maxCooldown); until.After(limit) {
		until = limit
	}
	a.extendCooldown(until)
}

func retryAfterHeader(h http.Header) time.Time {
	retryAfter := strings.TrimSpace(h.Get("retry-after"))
	if secs, err := strconv.ParseUint(retryAfter, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) && strings.Trim(retryAfter, "0123456789") == "" {
		// Saturate before converting seconds to a nanosecond duration.
		secs = min(secs, uint64(maxCooldown/time.Second))
		return time.Now().Add(time.Duration(secs) * time.Second)
	}
	if date, err := http.ParseTime(retryAfter); err == nil {
		return date
	}
	return time.Time{}
}

func (a *Account) failed() {
	a.extendCooldown(time.Now().Add(minCooldown))
}

func (a *Account) extendCooldown(until time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if until.After(a.cooldown) {
		a.cooldown = until
	}
}
