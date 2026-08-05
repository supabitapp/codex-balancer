package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	oauthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	refreshAfter  = 8 * 24 * time.Hour
)

var oauthEndpoint = "https://auth.openai.com/oauth/token"

type Account struct {
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	AccountID    string    `json:"account_id,omitempty"`
	LastRefresh  time.Time `json:"last_refresh"`

	mu          sync.Mutex
	inflight    chan struct{}
	lastRefresh error

	cooldown time.Time
	dead     string
	pressure float64
	lastUsed time.Time
}

type persistedAccount Account

func (a *Account) MarshalJSON() ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return json.Marshal(&persistedAccount{
		IDToken:      a.IDToken,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		AccountID:    a.AccountID,
		LastRefresh:  a.LastRefresh,
	})
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
	var c authClaims
	jwtClaims(a.IDToken, &c)
	return c
}

func (a *Account) Expires() time.Time {
	var c struct {
		Exp int64 `json:"exp"`
	}
	jwtClaims(a.AccessToken, &c)
	if c.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(c.Exp, 0)
}

func (a *Account) ID() string {
	if a.AccountID != "" {
		return a.AccountID
	}
	return a.claims().Auth.AccountID
}

func (a *Account) Email() string { return a.claims().Email }
func (a *Account) Plan() string  { return a.claims().Auth.Plan }

func (a *Account) available(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dead == "" && now.After(a.cooldown)
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

type refreshResponse struct {
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

func (a *Account) refresh(ctx context.Context, hc *http.Client) error {
	a.mu.Lock()
	if wait := a.inflight; wait != nil {
		a.mu.Unlock()
		select {
		case <-wait:
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.lastRefresh
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if a.dead != "" {
		err := fmt.Errorf("account %s needs reauth: %s", a.ID(), a.dead)
		a.mu.Unlock()
		return err
	}
	done := make(chan struct{})
	a.inflight = done
	token := a.RefreshToken
	a.mu.Unlock()

	tokens, permanent, err := exchangeRefreshToken(ctx, hc, token)

	a.mu.Lock()
	a.inflight = nil
	a.lastRefresh = err
	switch {
	case err != nil && permanent:
		a.dead = err.Error()
	case err == nil:
		a.AccessToken = tokens.AccessToken
		if tokens.RefreshToken != "" {
			a.RefreshToken = tokens.RefreshToken
		}
		if tokens.IDToken != "" {
			a.IDToken = tokens.IDToken
		}
		a.LastRefresh = time.Now()
	}
	a.mu.Unlock()
	close(done)
	return err
}

func exchangeRefreshToken(ctx context.Context, hc *http.Client, token string) (refreshResponse, bool, error) {
	var out refreshResponse
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
		text := readCapped(resp.Body, 4<<10)
		for _, reason := range permanentRefreshFailures {
			if strings.Contains(text, reason) {
				return out, true, fmt.Errorf("%s", reason)
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

func readCapped(r io.Reader, limit int64) string {
	data, _ := io.ReadAll(io.LimitReader(r, limit))
	return string(data)
}
