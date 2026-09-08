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
)

const adminSessionTTL = 12 * time.Hour

type adminSession struct {
	csrf       string
	credential string
	expires    time.Time
}

type adminAuth struct {
	mu            sync.Mutex
	sessions      map[[32]byte]adminSession
	loginWindow   time.Time
	loginAttempts int
}

func adminRandomToken() string { return rand.Text() }

func (a *adminAuth) newSession(credential string, now time.Time) (string, adminSession) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sessions == nil {
		a.sessions = make(map[[32]byte]adminSession)
	}
	for id, session := range a.sessions {
		if !now.Before(session.expires) {
			delete(a.sessions, id)
		}
	}
	if len(a.sessions) >= 128 {
		var oldest [32]byte
		oldestAt := now.Add(adminSessionTTL)
		for id, session := range a.sessions {
			if !session.expires.After(oldestAt) {
				oldest, oldestAt = id, session.expires
			}
		}
		delete(a.sessions, oldest)
	}
	token := adminRandomToken()
	session := adminSession{csrf: adminRandomToken(), credential: credential, expires: now.Add(adminSessionTTL)}
	a.sessions[sha256.Sum256([]byte(token))] = session
	return token, session
}

func (a *adminAuth) session(token, credential string, now time.Time) (adminSession, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := sha256.Sum256([]byte(token))
	session, ok := a.sessions[id]
	if !ok || credential == "" || session.credential != credential || !now.Before(session.expires) {
		delete(a.sessions, id)
		return adminSession{}, false
	}
	return session, true
}

func (a *adminAuth) logout(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, sha256.Sum256([]byte(token)))
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
		session, ok := s.admin.session(adminCookieValue(r, false), hash, time.Now())
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
		next(w, r, session)
	}
}

func (s *server) adminLoginPage(w http.ResponseWriter, r *http.Request) {
	hash, ok := s.adminCredential(w, r)
	if !ok {
		return
	}
	if _, ok := s.admin.session(adminCookieValue(r, false), hash, time.Now()); ok {
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
	s.admin.logout(adminCookieValue(r, false))
	token, session := s.admin.newSession(hash, time.Now())
	cookie := adminCookie(r, false)
	cookie.Value = token
	cookie.MaxAge = int(adminSessionTTL.Seconds())
	cookie.Expires = session.expires
	http.SetCookie(w, cookie)
	clearAdminCookie(w, r, true)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *server) adminLogout(w http.ResponseWriter, r *http.Request, _ adminSession) {
	s.admin.logout(adminCookieValue(r, false))
	clearAdminCookie(w, r, false)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (s *server) adminError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("admin request failed", "path", r.URL.Path, "error", err)
	http.Error(w, "Could not complete the admin request. Please try again.", http.StatusInternalServerError)
}
