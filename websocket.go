package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const responsesWebSocketBeta = "responses_websockets=2026-02-06"

var errWebSocketAccountPaused = errors.New("account paused")

type websocketDial struct {
	conn    *websocket.Conn
	resp    *http.Response
	account *Account
}

func (s *server) responsesWebSocket(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	if !websocketHandshake(w, r) {
		return
	}

	key := stickyKey(r.Header)
	dial, failed, err := s.dialResponsesWebSocket(r, key)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if failed != nil {
		copyWebSocketHeaders(w.Header(), failed.Header)
		w.WriteHeader(failed.StatusCode)
		if failed.Body != nil {
			defer failed.Body.Close()
			io.Copy(w, failed.Body)
		}
		return
	}
	defer dial.conn.CloseNow()

	copyWebSocketHeaders(w.Header(), dial.resp.Header)
	downstream, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log.Warn("websocket upgrade failed", "error", err)
		return
	}
	defer downstream.CloseNow()

	id := dial.account.id()
	s.sticky.bind(key, id)
	s.stats.websocketOpened(id)
	defer s.stats.websocketClosed(id)

	downstream.SetReadLimit(maxRequestBody)
	dial.conn.SetReadLimit(maxRequestBody)
	s.relayResponsesWebSocket(downstream, dial.conn, dial.account, key)
}

func websocketHandshake(w http.ResponseWriter, r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.WriteHeader(http.StatusUpgradeRequired)
		return false
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" || r.Header.Get("Sec-WebSocket-Key") == "" {
		http.Error(w, "invalid websocket handshake", http.StatusBadRequest)
		return false
	}
	return true
}

func headerHasToken(headers http.Header, name, target string) bool {
	for _, value := range headers.Values(name) {
		for token := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), target) {
				return true
			}
		}
	}
	return false
}

func (s *server) dialResponsesWebSocket(r *http.Request, key string) (*websocketDial, *http.Response, error) {
	upstream, err := responsesWebSocketURL(s.upstream)
	if err != nil {
		return nil, nil, err
	}
	pinned := s.sticky.get(key)
	skip := map[string]bool{}
	reauthed := map[string]bool{}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		account := s.pool.pick(pinned, skip)
		if account == nil {
			return nil, nil, errors.New("no account available")
		}
		id := account.id()

		if account.stale(time.Now()) && !reauthed[id] {
			reauthed[id] = true
			if !s.refreshed(account, id) {
				skip[id] = true
				continue
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), upstreamWait)
		conn, resp, err := websocket.Dial(ctx, upstream, &websocket.DialOptions{
			HTTPClient: s.client,
			HTTPHeader: responsesWebSocketHeaders(r.Header, account),
		})
		cancel()
		if err == nil {
			account.observe(resp.Header)
			return &websocketDial{conn: conn, resp: resp, account: account}, nil, nil
		}

		if resp != nil && resp.StatusCode == http.StatusUnauthorized && !reauthed[id] {
			reauthed[id] = true
			if !s.refreshed(account, id) {
				skip[id] = true
			}
			attempt--
			continue
		}

		if resp == nil {
			s.log.Warn("upstream websocket unreachable", "account", id, "error", err)
			s.stats.failedOver(id, "unreachable")
			account.failed(attempt)
			skip[id] = true
			continue
		}

		status := resp.StatusCode
		if status == http.StatusSwitchingProtocols {
			s.log.Warn("upstream websocket handshake invalid", "account", id, "error", err)
			s.stats.failedOver(id, "invalid handshake")
			account.failed(attempt)
			skip[id] = true
			continue
		}
		if status == http.StatusTooManyRequests || status == http.StatusUnauthorized || status >= 500 {
			if status == http.StatusTooManyRequests {
				account.observe(resp.Header)
				account.rateLimited(resp.Header, attempt)
				s.stats.rateLimited(id)
			} else {
				account.failed(attempt)
			}
			if pinned == "" {
				s.stats.failedOver(id, resp.Status)
				skip[id] = true
				continue
			}
		}
		return nil, resp, nil
	}
	return nil, nil, errors.New("every account failed this websocket")
}

func responsesWebSocketURL(upstream string) (string, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return "", fmt.Errorf("invalid upstream URL: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/responses"
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("invalid upstream URL scheme %q", u.Scheme)
	}
	return u.String(), nil
}

func responsesWebSocketHeaders(inbound http.Header, account *Account) http.Header {
	headers := http.Header{}
	copyWebSocketHeaders(headers, inbound)
	account.mu.Lock()
	token := account.AccessToken
	account.mu.Unlock()
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("chatgpt-account-id", account.id())
	ensureResponsesWebSocketBeta(headers)
	return headers
}

func ensureResponsesWebSocketBeta(headers http.Header) {
	tokens := []string{}
	seen := false
	for _, value := range headers.Values("OpenAI-Beta") {
		for token := range strings.SplitSeq(value, ",") {
			token = strings.TrimSpace(token)
			if token == "" || strings.EqualFold(token, "responses=experimental") {
				continue
			}
			seen = seen || strings.EqualFold(token, responsesWebSocketBeta)
			tokens = append(tokens, token)
		}
	}
	if !seen {
		tokens = append(tokens, responsesWebSocketBeta)
	}
	headers.Del("OpenAI-Beta")
	headers.Set("OpenAI-Beta", strings.Join(tokens, ", "))
}

func (s *server) relayResponsesWebSocket(downstream, upstream *websocket.Conn, account *Account, thread string) {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tracker := websocketTracker{stats: s.stats, account: account, thread: thread}
	errs := make(chan error, 2)
	go func() {
		errs <- relayWebSocketMessages(ctx, upstream, downstream, tracker.sent)
	}()
	go func() {
		errs <- relayWebSocketMessages(ctx, downstream, upstream, tracker.received)
	}()

	err := <-errs
	cancel()
	downstream.CloseNow()
	upstream.CloseNow()
	<-errs

	if errors.Is(err, errWebSocketAccountPaused) {
		s.log.Info("websocket stopped for paused account", "account", account.id())
		return
	}
	if errors.Is(err, context.Canceled) || websocket.CloseStatus(err) == websocket.StatusNormalClosure {
		return
	}
	s.log.Warn("websocket closed", "account", account.id(), "error", err)
}

func relayWebSocketMessages(
	ctx context.Context,
	dst, src *websocket.Conn,
	inspect func(websocket.MessageType, []byte) error,
) error {
	for {
		kind, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		if err := inspect(kind, data); err != nil {
			return err
		}
		if err := dst.Write(ctx, kind, data); err != nil {
			return err
		}
	}
}

type websocketTurn struct {
	sent        time.Time
	serviceTier string
	counted     bool
	created     bool
}

type websocketTracker struct {
	mu      sync.Mutex
	stats   *Stats
	account *Account
	thread  string
	turns   []websocketTurn
}

type websocketEnvelope struct {
	Type        string                     `json:"type"`
	Generate    *bool                      `json:"generate"`
	Status      int                        `json:"status"`
	StatusCode  int                        `json:"status_code"`
	ServiceTier string                     `json:"service_tier"`
	Headers     map[string]json.RawMessage `json:"headers"`
	Error       struct {
		Code string `json:"code"`
	} `json:"error"`
	Response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	} `json:"response"`
}

func (t *websocketTracker) sent(kind websocket.MessageType, data []byte) error {
	if kind != websocket.MessageText {
		return nil
	}
	var event websocketEnvelope
	if json.Unmarshal(data, &event) != nil || event.Type != "response.create" {
		return nil
	}
	if t.account.paused() {
		return errWebSocketAccountPaused
	}
	counted := event.Generate == nil || *event.Generate
	t.mu.Lock()
	t.turns = append(t.turns, websocketTurn{
		sent:        time.Now(),
		serviceTier: event.ServiceTier,
		counted:     counted,
	})
	t.mu.Unlock()
	return nil
}

func (t *websocketTracker) received(kind websocket.MessageType, data []byte) error {
	if kind != websocket.MessageText {
		return nil
	}
	var event websocketEnvelope
	if json.Unmarshal(data, &event) != nil {
		return nil
	}
	headers := websocketEventHeaders(event.Headers)
	if len(headers) > 0 {
		t.account.observe(headers)
	}
	status := event.Status
	if status == 0 {
		status = event.StatusCode
	}
	code := event.Error.Code
	if code == "" {
		code = event.Response.Error.Code
	}
	if status == http.StatusTooManyRequests || code == "rate_limit_exceeded" {
		t.account.rateLimited(headers, 0)
		t.stats.rateLimited(t.account.id())
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	switch event.Type {
	case "response.created":
		for i := range t.turns {
			if t.turns[i].created {
				continue
			}
			t.turns[i].created = true
			if t.turns[i].counted {
				t.stats.routed(
					t.thread,
					t.account.id(),
					t.turns[i].serviceTier,
					transportWebSocket,
				)
				t.stats.answered(time.Since(t.turns[i].sent))
			}
			break
		}
	case "error", "response.completed", "response.failed", "response.incomplete":
		if len(t.turns) > 0 {
			t.turns = t.turns[1:]
		}
	}
	return nil
}

func websocketEventHeaders(values map[string]json.RawMessage) http.Header {
	headers := http.Header{}
	for name, raw := range values {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) != nil {
			continue
		}
		switch value := value.(type) {
		case string:
			headers.Set(name, value)
		case json.Number:
			headers.Set(name, value.String())
		case bool:
			headers.Set(name, fmt.Sprint(value))
		}
	}
	return headers
}
