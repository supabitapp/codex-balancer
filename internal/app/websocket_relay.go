package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type responsesWebSocketRelay struct {
	*responseAccounting
	downstream            responsesDownstream
	cancel                context.CancelFunc
	messages              chan websocketMessage
	readers               []<-chan struct{}
	invalidations         chan websocketInvalidation
	current               *websocketDial
	pending               []websocketMessage
	pendingBytes          int
	pinned                bool
	socketID              uint64
	fastMode              fastMode
	policyChanged         <-chan struct{}
	idleTimeout           time.Duration
	messageLimit          int64
	generationSpan        trace.Span
	upstreamOpened        time.Time
	lastUpstreamEvent     time.Time
	lastUpstreamKind      string
	receivedUpstreamBytes int64
}

type websocketInvalidation struct {
	account string
	reason  string
}

func newResponsesWebSocketRelay(s *server, downstream responsesDownstream, request *http.Request, initial *websocketDial, route websocketRoute, apiKey apiKeyIdentity, mode fastMode, changed <-chan struct{}) *responsesWebSocketRelay {
	ctx, cancel := context.WithCancel(request.Context())
	if s.ctx != nil {
		stop := context.AfterFunc(s.ctx, cancel)
		cancelContext := cancel
		cancel = func() { stop(); cancelContext() }
	}
	return &responsesWebSocketRelay{
		responseAccounting: &responseAccounting{server: s, request: request, apiKey: apiKey, via: transportWebSocket, route: route, thread: route.key(), ctx: ctx, liveThreads: map[string]struct{}{}, account: initial.responseAccount},
		messageLimit:       maxWebSocketMessage,
		fastMode:           mode,
		policyChanged:      changed,
		downstream:         downstream,
		cancel:             cancel,
		messages:           make(chan websocketMessage, 8),
		invalidations:      make(chan websocketInvalidation, 4),
		current:            initial,
		upstreamOpened:     time.Now(),
	}
}

func (r *responsesWebSocketRelay) run() {
	r.readers = append(r.readers, readWebSocketMessages(r.ctx, r.downstream, true, r.messages))
	r.socketID = r.registerActiveSocket(r.current.account.id())
	r.server.websocketOpened(r.thread, r.current.account)
	defer r.close()
	if !r.server.accountRoutable(r.current.account.id()) || !r.current.claim.active() {
		r.closeDownstream(websocket.StatusServiceRestart, "account became unavailable during connection setup")
		return
	}
	var idle <-chan time.Time
	var timer *time.Timer
	if r.idleTimeout > 0 {
		timer = time.NewTimer(r.idleTimeout)
		defer timer.Stop()
		idle = timer.C
	}
	for {
		select {
		case <-idle:
			r.closeDownstream(websocket.StatusServiceRestart, "upstream response timed out")
			return
		case <-r.policyChanged:
			r.restartForFastMode()
			return
		case message := <-r.messages:
			if timer != nil {
				timer.Reset(r.idleTimeout)
			}
			if r.fastModeChanged() {
				return
			}
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
			r.closeDownstream(websocket.StatusServiceRestart, "request canceled or server shutting down")
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
	for _, done := range r.readers {
		<-done
	}
	r.current.releaseClaim()
	r.server.activeWebSockets.remove(r.socketID, r.current.account.id())
	r.server.websocketClosed(r.thread, r.current.account)
	for thread := range r.liveThreads {
		r.server.stats.deactivateThread(thread)
	}
	if observed := observation(r.ctx); observed != nil {
		observed.cleaned, observed.account = true, r.current.account.id()
		observed.event(r.ctx, "cleanup", attribute.String("account", observed.account), attribute.Int("pending_turns", len(r.turns)), attribute.Int("readers_joined", len(r.readers)), attribute.Bool("claim_released", true), attribute.Bool("active_registry_removed", true))
		if r.generationSpan != nil {
			if observed.outcome != "completed" && observed.outcome != "incomplete" {
				r.generationSpan.SetStatus(codes.Error, observed.outcome)
			}
			r.generationSpan.End()
		}
	}
}

func (r *responsesWebSocketRelay) closeDownstream(status websocket.StatusCode, reason string) {
	observation(r.ctx).event(r.ctx, "relay_close", attribute.Int("websocket_close_status", int(status)), attribute.String("reason", reason), attribute.String("retry_owner", "client"), attribute.Bool("inference_replayed", false))
	if err := r.downstream.Close(status, reason); err != nil {
		r.server.log.Debug("downstream websocket close failed", "thread", r.thread, "status", status, "error", err)
	}
}

func (r *responsesWebSocketRelay) refuseTurn(status websocket.StatusCode, failure httpResponseFailure) {
	observation(r.ctx).event(r.ctx, "relay_close", attribute.Int("websocket_close_status", int(status)), attribute.String("reason", failure.Code), attribute.String("retry_owner", "client"), attribute.Bool("inference_replayed", false))
	if err := r.downstream.requestFailed(r.ctx, failure, status); err != nil && !errors.Is(err, errResponseFinished) {
		r.server.log.Debug("downstream request refusal failed", "thread", r.thread, "status", status, "error", err)
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
	r.account = next.responseAccount
	r.upstreamOpened = time.Now()
	r.lastUpstreamEvent = time.Time{}
	r.lastUpstreamKind = ""
	r.receivedUpstreamBytes = 0
	if !r.server.activeWebSockets.move(r.socketID, previous.account.id(), r.current.account.id()) {
		r.socketID = r.registerActiveSocket(r.current.account.id())
	}
	r.current.conn.SetReadLimit(r.messageLimit)
	r.server.websocketOpened(r.thread, r.current.account)
	if !r.server.accountRoutable(r.current.account.id()) || !r.current.claim.active() {
		r.closeDownstream(websocket.StatusServiceRestart, "account became unavailable during connection setup")
		return false
	}
	if observed := observation(r.ctx); observed != nil {
		observed.account = r.current.account.id()
		observed.event(r.ctx, "account_switch_ready", attribute.String("from_account", previous.account.id()), attribute.String("account", observed.account), attribute.String("phase", "before_transmission"), attribute.Bool("write_attempted", observed.writeAttempted), attribute.Bool("response_created", false))
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
	ctx, cancel := context.WithTimeout(r.ctx, upstreamWait)
	defer cancel()
	observed := observation(r.ctx)
	if observed != nil {
		observed.writeAttempted = true
		observed.writeAt = time.Now()
	}
	observed.event(r.ctx, "upstream_write_started", attribute.String("account", r.current.account.id()), attribute.Int("bytes", len(message.data)), attribute.String("replay_owner_after_write_attempt", "client"))
	started := time.Now()
	if err := r.current.conn.Write(ctx, message.kind, message.data); err != nil {
		err = r.upstreamWriteFailure(err)
		r.logUpstreamFailure(err, "write", len(message.data))
		observed.event(r.ctx, "upstream_write_failed", attribute.String("error_type", telemetryErrorClass(err)), attribute.Bool("possibly_transmitted", true))
		r.downstream.upstreamFailed(r.ctx, err)
		return false
	}
	if observed != nil {
		observed.writeSucceeded = true
	}
	observed.event(r.ctx, "upstream_write_finished", attribute.Int64("elapsed_ms", time.Since(started).Milliseconds()), attribute.Bool("success", true))
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
	if r.fastModeChanged() {
		return false
	}
	var err error
	message.data, event.ServiceTier, err = r.fastMode.override(message.data, event.ServiceTier)
	if err != nil {
		r.closeDownstream(websocket.StatusInternalError, "could not apply fast mode")
		return false
	}

	if !r.current.claim.active() {
		r.closeDownstream(websocket.StatusServiceRestart, "route owner changed before turn")
		return false
	}
	candidate := r.current.account.routingCandidate()
	if r.pinned && (candidate.spent || candidate.weeklyExhausted()) && websocketRequestPortable(event) {
		decision := r.server.pickAccount(r.thread, []string{candidate.id}, event.Model, event.ServiceTier, nil, 0)
		if decision.account != r.current.account || !decision.creditFallback {
			r.closeDownstream(websocket.StatusServiceRestart, "account exhausted before a new turn")
			return false
		}
	}
	allowed := r.server.allowedAccounts(event.Model, event.ServiceTier)
	observation(r.ctx).event(r.ctx, "turn_preflight", attribute.String("account", r.current.account.id()), attribute.Bool("pinned", r.pinned), attribute.Bool("account_move", r.current.moved), attribute.Bool("model_tier_allowed", accountAllowed(allowed, r.current.account.id())), attribute.Bool("catalog_filter_active", allowed != nil), attribute.Bool("portable_frame", websocketRequestPortable(event)), attribute.Bool("turn_state_header_present", strings.TrimSpace(r.request.Header.Get(codexTurnStateKey)) != ""))
	if (r.current.moved || !accountAllowed(allowed, r.current.account.id())) && !websocketRequestPortable(event) {
		r.refuseTurn(websocket.StatusTryAgainLater, httpResponseFailure{Status: 400, Code: "account_bound_request", Type: "invalid_request_error", Message: errAccountBoundTurn.Error()})
		return false
	}
	if !r.ensureCompatibleAccount(event, allowed) {
		return false
	}
	if !r.pinned && !r.pin() {
		return false
	}
	// First-turn model selection may have waited for another handshake.
	if r.fastModeChanged() {
		return false
	}
	if observed := observation(r.ctx); observed != nil && r.generationSpan == nil {
		r.ctx, r.generationSpan = observed.start(r.ctx, "codex.response", attribute.String("account", r.current.account.id()), attribute.String("model", event.Model), attribute.String("effective_tier", event.ServiceTier))
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
	preflight, span := observation(r.ctx).start(r.request.Context(), "codex.model_preflight", attribute.String("from_account", r.current.account.id()), attribute.Bool("portable_frame", websocketRequestPortable(event)))
	defer span.End()
	request := r.request.WithContext(preflight)
	var next *websocketDial
	var failed *http.Response
	var err error
	if r.current.claim != nil {
		next, failed, err = r.server.dialResponsesWebSocketReplacing(request, r.route, event.Model, event.ServiceTier, r.current.claim)
	} else {
		next, failed, err = r.server.dialResponsesWebSocket(request, r.route, event.Model, event.ServiceTier)
	}
	if err != nil || failed != nil {
		span.SetStatus(codes.Error, "model_preflight_failed")
		defer closeWebSocketResponse(failed)
		r.server.log.Warn("model-compatible websocket unavailable", "thread", r.thread, "model", event.Model, "service_tier", event.ServiceTier, "error", err)
		r.downstream.setupFailed(r.ctx, failed, err)
		return false
	}
	return r.switchAccount(next, event.Model, event.ServiceTier)
}

func (r *responsesWebSocketRelay) pin() bool {
	r.pinned = true
	r.readers = append(r.readers, readWebSocketMessages(r.ctx, r.current.conn, false, r.messages))
	for _, queued := range r.pending {
		if !r.writeUpstream(queued) {
			return false
		}
	}
	r.pending = nil
	return true
}

func (r *responsesWebSocketRelay) handleUpstream(message websocketMessage) bool {
	if message.err != nil {
		r.logUpstreamFailure(message.err, "read", len(message.data))
		observation(r.ctx).event(r.ctx, "upstream_closed", attribute.String("error_type", telemetryErrorClass(message.err)), attribute.Int("close_status", int(websocket.CloseStatus(message.err))))
		r.downstream.upstreamFailed(r.ctx, message.err)
		return false
	}
	r.lastUpstreamEvent = time.Now()
	r.receivedUpstreamBytes += int64(len(message.data))
	r.lastUpstreamKind = "invalid_frame"
	var err error
	message, err = r.downstream.prepare(message)
	if err != nil {
		r.closeDownstream(websocket.StatusInternalError, "invalid upstream response")
		return false
	}
	var event websocketEnvelope
	rejection := websocketRejectionNone
	parsed := message.kind == websocket.MessageText && json.Unmarshal(message.data, &event) == nil
	if parsed {
		r.lastUpstreamKind = websocketDiagnosticEvent(event.Type)
		message.data = r.server.pool.clientUsageEvent(r.current.account, message.data, event)
	}
	if parsed && websocketRejection(event) == websocketRejectionUnauthorized {
		r.handleInBandUnauthorized()
		r.downstream.reject(r.ctx, message, websocket.StatusServiceRestart, "account rejected websocket request")
		return false
	}
	if parsed {
		rejected := websocketRejection(event)
		retryUsage := rejected == websocketRejectionUsageLimit && r.canRetryUsageLimit()
		bound := rejected == websocketRejectionRateLimited && r.hasTurnState()
		retryRateLimit := rejected == websocketRejectionRateLimited && event.Type == "error" && websocketErrorIs(event, "rate_limit_exceeded") && (!bound || r.route.key() != "")
		if rejected != websocketRejectionNone {
			observation(r.ctx).event(r.ctx, "reconnect_decision", attribute.String("rejection", string(rejected)), attribute.Bool("usage_reconnect_signal", retryUsage), attribute.Int("pending_turns", len(r.turns)), attribute.Bool("accepted_before_rejection", len(r.turns) > 0 && r.turns[0].created), attribute.Bool("turn_state_metadata_present", len(r.turns) > 0 && strings.TrimSpace(r.turns[0].turnState) != ""), attribute.Bool("turn_state_header_present", strings.TrimSpace(r.request.Header.Get(codexTurnStateKey)) != ""), attribute.Bool("upstream_turn_state_present", r.current.resp != nil && strings.TrimSpace(r.current.resp.Header.Get(codexTurnStateKey)) != ""), attribute.String("retry_owner", "client"), attribute.Bool("inference_replayed", false))
		}
		var allowed bool
		rejection, allowed = r.handleUpstreamEvent(event)
		if !allowed {
			return false
		}
		if rejection == websocketRejectionRateLimited && bound {
			r.server.preserveResponseOwner(r.current.responseAccount)
		}
		if retryUsage {
			r.server.preserveResponseOwner(r.current.responseAccount)
			r.downstream.reject(r.ctx, message, websocket.StatusServiceRestart, "account exhausted; reconnect with full history")
			return false
		}
		if retryRateLimit {
			r.downstream.reject(r.ctx, message, websocket.StatusServiceRestart, "account temporarily rate limited")
			return false
		}
		if rejection == websocketRejectionModelCapacity {
			r.server.preserveResponseOwner(r.current.responseAccount)
			r.downstream.reject(r.ctx, message, websocket.StatusServiceRestart, "model at capacity")
			return false
		}
	}
	if err := r.downstream.Write(r.ctx, message.kind, message.data); err != nil {
		if errors.Is(err, errResponseFinished) {
			return false
		}
		r.server.log.Warn("downstream websocket response write failed", "thread", r.thread, "account", r.current.account.id(), "active_turns", len(r.turns), "error", err)
		return false
	}
	return r.afterUpstreamEvent(rejection)
}

// A reconnect drops Codex's socket-scoped response ID, but not its turn-state
// token. Only retry a single unaccepted request with no account-bound token.
// The replacement relay still requires a full replay before forwarding it.
func (r *responsesWebSocketRelay) canRetryUsageLimit() bool {
	if r.route.key() == "" || len(r.turns) != 1 || r.turns[0].created {
		return false
	}
	if r.hasTurnState() {
		return false
	}
	turn := r.turns[0]
	allowed := r.server.allowedAccounts(turn.model, turn.serviceTier)
	now := time.Now()
	for _, account := range r.server.pool.all() {
		candidate := account.routingCandidate()
		if candidate.id != r.current.account.id() && candidate.available(now) && accountAllowed(allowed, candidate.id) {
			return true
		}
	}
	return false
}

func (r *responsesWebSocketRelay) hasTurnState() bool {
	if strings.TrimSpace(r.request.Header.Get(codexTurnStateKey)) != "" ||
		r.current.resp != nil && strings.TrimSpace(r.current.resp.Header.Get(codexTurnStateKey)) != "" {
		return true
	}
	for _, turn := range r.turns {
		if strings.TrimSpace(turn.turnState) != "" {
			return true
		}
	}
	return false
}

func (r *responsesWebSocketRelay) handleInBandUnauthorized() {
	account := r.current.account
	if !account.markRejectedAccessToken(r.current.accessToken) {
		return
	}
	if !r.server.refreshedContext(r.ctx, account, account.id()) && !account.needsReauth() {
		account.clearRejectedAccessToken(r.current.accessToken)
	}
}

func (r *responsesWebSocketRelay) handleUpstreamEvent(event websocketEnvelope) (websocketRejectionKind, bool) {
	headers := websocketEventHeaders(event.Headers)
	rejection := websocketRejection(event)
	if rejection != websocketRejectionNone {
		r.server.handleWebSocketRejection(r.current.account, rejection, headers, r.thread)
	}
	switch event.Type {
	case "response.created":
		if !r.responseCreated() {
			r.closeDownstream(websocket.StatusServiceRestart, "route owner became unavailable")
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

func (r *responsesWebSocketRelay) restartForFastMode() {
	r.server.preserveResponseOwner(r.current.responseAccount)
	observation(r.ctx).event(r.ctx, "policy_changed", attribute.String("account", r.current.account.id()), attribute.Bool("owner_preserved", true), attribute.String("retry_owner", "client"))
	r.closeDownstream(websocket.StatusServiceRestart, "fast mode changed; reconnect")
}

func (r *responsesWebSocketRelay) fastModeChanged() bool {
	select {
	case <-r.policyChanged:
		r.restartForFastMode()
		return true
	default:
		return false
	}
}
