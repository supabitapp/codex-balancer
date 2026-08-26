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
	mu     sync.Mutex
	active *accountLogin
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
	return login, true, nil
}

func (s *accountLoginStore) release(authID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active.device.authID == authID {
		s.active = nil
	}
}

type accountLoginPageData struct {
	VerificationURL string
	UserCode        string
	ExpiresIn       int
}

var accountLoginPage = template.Must(template.ParseFS(dashboardFiles, "web/accounts.html"))

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
		ExpiresIn:       minutesUntil(time.Now(), login.expiresAt),
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
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(page.Bytes())
}

func minutesUntil(now, deadline time.Time) int {
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return int((remaining + time.Minute - 1) / time.Minute)
}

func (s *server) completeAccountLogin(ctx context.Context, issuer string, device deviceAuthorization) {
	defer s.logins.release(device.authID)
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
