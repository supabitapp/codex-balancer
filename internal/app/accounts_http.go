package app

import (
	"bytes"
	"context"
	"html/template"
	"net/http"
	"sync"
	"time"
)

type accountLogin struct {
	device    deviceAuthorization
	expiresAt time.Time
}

type accountLoginStore struct {
	mu        sync.Mutex
	active    *accountLogin
	completed bool
}

func (s *accountLoginStore) getOrCreate(create func() (deviceAuthorization, error)) (accountLogin, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		return *s.active, false, nil
	}
	device, err := create()
	if err != nil {
		return accountLogin{}, false, err
	}
	login := accountLogin{
		device:    device,
		expiresAt: time.Now().Add(deviceAuthTimeout),
	}
	s.active = &login
	s.completed = false
	return login, true, nil
}

func (s *accountLoginStore) release(authID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active.device.authID == authID {
		s.active = nil
	}
}

func (s *accountLoginStore) finish(authID string, completed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active.device.authID == authID {
		s.active = nil
		s.completed = completed
	}
}

func (s *accountLoginStore) status() (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active != nil, s.completed
}

type accountLoginPageData struct {
	VerificationURL string
	UserCode        string
}

var accountLoginPage = template.Must(webTemplate("accounts.html").ParseFS(dashboardFiles, "web/accounts.html"))

func (s *server) accountsPage(w http.ResponseWriter, r *http.Request) {
	issuer := s.accountAuthIssuer()
	login, created, err := s.logins.getOrCreate(func() (deviceAuthorization, error) {
		return requestDeviceAuthorization(
			r.Context(),
			s.client,
			deviceAuthIssuer(issuer)+"/api/accounts/deviceauth/usercode",
		)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var page bytes.Buffer
	if err := accountLoginPage.Execute(&page, accountLoginPageData{
		VerificationURL: deviceVerificationURL(issuer),
		UserCode:        login.device.userCode,
	}); err != nil {
		if created {
			s.logins.release(login.device.authID)
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if created {
		ctx := s.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		go s.completeAccountLogin(ctx, issuer, login.device)
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self' "+waterCSSURL+"; script-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(page.Bytes())
}

func (s *server) accountLoginStatus(w http.ResponseWriter, _ *http.Request) {
	active, completed := s.logins.status()
	if completed {
		w.WriteHeader(http.StatusOK)
		return
	}
	if active {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "account sign-in failed", http.StatusGone)
}

func minutesUntil(now, deadline time.Time) int {
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return int((remaining + time.Minute - 1) / time.Minute)
}

func (s *server) completeAccountLogin(ctx context.Context, issuer string, device deviceAuthorization) {
	account, err := completeDeviceAuthorization(ctx, s.client, issuer, device)
	if err == nil {
		err = s.pool.add(account)
	}
	if err != nil {
		s.logins.finish(device.authID, false)
		s.stats.note("account login failed", "", err.Error())
		return
	}
	s.logins.finish(device.authID, true)
	s.stats.note("account added", account.id(), "")
}

func (s *server) accountAuthIssuer() string {
	if s.authIssuer == "" {
		return authBaseURL
	}
	return s.authIssuer
}
