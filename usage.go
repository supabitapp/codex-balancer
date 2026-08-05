package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

var usageEndpoint = "https://chatgpt.com/backend-api/wham/usage"

type usagePayload struct {
	RateLimit struct {
		PrimaryWindow   usageWindow `json:"primary_window"`
		SecondaryWindow usageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

type usageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	ResetAt            int64    `json:"reset_at"`
	LimitWindowSeconds int      `json:"limit_window_seconds"`
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageEndpoint, nil)
	if err != nil {
		return err
	}
	account.mu.Lock()
	token := account.AccessToken
	account.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("chatgpt-account-id", account.id())

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return s.reauthorize(account)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("usage returned %s", resp.Status)
	}

	var payload usagePayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	account.adopt(payload.RateLimit.PrimaryWindow.window(), payload.RateLimit.SecondaryWindow.window())
	return nil
}

func (s *server) reauthorize(account *Account) error {
	if !s.refreshed(account, account.id()) {
		return fmt.Errorf("account %s needs reauth", account.id())
	}
	return nil
}

// adopt records freshly polled windows and lifts a cooldown the moment upstream
// says the window that caused it has room again.
func (a *Account) adopt(primary, secondary window) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if primary.known() {
		a.primary = primary
	}
	if secondary.known() {
		a.secondary = secondary
	}
	if a.dead == "" && !a.cooldown.IsZero() && a.pressure() < 100 {
		a.cooldown = time.Time{}
	}
}

func (s *server) watchUsage(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	poll := func() {
		for _, account := range s.pool.accounts {
			if err := s.pollUsage(ctx, account); err != nil && ctx.Err() == nil {
				s.log.Warn("usage poll failed", "account", account.id(), "error", err)
				s.stats.note("usage poll failed", account.id(), err.Error())
			}
		}
	}
	poll()

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}
