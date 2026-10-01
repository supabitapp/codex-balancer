package app

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type responsesWebSocketDialer struct {
	*responseAccountRouter
	upstream      string
	lastRejection *websocketSetupError
	replacing     *routeClaimHandle
}

// Keep a structured setup rejection available to HTTP without changing the
// WebSocket router's error identity or its safe pre-generation retry policy.
type websocketSetupError struct {
	cause      error
	status     int
	details    responseErrorPayload
	retryAfter string
}

func (e *websocketSetupError) Error() string { return e.cause.Error() }
func (e *websocketSetupError) Unwrap() error { return e.cause }

func (d *responsesWebSocketDialer) unavailable(cause error) error {
	if d.lastRejection != nil {
		d.lastRejection.cause = cause
		return d.lastRejection
	}
	return cause
}

type upstreamWebSocketDial struct {
	conn        *websocket.Conn
	response    *http.Response
	err         error
	sent        time.Time
	accessToken authorizationRevision
}

func newResponsesWebSocketDialer(s *server, request *http.Request, route websocketRoute, model, serviceTier string) (*responsesWebSocketDialer, error) {
	upstream, err := responsesWebSocketURL(s.upstream)
	if err != nil {
		return nil, err
	}
	router, err := newResponseAccountRouter(s, request, route, model, serviceTier)
	if err != nil {
		return nil, err
	}
	return &responsesWebSocketDialer{responseAccountRouter: router, upstream: upstream}, nil
}

func (d *responsesWebSocketDialer) dial() (dial *websocketDial, failed *http.Response, err error) {
	observed := observation(d.request.Context())
	ctx, routeSpan := observed.start(d.request.Context(), "codex.route", attribute.String("model", d.model), attribute.String("effective_tier", d.serviceTier), attribute.Bool("replacing_claim", d.replacing != nil))
	d.request = d.request.WithContext(ctx)
	defer func() {
		spanFailure(routeSpan, err)
		if failed != nil {
			routeSpan.SetStatus(codes.Error, "handshake_rejected")
			routeSpan.SetAttributes(attribute.Int("upstream_status", failed.StatusCode))
		}
		routeSpan.End()
	}()
	for attempt := 0; ; attempt++ {
		var selection claimedRoutingDecision
		if d.replacing != nil {
			selection = d.server.claimReplacement(d.replacing, d.route, d.durable, d.model, d.serviceTier, d.skip, attempt)
		} else {
			selection = d.server.claimAccount(d.route, d.durable, d.model, d.serviceTier, d.skip, attempt)
		}
		decision := selection.routingDecision
		observed.selection(ctx, selection, attempt)
		if decision.blocked != "" {
			return nil, nil, d.unavailable(errRouteOwnerUnavailable)
		}
		account := decision.account
		if account == nil {
			return nil, nil, d.unavailable(errNoAccountAvailable)
		}
		if decision.moved() && strings.TrimSpace(d.request.Header.Get(codexTurnStateKey)) != "" {
			observed.event(ctx, "account_move_blocked", attribute.String("reason", "turn_state_header"), attribute.Bool("write_attempted", false))
			selection.claim.release()
			return nil, nil, errAccountBoundTurn
		}
		retained := slices.Contains(d.owners, account.id()) || selection.joined
		if skip, err := d.refreshBeforeDial(account, retained); err != nil {
			selection.claim.release()
			return nil, nil, err
		} else if skip {
			selection.claim.release()
			continue
		}

		result := d.open(account)
		if result.err == nil {
			if d.replacing != nil && !d.replacing.active() {
				result.conn.CloseNow()
				closeWebSocketResponse(result.response)
				selection.claim.release()
				return nil, nil, errRouteOwnerUnavailable
			}
			if !d.server.accountRoutable(account.id()) || !selection.claim.active() {
				result.conn.CloseNow()
				closeWebSocketResponse(result.response)
				selection.claim.release()
				d.skip[account.id()] = true
				continue
			}
			return d.routed(result, selection, account, attempt), nil, nil
		}
		retry, done, response, err := d.handleFailure(result, account, attempt, retained)
		selection.claim.release()
		observed.event(ctx, "setup_attempt_finished", attribute.String("account", account.id()), attribute.Bool("same_attempt_retry", retry), attribute.Bool("finished", done), attribute.String("retry_phase", "setup_only"), attribute.Bool("claim_released", true))
		if done {
			return nil, response, err
		}
		if retry {
			attempt--
		}
	}
}

func (d *responsesWebSocketDialer) open(account *Account) upstreamWebSocketDial {
	result := upstreamWebSocketDial{sent: time.Now()}
	ctx, cancel := context.WithTimeout(d.request.Context(), upstreamWait)
	defer cancel()
	headers, digest := responsesWebSocketHeaders(d.request.Header, account)
	observed := observation(ctx)
	if observed != nil {
		observed.secrets = append(observed.secrets, strings.TrimPrefix(headers.Get("Authorization"), "Bearer "))
		observed.remember(account.persisted())
	}
	ctx, span := observed.start(ctx, "codex.websocket.handshake", attribute.String("account", account.id()))
	defer span.End()
	observed.event(ctx, "handshake_started", attribute.String("account", account.id()))
	result.accessToken = digest
	result.conn, result.response, result.err = websocket.Dial(ctx, d.upstream, &websocket.DialOptions{
		HTTPClient: d.server.client,
		HTTPHeader: headers,
	})
	spanFailure(span, result.err)
	status := 0
	if result.response != nil {
		status = result.response.StatusCode
	}
	observed.event(ctx, "handshake_finished", attribute.String("account", account.id()), attribute.Int("upstream_status", status), attribute.Bool("success", result.err == nil), attribute.String("error_type", telemetryErrorClass(result.err)), attribute.Int64("elapsed_ms", time.Since(result.sent).Milliseconds()))
	return result
}

func (s *server) accountRoutable(id string) bool {
	account := s.pool.find(id)
	if account == nil {
		return false
	}
	candidate := account.routingCandidate()
	return candidate.routingEnabled() && !candidate.paused && candidate.reauth == ""
}

func (d *responsesWebSocketDialer) routed(result upstreamWebSocketDial, selection claimedRoutingDecision, account *Account, attempt int) *websocketDial {
	if result.response != nil {
		account.observe(result.response.Header)
	}
	// Without a thread or session key there is no affinity to protect with a
	// provisional claim. Reserve anonymous sockets at handshake time so bursts
	// still spread across accounts before any response.created arrives.
	if len(routeClaimKeys(d.route)) == 0 {
		account.accepted(time.Now())
	}
	decision := selection.routingDecision
	if observed := observation(d.request.Context()); observed != nil {
		observed.account = account.id()
		observed.event(d.request.Context(), "upstream_ready", attribute.String("account", account.id()), attribute.String("prior_owner", decision.priorOwner), attribute.String("routing_reason", string(decision.reason)), attribute.Bool("account_move", decision.moved()), attribute.Bool("response_created", false), attribute.Bool("upstream_turn_state_present", result.response != nil && strings.TrimSpace(result.response.Header.Get(codexTurnStateKey)) != ""))
	}
	attrs := []any{
		"thread", d.thread,
		"attempt", attempt + 1,
		"prior_owner", decision.priorOwner,
		"account_move", decision.moved(),
		"routing_reason", decision.reason,
		"provisional_claim", selection.claim != nil,
		"claim_joined", selection.joined,
	}
	attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
	d.server.log.Debug("websocket routed", attrs...)
	return &websocketDial{
		conn: result.conn,
		resp: result.response,
		responseAccount: &responseAccount{
			account:       account,
			accessToken:   result.accessToken,
			claim:         selection.claim,
			priorOwner:    decision.priorOwner,
			routingReason: decision.reason,
			moved:         decision.moved(),
		},
	}
}

func (d *responsesWebSocketDialer) handleFailure(result upstreamWebSocketDial, account *Account, attempt int, retained bool) (retry, done bool, response *http.Response, err error) {
	if cause := context.Cause(d.request.Context()); cause != nil {
		closeWebSocketResponse(result.response)
		return false, true, nil, cause
	}
	id := account.id()
	if result.response != nil && result.response.StatusCode == http.StatusUnauthorized && !d.reauthed[id] {
		if err := d.refreshAfterUnauthorized(result.response, account, retained); err != nil {
			return false, true, nil, err
		}
		return true, false, nil, nil
	}
	if result.response == nil {
		d.server.log.Warn("upstream websocket unreachable", "thread", d.thread, "account", id, "attempt", attempt+1, "error", result.err)
		return false, true, nil, result.err
	}
	status := result.response.StatusCode
	if status == http.StatusSwitchingProtocols {
		d.invalidHandshake(result, account, attempt)
		return false, false, nil, nil
	}
	if status >= http.StatusInternalServerError {
		d.server.log.Warn("upstream websocket server failure", "thread", d.thread, "account", id, "attempt", attempt+1, "status", status)
		return false, true, result.response, nil
	}
	usageLimit := (status == http.StatusTooManyRequests || status == http.StatusForbidden) && responseUsageLimitReached(result.response)
	if status != http.StatusTooManyRequests && !usageLimit && status != http.StatusUnauthorized {
		return false, true, result.response, nil
	}
	if d.rejectAccount(result, account, attempt, usageLimit) {
		return true, false, nil, nil
	}
	return false, false, nil, nil
}

func (d *responsesWebSocketDialer) refreshAfterUnauthorized(response *http.Response, account *Account, retained bool) error {
	d.lastRejection = &websocketSetupError{status: response.StatusCode, details: responseError(response), retryAfter: response.Header.Get("Retry-After")}
	closeWebSocketResponse(response)
	id := account.id()
	d.reauthed[id] = true
	if d.server.refreshedContext(d.request.Context(), account, id) {
		return nil
	}
	if retained {
		return d.unavailable(errRouteOwnerUnavailable)
	}
	d.skip[id] = true
	return nil
}

func (d *responsesWebSocketDialer) invalidHandshake(result upstreamWebSocketDial, account *Account, attempt int) {
	closeWebSocketResponse(result.response)
	id := account.id()
	d.server.log.Warn("upstream websocket handshake invalid", "thread", d.thread, "account", id, "attempt", attempt+1, "error", result.err)
	d.server.stats.failedOver(id, "invalid handshake")
	account.failed(attempt)
	d.skip[id] = true
}

func (d *responsesWebSocketDialer) rejectAccount(result upstreamWebSocketDial, account *Account, attempt int, usageLimit bool) bool {
	response := result.response
	status := response.StatusCode
	id := account.id()
	details := responseError(response)
	if details.Code == "" {
		details.Code = "upstream_rejected"
		if usageLimit {
			details.Code = "usage_limit_reached"
		} else if status == http.StatusTooManyRequests {
			details.Code = "rate_limit_exceeded"
		}
	}
	d.lastRejection = &websocketSetupError{status: status, details: details, retryAfter: response.Header.Get("Retry-After")}
	if status == http.StatusTooManyRequests || usageLimit {
		account.observe(response.Header)
		if usageLimit {
			account.rejectCredits()
			if account.markSpent() {
				d.server.log.Info("account stopped accepting new websockets", "account", id, "source", "handshake", "thread", d.thread, "status", status)
			}
			if workspaceUsageLimitReached(response.Header) {
				d.usageRetried[id] = true
			}
			if !d.usageRetried[id] && d.server.pool.route(nil, nil).account != nil && account.restoreFromUsageAfter(result.sent) {
				d.usageRetried[id] = true
				closeWebSocketResponse(response)
				return true
			}
		} else {
			account.rateLimited(response.Header, attempt)
		}
		d.server.stats.rateLimited(id)
		attrs := []any{"thread", d.thread, "attempt", attempt + 1}
		attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
		d.server.log.Info("account rate limited", attrs...)
	} else {
		d.server.log.Warn("upstream websocket rejected credentials", "thread", d.thread, "account", id, "attempt", attempt+1, "status", status)
		account.failed(attempt)
	}
	closeWebSocketResponse(response)
	d.server.stats.failedOver(id, response.Status)
	d.skip[id] = true
	return false
}

func closeWebSocketResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
}
