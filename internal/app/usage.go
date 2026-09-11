package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

var accountAPIBaseURL = "https://chatgpt.com/backend-api/wham"

const (
	urgentUsageRefreshInterval   = 2 * time.Minute
	accountDetailRefreshInterval = time.Hour
)

type usagePayload struct {
	PlanType  string `json:"plan_type"`
	RateLimit struct {
		LimitReached    *bool       `json:"limit_reached"`
		PrimaryWindow   usageWindow `json:"primary_window"`
		SecondaryWindow usageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	RateLimitResetCredits *struct {
		AvailableCount int64 `json:"available_count"`
	} `json:"rate_limit_reset_credits"`
	SpendControl *spendControlPayload `json:"spend_control"`
}

type spendControlPayload struct {
	Reached         bool               `json:"reached"`
	IndividualLimit *spendControlLimit `json:"individual_limit"`
}

type spendControlLimit struct {
	Source            string   `json:"source"`
	Limit             string   `json:"limit"`
	Used              string   `json:"used"`
	Remaining         string   `json:"remaining"`
	UsedPercent       *float64 `json:"used_percent"`
	RemainingPercent  *float64 `json:"remaining_percent"`
	ResetAfterSeconds int64    `json:"reset_after_seconds"`
	ResetAt           int64    `json:"reset_at"`
}

func spendLimitReached(control *spendControlPayload) bool {
	return control != nil && control.Reached
}

func cloneSpendControl(value *spendControlPayload) *spendControlPayload {
	if value == nil {
		return nil
	}
	cloned := *value
	if value.IndividualLimit != nil {
		limit := *value.IndividualLimit
		if value.IndividualLimit.UsedPercent != nil {
			used := *value.IndividualLimit.UsedPercent
			limit.UsedPercent = &used
		}
		if value.IndividualLimit.RemainingPercent != nil {
			remaining := *value.IndividualLimit.RemainingPercent
			limit.RemainingPercent = &remaining
		}
		cloned.IndividualLimit = &limit
	}
	return &cloned
}

func (p usagePayload) bankedResets() *int64 {
	if p.RateLimitResetCredits == nil {
		return nil
	}
	return &p.RateLimitResetCredits.AvailableCount
}

type usageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	ResetAt            int64    `json:"reset_at"`
	LimitWindowSeconds int      `json:"limit_window_seconds"`
}

type resetCreditsPayload struct {
	Credits        []resetCredit `json:"credits"`
	AvailableCount int64         `json:"available_count"`
}

type resetCredit struct {
	ID          string     `json:"id"`
	ResetType   string     `json:"reset_type"`
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expires_at"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
}

func (c resetCredit) available() bool {
	return c.ResetType == "codex_rate_limits" && c.Status == "available"
}

type consumeResetCreditRequest struct {
	RedeemRequestID string `json:"redeem_request_id"`
	CreditID        string `json:"credit_id"`
}

type consumeResetCreditResponse struct {
	Code         string `json:"code"`
	WindowsReset int64  `json:"windows_reset"`
}

func (u usageWindow) window(fetchedAt time.Time) window {
	if u.UsedPercent == nil {
		return window{}
	}
	w := window{
		usedPercent: *u.UsedPercent,
		minutes:     u.LimitWindowSeconds / 60,
		seenAt:      fetchedAt,
	}
	if u.ResetAt > 0 {
		w.resetsAt = time.Unix(u.ResetAt, 0)
	}
	return w
}

func (s *server) pollUsage(ctx context.Context, account *Account) error {
	resp, err := s.doAccountRequest(ctx, account, http.MethodGet, accountAPIBaseURL+"/usage", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("usage returned %s", resp.Status)
	}

	var payload usagePayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	fetchedAt := time.Now()
	account.adopt(
		fetchedAt,
		payload.PlanType,
		payload.RateLimit.PrimaryWindow.window(fetchedAt),
		payload.RateLimit.SecondaryWindow.window(fetchedAt),
		payload.bankedResets(),
		payload.SpendControl,
	)
	var limitReached any
	if payload.RateLimit.LimitReached != nil {
		limitReached = *payload.RateLimit.LimitReached
		if *payload.RateLimit.LimitReached && account.markSpent() {
			s.log.Info("account stopped accepting new websockets",
				"account", account.id(),
				"source", "usage_poll",
			)
		}
	}
	attrs := []any{"reported_limit_reached", limitReached}
	attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
	s.log.Debug("usage polled", attrs...)
	return nil
}

func (s *server) pollResetCredits(ctx context.Context, account *Account) error {
	resp, err := s.doAccountRequest(ctx, account, http.MethodGet, accountAPIBaseURL+"/rate-limit-reset-credits", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("reset credits returned %s", resp.Status)
	}
	var payload resetCreditsPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	fetchedAt := time.Now()
	account.adoptResetCredits(fetchedAt, payload.AvailableCount, payload.Credits)
	return nil
}

func (s *server) doAccountRequest(ctx context.Context, account *Account, method, endpoint string, body []byte) (*http.Response, error) {
	canReauth := true
	if account.refreshDue(time.Now()) {
		if err := s.reauthorize(account); err != nil {
			return nil, err
		}
		canReauth = false
	}
	for ; ; canReauth = false {
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		account.mu.Lock()
		token := account.AccessToken
		account.mu.Unlock()
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("chatgpt-account-id", account.id())
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := s.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || !canReauth {
			return resp, nil
		}
		resp.Body.Close()
		if err := s.reauthorize(account); err != nil {
			return nil, err
		}
	}
}

// A zero lead allows any unexpired credit, with undated credits ordered last.
func nextResetCredit(credits []resetCredit, now time.Time, lead time.Duration) (resetCredit, bool) {
	deadline := now.Add(lead)
	var next resetCredit
	found := false
	for _, credit := range credits {
		if !credit.available() {
			continue
		}
		if credit.ExpiresAt != nil && !credit.ExpiresAt.After(now) {
			continue
		}
		if lead > 0 && (credit.ExpiresAt == nil || credit.ExpiresAt.After(deadline)) {
			continue
		}
		if !found || resetCreditExpiresBefore(credit, next) {
			next = credit
			found = true
		}
	}
	return next, found
}

func resetCreditExpiresBefore(credit, other resetCredit) bool {
	return credit.ExpiresAt != nil && (other.ExpiresAt == nil || credit.ExpiresAt.Before(*other.ExpiresAt))
}

func expiringResetCredit(credits []resetCredit, now time.Time) (resetCredit, bool) {
	return nextResetCredit(credits, now, resetPriorityLead)
}

func (s *server) consumeResetCredit(ctx context.Context, account *Account, choose func([]resetCredit) (resetCredit, bool)) (consumeResetCreditResponse, string, error) {
	resp, err := s.doAccountRequest(ctx, account, http.MethodGet, accountAPIBaseURL+"/rate-limit-reset-credits", nil)
	if err != nil {
		return consumeResetCreditResponse{}, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return consumeResetCreditResponse{}, "", fmt.Errorf("reset credits returned %s", resp.Status)
	}

	var payload resetCreditsPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return consumeResetCreditResponse{}, "", err
	}
	fetchedAt := time.Now()
	account.adoptResetCredits(fetchedAt, payload.AvailableCount, payload.Credits)
	credit, ok := choose(payload.Credits)
	if !ok {
		return consumeResetCreditResponse{}, "", nil
	}

	body, err := json.Marshal(consumeResetCreditRequest{
		RedeemRequestID: credit.ID,
		CreditID:        credit.ID,
	})
	if err != nil {
		return consumeResetCreditResponse{}, credit.ID, err
	}
	resp, err = s.doAccountRequest(ctx, account, http.MethodPost, accountAPIBaseURL+"/rate-limit-reset-credits/consume", body)
	if err != nil {
		return consumeResetCreditResponse{}, credit.ID, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return consumeResetCreditResponse{}, credit.ID, fmt.Errorf("reset credit consume returned %s", resp.Status)
	}

	var result consumeResetCreditResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return consumeResetCreditResponse{}, credit.ID, err
	}
	switch result.Code {
	case "reset", "nothing_to_reset", "no_credit", "already_redeemed":
		return result, credit.ID, nil
	default:
		return consumeResetCreditResponse{}, credit.ID, fmt.Errorf("reset credit consume returned code %q", result.Code)
	}
}

func (s *server) recoverUsageLimit(ctx context.Context, account *Account, requestSent time.Time) bool {
	account.resetMu.Lock()
	defer account.resetMu.Unlock()
	if account.restoreFromUsageAfter(requestSent) {
		return true
	}
	candidate := account.routingCandidate()
	// Rate-limit reset credits cannot restore a workspace spending allowance.
	if spendLimitReached(candidate.spendControl) {
		return false
	}
	if !candidate.spent {
		return true
	}
	return s.resetAccountUsage(ctx, account, resetPriorityLead)
}

// The caller holds account.resetMu through consumption and quota refresh.
func (s *server) resetAccountUsage(ctx context.Context, account *Account, lead time.Duration) bool {
	result, creditID, err := s.consumeResetCredit(ctx, account, func(credits []resetCredit) (resetCredit, bool) {
		return nextResetCredit(credits, time.Now(), lead)
	})
	if err != nil {
		s.log.Warn("account reset failed", "account", account.id(), "credit", creditID, "error", err)
		s.stats.note("account reset failed", account.id(), err.Error())
		return false
	}
	if creditID == "" {
		return false
	}
	if err := s.pollUsage(ctx, account); err != nil {
		s.log.Warn("usage refresh after reset failed", "account", account.id(), "error", err)
		s.stats.note("usage refresh after reset failed", account.id(), err.Error())
		return false
	}
	s.log.Info("account reset", "account", account.id(), "credit", creditID, "outcome", result.Code, "windows_reset", result.WindowsReset)
	s.stats.note("account reset", account.id(), result.Code)
	restored := !account.routingCandidate().spent
	return restored
}

// Recover one account when the entire pool is unavailable. Serialize selection
// as well as consumption so concurrent callers do not reset different accounts.
func (s *server) recoverPoolUsageLimit(ctx context.Context, excluded map[string]bool) *Account {
	s.poolResetMu.Lock()
	defer s.poolResetMu.Unlock()

	attempted := make(map[string]bool)
	for ctx.Err() == nil {
		if ready := s.pool.route(nil, nil).account; ready != nil {
			return ready
		}
		now := time.Now()
		var next *Account
		var credit resetCredit
		for _, account := range s.pool.all() {
			candidate := account.routingCandidate()
			if candidate.available(now) {
				return account
			}
			if excluded[candidate.id] || attempted[candidate.id] || !candidate.canResetUsage() {
				continue
			}
			if candidate.resetCredits.fetchedAt.IsZero() || now.Sub(candidate.resetCredits.fetchedAt) >= accountDetailRefreshInterval {
				if err := s.pollResetCredits(ctx, account); err != nil {
					s.log.Warn("reset credits poll failed", "account", candidate.id, "error", err)
					attempted[candidate.id] = true
					continue
				}
				candidate = account.routingCandidate()
			}
			available, ok := nextResetCredit(candidate.resetCredits.details, time.Now(), 0)
			if ok && (next == nil || resetCreditExpiresBefore(available, credit)) {
				next, credit = account, available
			}
		}
		if next == nil || ctx.Err() != nil {
			return nil
		}
		attempted[next.id()] = true
		next.resetMu.Lock()
		// Another recovery or quota poll may have restored capacity while we
		// fetched credit details or waited for the account's reset lock.
		ready := s.pool.route(nil, nil).account
		if ready == nil && s.pool.find(next.id()) == next && next.routingCandidate().canResetUsage() {
			s.resetAccountUsage(ctx, next, 0)
			ready = s.pool.route(nil, nil).account
		}
		next.resetMu.Unlock()
		if ready != nil {
			return ready
		}
	}
	return nil
}

func (c routingCandidate) canResetUsage() bool {
	return c.routingEnabled() && !c.paused && c.reauth == "" && c.spent &&
		!spendLimitReached(c.spendControl) && c.resetCredits.known && c.resetCredits.count > 0
}

func (s *server) reauthorize(account *Account) error {
	if !s.refreshed(account, account.id()) {
		return fmt.Errorf("account %s needs reauth", account.id())
	}
	return nil
}

func (a *Account) adopt(fetchedAt time.Time, planType string, primary, secondary window, banked *int64, spendControl *spendControlPayload) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usageFetchedAt = fetchedAt
	a.spendControl = cloneSpendControl(spendControl)
	if planType != "" {
		a.planType = planType
	}
	if primary.known() {
		a.primary = primary
	}
	if secondary.known() {
		a.secondary = secondary
	}
	if banked != nil && (!a.resetCredits.known || a.resetCredits.count != *banked) {
		a.resetCredits = resetCreditState{known: true, count: *banked}
	}
	if (a.primary.known() || a.secondary.known()) && a.pressure() < 100 && !spendLimitReached(a.spendControl) {
		a.spent = false
		if a.Reauth == "" {
			a.cooldown = time.Time{}
		}
	}
}

func (a *Account) adoptResetCredits(fetchedAt time.Time, count int64, credits []resetCredit) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resetCredits = resetCreditState{
		fetchedAt: fetchedAt,
		known:     true,
		count:     count,
		details:   append([]resetCredit(nil), credits...),
	}
}

func (a *Account) pollsDue(now time.Time, every time.Duration) (usage, resetCredits bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	usageEvery := every
	if a.spent || spendLimitReached(a.spendControl) || now.Before(a.cooldown) || a.pressure() >= 95 {
		usageEvery = min(usageEvery, urgentUsageRefreshInterval)
	}
	usage = a.usageFetchedAt.IsZero() || now.Sub(a.usageFetchedAt) >= usageEvery
	resetCredits = a.resetCredits.fetchedAt.IsZero() || now.Sub(a.resetCredits.fetchedAt) >= accountDetailRefreshInterval
	return usage, resetCredits
}

func (s *server) pollAllUsage(ctx context.Context) {
	s.pollAccountData(ctx, 0, true)
}

func (s *server) pollDueUsage(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	s.pollAccountData(ctx, every, false)
}

func (s *server) pollAccountData(ctx context.Context, every time.Duration, force bool) {
	for _, account := range s.pool.all() {
		if account.needsReauth() {
			continue
		}
		poll := func(due bool, kind string, run func() error) bool {
			if due {
				if err := run(); err != nil && ctx.Err() == nil {
					message := kind + " poll failed"
					s.log.Warn(message, "account", account.id(), "error", err)
					s.stats.note(message, account.id(), err.Error())
				}
			}
			return ctx.Err() == nil
		}
		usageDue, resetCreditsDue := true, true
		if !force {
			usageDue, resetCreditsDue = account.pollsDue(time.Now(), every)
		}
		if !poll(usageDue, "usage", func() error { return s.pollUsage(ctx, account) }) {
			return
		}
		if account.needsReauth() {
			continue
		}
		if !force {
			_, resetCreditsDue = account.pollsDue(time.Now(), every)
		}
		if !poll(resetCreditsDue, "reset credits", func() error {
			return s.pollResetCredits(ctx, account)
		}) {
			return
		}
	}
	s.recoverPoolUsageLimit(ctx, nil)
}

func (s *server) watchUsage(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}

	ticker := time.NewTicker(min(every, urgentUsageRefreshInterval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollDueUsage(ctx, every)
		}
	}
}
