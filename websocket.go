package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const responsesWebSocketBeta = "responses_websockets=2026-02-06"

const noAccountAvailableMessage = "WE ARE OUT OF TOKENS 😭 Go out, touch some grass 🌿 See https://codex-balancer.exe.xyz/dashboard"

var errNoAccountAvailable = errors.New("no account available")

type websocketDial struct {
	conn    *websocket.Conn
	resp    *http.Response
	account *Account
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
	Type           string                     `json:"type"`
	Generate       *bool                      `json:"generate"`
	Model          string                     `json:"model"`
	Reasoning      responseReasoning          `json:"reasoning"`
	ServiceTier    string                     `json:"service_tier"`
	ClientMetadata map[string]string          `json:"client_metadata"`
	Status         int                        `json:"status"`
	StatusCode     int                        `json:"status_code"`
	Headers        map[string]json.RawMessage `json:"headers"`
	Error          struct {
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
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	if !websocketHandshake(w, r) {
		return
	}

	thread := websocketThread(r.Header)
	s.log.Debug("websocket requested", "thread", thread)
	dial, failed, err := s.dialResponsesWebSocket(r, thread, "", "")
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
	s.relayResponsesWebSocket(downstream, r, dial, thread)
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
	thread string,
	model string,
	serviceTier string,
) (*websocketDial, *http.Response, error) {
	upstream, err := responsesWebSocketURL(s.upstream)
	if err != nil {
		return nil, nil, err
	}

	skip := map[string]bool{}
	reauthed := map[string]bool{}
	resetRetried := map[string]bool{}
	for attempt := 0; ; attempt++ {
		account := s.pickAccount(thread, model, serviceTier, skip, attempt)
		if account == nil {
			return nil, nil, errNoAccountAvailable
		}
		id := account.id()
		if account.stale(time.Now()) && !reauthed[id] {
			reauthed[id] = true
			if !s.refreshed(account, id) {
				skip[id] = true
				continue
			}
		}

		var conn *websocket.Conn
		var resp *http.Response
		var dialErr error
		var sent time.Time
		upstreamRetries := 0
		for {
			sent = time.Now()
			ctx, cancel := context.WithTimeout(r.Context(), upstreamWait)
			conn, resp, dialErr = websocket.Dial(ctx, upstream, &websocket.DialOptions{
				HTTPClient: s.client,
				HTTPHeader: responsesWebSocketHeaders(r.Header, account),
			})
			cancel()
			if dialErr == nil || resp == nil || resp.StatusCode < 500 || upstreamRetries == maxUpstreamRetries {
				break
			}
			status := resp.StatusCode
			if resp.Body != nil {
				resp.Body.Close()
			}
			upstreamRetries++
			s.log.Info("retrying upstream websocket server failure", "thread", thread, "account", id, "attempt", attempt+1, "retry", upstreamRetries, "status", status)
			if !s.waitForUpstreamRetry(r.Context(), upstreamRetries) {
				return nil, nil, context.Cause(r.Context())
			}
		}

		if dialErr == nil {
			if resp != nil {
				account.observe(resp.Header)
			}
			attrs := []any{"thread", thread, "attempt", attempt + 1}
			attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
			s.log.Debug("websocket routed", attrs...)
			return &websocketDial{conn: conn, resp: resp, account: account}, nil, nil
		}
		if cause := context.Cause(r.Context()); cause != nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			return nil, nil, cause
		}
		if resp != nil && resp.StatusCode == http.StatusUnauthorized && !reauthed[id] {
			if resp.Body != nil {
				resp.Body.Close()
			}
			reauthed[id] = true
			if !s.refreshed(account, id) {
				skip[id] = true
			}
			attempt--
			continue
		}
		if resp == nil {
			s.log.Warn("upstream websocket unreachable", "thread", thread, "account", id, "attempt", attempt+1, "error", dialErr)
			return nil, nil, dialErr
		}

		status := resp.StatusCode
		if status == http.StatusSwitchingProtocols {
			if resp.Body != nil {
				resp.Body.Close()
			}
			s.log.Warn("upstream websocket handshake invalid", "thread", thread, "account", id, "attempt", attempt+1, "error", dialErr)
			s.stats.failedOver(id, "invalid handshake")
			account.failed(attempt)
			skip[id] = true
			continue
		}
		if status >= http.StatusInternalServerError {
			s.log.Warn("upstream websocket server failure", "thread", thread, "account", id, "attempt", attempt+1, "status", status)
			return nil, resp, nil
		}
		usageLimit := (status == http.StatusTooManyRequests || status == http.StatusForbidden) && responseUsageLimitReached(resp)
		if status == http.StatusTooManyRequests || usageLimit || status == http.StatusUnauthorized {
			if status == http.StatusTooManyRequests || usageLimit {
				account.observe(resp.Header)
				if usageLimit {
					if account.markSpent() {
						s.log.Info("account stopped accepting new websockets", "account", id, "source", "handshake", "thread", thread, "status", status)
					}
					resettableUsageLimit := !workspaceUsageLimitReached(resp.Header)
					if resettableUsageLimit && !resetRetried[id] && s.recoverUsageLimit(r.Context(), account, sent) {
						resetRetried[id] = true
						if resp.Body != nil {
							resp.Body.Close()
						}
						attempt--
						continue
					}
				} else {
					account.rateLimited(resp.Header, attempt)
				}
				s.stats.rateLimited(id)
				attrs := []any{"thread", thread, "attempt", attempt + 1}
				attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
				s.log.Info("account rate limited", attrs...)
			} else {
				s.log.Warn("upstream websocket rejected credentials", "thread", thread, "account", id, "attempt", attempt+1, "status", status)
				account.failed(attempt)
			}
			if resp.Body != nil {
				resp.Body.Close()
			}
			s.stats.failedOver(id, resp.Status)
			skip[id] = true
			continue
		}
		return nil, resp, nil
	}
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

func (s *server) relayResponsesWebSocket(downstream *websocket.Conn, r *http.Request, initial *websocketDial, thread string) {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	messages := make(chan websocketMessage, 8)
	readWebSocketMessages(ctx, downstream, true, messages)
	liveThreads := map[string]struct{}{}
	defer func() {
		for thread := range liveThreads {
			s.stats.deactivateThread(thread)
		}
	}()
	current := initial
	s.websocketOpened(thread, current.account)
	defer func() {
		cancel()
		current.conn.CloseNow()
		s.websocketClosed(thread, current.account)
	}()

	closeDownstream := func(status websocket.StatusCode, reason string) {
		if err := downstream.Close(status, reason); err != nil {
			s.log.Debug("downstream websocket close failed", "thread", thread, "status", status, "error", err)
		}
	}
	switchAccount := func(next *websocketDial, model, serviceTier string) {
		previous := current
		previous.conn.CloseNow()
		s.websocketClosed(thread, previous.account)
		current = next
		current.conn.SetReadLimit(maxWebSocketMessage)
		s.websocketOpened(thread, current.account)
		s.log.Info("websocket selected model-compatible account",
			"thread", thread,
			"from_account", previous.account.id(),
			"to_account", current.account.id(),
			"model", model,
			"service_tier", serviceTier,
		)
	}
	writeUpstream := func(message websocketMessage) bool {
		if err := current.conn.Write(ctx, message.kind, message.data); err != nil {
			closeDownstream(websocket.StatusServiceRestart, "upstream websocket unavailable")
			return false
		}
		return true
	}

	var turns []websocketTurn
	var pending []websocketMessage
	pendingBytes := 0
	pinned := false
	for {
		var message websocketMessage
		select {
		case message = <-messages:
		case <-ctx.Done():
			return
		}
		if message.downstream {
			if message.err != nil {
				level := slog.LevelWarn
				status := websocket.CloseStatus(message.err)
				if errors.Is(message.err, context.Canceled) || status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
					level = slog.LevelDebug
				}
				s.log.Log(ctx, level, "downstream websocket closed", "thread", thread, "account", current.account.id(), "active_turns", len(turns), "status", status, "error", message.err)
				return
			}
			var event websocketEnvelope
			responseCreate := message.kind == websocket.MessageText && json.Unmarshal(message.data, &event) == nil && event.Type == "response.create"
			if !pinned && !responseCreate {
				pendingBytes += len(message.data)
				if pendingBytes > maxWebSocketMessage {
					closeDownstream(websocket.StatusMessageTooBig, "messages before first turn are too large")
					return
				}
				pending = append(pending, message)
				continue
			}
			if !responseCreate {
				if !writeUpstream(message) {
					return
				}
				continue
			}
			allowed := s.allowedAccounts(event.Model, event.ServiceTier)
			if !accountAllowed(allowed, current.account.id()) {
				if pinned {
					closeDownstream(websocket.StatusServiceRestart, "requested model requires another account")
					return
				}
				next, failed, err := s.dialResponsesWebSocket(r, thread, event.Model, event.ServiceTier)
				if failed != nil && failed.Body != nil {
					failed.Body.Close()
				}
				if err != nil || failed != nil {
					s.log.Warn("model-compatible websocket unavailable", "thread", thread, "model", event.Model, "service_tier", event.ServiceTier, "error", err)
					closeDownstream(websocket.StatusTryAgainLater, "no account supports requested model")
					return
				}
				switchAccount(next, event.Model, event.ServiceTier)
			}
			if !pinned {
				pinned = true
				readWebSocketMessages(ctx, current.conn, false, messages)
				for _, queued := range pending {
					if !writeUpstream(queued) {
						return
					}
				}
				pending = nil
			}
			if !writeUpstream(message) {
				return
			}
			metadata := requestTurnMetadata("", event.ClientMetadata)
			statsThread := statsThreadKey(thread, metadata)
			counted := event.Generate == nil || *event.Generate
			turns = append(turns, websocketTurn{
				sent:        time.Now(),
				model:       event.Model,
				effort:      event.Reasoning.Effort,
				serviceTier: event.ServiceTier,
				metadata:    metadata,
				counted:     counted,
				statsThread: statsThread,
			})
			attrs := []any{"thread", statsThread, "service_tier", event.ServiceTier}
			attrs = append(attrs, routingLogAttrs(current.account.routingCandidate(), time.Now())...)
			s.log.Debug("websocket turn received", attrs...)
			continue
		}

		if message.err != nil {
			closeDownstream(websocket.StatusServiceRestart, "upstream websocket unavailable")
			return
		}
		var event websocketEnvelope
		if message.kind == websocket.MessageText && json.Unmarshal(message.data, &event) == nil {
			headers := websocketEventHeaders(event.Headers)
			if len(headers) > 0 {
				current.account.observe(headers)
			}
			if rejection := websocketRejection(event); rejection != websocketRejectionNone {
				s.handleWebSocketRejection(current.account, rejection, headers, thread)
				closeDownstream(websocket.StatusServiceRestart, "account rejected websocket request")
				return
			}
			if event.Type == "response.created" {
				for index := range turns {
					if turns[index].created {
						continue
					}
					turns[index].created = true
					turn := &turns[index]
					if turn.counted {
						if _, live := liveThreads[turn.statsThread]; !live {
							s.stats.activateThread(turn.statsThread)
							liveThreads[turn.statsThread] = struct{}{}
						}
						s.stats.routed(turn.statsThread, requestIP(r), current.account.id(), turn.model, turn.effort, turn.serviceTier, turn.metadata)
						s.stats.answered(turn.statsThread, current.account.id(), time.Since(turn.sent))
					}
					s.log.Debug("websocket response created", "thread", turn.statsThread, "turn", turn.metadata.TurnID, "account", current.account.id(), "latency", time.Since(turn.sent))
					break
				}
			} else if event.Type == "error" || event.Type == "response.completed" || event.Type == "response.failed" || event.Type == "response.incomplete" {
				if len(turns) > 0 {
					turn := turns[0]
					if turn.counted && event.Type != "error" {
						model := event.Response.Model
						if model == "" {
							model = turn.model
						}
						serviceTier := event.Response.ServiceTier
						if serviceTier == "" {
							serviceTier = turn.serviceTier
						}
						if !event.Response.Usage.empty() {
							logResponseUsage(s.log, turn.statsThread, current.account.id(), model, serviceTier, turn.metadata, time.Since(turn.sent), event.Response.Usage)
						}
						s.stats.recordUsage(turn.statsThread, current.account.id(), model, turn.effort, serviceTier, event.Response.Usage)
						if event.Type == "response.completed" {
							s.stats.completed(turn.statsThread, current.account.id(), turn.metadata, time.Since(turn.sent))
						}
					}
					turns = turns[1:]
				}
			}
		}
		if err := downstream.Write(ctx, message.kind, message.data); err != nil {
			s.log.Warn("downstream websocket response write failed", "thread", thread, "account", current.account.id(), "active_turns", len(turns), "error", err)
			return
		}
	}
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
	s.log.Info("account rejected websocket request", "thread", thread, "account", id, "reason", kind)
	switch kind {
	case websocketRejectionUnauthorized, websocketRejectionConnectionLimit:
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

func websocketThread(headers http.Header) string {
	for _, name := range []string{"session_id", "session-id", "x-codex-session-id", "x-codex-conversation-id", "thread-id"} {
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
