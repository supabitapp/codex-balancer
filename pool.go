package main

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	minCooldown       = 30 * time.Second
	maxCooldown       = time.Hour
	resetPriorityLead = 24 * time.Hour
)

type Pool struct {
	store     *StateStore
	storageMu sync.Mutex
	mu        sync.RWMutex
	accounts  []*Account
}

type routingCandidate struct {
	account      *Account
	id           string
	paused       bool
	reauth       string
	cooldown     time.Time
	primary      window
	secondary    window
	resetCredits resetCreditState
	spent        bool
	pressure     float64
	lastUsed     time.Time
	mode         routingMode
}

type routingDecision struct {
	account    *Account
	candidates []routingCandidate
	now        time.Time
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
	id := a.id()
	paused := false
	err := p.mutate(func(accounts []*Account) ([]*Account, error) {
		i := indexOf(accounts, id)
		if i < 0 {
			return nil, fmt.Errorf("no account %q", id)
		}
		state := accounts[i].persisted()
		state.Paused = !state.Paused
		paused = state.Paused
		accounts[i] = accountFromState(state)
		return accounts, nil
	})
	return paused, err
}

func (p *Pool) cycleRoutingMode(a *Account) (routingMode, error) {
	id := a.id()
	mode := routingModeNormal
	err := p.mutate(func(accounts []*Account) ([]*Account, error) {
		i := indexOf(accounts, id)
		if i < 0 {
			return nil, fmt.Errorf("no account %q", id)
		}
		state := accounts[i].persisted()
		state.RoutingMode = state.RoutingMode.next()
		mode = state.RoutingMode
		accounts[i] = accountFromState(state)
		return accounts, nil
	})
	return mode, err
}

func (p *Pool) persistTokens(state accountState) (accountState, error) {
	id := claimsFromToken(state.IDToken).Auth.AccountID
	var persisted accountState
	err := p.mutate(func(accounts []*Account) ([]*Account, error) {
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
		persisted = current
		accounts[i] = accountFromState(current)
		return accounts, nil
	})
	return persisted, err
}

func (p *Pool) route(skip map[string]bool) routingDecision {
	now := time.Now()
	decision := routingDecision{now: now}
	for _, account := range p.all() {
		decision.candidates = append(decision.candidates, account.routingCandidate())
	}
	var best *routingCandidate
	for i := range decision.candidates {
		candidate := &decision.candidates[i]
		if skip[candidate.id] || !candidate.available(now) {
			continue
		}
		if best == nil || candidate.routesBefore(*best, now) {
			best = candidate
		}
	}
	if best != nil {
		decision.account = best.account
	}
	return decision
}

func (a *Account) routingCandidate() routingCandidate {
	a.mu.Lock()
	defer a.mu.Unlock()
	return routingCandidate{
		account:   a,
		id:        claimsFromToken(a.IDToken).Auth.AccountID,
		paused:    a.Paused,
		reauth:    a.dead,
		cooldown:  a.cooldown,
		primary:   a.primary,
		secondary: a.secondary,
		resetCredits: resetCreditState{
			known:   a.resetCredits.known,
			count:   a.resetCredits.count,
			details: append([]resetCredit(nil), a.resetCredits.details...),
		},
		spent:    a.spent,
		pressure: a.pressure(),
		lastUsed: a.lastUsed,
		mode:     a.RoutingMode.normalized(),
	}
}

func (c routingCandidate) available(now time.Time) bool {
	return !c.paused && c.reauth == "" && !c.spent && c.quotaKnown() && now.After(c.cooldown)
}

func (c routingCandidate) status(now time.Time) accountStatus {
	status := accountStatusAt(c.paused, c.reauth, c.cooldown, c.spent, c.quotaKnown(), now)
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

func (c routingCandidate) quotaKnown() bool {
	return c.primary.known() || c.secondary.known()
}

func (c routingCandidate) routingPriority(now time.Time) (routingPriority, bool) {
	remaining, known := remainingPercent(longestWindow(c.primary, c.secondary))
	if !known {
		return routingPriority{}, false
	}
	credit, ok := nextExpiringResetCredit(c.resetCredits.details, now, resetPriorityLead)
	if !ok {
		return routingPriority{}, false
	}
	return routingPriority{
		expiresAt:        *credit.ExpiresAt,
		remainingPercent: remaining,
	}, true
}

func (c routingCandidate) routesBefore(other routingCandidate, now time.Time) bool {
	manualPriority := c.mode == routingModePriority
	otherManualPriority := other.mode == routingModePriority
	if manualPriority != otherManualPriority {
		return manualPriority
	}
	priority, prioritized := c.routingPriority(now)
	otherPriority, otherPrioritized := other.routingPriority(now)
	if prioritized != otherPrioritized {
		return prioritized
	}
	if prioritized && !priority.expiresAt.Equal(otherPriority.expiresAt) {
		return priority.expiresAt.Before(otherPriority.expiresAt)
	}
	return c.roomierThan(other)
}

func (c routingCandidate) roomierThan(other routingCandidate) bool {
	if math.Abs(c.pressure-other.pressure) > 1 {
		return c.pressure < other.pressure
	}
	return cmp.Or(c.lastUsed.Compare(other.lastUsed), cmp.Compare(c.id, other.id)) < 0
}

func (p *Pool) sorted() []*Account {
	out := p.all()
	slices.SortFunc(out, func(x, y *Account) int {
		return cmp.Or(cmp.Compare(x.email(), y.email()), cmp.Compare(x.id(), y.id()))
	})
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
	secondary := readWindow(h, "x-codex-secondary-primary")

	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastUsed = time.Now()
	if primary.known() {
		a.primary = primary
	}
	if secondary.known() {
		a.secondary = secondary
	}
}

func (a *Account) rateLimited(h http.Header, attempt int) {
	until := time.Now().Add(backoff(attempt))
	if reset := resetHeader(h); !reset.IsZero() {
		until = reset
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cooldown = until
}

func resetHeader(h http.Header) time.Time {
	binding := window{usedPercent: -1}
	for _, prefix := range []string{"x-codex-primary", "x-codex-secondary-primary"} {
		if w := readWindow(h, prefix); w.known() && w.usedPercent > binding.usedPercent {
			binding = w
		}
	}
	if !binding.resetsAt.IsZero() {
		return binding.resetsAt
	}
	retryAfter := h.Get("retry-after")
	if secs, err := strconv.Atoi(retryAfter); err == nil {
		return time.Now().Add(time.Duration(secs) * time.Second)
	}
	if date, err := http.ParseTime(retryAfter); err == nil {
		return date
	}
	return time.Time{}
}

func (a *Account) failed(attempt int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cooldown = time.Now().Add(backoff(attempt))
}

func backoff(attempt int) time.Duration {
	d := minCooldown << attempt
	if d > maxCooldown {
		return maxCooldown
	}
	return d
}
