package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const maxToolRequestBody = 256 << 20

var toolEndpoints = []string{"/v1/alpha/search", "/v1/images/generations", "/v1/images/edits"}

func (s *server) proxyTool(w http.ResponseWriter, r *http.Request) {
	if _, authorized := s.authorizeAPIKey(r); !authorized {
		w.Header().Set("Connection", "close")
		writeHTTPResponseError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxToolRequestBody))
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		w.Header().Set("Connection", "close")
		writeHTTPResponseError(w, status, "could not read request body within size limits")
		return
	}
	var request struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &request)
	headers := httpResponseRequestHeaders(r.Header)
	for _, name := range []string{"Content-Type", "Accept"} {
		if value := r.Header.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), upstreamWait)
	defer cancel()
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	allowed := s.allowedAccounts(request.Model, "")
	skip := map[string]bool{}
	reauthed := map[string]bool{}
	var rejection *http.Response
	for range 2 * len(s.pool.all()) {
		account := s.pool.route(nil, skip).account
		if account == nil {
			break
		}
		id := account.id()
		if !accountAllowed(allowed, id) {
			skip[id] = true
			continue
		}
		resp, err := s.forwardTool(ctx, account, path, body, headers)
		if err != nil {
			s.log.Warn("tool upstream unreachable", "path", path, "account", id, "error", err)
			skip[id] = true
			continue
		}
		account.observe(resp.Header)
		usageLimit := (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden) && responseUsageLimitReached(resp)
		switch {
		case resp.StatusCode == http.StatusUnauthorized && !reauthed[id]:
			resp.Body.Close()
			reauthed[id] = true
			if !s.refreshed(account, id) {
				skip[id] = true
			}
			continue
		case usageLimit || resp.StatusCode == http.StatusTooManyRequests:
			if usageLimit {
				account.rejectCredits()
				account.markSpent()
			} else {
				account.rateLimited(resp.Header, 0)
			}
			if s.stats != nil {
				s.stats.rateLimited(id)
			}
			if rejection != nil {
				rejection.Body.Close()
			}
			rejection = resp
			skip[id] = true
			continue
		}
		if rejection != nil {
			rejection.Body.Close()
		}
		s.log.Debug("tool request proxied", "path", path, "account", id, "status", resp.StatusCode)
		s.writeToolResponse(w, resp)
		return
	}
	if rejection != nil {
		s.writeToolResponse(w, rejection)
		return
	}
	w.Header().Set("Retry-After", "1")
	writeHTTPResponseError(w, http.StatusServiceUnavailable, "no eligible account for tool request; retry")
}

func (s *server) forwardTool(ctx context.Context, account *Account, path string, body []byte, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.upstream+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	account.mu.Lock()
	token := account.AccessToken
	accountID := claimsFromToken(account.IDToken).Auth.AccountID
	account.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("chatgpt-account-id", accountID)
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.client.Do(req)
}

func (s *server) writeToolResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for _, name := range []string{"Content-Type", "X-Request-Id", "Retry-After"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBody))
		w.WriteHeader(resp.StatusCode)
		w.Write(s.responsesRedactor().redact(data))
		return
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, maxHTTPOutput))
}
