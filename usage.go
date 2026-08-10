package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const resetCreditLead = time.Hour

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

func (p usagePayload) spent() bool {
	if p.RateLimit.LimitReached != nil {
		return *p.RateLimit.LimitReached
	}
	return p.RateLimit.PrimaryWindow.window().usedPercent >= 100 ||
		p.RateLimit.SecondaryWindow.window().usedPercent >= 100
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
		payload.spent(),
	)
	var limitReached any
	if payload.RateLimit.LimitReached != nil {
		limitReached = *payload.RateLimit.LimitReached
	}
	attrs := []any{"reported_limit_reached", limitReached}
	attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
	s.log.Debug("usage polled", attrs...)
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

func expiringResetCredit(credits []resetCredit, now time.Time) (resetCredit, bool) {
	deadline := now.Add(resetCreditLead)
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

func (s *server) consumeExpiringResetCredit(ctx context.Context, account *Account, now time.Time) (consumeResetCreditResponse, string, error) {
	if count, _, known := account.bankedResets(); !known || count <= 0 {
		return consumeResetCreditResponse{}, "", nil
	}

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

func (s *server) reauthorize(account *Account) error {
	if !s.refreshed(account, account.id()) {
		return fmt.Errorf("account %s needs reauth", account.id())
	}
	return nil
}

func (a *Account) adopt(primary, secondary window, banked *int64, spent bool) {
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
	a.spent = spent
	if a.dead == "" && !spent {
		a.cooldown = time.Time{}
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
		if err := s.pollUsage(ctx, account); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("usage poll failed", "account", account.id(), "error", err)
			s.stats.note("usage poll failed", account.id(), err.Error())
			continue
		}

		result, creditID, err := s.consumeExpiringResetCredit(ctx, account, time.Now())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("automatic reset failed", "account", account.id(), "credit", creditID, "error", err)
			s.stats.note("automatic reset failed", account.id(), err.Error())
			continue
		}
		if creditID == "" {
			continue
		}

		attrs := []any{"account", account.id(), "credit", creditID, "outcome", result.Code, "windows_reset", result.WindowsReset}
		if result.Code == "reset" || result.Code == "already_redeemed" {
			s.log.Info("expiring reset credit redeemed", attrs...)
			s.stats.note("account reset", account.id(), result.Code)
		} else {
			s.log.Debug("expiring reset credit kept", attrs...)
		}
		if result.Code != "nothing_to_reset" {
			if err := s.pollUsage(ctx, account); err != nil && ctx.Err() == nil {
				s.log.Warn("usage refresh after reset failed", "account", account.id(), "error", err)
				s.stats.note("usage refresh after reset failed", account.id(), err.Error())
			}
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
