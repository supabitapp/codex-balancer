package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testAdminPassword = "local-test-password-only"

func enableTestAdmin(t *testing.T, s *server) string {
	t.Helper()
	hash, err := hashAdminPassword([]byte(testAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pool.store.raw.SetAdminPasswordHash(hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

func adminRequest(h http.Handler, method, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "https://balancer.test"+path, strings.NewReader(form.Encode()))
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	return response
}

func adminFormCSRF(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(response.Body.String())
	if len(match) != 2 {
		t.Fatalf("missing CSRF field; status=%d", response.Code)
	}
	return match[1]
}

func loginTestAdmin(t *testing.T, h http.Handler) (*http.Cookie, string) {
	t.Helper()
	login := adminRequest(h, "GET", "/admin/login", nil)
	response := adminRequest(h, "POST", "/admin/login", url.Values{"csrf": {adminFormCSRF(t, login)}, "password": {testAdminPassword}}, login.Result().Cookies()...)
	if response.Code != 303 {
		t.Fatalf("login status=%d: %s", response.Code, response.Body.String())
	}
	var cookie *http.Cookie
	for _, candidate := range response.Result().Cookies() {
		if candidate.Name == "__Host-cb-admin" {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("missing session cookie")
	}
	page := adminRequest(h, "GET", "/admin", nil, cookie)
	if page.Code != 200 {
		t.Fatalf("admin status=%d", page.Code)
	}
	return cookie, adminFormCSRF(t, page)
}

func TestAdminPasswordSetResetAndDisable(t *testing.T) {
	srv := newTestServer(t, nil)
	set := func(password, confirm string) error {
		values := []string{password, confirm}
		return changeAdminPassword(srv.pool.store, func() ([]byte, error) { value := values[0]; values = values[1:]; return []byte(value), nil }, io.Discard)
	}
	if err := set("short", "short"); err == nil {
		t.Fatal("accepted short password")
	}
	if err := set(testAdminPassword, "different"); err == nil {
		t.Fatal("accepted mismatched passwords")
	}
	if err := set(testAdminPassword, testAdminPassword); err != nil {
		t.Fatal(err)
	}
	first, _ := srv.pool.store.raw.AdminPasswordHash()
	if strings.Contains(first, testAdminPassword) || !verifyAdminPassword(first, []byte(testAdminPassword)) {
		t.Fatal("password not stored as a working hash")
	}
	cookie, _ := loginTestAdmin(t, srv.routes())
	// Resetting even to the same password generates a new salt and ends old sessions.
	if err := set(testAdminPassword, testAdminPassword); err != nil {
		t.Fatal(err)
	}
	second, _ := srv.pool.store.raw.AdminPasswordHash()
	if first == second {
		t.Fatal("password reset reused salt")
	}
	if got := adminRequest(srv.routes(), "GET", "/admin", nil, cookie).Code; got != 303 {
		t.Fatalf("old session survived reset: %d", got)
	}
	if err := adminCmd([]string{"disable", "-state", srv.pool.store.path}); err != nil {
		t.Fatal(err)
	}
	if got := adminRequest(srv.routes(), "GET", "/admin", nil, cookie).Code; got != 503 {
		t.Fatalf("disabled status=%d", got)
	}
	for _, args := range [][]string{{"password", "plaintext-password"}, {"unknown"}, {"disable", "extra"}} {
		if err := adminCmd(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, bad := range []string{"", "pbkdf2-sha256$999999999$YWJj$YWJj", second + "$extra"} {
		if verifyAdminPassword(bad, []byte(testAdminPassword)) {
			t.Fatal("invalid hash accepted")
		}
	}
}

func TestAdminAuthAndCookies(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.routes()
	if got := adminRequest(h, "GET", "/admin", nil).Code; got != 503 {
		t.Fatalf("unconfigured admin status=%d", got)
	}
	enableTestAdmin(t, srv)
	for _, path := range []string{"/admin", "/admin/status"} {
		if got := adminRequest(h, "GET", path, nil).Code; got != 303 {
			t.Fatalf("unprotected %s: %d", path, got)
		}
	}
	for _, path := range []string{"/admin/settings", "/admin/accounts/reset", "/admin/accounts/refresh", "/admin/accounts/pause", "/admin/accounts/remove", "/admin/accounts/mode", "/admin/keys/add", "/admin/keys/revoke", "/admin/logout"} {
		if got := adminRequest(h, "POST", path, nil).Code; got != 303 {
			t.Fatalf("unprotected %s: %d", path, got)
		}
	}
	page := adminRequest(h, "GET", "/admin/login", nil)
	bad := adminRequest(h, "POST", "/admin/login", url.Values{"csrf": {adminFormCSRF(t, page)}, "password": {"wrong"}}, page.Result().Cookies()...)
	if bad.Code != 401 || !strings.Contains(bad.Body.String(), "Incorrect password") {
		t.Fatalf("bad login status=%d", bad.Code)
	}
	cookie, csrf := loginTestAdmin(t, h)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("unsafe cookie: %+v", cookie)
	}
	page = adminRequest(h, "GET", "/admin", nil, cookie)
	if page.Header().Get("Cache-Control") != "no-store" || page.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing security headers")
	}
	loggedOut := adminRequest(h, "POST", "/admin/logout", url.Values{"csrf": {csrf}}, cookie)
	if loggedOut.Code != 303 {
		t.Fatalf("logout=%d", loggedOut.Code)
	}
	if got := adminRequest(h, "GET", "/admin", nil, cookie).Code; got != 303 {
		t.Fatal("session survived logout")
	}
	// The HTTP exception is limited to direct loopback development.
	request := httptest.NewRequest("GET", "http://127.0.0.1/admin", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	if adminCookie(request, false).Secure {
		t.Fatal("loopback HTTP cookie is secure")
	}
	request.RemoteAddr = "192.0.2.1:1234"
	if !adminCookie(request, false).Secure {
		t.Fatal("remote peer received insecure cookie")
	}
}

func TestAdminRejectsCSRFAndOversizedForms(t *testing.T) {
	srv := newTestServer(t, nil)
	enableTestAdmin(t, srv)
	h := srv.routes()
	cookie, csrf := loginTestAdmin(t, h)
	for _, token := range []string{"", "wrong"} {
		if got := adminRequest(h, "POST", "/admin/settings", url.Values{"csrf": {token}, "fast-mode": {"on"}}, cookie).Code; got != 403 {
			t.Fatalf("CSRF status=%d", got)
		}
	}
	request := httptest.NewRequest("POST", "https://balancer.test/admin/settings", strings.NewReader(url.Values{"csrf": {csrf}, "fast-mode": {"on"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://evil.test")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal("cross-origin action accepted")
	}
	response = adminRequest(h, "POST", "/admin/settings", url.Values{"csrf": {csrf}, "fast-mode": {strings.Repeat("x", 9000)}}, cookie)
	if response.Code != 400 {
		t.Fatalf("oversized form status=%d", response.Code)
	}
	login := adminRequest(h, "GET", "/admin/login", nil)
	response = adminRequest(h, "POST", "/admin/login", url.Values{"csrf": {"forged"}, "password": {testAdminPassword}}, login.Result().Cookies()...)
	if response.Code != 403 {
		t.Fatal("login CSRF accepted")
	}
	mode, _ := srv.pool.store.raw.FastMode()
	if mode != "default" {
		t.Fatal("rejected request changed mode")
	}
}

func TestAdminSessionExpiryAndLoginThrottle(t *testing.T) {
	srv := newTestServer(t, nil)
	hash := enableTestAdmin(t, srv)
	h := srv.routes()
	now := time.Now()
	token, session := srv.admin.newSession(hash, now)
	if _, ok := srv.admin.session(token, hash, session.expires); ok {
		t.Fatal("expired session accepted")
	}
	for i := 0; i < 10; i++ {
		if !srv.admin.allowLogin(now) {
			t.Fatal("early throttle")
		}
	}
	page := adminRequest(h, "GET", "/admin/login", nil)
	response := adminRequest(h, "POST", "/admin/login", url.Values{"csrf": {adminFormCSRF(t, page)}, "password": {testAdminPassword}}, page.Result().Cookies()...)
	if response.Code != 429 || response.Header().Get("Retry-After") == "" {
		t.Fatal("login not throttled")
	}
	if !srv.admin.allowLogin(now.Add(time.Minute)) {
		t.Fatal("throttle never resets")
	}
	expired := loginCSRFToken(hash, now.Add(-11*time.Minute))
	if validLoginCSRF(expired, hash, now) {
		t.Fatal("expired login token accepted")
	}
}

func TestAdminControlsAndSecretVisibility(t *testing.T) {
	account := testAccount("account-a", 0)
	srv := newTestServer(t, []*Account{account})
	enableTestAdmin(t, srv)
	h := srv.routes()
	cookie, csrf := loginTestAdmin(t, h)
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		form.Set("csrf", csrf)
		request := httptest.NewRequest("POST", "https://balancer.test"+path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("HX-Request", "true")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		return response
	}
	for _, mode := range []string{"on", "off", "default"} {
		response := post("/admin/settings", url.Values{"fast-mode": {mode}})
		if response.Code != 200 || !strings.Contains(response.Body.String(), `id="settings-panel"`) || strings.Contains(response.Body.String(), "<!doctype") {
			t.Fatalf("bad settings fragment: %d", response.Code)
		}
		stored, _ := srv.pool.store.raw.FastMode()
		applied, _ := srv.fastMode.snapshot()
		if stored != mode || string(applied) != mode {
			t.Fatal("setting not saved and applied")
		}
	}
	if response := post("/admin/settings", url.Values{"fast-mode": {"invalid"}}); response.Code != 422 {
		t.Fatal("invalid mode accepted")
	}
	for _, paused := range []string{"true", "true", "false"} {
		response := post("/admin/accounts/pause", url.Values{"account": {account.id()}, "paused": {paused}})
		if response.Code != 200 || account.routingCandidate().paused != (paused == "true") {
			t.Fatal("pause not idempotent")
		}
	}
	for _, mode := range []routingMode{routingModePriority, routingModeDraining, routingModeNormal} {
		response := post("/admin/accounts/mode", url.Values{"account": {account.id()}, "mode": {string(mode)}})
		if response.Code != 200 || account.routingCandidate().mode != mode {
			t.Fatalf("mode %s not applied", mode)
		}
		if !strings.Contains(response.Body.String(), `value="`+string(mode)+`" selected`) {
			t.Fatalf("mode %s is not selected in the admin control", mode)
		}
		reloaded, err := loadPool(srv.pool.store)
		if err != nil || reloaded.find(account.id()).routingCandidate().mode != mode {
			t.Fatalf("mode %s not persisted: %v", mode, err)
		}
		if applied, _ := srv.fastMode.snapshot(); applied != fastModeDefault {
			t.Fatal("account mode changed the global fast policy")
		}
	}
	response := post("/admin/accounts/mode", url.Values{"account": {account.id()}, "mode": {"invalid"}})
	if response.Code != 422 || account.routingCandidate().mode != routingModeNormal {
		t.Fatal("invalid account mode accepted or changed routing")
	}
	name := `laptop<script>alert(1)</script>`
	response = post("/admin/keys/add", url.Values{"name": {name}})
	if response.Code != 200 || strings.Contains(response.Body.String(), name) {
		t.Fatal("key creation failed or name not escaped")
	}
	keys, _ := srv.pool.store.readAPIKeys()
	if len(keys) != 1 || !strings.Contains(response.Body.String(), keys[0].Secret) {
		t.Fatal("new key not shown")
	}
	page := adminRequest(h, "GET", "/admin", nil, cookie)
	if strings.Contains(page.Body.String(), keys[0].Secret) || strings.Contains(page.Body.String(), account.AccessToken) {
		t.Fatal("credentials exposed")
	}
	response = post("/admin/keys/add", url.Values{"name": {name}})
	if response.Code != 409 {
		t.Fatal("duplicate key accepted")
	}
	response = post("/admin/keys/revoke", url.Values{"name": {name}})
	valid, err := srv.pool.store.validAPIKey(keys[0].Secret)
	if response.Code != 200 || err != nil || valid {
		t.Fatal("revocation failed")
	}
	response = post("/admin/accounts/remove", url.Values{"account": {account.id()}})
	if response.Code != 200 || srv.pool.find(account.id()) != nil {
		t.Fatal("removal failed")
	}
}

func TestAdminPauseDisconnectsWebSockets(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("owner", 0)})
	enableTestAdmin(t, srv)
	cookie, csrf := loginTestAdmin(t, srv.routes())
	conn, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "thread"))
	defer conn.CloseNow()
	completeWebSocketTurn(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	response := adminRequest(srv.routes(), "POST", "/admin/accounts/pause", url.Values{"csrf": {csrf}, "account": {"owner"}, "paused": {"true"}}, cookie)
	if response.Code != 200 {
		t.Fatalf("pause=%d", response.Code)
	}
	readCloseStatus(t, conn, websocket.StatusServiceRestart)
}

func TestAdminMigrationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.raw.SetFastMode("on"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("ALTER TABLE settings DROP COLUMN admin_password_hash; PRAGMA user_version = 4"); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := store.raw.AdminPasswordHash()
	if err != nil || hash != "" {
		t.Fatal("admin enabled during migration")
	}
	mode, _ := store.raw.FastMode()
	if mode != "on" {
		t.Fatal("migration lost fast mode")
	}
	encoded, err := hashAdminPassword([]byte(testAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.raw.SetAdminPasswordHash(encoded); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hash, err = store.raw.AdminPasswordHash()
	if err != nil || hash != encoded {
		t.Fatal("password not persisted")
	}
}

func TestAdminSessionDoesNotTrustAPIKeys(t *testing.T) {
	srv := newTestServer(t, nil)
	hash := enableTestAdmin(t, srv)
	request := httptest.NewRequest("GET", "https://balancer.test/admin", nil)
	request.Header.Set("Authorization", "Bearer inference-key")
	response := httptest.NewRecorder()
	srv.routes().ServeHTTP(response, request)
	if response.Code != 303 {
		t.Fatal("inference key granted admin access")
	}
	token, session := srv.admin.newSession(hash, time.Now())
	srv.admin.sessions[sha256.Sum256([]byte(token))] = adminSession{csrf: session.csrf, credential: hash, expires: time.Now().Add(-time.Second)}
	request = httptest.NewRequest("GET", "https://balancer.test/admin/status", nil)
	request.Header.Set("HX-Request", "true")
	request.AddCookie(&http.Cookie{Name: "__Host-cb-admin", Value: token})
	response = httptest.NewRecorder()
	srv.routes().ServeHTTP(response, request)
	if response.Code != 401 || response.Header().Get("HX-Redirect") != "/admin/login" {
		t.Fatal("expired HTMX session not redirected")
	}
}

func TestAdminPasswordPromptDoesNotPrintPassword(t *testing.T) {
	srv := newTestServer(t, nil)
	var out bytes.Buffer
	err := changeAdminPassword(srv.pool.store, func() ([]byte, error) { return []byte(testAdminPassword), nil }, &out)
	if err != nil || strings.Contains(out.String(), testAdminPassword) {
		t.Fatal("password prompt exposed password")
	}
}

func TestAdminBankedReset(t *testing.T) {
	for _, tc := range []struct {
		name, code                 string
		upstreamStatus, wantStatus int
		available                  bool
	}{
		{"success", "reset", 200, 200, true},
		{"nothing to reset", "nothing_to_reset", 200, 200, true},
		{"already redeemed", "already_redeemed", 200, 409, true},
		{"unavailable", "", 200, 409, false},
		{"upstream failure", "", 500, 502, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := testAccount("account-a", 100)
			expires := time.Now().Add(30 * 24 * time.Hour)
			credit := resetCredit{ID: "credit-a", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: &expires}
			account.adoptResetCredits(time.Now(), 1, []resetCredit{credit})
			consumed, refreshed := 0, false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /rate-limit-reset-credits":
					payload := resetCreditsPayload{}
					if tc.available && consumed == 0 {
						payload.AvailableCount = 1
						payload.Credits = []resetCredit{credit}
					}
					json.NewEncoder(w).Encode(payload)
				case "POST /rate-limit-reset-credits/consume":
					consumed++
					var body consumeResetCreditRequest
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.CreditID != credit.ID || body.RedeemRequestID != credit.ID {
						t.Errorf("unexpected redemption: %+v", body)
					}
					w.WriteHeader(tc.upstreamStatus)
					json.NewEncoder(w).Encode(consumeResetCreditResponse{Code: tc.code, WindowsReset: 2})
				case "GET /usage":
					refreshed = true
					w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":0},"secondary_window":{"used_percent":0}}}`))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer upstream.Close()
			old := accountAPIBaseURL
			accountAPIBaseURL = upstream.URL
			defer func() { accountAPIBaseURL = old }()
			srv := newTestServer(t, []*Account{account})
			enableTestAdmin(t, srv)
			h := srv.routes()
			cookie, csrf := loginTestAdmin(t, h)
			page := adminRequest(h, "GET", "/admin", nil, cookie)
			if !strings.Contains(page.Body.String(), "Use banked reset for") {
				t.Fatal("missing reset control")
			}
			form := url.Values{"account": {account.id()}, "credit": {credit.ID}}
			if got := adminRequest(h, "POST", "/admin/accounts/reset", form, cookie).Code; got != 403 || consumed != 0 {
				t.Fatal("reset accepted without CSRF")
			}
			form.Set("csrf", csrf)
			response := adminRequest(h, "POST", "/admin/accounts/reset", form, cookie)
			if response.Code != tc.wantStatus {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			wantConsumed := 0
			if tc.available {
				wantConsumed = 1
			}
			if consumed != wantConsumed {
				t.Fatalf("consumed %d credits", consumed)
			}
			if tc.upstreamStatus == 200 && !refreshed {
				t.Fatal("usage not refreshed")
			}
			if tc.code == "reset" {
				response = adminRequest(h, "POST", "/admin/accounts/reset", form, cookie)
				if response.Code != 409 || consumed != 1 {
					t.Fatal("stale submission consumed another credit")
				}
			}
		})
	}
}

func TestAdminAccountRefresh(t *testing.T) {
	for _, failPath := range []string{"", "/usage", "/rate-limit-reset-credits"} {
		t.Run("failure="+failPath, func(t *testing.T) {
			account := testAccount("account-a", 100)
			usageCalls, creditCalls := 0, 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /usage":
					usageCalls++
				case "GET /rate-limit-reset-credits":
					creditCalls++
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
					return
				}
				if r.URL.Path == failPath {
					w.WriteHeader(500)
					return
				}
				if r.URL.Path == "/usage" {
					w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":20},"secondary_window":{"used_percent":30}}}`))
				} else {
					w.Write([]byte(`{"available_count":2}`))
				}
			}))
			defer upstream.Close()
			old := accountAPIBaseURL
			accountAPIBaseURL = upstream.URL
			defer func() { accountAPIBaseURL = old }()
			srv := newTestServer(t, []*Account{account})
			enableTestAdmin(t, srv)
			h := srv.routes()
			cookie, csrf := loginTestAdmin(t, h)
			page := adminRequest(h, "GET", "/admin", nil, cookie)
			if !strings.Contains(page.Body.String(), "Refresh quota and banked credits for") {
				t.Fatal("missing refresh control")
			}
			form := url.Values{"account": {account.id()}}
			if got := adminRequest(h, "POST", "/admin/accounts/refresh", form, cookie).Code; got != 403 || usageCalls != 0 || creditCalls != 0 {
				t.Fatal("refresh accepted without CSRF")
			}
			form.Set("csrf", csrf)
			request := httptest.NewRequest("POST", "https://balancer.test/admin/accounts/refresh", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("HX-Request", "true")
			request.AddCookie(cookie)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			wantStatus, wantNotice := 200, "Quota and banked credits refreshed"
			if failPath != "" {
				wantStatus, wantNotice = 502, "Could not refresh all account data"
			}
			if response.Code != wantStatus || !strings.Contains(response.Body.String(), wantNotice) || strings.Contains(response.Body.String(), "<!doctype") {
				t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
			}
			if usageCalls != 1 || creditCalls != 1 {
				t.Fatalf("usage calls=%d, credit calls=%d", usageCalls, creditCalls)
			}
			if failPath != "/usage" && (account.primary.usedPercent != 20 || account.secondary.usedPercent != 30) {
				t.Fatal("quota not updated")
			}
			if failPath != "/rate-limit-reset-credits" && account.resetCredits.count != 2 {
				t.Fatal("banked credits not updated")
			}
		})
	}
}
