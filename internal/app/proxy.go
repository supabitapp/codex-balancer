package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxWebSocketMessage  = 64 << 20
	maxUpstreamErrorBody = 64 << 10
	refreshTimeout       = 30 * time.Second
	upstreamWait         = 90 * time.Second
)

func newProxyClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = upstreamWait
	return &http.Client{Transport: transport}
}

type server struct {
	admin            adminAuth
	settingsMu       sync.Mutex
	fastMode         fastModePolicy
	ctx              context.Context
	pool             *Pool
	catalog          *modelCatalog
	prices           *priceCatalog
	stats            *Stats
	logins           accountLoginStore
	upstream         string
	authIssuer       string
	lookupAPIKey     func(string) (string, bool, error)
	client           *http.Client
	log              *slog.Logger
	admission        *admissionGate
	resources        *resourceMonitor
	countries        countryResolver
	dashboardStreams atomic.Int64
	dashboardUpdates dashboardBroadcaster
	routeOwnership   contextMutex
	poolResetMu      sync.Mutex
	refreshes        refreshOperations
	routeClaims      routeClaimRegistry
	activeWebSockets activeWebSocketRegistry
}

var webSocketExcludedHeaders = map[string]bool{
	"accept-encoding":          true,
	"authorization":            true,
	"chatgpt-account-id":       true,
	"connection":               true,
	"content-length":           true,
	"cookie":                   true,
	"host":                     true,
	"keep-alive":               true,
	"proxy-authenticate":       true,
	"proxy-authorization":      true,
	"sec-websocket-accept":     true,
	"sec-websocket-extensions": true,
	"sec-websocket-key":        true,
	"sec-websocket-protocol":   true,
	"sec-websocket-version":    true,
	"te":                       true,
	"trailer":                  true,
	"transfer-encoding":        true,
	"upgrade":                  true,
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	admin := s.adminRoutes()
	mux.Handle("/admin", admin)
	mux.Handle("/admin/", admin)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusPermanentRedirect)
	})
	mux.HandleFunc("GET /accounts", s.accountsPage)
	mux.HandleFunc("GET /accounts/status", s.accountLoginStatus)
	mux.HandleFunc(
		"GET /dashboard/assets/admin.css",
		webAsset("web/admin.css", "text/css; charset=utf-8", "public, max-age=31536000, immutable"),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/accounts.css",
		webAsset(
			"web/accounts.css",
			"text/css; charset=utf-8",
			"public, max-age=31536000, immutable",
		),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/accounts.js",
		webAsset(
			"web/accounts.js",
			"text/javascript; charset=utf-8",
			"public, max-age=31536000, immutable",
		),
	)
	mux.HandleFunc("GET /dashboard", s.dashboardPage)
	mux.HandleFunc(
		"GET /favicon.svg",
		webAsset("web/favicon.svg", "image/svg+xml", "public, max-age=3600"),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/dashboard.js",
		webAsset(
			"web/dashboard.js",
			"text/javascript; charset=utf-8",
			"public, max-age=31536000, immutable",
		),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/htmx-2.0.10.min.js",
		webAsset(
			"web/htmx-2.0.10.min.js",
			"text/javascript; charset=utf-8",
			"public, max-age=31536000, immutable",
		),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/idiomorph-0.7.4.min.js",
		webAsset(
			"web/idiomorph-0.7.4.min.js",
			"text/javascript; charset=utf-8",
			"public, max-age=31536000, immutable",
		),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/sse-2.2.4.min.js",
		webAsset(
			"web/sse-2.2.4.min.js",
			"text/javascript; charset=utf-8",
			"public, max-age=31536000, immutable",
		),
	)
	mux.HandleFunc("GET /dashboard/events", s.dashboardEvents)
	mux.HandleFunc("GET /stats", s.statsJSON)
	// pi's Codex provider appends /codex/responses to its configured base URL.
	responses := s.admitted(s.responsesWebSocket)
	for _, path := range []string{"/v1/responses", "/codex/responses", "/v1/codex/responses"} {
		mux.Handle("GET "+path, responses)
	}
	mux.Handle("POST /v1/responses", s.admitted(s.responsesHTTP))
	mux.HandleFunc("GET /v1/models", s.models)
	return mux
}

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	if _, authorized := s.authorizeAPIKey(r); !authorized {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	clientVersion := strings.TrimSpace(r.URL.Query().Get("client_version"))
	if clientVersion != "" {
		ctx := r.Context()
		if s.ctx != nil {
			ctx = s.ctx
		}
		if err := s.refreshModels(ctx, clientVersion); err != nil && s.log != nil {
			s.log.Warn("model refresh failed", "error", err)
		}
	}
	models := []modelEntry{}
	if s.catalog != nil {
		models = s.catalog.entries()
	}
	w.Header().Set("Content-Type", "application/json")
	if clientVersion != "" {
		json.NewEncoder(w).Encode(map[string]any{"models": models})
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		data = append(data, map[string]any{
			"id":       modelSlug(model),
			"object":   "model",
			"owned_by": "openai",
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

type responseReasoning struct {
	Effort string `json:"effort"`
}

type responseErrorPayload struct {
	Type    string          `json:"type"`
	Code    string          `json:"code"`
	Message string          `json:"message,omitempty"`
	Param   json.RawMessage `json:"param,omitempty"`
}

func responseError(resp *http.Response) responseErrorPayload {
	original := resp.Body
	prefix, _ := io.ReadAll(io.LimitReader(original, maxUpstreamErrorBody))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{
		Reader: io.MultiReader(bytes.NewReader(prefix), original),
		Closer: original,
	}
	var envelope struct {
		Error responseErrorPayload `json:"error"`
	}
	if json.Unmarshal(prefix, &envelope) != nil {
		return responseErrorPayload{}
	}
	return envelope.Error
}

func responseUsageLimitReached(resp *http.Response) bool {
	err := responseError(resp)
	return err.Type == "usage_limit_reached" || err.Code == "usage_limit_reached"
}

func workspaceUsageLimitReached(headers http.Header) bool {
	return strings.HasPrefix(
		strings.ToLower(headers.Get("x-codex-rate-limit-reached-type")),
		"workspace_",
	)
}

func (s *server) refreshed(account *Account, id string) bool {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return s.refreshedContext(ctx, account, id)
}

func (s *server) refreshedContext(ctx context.Context, account *Account, id string) bool {
	s.log.Debug("refreshing account", "account", id)
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	lifetime := s.ctx
	if lifetime == nil {
		lifetime = context.Background()
	}
	owner := refreshOwner{
		lifetime: lifetime, client: s.client, operations: &s.refreshes,
		persist: s.pool.persistAccountStateContext,
		completed: func(completion context.Context, err error) error {
			// Completion, not a surviving waiter or a later pool reload, owns
			// this notification. No account/pool lock is held here.
			var notifyErr error
			if account.needsReauth() {
				notifyErr = s.invalidateAccountContext(completion, id, routingReasonOwnerSignedOut)
			}
			if resultErr := errors.Join(err, notifyErr); resultErr != nil {
				s.log.Warn("refresh failed", "account", id, "error", resultErr)
			} else {
				s.log.Debug("account refreshed", "account", id)
			}
			return notifyErr
		},
	}
	if err := account.refreshOwned(ctx, owner); err != nil {
		s.log.Debug("refresh wait ended", "account", id, "error", err)
		return false
	}
	return true
}

func (s *server) invalidateAccount(account string, reason routingReason) {
	_ = s.invalidateAccountContext(context.Background(), account, reason)
}

func (s *server) invalidateAccountContext(ctx context.Context, account string, reason routingReason) error {
	if err := s.routeOwnership.LockContext(ctx); err != nil {
		return err
	}
	invalidatedAt := time.Now()
	claims := s.routeClaims.invalidateAccount(account)
	callbacks := s.activeWebSockets.detachAccount(account)
	var persistErr error
	if len(claims.keys) > 0 && s.pool != nil && s.pool.store != nil {
		persistErr = s.pool.store.preserveRouteOwnersContext(ctx, invalidatedAt, account, claims.keys)
		if persistErr != nil {
			s.log.Warn(
				"provisional route owner preservation failed",
				"account",
				account,
				"routing_reason",
				reason,
				"routes",
				claims.keys,
				"error",
				persistErr,
			)
		}
	}
	s.routeOwnership.Unlock()
	for _, closeSocket := range callbacks {
		closeSocket(account, string(reason))
	}
	if claims.claims == 0 && len(callbacks) == 0 {
		return persistErr
	}
	s.log.Info("account websocket routing invalidated",
		"account", account,
		"routing_reason", reason,
		"provisional_claims", claims.claims,
		"closed_websockets", len(callbacks),
	)
	return persistErr
}

func copyWebSocketHeaders(dst, src http.Header) {
	connection := map[string]bool{}
	for _, value := range src.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			connection[strings.ToLower(strings.TrimSpace(token))] = true
		}
	}
	for name, values := range src {
		lower := strings.ToLower(name)
		if webSocketExcludedHeaders[lower] || connection[lower] {
			continue
		}
		dst[name] = values
	}
}

type apiKeyIdentity struct {
	name   string
	suffix string
}

func (s *server) authorizeAPIKey(r *http.Request) (apiKeyIdentity, bool) {
	if s.lookupAPIKey == nil {
		return apiKeyIdentity{}, true
	}
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	name, valid, err := s.lookupAPIKey(presented)
	if err != nil {
		if s.log != nil {
			s.log.Error("API key lookup failed", "error", err)
		}
		return apiKeyIdentity{}, false
	}
	if !valid {
		return apiKeyIdentity{}, false
	}
	return apiKeyIdentity{name: name, suffix: tokenSuffix(presented)}, true
}

func tokenSuffix(token string) string {
	if len(token) <= 3 {
		return token
	}
	return token[len(token)-3:]
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": "balancer_error"},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
