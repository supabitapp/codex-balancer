package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

var errAccountLoginPending = errors.New("an account login is already pending")

type accountLoginStatus string

const (
	accountLoginPending  accountLoginStatus = "pending"
	accountLoginComplete accountLoginStatus = "complete"
	accountLoginFailed   accountLoginStatus = "failed"
)

type accountLoginSession struct {
	status    accountLoginStatus
	device    deviceAuthorization
	expiresAt time.Time
	accountID string
	failure   string
}

type accountLoginStore struct {
	mu      sync.Mutex
	id      string
	session accountLoginSession
}

func (s *accountLoginStore) reserve() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session.status == accountLoginPending {
		return "", errAccountLoginPending
	}
	s.id = randomToken(24)
	s.session = accountLoginSession{status: accountLoginPending}
	return s.id, nil
}

func (s *accountLoginStore) start(id string, device deviceAuthorization, expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == id {
		s.session.device = device
		s.session.expiresAt = expiresAt
	}
}

func (s *accountLoginStore) finish(id, accountID, failure string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id != id {
		return
	}
	s.session.accountID = accountID
	s.session.failure = failure
	s.session.device = deviceAuthorization{}
	s.session.expiresAt = time.Time{}
	s.session.status = accountLoginComplete
	if failure != "" {
		s.session.status = accountLoginFailed
	}
}

func (s *accountLoginStore) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == id {
		s.id = ""
		s.session = accountLoginSession{}
	}
}

func (s *accountLoginStore) get(id string) (accountLoginSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id != id {
		return accountLoginSession{}, false
	}
	return s.session, true
}

type accountLoginResponse struct {
	ID              string                       `json:"id"`
	Status          accountLoginStatus           `json:"status"`
	VerificationURL string                       `json:"verification_url,omitempty"`
	UserCode        string                       `json:"user_code,omitempty"`
	ExpiresAt       *time.Time                   `json:"expires_at,omitempty"`
	Account         *accountLoginAccountResponse `json:"account,omitempty"`
	Error           string                       `json:"error,omitempty"`
}

type accountLoginAccountResponse struct {
	ID    string `json:"id"`
	Email string `json:"email,omitempty"`
	Plan  string `json:"plan"`
}

func (s *server) startAccountLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	id, err := s.logins.reserve()
	if err != nil {
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
		s.logins.remove(id)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.logins.start(id, device, time.Now().Add(deviceAuthTimeout))
	response, _ := s.accountLoginResponse(id)
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go s.completeAccountLogin(ctx, id, issuer, device)

	w.Header().Set("Location", "/accounts/"+id)
	writeJSON(w, http.StatusAccepted, response)
}

func (s *server) accountLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	response, ok := s.accountLoginResponse(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "account login not found")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) completeAccountLogin(ctx context.Context, id, issuer string, device deviceAuthorization) {
	account, err := completeDeviceAuthorization(ctx, s.client, issuer, device)
	if err == nil {
		err = s.pool.add(account)
	}
	if err != nil {
		s.logins.finish(id, "", err.Error())
		s.stats.note("account login failed", "", err.Error())
		return
	}
	s.logins.finish(id, account.id(), "")
	s.stats.note("account added", account.id(), "")
}

func (s *server) accountLoginResponse(id string) (accountLoginResponse, bool) {
	session, ok := s.logins.get(id)
	if !ok {
		return accountLoginResponse{}, false
	}
	response := accountLoginResponse{ID: id, Status: session.status, Error: session.failure}
	switch session.status {
	case accountLoginPending:
		expiresAt := session.expiresAt
		response.VerificationURL = deviceVerificationURL(s.accountAuthIssuer())
		response.UserCode = session.device.userCode
		response.ExpiresAt = &expiresAt
	case accountLoginComplete:
		response.Account = &accountLoginAccountResponse{ID: session.accountID}
		if account := s.pool.find(session.accountID); account != nil {
			claims := account.claims()
			response.Account.Email = maskEmail(claims.Email)
			response.Account.Plan = claims.Auth.Plan
		}
	}
	return response, true
}

func (s *server) accountAuthIssuer() string {
	if s.authIssuer == "" {
		return authBaseURL
	}
	return s.authIssuer
}
