package app

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"
	"time"
)

var adminTemplate = template.Must(webTemplate("admin").ParseFS(dashboardFiles, "web/admin.html"))

type adminLoginView struct {
	Disabled bool
	CSRF     string
	Error    string
}

type adminAccountView struct {
	ID     string
	Name   string
	Plan   string
	Status string
	Paused bool
	Mode   string
}

type adminKeyView struct {
	Name    string
	Active  bool
	Created string
	Input   string
	Cached  string
	Output  string
	Total   string
}

type adminView struct {
	CSRF        string
	Mode        fastMode
	ModeLabel   string
	Accounts    []adminAccountView
	Keys        []adminKeyView
	Connections int64
	Events      []Event
	Notice      string
	NoticePanel string
	Error       bool
	Secret      string
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
	mux.HandleFunc("GET /admin/status", s.requireAdmin(func(w http.ResponseWriter, r *http.Request, session adminSession) {
		s.renderAdmin(w, r, session, "status-panel", "", "", http.StatusOK)
	}))
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
	if err := adminTemplate.ExecuteTemplate(&body, "login", view); err != nil {
		http.Error(w, "Could not render login.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body.Bytes())
}

func (s *server) adminPage(w http.ResponseWriter, r *http.Request, session adminSession) {
	s.renderAdmin(w, r, session, "page", "", "", http.StatusOK)
}

func (s *server) renderAdmin(w http.ResponseWriter, r *http.Request, session adminSession, panel, notice, secret string, status int) {
	view := adminView{CSRF: session.csrf, Notice: notice, NoticePanel: panel, Secret: secret, Error: status >= 400}
	view.Mode, _ = s.fastMode.snapshot()
	view.ModeLabel = view.Mode.label()
	for _, account := range s.pool.sorted() {
		candidate := account.routingCandidate()
		view.Accounts = append(view.Accounts, adminAccountView{ID: account.id(), Name: label(account), Plan: account.plan(), Status: dashboardStatus(candidate.status(time.Now())).Label, Paused: candidate.paused, Mode: string(candidate.mode)})
	}
	keys, err := s.pool.store.readAPIKeys()
	if err != nil {
		s.adminError(w, r, err)
		return
	}
	usage, err := s.pool.store.apiKeyUsage()
	if err != nil {
		s.adminError(w, r, err)
		return
	}
	for _, key := range keys {
		used := usage[key.Name]
		view.Keys = append(view.Keys, adminKeyView{Name: key.Name, Active: key.RevokedAt.IsZero(), Created: key.CreatedAt.Format("2006-01-02"), Input: formatTokenCount(used.InputTokens), Cached: formatTokenCount(used.InputDetails.CachedTokens), Output: formatTokenCount(used.OutputTokens), Total: formatTokenCount(used.TotalTokens)})
	}
	snapshot := s.stats.snapshot()
	view.Connections = snapshot.WSOpen
	for i := len(snapshot.Events) - 1; i >= 0 && len(view.Events) < 20; i-- {
		view.Events = append(view.Events, snapshot.Events[i])
	}
	name := panel
	if r.Header.Get("HX-Request") != "true" {
		name = "page"
	}
	var body bytes.Buffer
	if err := adminTemplate.ExecuteTemplate(&body, name, view); err != nil {
		s.adminError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body.Bytes())
}

func (s *server) adminSettings(w http.ResponseWriter, r *http.Request, session adminSession) {
	mode := fastMode(r.PostForm.Get("fast-mode"))
	if !mode.valid() {
		s.renderAdmin(w, r, session, "settings-panel", "Choose a valid fast mode.", "", http.StatusUnprocessableEntity)
		return
	}
	if err := s.saveFastMode(mode); err != nil {
		s.adminError(w, r, err)
		return
	}
	s.stats.note("fast mode changed", "", mode.label())
	s.renderAdmin(w, r, session, "settings-panel", "Saved. Fast mode: "+mode.label()+".", "", http.StatusOK)
}

func (s *server) adminAccountAction(w http.ResponseWriter, r *http.Request, session adminSession) {
	action := r.PathValue("action")
	if action != "pause" && action != "mode" && action != "remove" {
		http.NotFound(w, r)
		return
	}
	account := s.pool.find(r.PostForm.Get("account"))
	if account == nil {
		s.renderAdmin(w, r, session, "accounts-panel", "Account no longer exists.", "", http.StatusNotFound)
		return
	}
	var err error
	notice := ""
	switch action {
	case "pause":
		value := r.PostForm.Get("paused")
		if value != "true" && value != "false" {
			s.renderAdmin(w, r, session, "accounts-panel", "Choose pause or resume.", "", http.StatusUnprocessableEntity)
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
		if mode != routingModeNormal && mode != routingModePriority {
			s.renderAdmin(w, r, session, "accounts-panel", "Choose normal or priority routing.", "", http.StatusUnprocessableEntity)
			return
		}
		err = s.pool.setRoutingMode(account, mode)
		notice = "Routing preference saved."
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
	s.renderAdmin(w, r, session, "accounts-panel", notice, "", http.StatusOK)
}

func (s *server) adminKeyAction(w http.ResponseWriter, r *http.Request, session adminSession) {
	action := r.PathValue("action")
	if action != "add" && action != "revoke" {
		http.NotFound(w, r)
		return
	}
	name := r.PostForm.Get("name")
	if name == "" || len(name) > 128 || name != strings.TrimSpace(name) || strings.ContainsAny(name, "\t\r\n") {
		s.renderAdmin(w, r, session, "keys-panel", "Use a key name of 1–128 bytes without surrounding whitespace or line breaks.", "", http.StatusUnprocessableEntity)
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
				s.renderAdmin(w, r, session, "keys-panel", "That key name is already in use. Choose another.", "", http.StatusConflict)
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
			s.renderAdmin(w, r, session, "keys-panel", "Active key not found.", "", http.StatusNotFound)
			return
		}
		notice = "Key revoked. New requests using it will be rejected."
	}
	s.stats.note("admin key "+action, "", name)
	s.renderAdmin(w, r, session, "keys-panel", notice, secret, http.StatusOK)
}
