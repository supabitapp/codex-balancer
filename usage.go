package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

var accountAPIBaseURL = "https://chatgpt.com/backend-api/wham"

type usagePayload struct {
	RateLimit struct {
		LimitReached    *bool       `json:"limit_reached"`
		PrimaryWindow   usageWindow `json:"primary_window"`
		SecondaryWindow usageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	RateLimitResetCredits *struct {
		AvailableCount int64 `json:"available_count"`
	} `json:"rate_limit_reset_credits"`
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

func (u usageWindow) window() window {
	if u.UsedPercent == nil {
		return window{}
	}
	w := window{
		usedPercent: *u.UsedPercent,
		minutes:     u.LimitWindowSeconds / 60,
		seenAt:      time.Now(),
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
	account.adopt(
		payload.RateLimit.PrimaryWindow.window(),
		payload.RateLimit.SecondaryWindow.window(),
		payload.bankedResets(),
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
	s.restartSocketsForDraining()
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
	account.adoptResetCredits(payload.AvailableCount, payload.Credits)
	return nil
}

func (s *server) doAccountRequest(ctx context.Context, account *Account, method, endpoint string, body []byte) (*http.Response, error) {
	for canReauth := true; ; canReauth = false {
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
	account.adoptResetCredits(payload.AvailableCount, payload.Credits)
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

func (a *Account) adopt(primary, secondary window, banked *int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if primary.known() {
		a.primary = primary
	}
	if secondary.known() {
		a.secondary = secondary
	}
	if banked == nil {
		a.resetCredits = resetCreditState{}
	} else if !a.resetCredits.known || a.resetCredits.count != *banked {
		a.resetCredits = resetCreditState{known: true, count: *banked}
	}
	if (a.primary.known() || a.secondary.known()) && a.pressure() < 100 {
		a.spent = false
		if a.dead == "" {
			a.cooldown = time.Time{}
		}
	}
}

func (a *Account) adoptResetCredits(count int64, credits []resetCredit) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resetCredits = resetCreditState{
		known:   true,
		count:   count,
		details: append([]resetCredit(nil), credits...),
	}
}

func (s *server) pollAllUsage(ctx context.Context) {
	for _, account := range s.pool.all() {
		if account.needsReauth() {
			continue
		}
		if err := s.pollUsage(ctx, account); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("usage poll failed", "account", account.id(), "error", err)
			s.stats.note("usage poll failed", account.id(), err.Error())
		}
		if account.needsReauth() {
			continue
		}
		if err := s.pollResetCredits(ctx, account); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("reset credits poll failed", "account", account.id(), "error", err)
			s.stats.note("reset credits poll failed", account.id(), err.Error())
		}
	}
}

func (s *server) watchUsage(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollAllUsage(ctx)
		}
	}
}
