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
	kind         websocket.MessageType
	data         []byte
	sent         time.Time
	model        string
	effort       string
	serviceTier  string
	metadata     turnMetadata
	counted      bool
	created      bool
	visible      bool
	modelRetried bool
	resolution   affinityResolution
	thread       string
	rotationFrom string
	excluded     map[string]bool
	reauthed     map[string]bool
}

type websocketEnvelope struct {
	Type           string                     `json:"type"`
	ID             string                     `json:"id"`
	ResponseID     string                     `json:"response_id"`
	Generate       *bool                      `json:"generate"`
	Model          string                     `json:"model"`
	Reasoning      responseReasoning          `json:"reasoning"`
	Status         int                        `json:"status"`
	StatusCode     int                        `json:"status_code"`
	ServiceTier    string                     `json:"service_tier"`
	ClientMetadata map[string]string          `json:"client_metadata"`
	Headers        map[string]json.RawMessage `json:"headers"`
	Error          struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Response struct {
		responsePayload
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
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
		s.log.Warn("websocket handshake affinity invalid", "error", err)
		status, message := affinityErrorStatus(err)
		writeError(w, status, message)
		return
	}
	resolution, err := s.affinity.resolve(affinity, s.pool)
	if err != nil {
		s.log.Warn("websocket handshake affinity unresolved",
			"thread", affinity.statsKey(r.Header),
			"hard_affinity", len(affinity.hard) > 0,
			"error", err,
		)
		status, message := affinityErrorStatus(err)
		writeError(w, status, message)
		return
	}
	thread := affinity.statsKey(r.Header)
	s.log.Debug("websocket requested", "thread", thread, "required_account", resolution.required, "preferred_account", resolution.preferred)
	skip, pendingRotation, rotating := s.compactionRotation.handshakeSkip(thread, resolution.hard)
	dial, failed, err := s.dialResponsesWebSocket(r, thread, resolution, skip, "", "")
	if rotating && (err != nil || failed != nil) {
		status := 0
		if failed != nil {
			status = failed.StatusCode
		}
		s.log.Warn("compaction rotation websocket handshake failed",
			"thread", thread,
			"source_account", pendingRotation.account,
			"compaction_turn", pendingRotation.turn,
			"status", status,
			"error", err,
		)
		if failed != nil && failed.Body != nil {
			failed.Body.Close()
		}
		s.compactionRotation.finish(thread, "handshake_failed", "")
		rotating = false
		dial, failed, err = s.dialResponsesWebSocket(r, thread, resolution, nil, "", "")
	}
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
	if rotating {
		s.log.Info("compaction rotation websocket connected",
			"thread", thread,
			"source_account", pendingRotation.account,
			"target_account", dial.account.id(),
			"compaction_turn", pendingRotation.turn,
		)
	}

	if dial.resp != nil {
		if err := s.affinity.bind(turnStateAffinity(dial.resp.Header), dial.account.id()); err != nil {
			dial.conn.CloseNow()
			status, message := affinityErrorStatus(err)
			writeError(w, status, message)
			return
		}
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
	model string,
	serviceTier string,
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
		account := s.pickAccount(thread, resolution.required, resolution.preferred, model, serviceTier, skip, attempt, transportWebSocket)
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

		var conn *websocket.Conn
		var resp *http.Response
		var err error
		upstreamRetries := 0
		for {
			ctx, cancel := context.WithTimeout(r.Context(), upstreamWait)
			conn, resp, err = websocket.Dial(ctx, upstream, &websocket.DialOptions{
				HTTPClient: s.client,
				HTTPHeader: responsesWebSocketHeaders(r.Header, account),
			})
			cancel()
			if err == nil || resp == nil || resp.StatusCode < 500 || upstreamRetries == maxUpstreamRetries {
				break
			}
			status := resp.StatusCode
			resp.Body.Close()
			upstreamRetries++
			s.log.Info("retrying upstream websocket server failure", "thread", thread, "account", id, "attempt", attempt+1, "retry", upstreamRetries, "status", status)
			if !s.waitForUpstreamRetry(r.Context(), upstreamRetries) {
				return nil, nil, context.Cause(r.Context())
			}
		}
		if err == nil {
			if resp != nil {
				account.observe(resp.Header)
			}
			attrs := []any{"transport", transportWebSocket, "thread", thread, "attempt", attempt + 1}
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
	redialTurn := func(turn *websocketTurn, resolution affinityResolution, skip map[string]bool, reason string) bool {
		next, failed, err := s.dialResponsesWebSocket(r, turn.thread, resolution, skip, turn.model, turn.serviceTier)
		if err != nil || failed != nil {
			status := 0
			if failed != nil {
				status = failed.StatusCode
			}
			s.log.Warn("websocket redial failed",
				"thread", turn.thread,
				"turn", turn.metadata.TurnID,
				"account", current.account.id(),
				"required_account", resolution.required,
				"preferred_account", resolution.preferred,
				"reason", reason,
				"status", status,
				"error", err,
			)
			if failed != nil && failed.Body != nil {
				failed.Body.Close()
			}
			return false
		}
		previousAccount := current.account.id()
		switchDial(next)
		s.log.Info("websocket redialed",
			"thread", turn.thread,
			"turn", turn.metadata.TurnID,
			"from_account", previousAccount,
			"to_account", current.account.id(),
			"reason", reason,
		)
		if reason != "" {
			s.stats.failedOver(previousAccount, reason)
		}
		if err := current.conn.Write(ctx, turn.kind, turn.data); err != nil {
			s.log.Warn("websocket replay write failed",
				"thread", turn.thread,
				"turn", turn.metadata.TurnID,
				"account", current.account.id(),
				"reason", reason,
				"error", err,
			)
			return false
		}
		turn.sent = time.Now()
		return true
	}
	replayTurn := func(turn *websocketTurn, reason string) bool {
		turn.excluded[current.account.id()] = true
		s.log.Debug("websocket replay requested",
			"thread", turn.thread,
			"turn", turn.metadata.TurnID,
			"account", current.account.id(),
			"reason", reason,
			"excluded_accounts", len(turn.excluded),
		)
		return redialTurn(turn, turn.resolution, turn.excluded, reason)
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
				level := slog.LevelWarn
				status := websocket.CloseStatus(message.err)
				if errors.Is(message.err, context.Canceled) || status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
					level = slog.LevelDebug
				}
				s.log.Log(ctx, level, "downstream websocket closed",
					"thread", thread,
					"account", current.account.id(),
					"active_turns", len(turns),
					"status", status,
					"error", message.err,
				)
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
				s.log.Warn("websocket request affinity invalid",
					"thread", thread,
					"account", current.account.id(),
					"error", err,
				)
				writeWebSocketAffinityError(ctx, downstream, err)
				continue
			}
			resolution, err := s.affinity.resolve(requestAffinity, s.pool)
			if err != nil {
				s.log.Warn("websocket request affinity unresolved",
					"thread", requestAffinity.statsKey(r.Header),
					"account", current.account.id(),
					"hard_affinity", len(requestAffinity.hard) > 0,
					"error", err,
				)
				writeWebSocketAffinityError(ctx, downstream, err)
				continue
			}
			turnThread := requestAffinity.statsKey(r.Header)
			if turnThread == "" {
				turnThread = thread
			}
			metadata := requestTurnMetadata("", event.ClientMetadata)
			allowed := s.allowedAccounts(event.Model, event.ServiceTier)
			alternate := s.pool.pick("", "", map[string]bool{current.account.id(): true}, allowed) != nil
			if s.compactionRotation.shouldReconnect(turnThread, current.account.id(), metadata, resolution.hard, len(turns), alternate) {
				s.stats.note("rotation reconnect", current.account.id(), turnThread)
				if err := downstream.Close(websocket.StatusServiceRestart, "account rotation after compaction"); err != nil {
					s.log.Warn("compaction rotation downstream restart failed",
						"thread", turnThread,
						"account", current.account.id(),
						"request_turn", metadata.TurnID,
						"error", err,
					)
				} else {
					s.log.Debug("compaction rotation downstream restarted",
						"thread", turnThread,
						"account", current.account.id(),
						"request_turn", metadata.TurnID,
					)
				}
				return
			}
			rotationFrom := ""
			if rotationFrom = s.compactionRotation.routeSource(turnThread, current.account.id(), metadata, resolution.hard); rotationFrom != "" {
				resolution.required = ""
				resolution.preferred = current.account.id()
			}
			target := s.pool.pick(resolution.required, resolution.preferred, nil, allowed)
			if target == nil {
				s.log.Warn("websocket turn has no account",
					"thread", turnThread,
					"turn", metadata.TurnID,
					"required_account", resolution.required,
					"preferred_account", resolution.preferred,
					"rotation_source", rotationFrom,
				)
				writeWebSocketAffinityError(ctx, downstream, errAffinityOwnerUnavailable)
				continue
			}
			if target.id() != current.account.id() {
				if len(turns) > 0 {
					s.log.Warn("websocket account switch blocked",
						"thread", turnThread,
						"turn", metadata.TurnID,
						"current_account", current.account.id(),
						"target_account", target.id(),
						"active_turns", len(turns),
						"rotation_source", rotationFrom,
					)
					writeWebSocketAffinityError(ctx, downstream, errAffinityOwnerUnavailable)
					continue
				}
				next, failed, err := s.dialResponsesWebSocket(r, turnThread, resolution, nil, event.Model, event.ServiceTier)
				if err != nil || failed != nil {
					status := 0
					if failed != nil {
						status = failed.StatusCode
					}
					s.log.Warn("websocket account switch failed",
						"thread", turnThread,
						"turn", metadata.TurnID,
						"current_account", current.account.id(),
						"target_account", target.id(),
						"rotation_source", rotationFrom,
						"status", status,
						"error", err,
					)
					if failed != nil && failed.Body != nil {
						failed.Body.Close()
					}
					writeWebSocketAffinityError(ctx, downstream, errAffinityOwnerUnavailable)
					continue
				}
				previousAccount := current.account.id()
				switchDial(next)
				s.log.Info("websocket account switched",
					"thread", turnThread,
					"turn", metadata.TurnID,
					"from_account", previousAccount,
					"to_account", current.account.id(),
					"rotation_source", rotationFrom,
				)
			}
			if resolution.hard {
				if err := s.affinity.claimAll(hardAffinityRefs(resolution.bindings), current.account.id()); err != nil {
					s.log.Warn("websocket hard affinity claim failed",
						"thread", turnThread,
						"turn", metadata.TurnID,
						"account", current.account.id(),
						"bindings", len(resolution.bindings),
						"error", err,
					)
					writeWebSocketAffinityError(ctx, downstream, err)
					continue
				}
			}
			if err := current.conn.Write(ctx, message.kind, message.data); err != nil {
				s.log.Warn("upstream websocket request write failed",
					"thread", turnThread,
					"turn", metadata.TurnID,
					"account", current.account.id(),
					"rotation_source", rotationFrom,
					"error", err,
				)
				return
			}
			counted := event.Generate == nil || *event.Generate
			attrs := []any{"transport", transportWebSocket, "thread", turnThread, "service_tier", event.ServiceTier}
			attrs = append(attrs, routingLogAttrs(current.account.routingCandidate(), time.Now())...)
			s.log.Debug("websocket turn received", attrs...)
			turns = append(turns, websocketTurn{
				kind:         message.kind,
				data:         append([]byte(nil), message.data...),
				sent:         time.Now(),
				model:        event.Model,
				effort:       event.Reasoning.Effort,
				serviceTier:  event.ServiceTier,
				metadata:     metadata,
				counted:      counted,
				resolution:   resolution,
				thread:       turnThread,
				rotationFrom: rotationFrom,
				excluded:     map[string]bool{},
				reauthed:     map[string]bool{},
			})
			continue
		}

		if message.generation != generation {
			continue
		}
		if message.err != nil {
			if len(turns) == 1 && !turns[0].created && !turns[0].visible && !turns[0].resolution.hard {
				current.account.failed(0)
				if replayTurn(&turns[0], "disconnected") {
					continue
				}
			}
			if errors.Is(message.err, context.Canceled) || websocket.CloseStatus(message.err) == websocket.StatusNormalClosure {
				return
			}
			turnID := ""
			rotationFrom := ""
			if len(turns) > 0 {
				turnID = turns[0].metadata.TurnID
				rotationFrom = turns[0].rotationFrom
			}
			s.log.Warn("upstream websocket closed",
				"thread", thread,
				"turn", turnID,
				"account", current.account.id(),
				"active_turns", len(turns),
				"rotation_source", rotationFrom,
				"status", websocket.CloseStatus(message.err),
				"error", message.err,
			)
			return
		}
		var event websocketEnvelope
		if message.kind == websocket.MessageText && json.Unmarshal(message.data, &event) == nil {
			headers := websocketEventHeaders(event.Headers)
			if len(headers) > 0 {
				current.account.observe(headers)
			}
			if len(turns) > 0 && websocketResponseVisible(event) {
				turns[0].visible = true
			}
			previsible := len(turns) == 1 && !turns[0].created && !turns[0].visible
			identitySafe := websocketReplaySafe(event)
			if websocketStatus(event) == http.StatusUnauthorized {
				if previsible && identitySafe {
					turn := &turns[0]
					id := current.account.id()
					if !turn.reauthed[id] {
						turn.reauthed[id] = true
						if s.refreshed(current.account, id) {
							resolution := turn.resolution
							resolution.required = id
							if redialTurn(turn, resolution, nil, "") {
								continue
							}
						}
					}
					current.account.failed(0)
					if !turn.resolution.hard && replayTurn(turn, "unauthorized") {
						continue
					}
				} else {
					current.account.failed(0)
				}
			}
			retryable := false
			retryReason := ""
			rateLimited := websocketRateLimited(event)
			if rateLimited {
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
			} else if len(turns) == 1 && !turns[0].modelRetried && websocketAccountModelUnsupported(event, turns[0].model) {
				retryable = true
				retryReason = "model unsupported"
			}
			if retryable && previsible && !turns[0].resolution.hard && (identitySafe || rateLimited) {
				if retryReason == "model unsupported" {
					turns[0].modelRetried = true
				}
				if replayTurn(&turns[0], retryReason) {
					continue
				}
			}
			if previsible && turns[0].rotationFrom != "" && websocketInvalidEncryptedContent(event) {
				turn := &turns[0]
				s.log.Warn("compaction rotation context rejected",
					"thread", turn.thread,
					"turn", turn.metadata.TurnID,
					"source_account", turn.rotationFrom,
					"target_account", current.account.id(),
					"status", websocketStatus(event),
					"code", websocketErrorCode(event),
				)
				resolution := turn.resolution
				resolution.required = turn.rotationFrom
				resolution.preferred = ""
				if redialTurn(turn, resolution, nil, "encrypted content rejected") {
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
					bindings := turns[index].resolution.bindings
					if turnState := turnStateAffinity(headers); turnState.valid() {
						bindings = append(bindings, turnState)
					}
					if err := s.affinity.bindAll(bindings, current.account.id()); err != nil {
						s.log.Warn("affinity save failed", "thread", turns[index].thread, "account", current.account.id(), "error", err)
					}
					if event.Response.ID != "" {
						if err := s.affinity.bind(affinityRef{kind: affinityResponse, value: event.Response.ID}, current.account.id()); err != nil {
							s.log.Warn("response affinity save failed", "response", event.Response.ID, "account", current.account.id(), "error", err)
						}
					}
					if turns[index].counted {
						s.stats.routed(turns[index].thread, requestClientID(r, s.clientIDKey), current.account.id(), turns[index].model, turns[index].effort, turns[index].serviceTier, transportWebSocket, turns[index].metadata)
						s.stats.answered(turns[index].thread, time.Since(turns[index].sent))
					}
					s.log.Debug("websocket response created",
						"thread", turns[index].thread,
						"turn", turns[index].metadata.TurnID,
						"request_kind", turns[index].metadata.RequestKind,
						"account", current.account.id(),
						"rotation_source", turns[index].rotationFrom,
						"latency", time.Since(turns[index].sent),
					)
					if turns[index].rotationFrom != "" {
						outcome := "source_fallback"
						if current.account.id() != turns[index].rotationFrom {
							s.stats.note("rotated", current.account.id(), "after compaction")
							outcome = "rotated"
						}
						s.compactionRotation.finish(turns[index].thread, outcome, current.account.id())
					}
					break
				}
			case "error", "response.completed", "response.failed", "response.incomplete":
				if len(turns) > 0 {
					turn := turns[0]
					if event.Type != "response.completed" {
						s.log.Warn("websocket turn ended without completion",
							"event", event.Type,
							"thread", turn.thread,
							"turn", turn.metadata.TurnID,
							"request_kind", turn.metadata.RequestKind,
							"account", current.account.id(),
							"rotation_source", turn.rotationFrom,
							"created", turn.created,
							"visible", turn.visible,
							"status", websocketStatus(event),
							"code", websocketErrorCode(event),
						)
					} else {
						s.log.Debug("websocket turn completed",
							"thread", turn.thread,
							"turn", turn.metadata.TurnID,
							"request_kind", turn.metadata.RequestKind,
							"account", current.account.id(),
							"rotation_source", turn.rotationFrom,
							"duration", time.Since(turn.sent),
						)
					}
					if turn.counted && event.Type != "error" {
						model := event.Response.Model
						if model == "" {
							model = turn.model
						}
						serviceTier := event.Response.ServiceTier
						if serviceTier == "" {
							serviceTier = turn.serviceTier
						}
						s.stats.recordUsage(turn.thread, model, serviceTier, event.Response.Usage)
						if event.Type == "response.completed" {
							s.stats.completed(turn.thread, turn.metadata, time.Since(turn.sent))
							s.compactionRotation.arm(turn.thread, current.account.id(), turn.metadata)
						}
					}
					if turn.rotationFrom != "" {
						s.compactionRotation.finish(turn.thread, "terminal_before_acceptance", current.account.id())
					}
					turns = turns[1:]
				}
			}
		}
		if err := downstream.Write(ctx, message.kind, message.data); err != nil {
			s.log.Warn("downstream websocket response write failed",
				"thread", thread,
				"account", current.account.id(),
				"active_turns", len(turns),
				"error", err,
			)
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

func websocketErrorMessage(event websocketEnvelope) string {
	if event.Error.Message != "" {
		return event.Error.Message
	}
	return event.Response.Error.Message
}

func websocketAccountModelUnsupported(event websocketEnvelope, model string) bool {
	return accountModelUnsupported(websocketErrorCode(event), websocketErrorMessage(event), model)
}

func websocketRateLimited(event websocketEnvelope) bool {
	code := websocketErrorCode(event)
	return websocketStatus(event) == http.StatusTooManyRequests || code == "rate_limit_exceeded" || code == "usage_limit_reached"
}

func websocketInvalidEncryptedContent(event websocketEnvelope) bool {
	return websocketErrorCode(event) == "invalid_encrypted_content"
}

func websocketReplaySafe(event websocketEnvelope) bool {
	return event.ID == "" && event.ResponseID == "" && event.Response.ID == ""
}

func websocketResponseVisible(event websocketEnvelope) bool {
	return strings.HasPrefix(event.Type, "response.") && event.Type != "response.failed" && event.Type != "response.incomplete"
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
