package app

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
	"time"

	"github.com/coder/websocket"
)

const responsesWebSocketBeta = "responses_websockets=2026-02-06"

const noAccountAvailableMessage = "WE ARE OUT OF TOKENS 😭 Go out, touch some grass 🌿 See https://codex-balancer.exe.xyz/dashboard"

var (
	errNoAccountAvailable    = errors.New("no account available")
	errRouteOwnerUnavailable = errors.New("session account temporarily unavailable; retry")
)

type websocketDial struct {
	conn    *websocket.Conn
	resp    *http.Response
	account *Account
	moved   bool
}

type websocketRoute struct {
	session string
	thread  string
}

func (r websocketRoute) key() string {
	if r.thread != "" {
		return r.thread
	}
	return r.session
}

type websocketMessage struct {
	downstream bool
	kind       websocket.MessageType
	data       []byte
	err        error
}

type websocketTurn struct {
	sent        time.Time
	model       string
	effort      string
	serviceTier string
	metadata    turnMetadata
	counted     bool
	created     bool
	statsThread string
}

type websocketEnvelope struct {
	Type               string                     `json:"type"`
	Generate           *bool                      `json:"generate"`
	Model              string                     `json:"model"`
	Reasoning          responseReasoning          `json:"reasoning"`
	ServiceTier        string                     `json:"service_tier"`
	PreviousResponseID string                     `json:"previous_response_id"`
	ClientMetadata     map[string]string          `json:"client_metadata"`
	Status             int                        `json:"status"`
	StatusCode         int                        `json:"status_code"`
	Headers            map[string]json.RawMessage `json:"headers"`
	Error              struct {
		Type string `json:"type"`
		Code string `json:"code"`
	} `json:"error"`
	Response struct {
		responsePayload
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	} `json:"response"`
}

func (s *server) responsesWebSocket(w http.ResponseWriter, r *http.Request) {
	apiKey, authorized := s.authorizeAPIKey(r)
	if !authorized {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	if !websocketHandshake(w, r) {
		return
	}

	route := websocketRouteFrom(r.Header)
	thread := route.key()
	s.log.Debug("websocket requested", "thread", thread)
	dial, failed, err := s.dialResponsesWebSocket(r, route, "", "")
	if err != nil {
		message := err.Error()
		if errors.Is(err, errNoAccountAvailable) {
			message = noAccountAvailableMessage
		}
		writeError(w, http.StatusServiceUnavailable, message)
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
	if dial.resp != nil {
		copyWebSocketHeaders(w.Header(), dial.resp.Header)
	}
	downstream, err := websocket.Accept(w, r, nil)
	if err != nil {
		dial.conn.CloseNow()
		s.log.Warn("websocket upgrade failed", "error", err)
		return
	}
	defer downstream.CloseNow()
	downstream.SetReadLimit(maxWebSocketMessage)
	dial.conn.SetReadLimit(maxWebSocketMessage)
	s.relayResponsesWebSocket(downstream, r, dial, route, apiKey)
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

func (s *server) dialResponsesWebSocket(
	r *http.Request,
	route websocketRoute,
	model string,
	serviceTier string,
) (*websocketDial, *http.Response, error) {
	dialer, err := newResponsesWebSocketDialer(s, r, route, model, serviceTier)
	if err != nil {
		return nil, nil, err
	}
	return dialer.dial()
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
	headers.Del("Accept")
	headers.Del("Content-Type")
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

func (s *server) relayResponsesWebSocket(downstream *websocket.Conn, r *http.Request, initial *websocketDial, route websocketRoute, apiKey apiKeyIdentity) {
	newResponsesWebSocketRelay(s, downstream, r, initial, route, apiKey).run()
}

type websocketRejectionKind string

const (
	websocketRejectionNone            websocketRejectionKind = ""
	websocketRejectionUnauthorized    websocketRejectionKind = "unauthorized"
	websocketRejectionRateLimited     websocketRejectionKind = "rate limited"
	websocketRejectionUsageLimit      websocketRejectionKind = "usage limit reached"
	websocketRejectionConnectionLimit websocketRejectionKind = "connection limit reached"
)

func websocketRejection(event websocketEnvelope) websocketRejectionKind {
	if websocketErrorIs(event, "websocket_connection_limit_reached") {
		return websocketRejectionConnectionLimit
	}
	if websocketStatus(event) == http.StatusUnauthorized {
		return websocketRejectionUnauthorized
	}
	if websocketErrorIs(event, "usage_limit_reached") {
		return websocketRejectionUsageLimit
	}
	if websocketStatus(event) == http.StatusTooManyRequests || websocketErrorIs(event, "rate_limit_exceeded") {
		return websocketRejectionRateLimited
	}
	return websocketRejectionNone
}

func (s *server) handleWebSocketRejection(account *Account, kind websocketRejectionKind, headers http.Header, thread string) {
	id := account.id()
	if kind == websocketRejectionConnectionLimit {
		s.log.Info("upstream websocket expired", "thread", thread, "account", id)
		return
	}
	s.log.Info("account rejected websocket request", "thread", thread, "account", id, "reason", kind)
	switch kind {
	case websocketRejectionUnauthorized:
		account.failed(0)
	case websocketRejectionRateLimited:
		account.rateLimited(headers, 0)
		s.stats.rateLimited(id)
	case websocketRejectionUsageLimit:
		if account.markSpent() {
			s.log.Info("account stopped accepting new websockets", "account", id, "source", "response", "thread", thread)
		}
		s.stats.rateLimited(id)
	}
}

func readWebSocketMessages(ctx context.Context, conn *websocket.Conn, downstream bool, messages chan<- websocketMessage) {
	go func() {
		for {
			kind, data, err := conn.Read(ctx)
			message := websocketMessage{downstream: downstream, kind: kind, data: data, err: err}
			select {
			case messages <- message:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
}

func (s *server) websocketOpened(thread string, account *Account) {
	s.stats.websocketOpened(account.id())
	s.log.Debug("websocket opened", "thread", thread, "account", account.id())
}

func (s *server) websocketClosed(thread string, account *Account) {
	s.stats.websocketClosed(account.id())
	s.log.Debug("websocket closed", "thread", thread, "account", account.id())
}

func websocketStatus(event websocketEnvelope) int {
	if event.Status != 0 {
		return event.Status
	}
	return event.StatusCode
}

func websocketErrorIs(event websocketEnvelope, code string) bool {
	return event.Error.Type == code || event.Error.Code == code || event.Response.Error.Type == code || event.Response.Error.Code == code
}

func websocketRequestPortable(event websocketEnvelope) bool {
	return strings.TrimSpace(event.PreviousResponseID) == "" && strings.TrimSpace(event.ClientMetadata[codexTurnStateKey]) == ""
}

func websocketOwnerMayRefresh(owners []string, account *Account) bool {
	candidate := account.routingCandidate()
	if candidate.reauth != "" {
		return false
	}
	for _, owner := range owners {
		if owner == candidate.id {
			return true
		}
	}
	return false
}

func websocketRouteFrom(headers http.Header) websocketRoute {
	return websocketRoute{
		session: firstWebSocketHeader(headers, "session_id", "session-id", "x-codex-session-id", "x-codex-conversation-id"),
		thread:  firstWebSocketHeader(headers, "thread-id", "x-client-request-id"),
	}
}

func firstWebSocketHeader(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
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
