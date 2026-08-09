package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	maxRequestBody = 64 << 20
	maxAttempts    = 3
	refreshTimeout = 30 * time.Second
	upstreamWait   = 90 * time.Second
)

func newProxyClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = upstreamWait
	return &http.Client{Transport: transport}
}

type server struct {
	ctx        context.Context
	pool       *Pool
	sticky     *Sticky
	stats      *Stats
	logins     accountLoginStore
	upstream   string
	authIssuer string
	key        string
	client     *http.Client
	log        *slog.Logger
}

var hopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"content-length":      true,
	"host":                true,
	"authorization":       true,
	"chatgpt-account-id":  true,
}

var websocketHopByHop = map[string]bool{
	"accept-encoding":          true,
	"cookie":                   true,
	"sec-websocket-accept":     true,
	"sec-websocket-extensions": true,
	"sec-websocket-key":        true,
	"sec-websocket-protocol":   true,
	"sec-websocket-version":    true,
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /accounts", s.accountsPage)
	mux.HandleFunc("GET /dashboard", s.dashboardPage)
	mux.HandleFunc("GET /dashboard/ws", s.dashboardWebSocket)
	mux.HandleFunc("GET /stats", s.statsJSON)
	mux.HandleFunc("POST /v1/responses", s.responses)
	mux.HandleFunc("GET /v1/responses", s.responsesWebSocket)
	mux.HandleFunc("GET /v1/models", s.models)
	return mux
}

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"models":[]}` + "\n"))
}

func (s *server) responses(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	serviceTier := requestServiceTier(body)

	key := stickyKey(r.Header)
	pinned := s.sticky.get(key)
	skip := map[string]bool{}
	reauthed := map[string]bool{}
	s.log.Debug("http turn received", "thread", key, "pinned_account", pinned, "service_tier", serviceTier)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		account := s.pickAccount(key, pinned, skip, attempt, transportHTTP)
		if account == nil {
			writeError(w, http.StatusServiceUnavailable, "no account available")
			return
		}
		id := account.id()

		if account.stale(time.Now()) && !reauthed[id] {
			reauthed[id] = true
			if !s.refreshed(account, id) {
				skip[id] = true
				continue
			}
		}

		sent := time.Now()
		resp, err := s.forward(r, body, account)
		if err != nil {
			s.log.Warn("upstream unreachable", "transport", transportHTTP, "thread", key, "account", id, "attempt", attempt+1, "error", err)
			s.stats.failedOver(id, "unreachable")
			account.failed(attempt)
			skip[id] = true
			continue
		}
		s.log.Debug("upstream responded",
			"transport", transportHTTP,
			"thread", key,
			"account", id,
			"attempt", attempt+1,
			"status", resp.StatusCode,
			"header_latency", time.Since(sent),
		)

		if resp.StatusCode == http.StatusUnauthorized && !reauthed[id] {
			resp.Body.Close()
			reauthed[id] = true
			if !s.refreshed(account, id) {
				skip[id] = true
			}
			attempt--
			continue
		}

		if status := resp.StatusCode; status == http.StatusTooManyRequests ||
			status == http.StatusUnauthorized || status >= 500 {
			if status == http.StatusTooManyRequests {
				account.observe(resp.Header)
				account.rateLimited(resp.Header, attempt)
				s.stats.rateLimited(id)
				attrs := []any{"transport", transportHTTP, "thread", key, "attempt", attempt + 1}
				attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
				s.log.Info("account rate limited", attrs...)
			} else {
				s.log.Warn("upstream refused the turn", "transport", transportHTTP, "thread", key, "account", id, "attempt", attempt+1, "status", status)
				account.failed(attempt)
			}
			if pinned != "" {
				s.log.Info("thread stays on its account", "transport", transportHTTP, "thread", key, "account", id, "status", status)
				copyHeaders(w.Header(), resp.Header)
				w.WriteHeader(status)
				io.Copy(w, resp.Body)
				resp.Body.Close()
				return
			}
			resp.Body.Close()
			s.stats.failedOver(id, resp.Status)
			skip[id] = true
			continue
		}

		account.observe(resp.Header)
		if err := s.sticky.bind(key, id); err != nil {
			s.log.Warn("thread binding save failed", "thread", key, "account", id, "error", err)
		}
		s.stats.routed(key, id, serviceTier, transportHTTP)
		attrs := []any{"transport", transportHTTP, "thread", key, "attempt", attempt + 1, "status", resp.StatusCode}
		attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
		s.log.Debug("http turn routed", attrs...)
		s.relay(w, resp, sent)
		return
	}
	s.log.Warn("every account failed", "transport", transportHTTP, "thread", key, "attempts", maxAttempts)
	writeError(w, http.StatusServiceUnavailable, "every account failed this turn")
}

func requestServiceTier(body []byte) string {
	var request struct {
		ServiceTier string `json:"service_tier"`
	}
	json.Unmarshal(body, &request)
	return request.ServiceTier
}

func (s *server) refreshed(account *Account, id string) bool {
	s.log.Debug("refreshing account", "account", id)
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	if err := account.refresh(ctx, s.client); err != nil {
		s.log.Warn("refresh failed", "account", id, "error", err)
		return false
	}
	if err := s.pool.persistTokens(account); err != nil {
		s.log.Warn("could not persist refreshed tokens", "account", id, "error", err)
	}
	s.log.Debug("account refreshed", "account", id)
	return true
}

func copyHeaders(dst, src http.Header) {
	copyHeadersExcept(dst, src, nil)
}

func copyWebSocketHeaders(dst, src http.Header) {
	copyHeadersExcept(dst, src, websocketHopByHop)
}

func copyHeadersExcept(dst, src http.Header, extra map[string]bool) {
	connection := map[string]bool{}
	for _, value := range src.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			connection[strings.ToLower(strings.TrimSpace(token))] = true
		}
	}
	for name, values := range src {
		lower := strings.ToLower(name)
		if hopByHop[lower] || extra[lower] || connection[lower] {
			continue
		}
		dst[name] = values
	}
}

func (s *server) forward(r *http.Request, body []byte, account *Account) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.upstream+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header)
	account.mu.Lock()
	token := account.AccessToken
	account.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("chatgpt-account-id", account.id())
	return s.client.Do(req)
}

func (s *server) relay(w http.ResponseWriter, resp *http.Response, sent time.Time) {
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	control := http.NewResponseController(w)
	first := true
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			control.Flush()
			if first {
				s.stats.answered(time.Since(sent))
				first = false
			}
		}
		if err != nil {
			if err != io.EOF {
				s.log.Warn("stream cut short", "error", err)
			}
			return
		}
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
