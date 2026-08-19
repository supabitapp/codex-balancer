package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	maxWebSocketMessage  = 64 << 20
	maxUpstreamErrorBody = 64 << 10
	maxUpstreamRetries   = 3
	upstreamRetryBudget  = 5 * time.Second
	refreshTimeout       = 30 * time.Second
	upstreamWait         = 90 * time.Second
)

func newProxyClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = upstreamWait
	return &http.Client{Transport: transport}
}

type server struct {
	ctx                  context.Context
	pool                 *Pool
	catalog              *modelCatalog
	prices               *priceCatalog
	stats                *Stats
	logins               accountLoginStore
	upstream             string
	authIssuer           string
	key                  string
	clientIDKey          []byte
	client               *http.Client
	log                  *slog.Logger
	admission            *admissionGate
	retryBackoff         func(int) time.Duration
	resources            *resourceMonitor
	countries            countryResolver
	websockets           websocketRegistry
	dashboardConnections atomic.Int64
}

func upstreamRetryBackoff(retry int) time.Duration {
	return upstreamRetryBudget * time.Duration(1<<(retry-1)) / time.Duration((1<<maxUpstreamRetries)-1)
}

func upstreamRetryDelay(retry int) time.Duration {
	return time.Duration(float64(upstreamRetryBackoff(retry)) * (0.9 + rand.Float64()*0.2))
}

func (s *server) waitForUpstreamRetry(ctx context.Context, retry int) bool {
	delay := upstreamRetryDelay(retry)
	if s.retryBackoff != nil {
		delay = s.retryBackoff(retry)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusPermanentRedirect)
	})
	mux.HandleFunc("GET /accounts", s.accountsPage)
	mux.HandleFunc("GET /dashboard", s.dashboardPage)
	mux.HandleFunc("GET /favicon.svg", webAsset("web/favicon.svg", "image/svg+xml", "public, max-age=3600"))
	mux.HandleFunc("GET /dashboard/assets/dashboard.js", webAsset("web/dashboard.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable"))
	mux.HandleFunc("GET /dashboard/assets/htmx-2.0.10.min.js", webAsset("web/htmx-2.0.10.min.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable"))
	mux.HandleFunc("GET /dashboard/assets/ws-2.0.4.min.js", webAsset("web/ws-2.0.4.min.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable"))
	mux.HandleFunc("GET /dashboard/ws", s.dashboardWebSocket)
	mux.HandleFunc("GET /stats", s.statsJSON)
	mux.Handle("GET /v1/responses", s.admitted(s.responsesWebSocket))
	mux.HandleFunc("GET /v1/models", s.models)
	return mux
}

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	clientVersion := strings.TrimSpace(r.URL.Query().Get("client_version"))
	if clientVersion != "" {
		if err := s.refreshModels(r.Context(), clientVersion); err != nil && s.log != nil {
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

func (s *server) forceFastTier(account *Account, model, serviceTier string, message []byte) ([]byte, bool) {
	if canonicalServiceTier(serviceTier) == serviceTierPriority {
		return message, false
	}
	if !account.routingCandidate().draining() {
		return message, false
	}
	if s.catalog == nil || !s.catalog.accountSupportsServiceTier(account.id(), model, serviceTierPriority) {
		return message, false
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(message, &payload) != nil || payload == nil {
		return message, false
	}
	payload["service_tier"] = json.RawMessage(strconv.Quote(serviceTierPriority))
	forced, err := json.Marshal(payload)
	if err != nil {
		return message, false
	}
	return forced, true
}

type responseErrorPayload struct {
	Type string `json:"type"`
	Code string `json:"code"`
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
	return strings.HasPrefix(strings.ToLower(headers.Get("x-codex-rate-limit-reached-type")), "workspace_")
}

func (s *server) refreshed(account *Account, id string) bool {
	s.log.Debug("refreshing account", "account", id)
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	if err := account.refresh(ctx, s.client, s.pool.persistTokens); err != nil {
		s.log.Warn("refresh failed", "account", id, "error", err)
		return false
	}
	s.log.Debug("account refreshed", "account", id)
	return true
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

func (s *server) authorized(r *http.Request) bool {
	if s.key == "" {
		return true
	}
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return s.validKey(presented)
}

func (s *server) validKey(presented string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.key)) == 1
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
