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
	pool     *Pool
	sticky   *Sticky
	upstream string
	key      string
	client   *http.Client
	log      *slog.Logger
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

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", s.responses)
	mux.HandleFunc("GET /v1/responses", s.refuseUpgrade)
	mux.HandleFunc("GET /v1/models", s.models)
	return mux
}

func (s *server) refuseUpgrade(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusUpgradeRequired)
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

	key := stickyKey(r.Header)
	skip := map[string]bool{}
	reauthed := map[string]bool{}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		account := s.pool.pick(s.sticky.get(key), skip)
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

		resp, err := s.forward(r, body, account)
		if err != nil {
			s.log.Warn("upstream unreachable", "account", id, "error", err)
			account.failed(attempt)
			skip[id] = true
			continue
		}

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
			resp.Body.Close()
			skip[id] = true
			if status == http.StatusTooManyRequests {
				s.log.Info("rate limited", "account", id)
				account.rateLimited(resp.Header, attempt)
			} else {
				s.log.Warn("upstream refused the turn", "account", id, "status", status)
				account.failed(attempt)
			}
			continue
		}

		account.observe(resp.Header)
		s.sticky.bind(key, id)
		s.relay(w, resp)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "every account failed this turn")
}

func (s *server) refreshed(account *Account, id string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	if err := account.refresh(ctx, s.client); err != nil {
		s.log.Warn("refresh failed", "account", id, "error", err)
		return false
	}
	if err := s.pool.save(); err != nil {
		s.log.Warn("could not persist refreshed tokens", "account", id, "error", err)
	}
	return true
}

func copyHeaders(dst, src http.Header) {
	for name, values := range src {
		if hopByHop[strings.ToLower(name)] {
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

func (s *server) relay(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	control := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			control.Flush()
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
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.key)) == 1
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": message, "type": "balancer_error"},
	})
}
