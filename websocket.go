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
	"time"

	"github.com/coder/websocket"
)

const responsesWebSocketBeta = "responses_websockets=2026-02-06"

type websocketDial struct {
	conn    *websocket.Conn
	resp    *http.Response
	account *Account
}

type websocketMessage struct {
	downstream bool
	generation int
	kind       websocket.MessageType
	data       []byte
	err        error
}

type websocketTurn struct {
	kind        websocket.MessageType
	data        []byte
	sent        time.Time
	serviceTier string
	counted     bool
	created     bool
	resolution  affinityResolution
	thread      string
	excluded    map[string]bool
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
		ID    string `json:"id"`
		Error struct {
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

	affinity, err := affinityFromRequest(r.Header, nil)
	if err != nil {
		status, message := affinityErrorStatus(err)
		writeError(w, status, message)
		return
	}
	resolution, err := s.affinity.resolve(affinity, s.pool)
	if err != nil {
		status, message := affinityErrorStatus(err)
		writeError(w, status, message)
		return
	}
	thread := affinity.statsKey(r.Header)
	s.log.Debug("websocket requested", "thread", thread, "required_account", resolution.required, "preferred_account", resolution.preferred)
	dial, failed, err := s.dialResponsesWebSocket(r, thread, resolution, nil)
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
	downstream.SetReadLimit(maxRequestBody)
	dial.conn.SetReadLimit(maxRequestBody)
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
	resolution affinityResolution,
	skip map[string]bool,
) (*websocketDial, *http.Response, error) {
	upstream, err := responsesWebSocketURL(s.upstream)
	if err != nil {
		return nil, nil, err
	}
	if skip == nil {
		skip = map[string]bool{}
	}
	reauthed := map[string]bool{}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		account := s.pickAccount(thread, resolution.required, resolution.preferred, skip, attempt, transportWebSocket)
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
			if resp != nil {
				account.observe(resp.Header)
			}
			attrs := []any{"transport", transportWebSocket, "thread", thread, "attempt", attempt + 1}
			attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
			s.log.Debug("websocket routed", attrs...)
			return &websocketDial{conn: conn, resp: resp, account: account}, nil, nil
		}

		if resp != nil && resp.StatusCode == http.StatusUnauthorized && !reauthed[id] {
			resp.Body.Close()
			reauthed[id] = true
			if !s.refreshed(account, id) {
				skip[id] = true
			}
			attempt--
			continue
		}

		if resp == nil {
			s.log.Warn("upstream websocket unreachable", "thread", thread, "account", id, "attempt", attempt+1, "error", err)
			s.stats.failedOver(id, "unreachable")
			account.failed(attempt)
			skip[id] = true
			continue
		}

		status := resp.StatusCode
		if status == http.StatusSwitchingProtocols {
			resp.Body.Close()
			s.log.Warn("upstream websocket handshake invalid", "thread", thread, "account", id, "attempt", attempt+1, "error", err)
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
				attrs := []any{"transport", transportWebSocket, "thread", thread, "attempt", attempt + 1}
				attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
				s.log.Info("account rate limited", attrs...)
			} else {
				s.log.Warn("upstream websocket refused the connection", "thread", thread, "account", id, "attempt", attempt+1, "status", status)
				account.failed(attempt)
			}
			if !resolution.hard {
				s.stats.failedOver(id, resp.Status)
				skip[id] = true
				resp.Body.Close()
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

func (s *server) relayResponsesWebSocket(
	downstream *websocket.Conn,
	r *http.Request,
	initial *websocketDial,
	thread string,
) {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	current := initial
	generation := 1
	messages := make(chan websocketMessage, 8)
	readWebSocketMessages(ctx, downstream, true, 0, messages)
	readWebSocketMessages(ctx, current.conn, false, generation, messages)
	s.websocketOpened(thread, current.account)
	defer func() {
		current.conn.CloseNow()
		s.websocketClosed(thread, current.account)
	}()

	var turns []websocketTurn
	switchDial := func(next *websocketDial) {
		previous := current
		current = next
		generation++
		previous.conn.CloseNow()
		s.websocketClosed(thread, previous.account)
		current.conn.SetReadLimit(maxRequestBody)
		readWebSocketMessages(ctx, current.conn, false, generation, messages)
		s.websocketOpened(thread, current.account)
	}
	replayTurn := func(turn *websocketTurn, reason string) bool {
		turn.excluded[current.account.id()] = true
		next, failed, err := s.dialResponsesWebSocket(r, turn.thread, turn.resolution, turn.excluded)
		if err != nil || failed != nil {
			if failed != nil && failed.Body != nil {
				failed.Body.Close()
			}
			return false
		}
		previousAccount := current.account.id()
		switchDial(next)
		s.stats.failedOver(previousAccount, reason)
		if current.conn.Write(ctx, turn.kind, turn.data) != nil {
			return false
		}
		turn.sent = time.Now()
		return true
	}
	for {
		var message websocketMessage
		select {
		case message = <-messages:
		case <-ctx.Done():
			return
		}
		if message.downstream {
			if message.err != nil {
				return
			}
			if message.kind != websocket.MessageText {
				if current.conn.Write(ctx, message.kind, message.data) != nil {
					return
				}
				continue
			}
			var event websocketEnvelope
			if json.Unmarshal(message.data, &event) != nil || event.Type != "response.create" {
				if current.conn.Write(ctx, message.kind, message.data) != nil {
					return
				}
				continue
			}

			requestAffinity, err := affinityFromRequest(r.Header, message.data)
			if err != nil {
				writeWebSocketAffinityError(ctx, downstream, err)
				continue
			}
			resolution, err := s.affinity.resolve(requestAffinity, s.pool)
			if err != nil {
				writeWebSocketAffinityError(ctx, downstream, err)
				continue
			}
			target := s.pool.pick(resolution.required, resolution.preferred, nil)
			if target == nil {
				writeWebSocketAffinityError(ctx, downstream, errAffinityOwnerUnavailable)
				continue
			}
			turnThread := requestAffinity.statsKey(r.Header)
			if turnThread == "" {
				turnThread = thread
			}
			if target.id() != current.account.id() {
				if len(turns) > 0 {
					writeWebSocketAffinityError(ctx, downstream, errAffinityOwnerUnavailable)
					continue
				}
				next, failed, err := s.dialResponsesWebSocket(r, turnThread, resolution, nil)
				if err != nil || failed != nil {
					if failed != nil && failed.Body != nil {
						failed.Body.Close()
					}
					writeWebSocketAffinityError(ctx, downstream, errAffinityOwnerUnavailable)
					continue
				}
				switchDial(next)
			}
			if resolution.hard {
				if err := s.affinity.bindAll(hardAffinityRefs(resolution.bindings), current.account.id()); err != nil {
					writeWebSocketAffinityError(ctx, downstream, err)
					continue
				}
			}
			if current.conn.Write(ctx, message.kind, message.data) != nil {
				return
			}
			counted := event.Generate == nil || *event.Generate
			attrs := []any{"transport", transportWebSocket, "thread", turnThread, "service_tier", event.ServiceTier}
			attrs = append(attrs, routingLogAttrs(current.account.routingCandidate(), time.Now())...)
			s.log.Debug("websocket turn received", attrs...)
			turns = append(turns, websocketTurn{
				kind:        message.kind,
				data:        append([]byte(nil), message.data...),
				sent:        time.Now(),
				serviceTier: event.ServiceTier,
				counted:     counted,
				resolution:  resolution,
				thread:      turnThread,
				excluded:    map[string]bool{},
			})
			continue
		}

		if message.generation != generation {
			continue
		}
		if message.err != nil {
			if len(turns) == 1 && !turns[0].created && !turns[0].resolution.hard {
				current.account.failed(0)
				if replayTurn(&turns[0], "disconnected") {
					continue
				}
			}
			if errors.Is(message.err, context.Canceled) || websocket.CloseStatus(message.err) == websocket.StatusNormalClosure {
				return
			}
			s.log.Warn("upstream websocket closed", "account", current.account.id(), "error", message.err)
			return
		}
		var event websocketEnvelope
		if message.kind == websocket.MessageText && json.Unmarshal(message.data, &event) == nil {
			headers := websocketEventHeaders(event.Headers)
			if len(headers) > 0 {
				current.account.observe(headers)
			}
			retryable := false
			retryReason := ""
			if websocketRateLimited(event) {
				current.account.rateLimited(headers, 0)
				s.stats.rateLimited(current.account.id())
				attrs := []any{"transport", transportWebSocket, "thread", thread, "status", websocketStatus(event), "code", websocketErrorCode(event)}
				attrs = append(attrs, routingLogAttrs(current.account.routingCandidate(), time.Now())...)
				s.log.Info("account rate limited", attrs...)
				retryable = true
				retryReason = "rate limited"
			} else if websocketStatus(event) >= 500 {
				current.account.failed(0)
				retryable = true
				retryReason = "server failure"
			}
			if retryable && len(turns) == 1 && !turns[0].created && !turns[0].resolution.hard {
				if replayTurn(&turns[0], retryReason) {
					continue
				}
			}
			switch event.Type {
			case "response.created":
				for index := range turns {
					if turns[index].created {
						continue
					}
					turns[index].created = true
					if err := s.affinity.bindAll(turns[index].resolution.bindings, current.account.id()); err != nil {
						s.log.Warn("affinity save failed", "thread", turns[index].thread, "account", current.account.id(), "error", err)
					}
					if event.Response.ID != "" {
						if err := s.affinity.bind(affinityRef{kind: affinityResponse, value: event.Response.ID}, current.account.id()); err != nil {
							s.log.Warn("response affinity save failed", "response", event.Response.ID, "account", current.account.id(), "error", err)
						}
					}
					if turns[index].counted {
						s.stats.routed(turns[index].thread, current.account.id(), turns[index].serviceTier, transportWebSocket)
						s.stats.answered(time.Since(turns[index].sent))
					}
					break
				}
			case "error", "response.completed", "response.failed", "response.incomplete":
				if len(turns) > 0 {
					turns = turns[1:]
				}
			}
		}
		if downstream.Write(ctx, message.kind, message.data) != nil {
			return
		}
	}
}

func readWebSocketMessages(
	ctx context.Context,
	conn *websocket.Conn,
	downstream bool,
	generation int,
	messages chan<- websocketMessage,
) {
	go func() {
		for {
			kind, data, err := conn.Read(ctx)
			message := websocketMessage{
				downstream: downstream,
				generation: generation,
				kind:       kind,
				data:       data,
				err:        err,
			}
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

func writeWebSocketAffinityError(ctx context.Context, conn *websocket.Conn, err error) {
	status, message := affinityErrorStatus(err)
	data, marshalErr := json.Marshal(map[string]any{
		"type":   "error",
		"status": status,
		"error": map[string]string{
			"code":    "affinity_error",
			"message": message,
		},
	})
	if marshalErr == nil {
		conn.Write(ctx, websocket.MessageText, data)
	}
}

func websocketStatus(event websocketEnvelope) int {
	if event.Status != 0 {
		return event.Status
	}
	return event.StatusCode
}

func websocketErrorCode(event websocketEnvelope) string {
	if event.Error.Code != "" {
		return event.Error.Code
	}
	return event.Response.Error.Code
}

func websocketRateLimited(event websocketEnvelope) bool {
	return websocketStatus(event) == http.StatusTooManyRequests || websocketErrorCode(event) == "rate_limit_exceeded"
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
