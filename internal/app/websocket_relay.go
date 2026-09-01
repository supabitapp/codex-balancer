package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

type responsesWebSocketRelay struct {
	server        *server
	downstream    *websocket.Conn
	request       *http.Request
	apiKey        apiKeyIdentity
	route         websocketRoute
	thread        string
	ctx           context.Context
	cancel        context.CancelFunc
	messages      chan websocketMessage
	invalidations chan websocketInvalidation
	liveThreads   map[string]struct{}
	current       *websocketDial
	turns         []websocketTurn
	pending       []websocketMessage
	pendingBytes  int
	pinned        bool
	socketID      uint64
}

type websocketInvalidation struct {
	account string
	reason  string
}

func newResponsesWebSocketRelay(s *server, downstream *websocket.Conn, request *http.Request, initial *websocketDial, route websocketRoute, apiKey apiKeyIdentity) *responsesWebSocketRelay {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	return &responsesWebSocketRelay{
		server:        s,
		downstream:    downstream,
		request:       request,
		apiKey:        apiKey,
		route:         route,
		thread:        route.key(),
		ctx:           ctx,
		cancel:        cancel,
		messages:      make(chan websocketMessage, 8),
		invalidations: make(chan websocketInvalidation, 4),
		liveThreads:   map[string]struct{}{},
		current:       initial,
	}
}

func (r *responsesWebSocketRelay) run() {
	readWebSocketMessages(r.ctx, r.downstream, true, r.messages)
	r.socketID = r.registerActiveSocket(r.current.account.id())
	r.server.websocketOpened(r.thread, r.current.account)
	defer r.close()
	if !r.server.accountRoutable(r.current.account.id()) || !r.current.claim.active() {
		r.closeDownstream(websocket.StatusServiceRestart, "account became unavailable during connection setup")
		return
	}
	for {
		select {
		case message := <-r.messages:
			if message.downstream {
				if !r.handleDownstream(message) {
					return
				}
			} else if !r.handleUpstream(message) {
				return
			}
		case invalidation := <-r.invalidations:
			if invalidation.account != r.current.account.id() {
				continue
			}
			r.closeDownstream(websocket.StatusServiceRestart, "account unavailable: "+invalidation.reason)
			return
		case <-r.ctx.Done():
			return
		}
	}
}

func (r *responsesWebSocketRelay) registerActiveSocket(account string) uint64 {
	return r.server.activeWebSockets.add(account, func(account, reason string) {
		select {
		case r.invalidations <- websocketInvalidation{account: account, reason: reason}:
		case <-r.ctx.Done():
		}
	})
}

func (r *responsesWebSocketRelay) close() {
	r.cancel()
	r.current.conn.CloseNow()
	r.current.releaseClaim()
	r.server.activeWebSockets.remove(r.socketID, r.current.account.id())
	r.server.websocketClosed(r.thread, r.current.account)
	for thread := range r.liveThreads {
		r.server.stats.deactivateThread(thread)
	}
}

func (r *responsesWebSocketRelay) closeDownstream(status websocket.StatusCode, reason string) {
	if err := r.downstream.Close(status, reason); err != nil {
		r.server.log.Debug("downstream websocket close failed", "thread", r.thread, "status", status, "error", err)
	}
}

func (r *responsesWebSocketRelay) switchAccount(next *websocketDial, model, serviceTier string) bool {
	previous := r.current
	if previous.claim != nil {
		transferred := r.server.transferWebSocketClaim(previous.claim, next.account.id(), next.priorOwner, next.routingReason)
		if transferred == nil {
			next.conn.CloseNow()
			next.releaseClaim()
			r.closeDownstream(websocket.StatusServiceRestart, "account became unavailable during account switch")
			return false
		}
		previous.claim = nil
		next.claim = transferred
	}
	previous.conn.CloseNow()
	r.server.websocketClosed(r.thread, previous.account)
	r.current = next
	if !r.server.activeWebSockets.move(r.socketID, previous.account.id(), r.current.account.id()) {
		r.socketID = r.registerActiveSocket(r.current.account.id())
	}
	r.current.conn.SetReadLimit(maxWebSocketMessage)
	r.server.websocketOpened(r.thread, r.current.account)
	if !r.server.accountRoutable(r.current.account.id()) || !r.current.claim.active() {
		r.closeDownstream(websocket.StatusServiceRestart, "account became unavailable during connection setup")
		return false
	}
	r.server.log.Info("websocket selected model-compatible account",
		"thread", r.thread,
		"from_account", previous.account.id(),
		"to_account", r.current.account.id(),
		"model", model,
		"service_tier", serviceTier,
	)
	return true
}

func (r *responsesWebSocketRelay) writeUpstream(message websocketMessage) bool {
	if err := r.current.conn.Write(r.ctx, message.kind, message.data); err != nil {
		r.closeDownstream(websocket.StatusServiceRestart, "upstream websocket unavailable")
		return false
	}
	return true
}

func (r *responsesWebSocketRelay) handleDownstream(message websocketMessage) bool {
	if message.err != nil {
		r.logDownstreamClose(message.err)
		return false
	}
	var event websocketEnvelope
	responseCreate := message.kind == websocket.MessageText && json.Unmarshal(message.data, &event) == nil && event.Type == "response.create"
	if !r.pinned && !responseCreate {
		return r.queuePending(message)
	}
	if !responseCreate {
		return r.writeUpstream(message)
	}
	return r.handleResponseCreate(message, event)
}

func (r *responsesWebSocketRelay) logDownstreamClose(err error) {
	level := slog.LevelWarn
	status := websocket.CloseStatus(err)
	if errors.Is(err, context.Canceled) || status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
		level = slog.LevelDebug
	}
	r.server.log.Log(r.ctx, level, "downstream websocket closed", "thread", r.thread, "account", r.current.account.id(), "active_turns", len(r.turns), "status", status, "error", err)
}

func (r *responsesWebSocketRelay) queuePending(message websocketMessage) bool {
	r.pendingBytes += len(message.data)
	if r.pendingBytes > maxWebSocketMessage {
		r.closeDownstream(websocket.StatusMessageTooBig, "messages before first turn are too large")
		return false
	}
	r.pending = append(r.pending, message)
	return true
}

func (r *responsesWebSocketRelay) handleResponseCreate(message websocketMessage, event websocketEnvelope) bool {
	if !r.current.claim.active() {
		r.closeDownstream(websocket.StatusServiceRestart, "route owner changed before turn")
		return false
	}
	if r.pinned && r.current.account.routingCandidate().spent && websocketRequestPortable(event) {
		r.closeDownstream(websocket.StatusServiceRestart, "account exhausted before a new turn")
		return false
	}
	allowed := r.server.allowedAccounts(event.Model, event.ServiceTier)
	if (r.current.moved || !accountAllowed(allowed, r.current.account.id())) && !websocketRequestPortable(event) {
		r.closeDownstream(websocket.StatusTryAgainLater, "account-bound turn cannot move accounts")
		return false
	}
	if !r.ensureCompatibleAccount(event, allowed) {
		return false
	}
	if !r.pinned && !r.pin() {
		return false
	}
	if !r.writeUpstream(message) {
		return false
	}
	r.startTurn(event)
	return true
}

func (r *responsesWebSocketRelay) ensureCompatibleAccount(event websocketEnvelope, allowed map[string]bool) bool {
	if accountAllowed(allowed, r.current.account.id()) {
		return true
	}
	if r.pinned {
		r.closeDownstream(websocket.StatusServiceRestart, "requested model requires another account")
		return false
	}
	var next *websocketDial
	var failed *http.Response
	var err error
	if r.current.claim != nil {
		next, failed, err = r.server.dialResponsesWebSocketReplacing(r.request, r.route, event.Model, event.ServiceTier, r.current.claim)
	} else {
		next, failed, err = r.server.dialResponsesWebSocket(r.request, r.route, event.Model, event.ServiceTier)
	}
	closeWebSocketResponse(failed)
	if err != nil || failed != nil {
		r.server.log.Warn("model-compatible websocket unavailable", "thread", r.thread, "model", event.Model, "service_tier", event.ServiceTier, "error", err)
		r.closeDownstream(websocket.StatusTryAgainLater, "no account supports requested model")
		return false
	}
	return r.switchAccount(next, event.Model, event.ServiceTier)
}

func (r *responsesWebSocketRelay) pin() bool {
	r.pinned = true
	readWebSocketMessages(r.ctx, r.current.conn, false, r.messages)
	for _, queued := range r.pending {
		if !r.writeUpstream(queued) {
			return false
		}
	}
	r.pending = nil
	return true
}

func (r *responsesWebSocketRelay) startTurn(event websocketEnvelope) {
	metadata := requestTurnMetadata("", event.ClientMetadata)
	statsThread := statsThreadKey(r.thread, metadata)
	counted := event.Generate == nil || *event.Generate
	r.turns = append(r.turns, websocketTurn{
		sent:        time.Now(),
		model:       event.Model,
		effort:      event.Reasoning.Effort,
		serviceTier: event.ServiceTier,
		metadata:    metadata,
		counted:     counted,
		statsThread: statsThread,
	})
	attrs := []any{"thread", statsThread, "service_tier", event.ServiceTier}
	attrs = append(attrs, routingLogAttrs(r.current.account.routingCandidate(), time.Now())...)
	r.server.log.Debug("websocket turn received", attrs...)
}

func (r *responsesWebSocketRelay) handleUpstream(message websocketMessage) bool {
	if message.err != nil {
		r.closeDownstream(websocket.StatusServiceRestart, "upstream websocket unavailable")
		return false
	}
	var event websocketEnvelope
	rejection := websocketRejectionNone
	parsed := message.kind == websocket.MessageText && json.Unmarshal(message.data, &event) == nil
	if parsed && websocketRejection(event) == websocketRejectionUnauthorized {
		r.handleInBandUnauthorized()
		r.closeDownstream(websocket.StatusServiceRestart, "account rejected websocket request")
		return false
	}
	if parsed {
		var allowed bool
		rejection, allowed = r.handleUpstreamEvent(event)
		if !allowed {
			return false
		}
		if rejection == websocketRejectionModelCapacity {
			r.server.preserveWebSocketRetryOwner(r.current)
			r.closeDownstream(websocket.StatusServiceRestart, "model at capacity")
			return false
		}
	}
	if err := r.downstream.Write(r.ctx, message.kind, message.data); err != nil {
		r.server.log.Warn("downstream websocket response write failed", "thread", r.thread, "account", r.current.account.id(), "active_turns", len(r.turns), "error", err)
		return false
	}
	return r.afterUpstreamEvent(rejection)
}

func (r *responsesWebSocketRelay) handleInBandUnauthorized() {
	account := r.current.account
	if !account.markRejectedAccessToken(r.current.accessToken) {
		return
	}
	if !r.server.refreshed(account, account.id()) && !account.needsReauth() {
		account.clearRejectedAccessToken(r.current.accessToken)
	}
}

func (r *responsesWebSocketRelay) handleUpstreamEvent(event websocketEnvelope) (websocketRejectionKind, bool) {
	headers := websocketEventHeaders(event.Headers)
	if len(headers) > 0 {
		r.current.account.observe(headers)
	}
	rejection := websocketRejection(event)
	if rejection != websocketRejectionNone {
		r.server.handleWebSocketRejection(r.current.account, rejection, headers, r.thread)
	}
	switch event.Type {
	case "response.created":
		if !r.responseCreated() {
			return rejection, false
		}
	case "error", "response.completed", "response.failed", "response.incomplete":
		r.responseFinished(event)
	}
	return rejection, true
}

func (r *responsesWebSocketRelay) afterUpstreamEvent(rejection websocketRejectionKind) bool {
	switch rejection {
	case websocketRejectionNone, websocketRejectionConnectionLimit:
		return true
	}
	r.closeDownstream(websocket.StatusServiceRestart, "account rejected websocket request")
	return false
}

func (r *responsesWebSocketRelay) responseCreated() bool {
	for index := range r.turns {
		if r.turns[index].created {
			continue
		}
		turn := &r.turns[index]
		routeThread := r.route.thread
		if routeThread == "" {
			routeThread = turn.statsThread
		}
		acceptedAt := time.Now()
		acceptance := r.server.acceptWebSocketRoute(r.current, storedRoute{At: acceptedAt, Session: r.route.session, Thread: routeThread, Account: r.current.account.id()})
		if !acceptance.allowed {
			r.closeDownstream(websocket.StatusServiceRestart, "route owner became unavailable")
			return false
		}
		r.turns[index].created = true
		if turn.counted {
			if _, live := r.liveThreads[turn.statsThread]; !live {
				r.server.stats.activateThread(turn.statsThread)
				r.liveThreads[turn.statsThread] = struct{}{}
			}
		}
		r.server.stats.recordAccepted(acceptedAt, turn.statsThread, requestIP(r.request), r.apiKey.suffix, r.current.account.id(), turn.model, turn.effort, turn.serviceTier, turn.metadata, turn.counted)
		if acceptance.logSwitch {
			r.server.log.Info("websocket account switch accepted",
				"thread", turn.statsThread,
				"from_account", r.current.priorOwner,
				"to_account", r.current.account.id(),
				"routing_reason", r.current.routingReason,
				"route_persisted", acceptance.persisted,
			)
		}
		r.current.moved = false
		if turn.counted {
			r.server.stats.answered(turn.statsThread, r.current.account.id(), time.Since(turn.sent))
		}
		r.server.log.Debug("websocket response created", "thread", turn.statsThread, "turn", turn.metadata.TurnID, "account", r.current.account.id(), "latency", time.Since(turn.sent))
		return true
	}
	return true
}

func (r *responsesWebSocketRelay) responseFinished(event websocketEnvelope) {
	if len(r.turns) == 0 {
		return
	}
	turn := r.turns[0]
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
			logResponseUsage(r.server.log, turn.statsThread, r.current.account.id(), model, serviceTier, turn.metadata, time.Since(turn.sent), event.Response.Usage)
		}
		r.server.stats.recordAPIKeyUsage(r.apiKey.name, turn.statsThread, r.current.account.id(), model, turn.effort, serviceTier, event.Response.Usage)
		if event.Type == "response.completed" {
			r.server.stats.completed(turn.statsThread, r.current.account.id(), turn.metadata, time.Since(turn.sent))
		}
	}
	r.turns = r.turns[1:]
}
