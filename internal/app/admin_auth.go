package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	statepkg "github.com/supabitapp/codex-balancer/internal/state"
)

const (
	adminSessionTTL     = 30 * 24 * time.Hour
	adminSessionRenewal = 24 * time.Hour
	adminSessionLimit   = 128
)

type adminSession struct {
	id      []byte
	csrf    string
	expires time.Time
}

type adminAuth struct {
	mu            sync.Mutex
	loginWindow   time.Time
	loginAttempts int
}

func adminRandomToken() string { return rand.Text() }

func adminDigest(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}

func (s *server) newAdminSession(credential string, now time.Time) (string, adminSession, error) {
	token := adminRandomToken()
	session := adminSession{id: adminDigest(token), csrf: adminRandomToken(), expires: now.Add(adminSessionTTL)}
	err := s.pool.store.raw.CreateAdminSession(statepkg.AdminSession{
		ID:         session.id,
		CSRF:       session.csrf,
		Credential: adminDigest(credential),
		ExpiresAt:  session.expires,
	}, now, adminSessionLimit)
	return token, session, err
}

func (s *server) adminSession(token, credential string, now time.Time) (adminSession, bool, error) {
	if token == "" || credential == "" {
		return adminSession{}, false, nil
	}
	id := adminDigest(token)
	stored, ok, err := s.pool.store.raw.AdminSession(id)
	if err != nil || !ok {
		return adminSession{}, false, err
	}
	if subtle.ConstantTimeCompare(stored.Credential, adminDigest(credential)) != 1 || !now.Before(stored.ExpiresAt) {
		return adminSession{}, false, s.pool.store.raw.DeleteAdminSession(id)
	}
	return adminSession{id: id, csrf: stored.CSRF, expires: stored.ExpiresAt}, true, nil
}

func (s *server) endAdminSession(token string) error {
	if token == "" {
		return nil
	}
	return s.pool.store.raw.DeleteAdminSession(adminDigest(token))
}

func (s *server) renewAdminSession(w http.ResponseWriter, r *http.Request, token string, session *adminSession, now time.Time) error {
	if session.expires.Sub(now) > adminSessionTTL-adminSessionRenewal {
		return nil
	}
	expires := now.Add(adminSessionTTL)
	if err := s.pool.store.raw.ExtendAdminSession(session.id, expires); err != nil {
		return err
	}
	session.expires = expires
	setAdminSessionCookie(w, r, token, expires)
	return nil
}

func setAdminSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	cookie := adminCookie(r, false)
	cookie.Value = token
	cookie.MaxAge = int(adminSessionTTL.Seconds())
	cookie.Expires = expires
	http.SetCookie(w, cookie)
}

// A global bound also limits password-hashing work behind a reverse proxy,
// without relying on untrusted forwarded client addresses.
func (a *adminAuth) allowLogin(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if now.Sub(a.loginWindow) >= time.Minute {
		a.loginWindow = now
		a.loginAttempts = 0
	}
	if a.loginAttempts >= 10 {
		return false
	}
	a.loginAttempts++
	return true
}

func adminCookie(r *http.Request, login bool) *http.Cookie {
	host := r.Host
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	localHost := host == "localhost" || net.ParseIP(strings.Trim(host, "[]")).IsLoopback()
	local := localHost && net.ParseIP(peer).IsLoopback() && r.TLS == nil
	name := "__Host-cb-admin"
	if local {
		name = "cb-admin-local"
	}
	if login {
		name += "-login"
	}
	return &http.Cookie{Name: name, Path: "/", HttpOnly: true, Secure: !local, SameSite: http.SameSiteStrictMode}
}

func adminCookieValue(r *http.Request, login bool) string {
	cookie, err := r.Cookie(adminCookie(r, login).Name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func clearAdminCookie(w http.ResponseWriter, r *http.Request, login bool) {
	cookie := adminCookie(r, login)
	cookie.MaxAge = -1
	cookie.Expires = time.Unix(1, 0)
	http.SetCookie(w, cookie)
}

func loginCSRFToken(credential string, now time.Time) string {
	payload := strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10) + "." + adminRandomToken()
	mac := hmac.New(sha256.New, []byte(credential))
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validLoginCSRF(token, credential string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || expiry <= now.Unix() || expiry > now.Add(10*time.Minute).Unix() {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(credential))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	return hmac.Equal(signature, mac.Sum(nil))
}

func adminCSRFMatches(got, want string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *server) adminCredential(w http.ResponseWriter, r *http.Request) (string, bool) {
	hash, err := s.pool.store.raw.AdminPasswordHash()
	if err != nil {
		s.adminError(w, r, err)
		return "", false
	}
	if hash == "" {
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/admin/login")
		}
		s.renderAdminLogin(w, http.StatusServiceUnavailable, adminLoginView{Disabled: true})
		return "", false
	}
	return hash, true
}

func (s *server) requireAdmin(next func(http.ResponseWriter, *http.Request, adminSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash, ok := s.adminCredential(w, r)
		if !ok {
			return
		}
		now := time.Now()
		token := adminCookieValue(r, false)
		session, ok, err := s.adminSession(token, hash, now)
		if err != nil {
			s.adminError(w, r, err)
			return
		}
		if !ok {
			clearAdminCookie(w, r, false)
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/admin/login")
				w.WriteHeader(http.StatusUnauthorized)
			} else {
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			}
			return
		}
		if r.Method == http.MethodPost && !adminCSRFMatches(r.PostForm.Get("csrf"), session.csrf) {
			http.Error(w, "This form has expired. Reload the admin page and try again.", http.StatusForbidden)
			return
		}
		if err := s.renewAdminSession(w, r, token, &session, now); err != nil {
			s.adminError(w, r, err)
			return
		}
		next(w, r, session)
	}
}

func (s *server) adminLoginPage(w http.ResponseWriter, r *http.Request) {
	hash, ok := s.adminCredential(w, r)
	if !ok {
		return
	}
	if _, ok, err := s.adminSession(adminCookieValue(r, false), hash, time.Now()); err == nil && ok {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.showAdminLogin(w, r, hash, http.StatusOK, "")
}

func (s *server) showAdminLogin(w http.ResponseWriter, r *http.Request, hash string, status int, message string) {
	token := loginCSRFToken(hash, time.Now())
	cookie := adminCookie(r, true)
	cookie.Value = token
	cookie.MaxAge = 600
	http.SetCookie(w, cookie)
	s.renderAdminLogin(w, status, adminLoginView{CSRF: token, Error: message})
}

func (s *server) adminLogin(w http.ResponseWriter, r *http.Request) {
	hash, ok := s.adminCredential(w, r)
	if !ok {
		return
	}
	csrf := r.PostForm.Get("csrf")
	if !adminCSRFMatches(csrf, adminCookieValue(r, true)) || !validLoginCSRF(csrf, hash, time.Now()) {
		s.showAdminLogin(w, r, hash, http.StatusForbidden, "This login form has expired. Please try again.")
		return
	}
	if !s.admin.allowLogin(time.Now()) {
		w.Header().Set("Retry-After", "60")
		s.showAdminLogin(w, r, hash, http.StatusTooManyRequests, "Too many login attempts. Please wait a minute.")
		return
	}
	if !verifyAdminPassword(hash, []byte(r.PostForm.Get("password"))) {
		s.showAdminLogin(w, r, hash, http.StatusUnauthorized, "Incorrect password.")
		return
	}
	if err := s.endAdminSession(adminCookieValue(r, false)); err != nil {
		s.adminError(w, r, err)
		return
	}
	token, session, err := s.newAdminSession(hash, time.Now())
	if err != nil {
		s.adminError(w, r, err)
		return
	}
	setAdminSessionCookie(w, r, token, session.expires)
	clearAdminCookie(w, r, true)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *server) adminLogout(w http.ResponseWriter, r *http.Request, _ adminSession) {
	clearAdminCookie(w, r, false)
	if err := s.endAdminSession(adminCookieValue(r, false)); err != nil {
		s.adminError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (s *server) adminError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("admin request failed", "path", r.URL.Path, "error", err)
	http.Error(w, "Could not complete the admin request. Please try again.", http.StatusInternalServerError)
}
