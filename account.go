package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	oauthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	refreshAfter  = 8 * 24 * time.Hour
)

var oauthEndpoint = authBaseURL + "/oauth/token"

type accountState struct {
	IDToken      string
	AccessToken  string
	RefreshToken string
	Paused       bool
	RoutingMode  routingMode
	LastRefresh  time.Time
}

type Account struct {
	accountState
	mu          sync.Mutex
	resetMu     sync.Mutex
	inflight    chan struct{}
	lastRefresh error

	cooldown     time.Time
	dead         string
	primary      window
	secondary    window
	spent        bool
	resetCredits resetCreditState
	creditBurn   creditBurnState
	lastUsed     time.Time
}

type resetCreditState struct {
	known   bool
	count   int64
	details []resetCredit
}

type creditBurnState struct {
	fetchedAt time.Time
	credits   float64
}

type window struct {
	usedPercent float64
	minutes     int
	resetsAt    time.Time
	seenAt      time.Time
}

type accountStatus string

type routingMode string

type usagePace uint8

type usagePaceEstimate struct {
	shortfallPercent float64
	runway           time.Duration
	known            bool
}

type weightedWindow struct {
	capacity float64
	window   window
}

const (
	accountLive        accountStatus = "live"
	accountPriority    accountStatus = "priority"
	accountChecking    accountStatus = "checking"
	accountCooling     accountStatus = "cooling"
	accountPaused      accountStatus = "paused"
	accountNeedsReauth accountStatus = "needs_reauth"

	routingModeNormal   routingMode = "normal"
	routingModePriority routingMode = "priority"

	usagePaceUnknown usagePace = iota
	usagePaceOnTrack
	usagePaceClose
	usagePaceOffTrack
)

func (m routingMode) normalized() routingMode {
	switch m {
	case routingModePriority:
		return m
	default:
		return routingModeNormal
	}
}

func (m routingMode) next() routingMode {
	switch m.normalized() {
	case routingModeNormal:
		return routingModePriority
	default:
		return routingModeNormal
	}
}

func (w window) known() bool { return !w.seenAt.IsZero() }

func remainingPercent(w window) (float64, bool) {
	if !w.known() {
		return 0, false
	}
	return min(max(100-w.usedPercent, 0), 100), true
}

func longestWindow(windows ...window) window {
	var longest window
	for _, w := range windows {
		if w.known() && (!longest.known() || w.minutes > longest.minutes) {
			longest = w
		}
	}
	return longest
}

func creditCycleStart(now time.Time, windows ...window) (time.Time, bool) {
	cycle := longestWindow(windows...)
	duration := time.Duration(cycle.minutes) * time.Minute
	if !cycle.known() || duration <= 0 || !cycle.resetsAt.After(now) {
		return time.Time{}, false
	}
	start := cycle.resetsAt.Add(-duration)
	if start.After(now) {
		return time.Time{}, false
	}
	return start, true
}

func (e usagePaceEstimate) pace() usagePace {
	if !e.known {
		return usagePaceUnknown
	}
	switch {
	case e.shortfallPercent <= 0:
		return usagePaceOnTrack
	case e.shortfallPercent <= 5:
		return usagePaceClose
	default:
		return usagePaceOffTrack
	}
}

func usagePaceAt(now time.Time, windows ...weightedWindow) usagePaceEstimate {
	var totalShortfall float64
	var totalCapacity float64
	var totalRemaining float64
	var burnPerHour float64
	for _, weighted := range windows {
		w := weighted.window
		remaining, ok := remainingPercent(w)
		duration := time.Duration(w.minutes) * time.Minute
		resetIn := w.resetsAt.Sub(now)
		if !ok || weighted.capacity <= 0 || duration <= 0 || resetIn <= 0 || resetIn > duration {
			continue
		}
		totalShortfall += (float64(resetIn)/float64(duration)*100 - remaining) * weighted.capacity
		totalCapacity += weighted.capacity
		totalRemaining += remaining * weighted.capacity
		elapsed := duration - resetIn
		if elapsed > 0 {
			burnPerHour += (100 - remaining) * weighted.capacity / elapsed.Hours()
		}
	}
	if totalCapacity == 0 {
		return usagePaceEstimate{}
	}
	var runway time.Duration
	if totalShortfall > 0 && burnPerHour > 0 {
		runway = time.Duration(totalRemaining / burnPerHour * float64(time.Hour))
	}
	return usagePaceEstimate{shortfallPercent: totalShortfall / totalCapacity, runway: runway, known: true}
}

func weeklyPlanCapacity(plan string) float64 {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "free", "go":
		return 1_134
	case "plus", "team", "business", "edu":
		return 7_560
	case "prolite":
		return 37_800
	case "pro", "enterprise":
		return 50_400
	default:
		return 0
	}
}

func (a *Account) pressure() float64 {
	return math.Max(a.primary.usedPercent, a.secondary.usedPercent)
}

func (a *Account) health() (primary, secondary window, cooldown time.Time, reauth string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.primary, a.secondary, a.cooldown, a.dead
}

func (a *Account) markSpent() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := !a.spent
	a.spent = true
	return changed
}

func (a *Account) restoreFromUsageAfter(t time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pressure() >= 100 {
		return false
	}
	known := false
	for _, window := range []window{a.primary, a.secondary} {
		if !window.known() {
			continue
		}
		known = true
		if !window.seenAt.After(t) {
			return false
		}
	}
	if known {
		a.spent = false
	}
	return known
}

func (a *Account) status(now time.Time) accountStatus {
	return a.routingCandidate().status(now)
}

func accountStatusAt(paused bool, reauth string, cooldown time.Time, spent, known bool, now time.Time) accountStatus {
	switch {
	case paused:
		return accountPaused
	case reauth != "":
		return accountNeedsReauth
	case spent || now.Before(cooldown):
		return accountCooling
	case !known:
		return accountChecking
	default:
		return accountLive
	}
}

func (a *Account) bankedResets() (int64, []resetCredit, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.resetCredits.known {
		return 0, nil, false
	}
	return a.resetCredits.count, append([]resetCredit(nil), a.resetCredits.details...), true
}

func (a *Account) creditBurnSinceReset(now time.Time) (float64, time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	start, known := creditCycleStart(now, a.primary, a.secondary)
	if !known || a.creditBurn.fetchedAt.Before(start) {
		return 0, time.Time{}, false
	}
	return a.creditBurn.credits, start, true
}

func (a *Account) persisted() accountState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accountState
}

func accountFromState(state accountState) *Account {
	state.RoutingMode = state.RoutingMode.normalized()
	return &Account{accountState: state}
}

func (a *Account) applyPersisted(next accountState) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	before := a.accountState
	if !next.LastRefresh.Before(a.LastRefresh) {
		credentialsChanged := a.IDToken != next.IDToken ||
			a.AccessToken != next.AccessToken ||
			a.RefreshToken != next.RefreshToken
		a.IDToken = next.IDToken
		a.AccessToken = next.AccessToken
		a.RefreshToken = next.RefreshToken
		a.LastRefresh = next.LastRefresh
		if credentialsChanged {
			a.dead = ""
			a.lastRefresh = nil
		}
	}
	a.Paused = next.Paused
	a.RoutingMode = next.RoutingMode.normalized()
	return a.accountState != before
}

type authClaims struct {
	Email string `json:"email"`
	Auth  struct {
		AccountID string `json:"chatgpt_account_id"`
		Plan      string `json:"chatgpt_plan_type"`
	} `json:"https://api.openai.com/auth"`
}

func jwtClaims(token string, into any) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return
	}
	json.Unmarshal(raw, into)
}

func (a *Account) claims() authClaims {
	a.mu.Lock()
	defer a.mu.Unlock()
	return claimsFromToken(a.IDToken)
}

func claimsFromToken(token string) authClaims {
	var c authClaims
	jwtClaims(token, &c)
	return c
}

func (a *Account) expires() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	var c struct {
		Exp int64 `json:"exp"`
	}
	jwtClaims(a.AccessToken, &c)
	if c.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(c.Exp, 0)
}

func (a *Account) id() string    { return a.claims().Auth.AccountID }
func (a *Account) email() string { return a.claims().Email }
func (a *Account) plan() string  { return a.claims().Auth.Plan }

func (a *Account) paused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Paused
}

func (a *Account) needsReauth() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dead != ""
}

func (a *Account) stale(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return now.Sub(a.LastRefresh) > refreshAfter
}

type refreshRequest struct {
	ClientID     string `json:"client_id"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
}

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

var permanentRefreshFailures = []string{
	"refresh_token_expired",
	"refresh_token_reused",
	"refresh_token_invalidated",
	"invalid_grant",
}

func (a *Account) refresh(ctx context.Context, hc *http.Client, persist func(accountState) (accountState, error)) error {
	a.mu.Lock()
	if wait := a.inflight; wait != nil {
		a.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.lastRefresh
	}
	if a.dead != "" {
		err := fmt.Errorf("account %s needs reauth: %s", claimsFromToken(a.IDToken).Auth.AccountID, a.dead)
		a.mu.Unlock()
		return err
	}
	done := make(chan struct{})
	a.inflight = done
	state := a.accountState
	token := a.RefreshToken
	a.mu.Unlock()

	tokens, permanent, err := exchangeRefreshToken(ctx, hc, token)
	next := state
	if err == nil {
		next.AccessToken = tokens.AccessToken
		if tokens.RefreshToken != "" {
			next.RefreshToken = tokens.RefreshToken
		}
		if tokens.IDToken != "" {
			next.IDToken = tokens.IDToken
		}
		next.LastRefresh = time.Now()
		if persist != nil {
			next, err = persist(next)
		}
	}

	a.mu.Lock()
	superseded := a.RefreshToken != token && a.RefreshToken != next.RefreshToken
	if superseded {
		err = nil
		permanent = false
	}
	a.inflight = nil
	a.lastRefresh = err
	if !superseded {
		switch {
		case err != nil && permanent:
			a.dead = err.Error()
		case err == nil:
			a.accountState = next
		}
	}
	a.mu.Unlock()
	close(done)
	return err
}

func exchangeRefreshToken(ctx context.Context, hc *http.Client, token string) (tokenResponse, bool, error) {
	var out tokenResponse
	body, err := json.Marshal(refreshRequest{
		ClientID:     oauthClientID,
		GrantType:    "refresh_token",
		RefreshToken: token,
	})
	if err != nil {
		return out, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthEndpoint, bytes.NewReader(body))
	if err != nil {
		return out, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return out, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		for _, reason := range permanentRefreshFailures {
			if strings.Contains(string(body), reason) {
				return out, true, errors.New(reason)
			}
		}
		return out, resp.StatusCode == http.StatusUnauthorized,
			fmt.Errorf("token endpoint returned %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, false, err
	}
	return out, false, nil
}
