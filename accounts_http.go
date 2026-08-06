package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

var errAccountLoginPending = errors.New("an account login is already pending")

type accountLoginStore struct {
	mu      sync.Mutex
	pending bool
}

func (s *accountLoginStore) reserve() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending {
		return errAccountLoginPending
	}
	s.pending = true
	return nil
}

func (s *accountLoginStore) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = false
}

type accountLoginResponse struct {
	Status          string    `json:"status"`
	VerificationURL string    `json:"verification_url"`
	UserCode        string    `json:"user_code"`
	ExpiresAt       time.Time `json:"expires_at"`
}

func (s *server) startAccountLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	if err := s.logins.reserve(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	issuer := s.accountAuthIssuer()
	device, err := requestDeviceAuthorization(
		r.Context(),
		s.client,
		deviceAuthIssuer(issuer)+"/api/accounts/deviceauth/usercode",
	)
	if err != nil {
		s.logins.release()
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go s.completeAccountLogin(ctx, issuer, device)

	writeJSON(w, http.StatusAccepted, accountLoginResponse{
		Status:          "pending",
		VerificationURL: deviceVerificationURL(issuer),
		UserCode:        device.userCode,
		ExpiresAt:       time.Now().Add(deviceAuthTimeout),
	})
}

func (s *server) completeAccountLogin(ctx context.Context, issuer string, device deviceAuthorization) {
	defer s.logins.release()
	account, err := completeDeviceAuthorization(ctx, s.client, issuer, device)
	if err == nil {
		err = s.pool.add(account)
	}
	if err != nil {
		s.stats.note("account login failed", "", err.Error())
		return
	}
	s.stats.note("account added", account.id(), "")
}

func (s *server) accountAuthIssuer() string {
	if s.authIssuer == "" {
		return authBaseURL
	}
	return s.authIssuer
}
