package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

var accountAPIBaseURL = "https://chatgpt.com/backend-api/wham"

const (
	usageSnapshotKind            = "usage"
	resetCreditsSnapshotKind     = "reset_credits"
	creditBurnSnapshotKind       = "credit_burn"
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

type creditBurnPayload struct {
	Data []struct {
		Totals struct {
			Credits float64 `json:"credits"`
		} `json:"totals"`
	} `json:"data"`
}

func (p creditBurnPayload) total() float64 {
	var total float64
	for _, day := range p.Data {
		total += day.Totals.Credits
	}
	return total
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
	if err := s.saveAccountSnapshot(account.id(), usageSnapshotKind, fetchedAt, payload); err != nil {
		return err
	}
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
	if err := s.saveAccountSnapshot(account.id(), resetCreditsSnapshotKind, fetchedAt, payload); err != nil {
		return err
	}
	account.adoptResetCredits(fetchedAt, payload.AvailableCount, payload.Credits)
	return nil
}

func (s *server) pollCreditBurn(ctx context.Context, account *Account, now time.Time) error {
	account.mu.Lock()
	start, known := creditCycleStart(now, account.primary, account.secondary)
	account.mu.Unlock()
	if !known {
		return nil
	}
	query := url.Values{
		"end_date":       {now.Format(time.DateOnly)},
		"group_by":       {"day"},
		"start_date":     {start.In(now.Location()).Format(time.DateOnly)},
		"workspace_user": {"true"},
	}
	endpoint := accountAPIBaseURL + "/analytics/daily-workspace-usage-counts?" + query.Encode()
	resp, err := s.doAccountRequest(ctx, account, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("credit burn returned %s", resp.Status)
	}
	var payload creditBurnPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	if err := s.saveAccountSnapshot(account.id(), creditBurnSnapshotKind, now, payload); err != nil {
		return err
	}
	account.adoptCreditBurn(now, payload.total())
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

func nextExpiringResetCredit(credits []resetCredit, now time.Time, lead time.Duration) (resetCredit, bool) {
	deadline := now.Add(lead)
	var next resetCredit
	found := false
	for _, credit := range credits {
		if !credit.available() || credit.ExpiresAt == nil {
			continue
		}
		if !credit.ExpiresAt.After(now) || credit.ExpiresAt.After(deadline) {
			continue
		}
		if !found || credit.ExpiresAt.Before(*next.ExpiresAt) {
			next = credit
			found = true
		}
	}
	return next, found
}

func expiringResetCredit(credits []resetCredit, now time.Time) (resetCredit, bool) {
	return nextExpiringResetCredit(credits, now, resetPriorityLead)
}

func (s *server) consumeExpiringResetCredit(ctx context.Context, account *Account, now time.Time) (consumeResetCreditResponse, string, error) {
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
	if err := s.saveAccountSnapshot(account.id(), resetCreditsSnapshotKind, fetchedAt, payload); err != nil {
		return consumeResetCreditResponse{}, "", err
	}
	account.adoptResetCredits(fetchedAt, payload.AvailableCount, payload.Credits)
	credit, ok := expiringResetCredit(payload.Credits, now)
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
	if !account.routingCandidate().spent {
		return true
	}
	result, creditID, err := s.consumeExpiringResetCredit(ctx, account, time.Now())
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
	if (a.primary.known() || a.secondary.known()) && a.pressure() < 100 {
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

func (a *Account) adoptCreditBurn(fetchedAt time.Time, credits float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.creditBurn = creditBurnState{fetchedAt: fetchedAt, credits: credits}
}

func (a *Account) pollsDue(now time.Time, every time.Duration) (usage, creditBurn, resetCredits bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	usageEvery := every
	if a.spent || now.Before(a.cooldown) || a.pressure() >= 95 {
		usageEvery = min(usageEvery, urgentUsageRefreshInterval)
	}
	usage = a.usageFetchedAt.IsZero() || now.Sub(a.usageFetchedAt) >= usageEvery
	_, cycleKnown := creditCycleStart(now, a.primary, a.secondary)
	creditBurn = cycleKnown && (a.creditBurn.fetchedAt.IsZero() || now.Sub(a.creditBurn.fetchedAt) >= accountDetailRefreshInterval)
	resetCredits = a.resetCredits.fetchedAt.IsZero() || now.Sub(a.resetCredits.fetchedAt) >= accountDetailRefreshInterval
	return usage, creditBurn, resetCredits
}

func (s *server) saveAccountSnapshot(account, kind string, fetchedAt time.Time, payload any) error {
	if s.pool == nil || s.pool.store == nil {
		return nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.pool.store.saveAccountSnapshot(storedAccountSnapshot{
		Account:   account,
		Kind:      kind,
		FetchedAt: fetchedAt,
		Payload:   data,
	})
}

func restoreUsageSnapshots(store *StateStore, accounts []*Account) error {
	byID := make(map[string]*Account, len(accounts))
	for _, account := range accounts {
		byID[account.id()] = account
	}
	for _, kind := range []string{usageSnapshotKind, resetCreditsSnapshotKind, creditBurnSnapshotKind} {
		snapshots, err := store.readAccountSnapshots(kind)
		if err != nil {
			return err
		}
		for _, snapshot := range snapshots {
			account := byID[snapshot.Account]
			if account == nil {
				continue
			}
			switch kind {
			case usageSnapshotKind:
				var payload usagePayload
				if err := json.Unmarshal(snapshot.Payload, &payload); err != nil {
					return fmt.Errorf("restore usage for %s: %w", snapshot.Account, err)
				}
				account.adopt(
					snapshot.FetchedAt,
					payload.PlanType,
					payload.RateLimit.PrimaryWindow.window(snapshot.FetchedAt),
					payload.RateLimit.SecondaryWindow.window(snapshot.FetchedAt),
					payload.bankedResets(),
					payload.SpendControl,
				)
				if payload.RateLimit.LimitReached != nil && *payload.RateLimit.LimitReached {
					account.markSpent()
				}
			case resetCreditsSnapshotKind:
				var payload resetCreditsPayload
				if err := json.Unmarshal(snapshot.Payload, &payload); err != nil {
					return fmt.Errorf("restore reset credits for %s: %w", snapshot.Account, err)
				}
				account.adoptResetCredits(snapshot.FetchedAt, payload.AvailableCount, payload.Credits)
			case creditBurnSnapshotKind:
				var payload creditBurnPayload
				if err := json.Unmarshal(snapshot.Payload, &payload); err != nil {
					return fmt.Errorf("restore credit burn for %s: %w", snapshot.Account, err)
				}
				account.adoptCreditBurn(snapshot.FetchedAt, payload.total())
			}
		}
	}
	return nil
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
		usageDue, creditBurnDue, resetCreditsDue := true, true, true
		if !force {
			usageDue, creditBurnDue, resetCreditsDue = account.pollsDue(time.Now(), every)
		}
		if !poll(usageDue, "usage", func() error { return s.pollUsage(ctx, account) }) {
			return
		}
		if account.needsReauth() {
			continue
		}
		if !force {
			_, creditBurnDue, resetCreditsDue = account.pollsDue(time.Now(), every)
		}
		if !poll(creditBurnDue, "credit burn", func() error {
			return s.pollCreditBurn(ctx, account, time.Now())
		}) {
			return
		}
		if account.needsReauth() {
			continue
		}
		if !poll(resetCreditsDue, "reset credits", func() error {
			return s.pollResetCredits(ctx, account)
		}) {
			return
		}
	}
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
