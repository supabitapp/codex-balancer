package main

import (
	"bytes"
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
)

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

	key := stickyKey(r.Header)
	skip := map[string]bool{}
	reauthed := map[string]bool{}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		account := s.pool.pick(s.sticky.get(key), skip)
		if account == nil {
			writeError(w, http.StatusServiceUnavailable, "no account available")
			return
		}
		id := account.ID()

		if account.stale(time.Now()) && !reauthed[id] {
			reauthed[id] = true
			if err := s.reauth(r, account); err != nil {
				s.log.Warn("refresh failed", "account", id, "error", err)
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

		switch {
		case resp.StatusCode == http.StatusUnauthorized && !reauthed[id]:
			resp.Body.Close()
			reauthed[id] = true
			if err := s.reauth(r, account); err != nil {
				s.log.Warn("refresh failed", "account", id, "error", err)
				skip[id] = true
			}
			attempt--
			continue

		case resp.StatusCode == http.StatusUnauthorized:
			s.log.Warn("upstream rejected credentials", "account", id)
			resp.Body.Close()
			account.failed(attempt)
			skip[id] = true
			continue

		case resp.StatusCode == http.StatusTooManyRequests:
			s.log.Info("rate limited", "account", id)
			account.rateLimited(resp.Header, attempt)
			resp.Body.Close()
			skip[id] = true
			continue

		case resp.StatusCode >= 500:
			s.log.Warn("upstream error", "account", id, "status", resp.StatusCode)
			account.failed(attempt)
			resp.Body.Close()
			skip[id] = true
			continue
		}

		account.observe(resp.Header)
		s.sticky.bind(key, id)
		s.relay(w, resp)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "every account failed this turn")
}

func (s *server) reauth(r *http.Request, account *Account) error {
	if err := account.refresh(r.Context(), s.client); err != nil {
		return err
	}
	return s.pool.save()
}

func (s *server) forward(r *http.Request, body []byte, account *Account) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.upstream+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for name, values := range r.Header {
		if hopByHop[strings.ToLower(name)] {
			continue
		}
		req.Header[name] = values
	}
	account.mu.Lock()
	token := account.AccessToken
	account.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("chatgpt-account-id", account.ID())
	return s.client.Do(req)
}

func (s *server) relay(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for name, values := range resp.Header {
		if hopByHop[strings.ToLower(name)] {
			continue
		}
		w.Header()[name] = values
	}
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

func readCapped(r io.Reader, limit int64) string {
	data, _ := io.ReadAll(io.LimitReader(r, limit))
	return string(data)
}
