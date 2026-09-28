package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const responsesWebSocketBeta = "responses_websockets=2026-02-06"

var (
	errNoAccountAvailable    = errors.New("no account available")
	errRouteOwnerUnavailable = errors.New("session account temporarily unavailable; retry")
	errAccountBoundTurn      = errors.New("account-bound turn cannot move accounts; start a new turn or resume")
	errUpgradeBudget         = errors.New("upstream connection not ready within the upgrade budget; retry")
)

type websocketDial struct {
	*responseAccount
	conn *websocket.Conn
	resp *http.Response
}

func (s *server) transferWebSocketClaim(current *routeClaimHandle, account, priorOwner string, reason routingReason) *routeClaimHandle {
	s.routeOwnership.Lock()
	defer s.routeOwnership.Unlock()
	return current.transfer(account, priorOwner, reason)
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
	turnState   string
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
	Status             responseEventStatus        `json:"status"`
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
		s.logResponseRejection(w, r, http.StatusUnauthorized, "invalid_bearer_key")
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	if !s.websocketHandshake(w, r) {
		return
	}

	mode, changed := s.fastMode.snapshot()
	route := websocketRouteFrom(r.Header)
	thread := route.key()
	s.log.Debug("websocket requested", "thread", thread)
	redactor := s.responsesRedactor()
	wait := s.upgradeWait
	if wait == 0 {
		wait = websocketUpgradeWait
	}
	dialCtx, cancelDial := context.WithTimeoutCause(r.Context(), wait, errUpgradeBudget)
	dial, failed, err := s.dialResponsesWebSocket(r.WithContext(dialCtx), route, "", "")
	cancelDial()
	if err != nil || failed != nil {
		defer closeWebSocketResponse(failed)
		peer := &httpResponsesDownstream{writer: w, controller: http.NewResponseController(w), ctx: r.Context(), responsesRedactor: redactor}
		failure, headers := responseSetupFailure(r.Context(), failed, err)
		s.logResponseRejection(w, r, failure.Status, "upstream_setup_failed")
		peer.writeSetupFailure(failure, headers)
		return
	}
	if dial.resp != nil {
		copyWebSocketHeaders(w.Header(), dial.resp.Header)
	}
	s.pool.clientUsage().writeHeaders(w.Header())
	downstream, err := websocket.Accept(w, r, nil)
	if err != nil {
		dial.conn.CloseNow()
		dial.releaseClaim()
		s.log.Warn("websocket upgrade failed", "error", err)
		return
	}
	defer downstream.CloseNow()
	downstream.SetReadLimit(maxWebSocketMessage)
	dial.conn.SetReadLimit(maxWebSocketMessage)
	newResponsesWebSocketRelay(s, websocketDownstream{Conn: downstream, responsesRedactor: redactor}, r, dial, route, apiKey, mode, changed).run()
}

func (s *server) websocketHandshake(w http.ResponseWriter, r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.logResponseRejection(w, r, http.StatusMethodNotAllowed, "websocket_upgrade_required")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") {
		s.logResponseRejection(w, r, http.StatusUpgradeRequired, "connection_upgrade_required")
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.WriteHeader(http.StatusUpgradeRequired)
		return false
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" || r.Header.Get("Sec-WebSocket-Key") == "" {
		s.logResponseRejection(w, r, http.StatusBadRequest, "invalid_websocket_handshake")
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

func (s *server) dialResponsesWebSocketReplacing(
	r *http.Request,
	route websocketRoute,
	model string,
	serviceTier string,
	current *routeClaimHandle,
) (*websocketDial, *http.Response, error) {
	dialer, err := newResponsesWebSocketDialer(s, r, route, model, serviceTier)
	if err != nil {
		return nil, nil, err
	}
	dialer.replacing = current
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

func responsesWebSocketHeaders(inbound http.Header, account *Account) (http.Header, authorizationRevision) {
	headers := http.Header{}
	copyWebSocketHeaders(headers, inbound)
	headers.Del("Accept")
	headers.Del("Content-Type")
	account.mu.Lock()
	token := account.AccessToken
	digest := accessTokenDigest(token)
	accountID := claimsFromToken(account.IDToken).Auth.AccountID
	account.mu.Unlock()
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("chatgpt-account-id", accountID)
	ensureResponsesWebSocketBeta(headers)
	return headers, digest
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

type websocketRejectionKind string

const (
	websocketRejectionNone            websocketRejectionKind = ""
	websocketRejectionUnauthorized    websocketRejectionKind = "unauthorized"
	websocketRejectionRateLimited     websocketRejectionKind = "rate limited"
	websocketRejectionUsageLimit      websocketRejectionKind = "usage limit reached"
	websocketRejectionConnectionLimit websocketRejectionKind = "connection limit reached"
	websocketRejectionModelCapacity   websocketRejectionKind = "model capacity"
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
	if websocketErrorIs(event, "server_is_overloaded") || websocketErrorIs(event, "slow_down") {
		return websocketRejectionModelCapacity
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

func readWebSocketMessages(ctx context.Context, conn interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
}, downstream bool, messages chan<- websocketMessage) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
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
	return done
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
		return int(event.Status)
	}
	return event.StatusCode
}

func websocketErrorIs(event websocketEnvelope, code string) bool {
	return event.Error.Type == code || event.Error.Code == code || event.Response.Error.Type == code || event.Response.Error.Code == code
}

func websocketRequestPortable(event websocketEnvelope) bool {
	return strings.TrimSpace(event.PreviousResponseID) == "" &&
		strings.TrimSpace(event.ClientMetadata[codexTurnStateKey]) == ""
}

func websocketRouteFrom(headers http.Header) websocketRoute {
	return websocketRoute{
		session: firstWebSocketHeader(headers, "session_id", "session-id", "x-codex-session-id", "x-codex-conversation-id", "x-session-affinity", "x-session-id"),
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
