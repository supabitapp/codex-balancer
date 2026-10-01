package app

import (
	"bytes"
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

const adminKeysInterval = 30 * time.Second

var adminUpdateTemplates = append(append([]string{}, dashboardUpdateTemplates...), "keys-update")

type adminLoginView struct {
	Disabled bool
	CSRF     string
	Error    string
}

type adminKeyView struct {
	DOMID   string
	Name    string
	Active  bool
	Created string
	Input   string
	Cached  string
	Output  string
	Total   string
}

type dashboardAdminView struct {
	CSRF     string
	FastMode fastMode
	Keys     []adminKeyView
	Notice   string
	Error    bool
	Secret   string
}

type dashboardAdminControl struct {
	CSRF string
	Row  any
}

func (a *dashboardAdminView) With(row any) dashboardAdminControl {
	return dashboardAdminControl{CSRF: a.CSRF, Row: row}
}

func (a *dashboardAdminView) ActiveKeys() int {
	active := 0
	for _, key := range a.Keys {
		if key.Active {
			active++
		}
	}
	return active
}

func (a *dashboardAdminView) RevokedKeys() int {
	return len(a.Keys) - a.ActiveKeys()
}

func (s *server) adminRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin", s.requireAdmin(s.adminPage))
	mux.HandleFunc("GET /admin/{$}", s.requireAdmin(s.adminPage))
	mux.HandleFunc("GET /admin/login", s.adminLoginPage)
	mux.HandleFunc("POST /admin/login", s.adminLogin)
	mux.HandleFunc("POST /admin/logout", s.requireAdmin(s.adminLogout))
	mux.HandleFunc("POST /admin/settings", s.requireAdmin(s.adminSettings))
	mux.HandleFunc("POST /admin/accounts/{action}", s.requireAdmin(s.adminAccountAction))
	mux.HandleFunc("POST /admin/keys/{action}", s.requireAdmin(s.adminKeyAction))
	mux.HandleFunc("GET /admin/events", s.requireAdmin(s.adminEvents))
	protection := http.NewCrossOriginProtection()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' https://cdn.jsdelivr.net; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		if err := protection.Check(r); err != nil {
			http.Error(w, "Cross-site admin requests are not allowed.", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "Invalid or oversized form.", http.StatusBadRequest)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *server) renderAdminLogin(w http.ResponseWriter, status int, view adminLoginView) {
	var body bytes.Buffer
	if err := dashboardTemplate.ExecuteTemplate(&body, "login", view); err != nil {
		http.Error(w, "Could not render login.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body.Bytes())
}

func (s *server) adminPage(w http.ResponseWriter, r *http.Request, session adminSession) {
	s.renderAdmin(w, r, session, "", "", http.StatusOK)
}

func (s *server) adminKeys() ([]adminKeyView, error) {
	keys, err := s.pool.store.readAPIKeys()
	if err != nil {
		return nil, err
	}
	usage, err := s.pool.store.apiKeyUsage()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].RevokedAt.IsZero() != keys[j].RevokedAt.IsZero() {
			return keys[i].RevokedAt.IsZero()
		}
		return keys[i].CreatedAt.After(keys[j].CreatedAt)
	})
	views := make([]adminKeyView, 0, len(keys))
	for _, key := range keys {
		used := usage[key.Name]
		views = append(views, adminKeyView{
			DOMID:   dashboardDOMID("key", key.Name),
			Name:    key.Name,
			Active:  key.RevokedAt.IsZero(),
			Created: key.CreatedAt.Format("2006-01-02"),
			Input:   formatTokenCount(used.InputTokens),
			Cached:  formatTokenCount(used.InputDetails.CachedTokens),
			Output:  formatTokenCount(used.OutputTokens),
			Total:   formatTokenCount(used.TotalTokens),
		})
	}
	return views, nil
}

func (s *server) adminDashboard(now time.Time, session adminSession, keys []adminKeyView) dashboardView {
	view := s.dashboardAt(now, true)
	mode, _ := s.fastMode.snapshot()
	view.Admin = &dashboardAdminView{CSRF: session.csrf, FastMode: mode, Keys: keys}
	return view
}

func (s *server) renderAdmin(w http.ResponseWriter, r *http.Request, session adminSession, notice, secret string, status int) {
	keys, err := s.adminKeys()
	if err != nil {
		s.adminError(w, r, err)
		return
	}
	view := s.adminDashboard(time.Now(), session, keys)
	view.Admin.Notice, view.Admin.Secret, view.Admin.Error = notice, secret, status >= 400
	name := "admin-response"
	if r.Header.Get("HX-Request") != "true" {
		name = "page"
	}
	body, err := renderDashboard(name, view)
	if err != nil {
		s.adminError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}

func (s *server) adminSessionActive(token string, now time.Time) bool {
	hash, err := s.pool.store.raw.AdminPasswordHash()
	if err != nil || hash == "" {
		return false
	}
	_, ok, err := s.adminSession(token, hash, now)
	return err == nil && ok
}

func (s *server) adminEvents(w http.ResponseWriter, r *http.Request, session adminSession) {
	token := adminCookieValue(r, false)
	s.streamDashboard(w, func(send func([]byte) error) {
		ticker := time.NewTicker(dashboardInterval)
		defer ticker.Stop()
		done := s.done()
		previous := make(map[string][]byte, len(adminUpdateTemplates))
		var keys []adminKeyView
		var keysAt time.Time
		for {
			now := time.Now()
			if !s.adminSessionActive(token, now) {
				return
			}
			if now.Sub(keysAt) >= adminKeysInterval {
				loaded, err := s.adminKeys()
				if err != nil {
					s.log.Error("admin stream failed", "error", err)
					return
				}
				keys, keysAt = loaded, now
			}
			payload, err := renderDashboardUpdates(adminUpdateTemplates, s.adminDashboard(now, session, keys), previous)
			if err != nil {
				s.log.Error("admin stream failed", "error", err)
				return
			}
			if len(payload) > 0 && send(payload) != nil {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-done:
				return
			case <-ticker.C:
			}
		}
	})
}

func (s *server) adminSettings(w http.ResponseWriter, r *http.Request, session adminSession) {
	mode := fastMode(r.PostForm.Get("fast-mode"))
	if !mode.valid() {
		s.renderAdmin(w, r, session, "Choose a valid fast mode.", "", http.StatusUnprocessableEntity)
		return
	}
	if err := s.saveFastMode(mode); err != nil {
		s.adminError(w, r, err)
		return
	}
	s.stats.note("fast mode changed", "", mode.label())
	s.renderAdmin(w, r, session, "Saved. Fast mode: "+mode.label()+".", "", http.StatusOK)
}

func (s *server) adminAccountAction(w http.ResponseWriter, r *http.Request, session adminSession) {
	action := r.PathValue("action")
	if action != "pause" && action != "mode" && action != "remove" && action != "reset" && action != "refresh" {
		http.NotFound(w, r)
		return
	}
	account := s.pool.find(r.PostForm.Get("account"))
	if account == nil {
		s.renderAdmin(w, r, session, "Account no longer exists.", "", http.StatusNotFound)
		return
	}
	var err error
	notice := ""
	switch action {
	case "refresh":
		s.adminRefreshAccount(w, r, session, account)
		return
	case "reset":
		s.adminBankedReset(w, r, session, account)
		return
	case "pause":
		value := r.PostForm.Get("paused")
		if value != "true" && value != "false" {
			s.renderAdmin(w, r, session, "Choose pause or resume.", "", http.StatusUnprocessableEntity)
			return
		}
		paused := value == "true"
		err = s.pool.setPaused(account, paused)
		if err == nil && paused {
			s.invalidateAccount(account.id(), routingReasonOwnerPaused)
		}
		notice = "Account resumed."
		if paused {
			notice = "Account paused. Existing connections are reconnecting."
		}
	case "mode":
		mode := routingMode(r.PostForm.Get("mode"))
		if mode != routingModeNormal && mode != routingModePriority && mode != routingModePaused {
			s.renderAdmin(w, r, session, "Choose normal, priority, or paused routing.", "", http.StatusUnprocessableEntity)
			return
		}
		err = s.pool.setRoutingMode(account, mode)
		notice = "Routing preference saved."
		if err == nil && mode == routingModePaused {
			s.invalidateAccount(account.id(), routingReasonOwnerPaused)
			notice = "Account paused. Existing connections are reconnecting."
		}
	case "remove":
		err = s.pool.remove(account)
		if err == nil {
			s.invalidateAccount(account.id(), routingReasonOwnerRemoved)
			s.catalog.invalidate()
		}
		notice = "Account removed."
	}
	if err != nil {
		s.adminError(w, r, err)
		return
	}
	s.stats.note("admin account "+action, account.id(), notice)
	s.renderAdmin(w, r, session, notice, "", http.StatusOK)
}

func (s *server) adminKeyAction(w http.ResponseWriter, r *http.Request, session adminSession) {
	action := r.PathValue("action")
	if action != "add" && action != "revoke" {
		http.NotFound(w, r)
		return
	}
	name := r.PostForm.Get("name")
	if name == "" || len(name) > 128 || name != strings.TrimSpace(name) || strings.ContainsAny(name, "\t\r\n") {
		s.renderAdmin(w, r, session, "Use a key name of 1–128 bytes without surrounding whitespace or line breaks.", "", http.StatusUnprocessableEntity)
		return
	}
	secret, notice := "", ""
	if action == "add" {
		keys, err := s.pool.store.readAPIKeys()
		if err != nil {
			s.adminError(w, r, err)
			return
		}
		for _, key := range keys {
			if key.Name == name {
				s.renderAdmin(w, r, session, "That key name is already in use. Choose another.", "", http.StatusConflict)
				return
			}
		}
		secret, err = generateAPIKey()
		if err == nil {
			err = s.pool.store.addAPIKey(storedAPIKey{Name: name, Secret: secret, CreatedAt: time.Now()})
		}
		if err != nil {
			s.adminError(w, r, err)
			return
		}
		notice = "Key created. Copy it now; it will not be shown again."
	} else {
		revoked, err := s.pool.store.revokeAPIKey(name, time.Now())
		if err != nil {
			s.adminError(w, r, err)
			return
		}
		if !revoked {
			s.renderAdmin(w, r, session, "Active key not found.", "", http.StatusNotFound)
			return
		}
		notice = "Key revoked. New requests using it will be rejected."
	}
	s.stats.note("admin key "+action, "", name)
	s.renderAdmin(w, r, session, notice, secret, http.StatusOK)
}

func (s *server) adminRefreshAccount(w http.ResponseWriter, r *http.Request, session adminSession, account *Account) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	usageErr := s.pollUsage(ctx, account)
	creditsErr := s.pollResetCredits(ctx, account)
	notice, status := "Quota and banked credits refreshed for "+label(account)+".", http.StatusOK
	if usageErr != nil || creditsErr != nil {
		s.log.Warn("admin account refresh failed", "account", account.id(), "usage_error", usageErr, "credits_error", creditsErr)
		notice, status = "Could not refresh all account data. Try again.", http.StatusBadGateway
	}
	s.stats.note("admin account refresh", account.id(), notice)
	s.renderAdmin(w, r, session, notice, "", status)
}

func (s *server) adminBankedReset(w http.ResponseWriter, r *http.Request, session adminSession, account *Account) {
	creditID := r.PostForm.Get("credit")
	if creditID == "" {
		s.renderAdmin(w, r, session, "Choose an available banked reset.", "", http.StatusUnprocessableEntity)
		return
	}
	account.resetMu.Lock()
	defer account.resetMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, selected, err := s.consumeResetCredit(ctx, account, func(credits []resetCredit) (resetCredit, bool) {
		for _, credit := range credits {
			if credit.ID == creditID && credit.available() && (credit.ExpiresAt == nil || credit.ExpiresAt.After(time.Now())) {
				return credit, true
			}
		}
		return resetCredit{}, false
	})
	if err != nil {
		s.log.Warn("admin banked reset failed", "account", account.id(), "error", err)
		s.renderAdmin(w, r, session, "Could not confirm the banked reset. Refresh before trying again.", "", http.StatusBadGateway)
		return
	}
	notice, status := "Banked reset applied.", http.StatusOK
	switch {
	case selected == "", result.Code == "no_credit", result.Code == "already_redeemed":
		notice, status = "That banked reset is no longer available.", http.StatusConflict
	case result.Code == "nothing_to_reset":
		notice = "The account has no rate limits to reset."
	}
	s.stats.note("admin account reset", account.id(), notice)
	if result.Code == "reset" {
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	usageErr := s.pollUsage(ctx, account)
	creditsErr := s.pollResetCredits(ctx, account)
	if usageErr != nil || creditsErr != nil {
		notice += " Could not refresh account data; it will update on the next successful poll."
	}
	s.renderAdmin(w, r, session, notice, "", status)
}
