package app

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/coder/websocket"
)

type responsesWebSocketDialer struct {
	server       *server
	request      *http.Request
	model        string
	serviceTier  string
	upstream     string
	route        websocketRoute
	thread       string
	durable      durableRouteOwners
	owners       []string
	skip         map[string]bool
	reauthed     map[string]bool
	resetRetried map[string]bool
}

type upstreamWebSocketDial struct {
	conn     *websocket.Conn
	response *http.Response
	err      error
	sent     time.Time
}

func newResponsesWebSocketDialer(s *server, request *http.Request, route websocketRoute, model, serviceTier string) (*responsesWebSocketDialer, error) {
	upstream, err := responsesWebSocketURL(s.upstream)
	if err != nil {
		return nil, err
	}
	durable := durableRouteOwners{}
	if s.pool.store != nil {
		if route.thread != "" {
			owners, ownerErr := s.pool.store.routeOwners(route.thread, "")
			if ownerErr != nil {
				return nil, ownerErr
			}
			if len(owners) > 0 {
				durable.thread = owners[0]
			}
		}
		if route.session != "" {
			owners, ownerErr := s.pool.store.routeOwners("", route.session)
			if ownerErr != nil {
				return nil, ownerErr
			}
			if len(owners) > 0 {
				durable.session = owners[0]
			}
		}
	}
	return &responsesWebSocketDialer{
		server:       s,
		request:      request,
		model:        model,
		serviceTier:  serviceTier,
		upstream:     upstream,
		route:        route,
		thread:       route.key(),
		durable:      durable,
		owners:       durable.ordered(),
		skip:         map[string]bool{},
		reauthed:     map[string]bool{},
		resetRetried: map[string]bool{},
	}, nil
}

func (d *responsesWebSocketDialer) dial() (*websocketDial, *http.Response, error) {
	for attempt := 0; ; attempt++ {
		selection := d.server.claimAccount(d.route, d.durable, d.model, d.serviceTier, d.skip, attempt)
		decision := selection.routingDecision
		if decision.blocked != "" {
			return nil, nil, errRouteOwnerUnavailable
		}
		account := decision.account
		if account == nil {
			return nil, nil, errNoAccountAvailable
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
		if done {
			return nil, response, err
		}
		if retry {
			attempt--
		}
	}
}

func (d *responsesWebSocketDialer) refreshBeforeDial(account *Account, retained bool) (bool, error) {
	id := account.id()
	if !account.refreshDue(time.Now()) || d.reauthed[id] {
		return false, nil
	}
	d.reauthed[id] = true
	if d.server.refreshed(account, id) {
		return false, nil
	}
	if retained {
		return false, errRouteOwnerUnavailable
	}
	d.skip[id] = true
	return true, nil
}

func (d *responsesWebSocketDialer) open(account *Account) upstreamWebSocketDial {
	result := upstreamWebSocketDial{sent: time.Now()}
	ctx, cancel := context.WithTimeout(d.request.Context(), upstreamWait)
	defer cancel()
	result.conn, result.response, result.err = websocket.Dial(ctx, d.upstream, &websocket.DialOptions{
		HTTPClient: d.server.client,
		HTTPHeader: responsesWebSocketHeaders(d.request.Header, account),
	})
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
		conn:          result.conn,
		resp:          result.response,
		account:       account,
		claim:         selection.claim,
		priorOwner:    decision.priorOwner,
		routingReason: decision.reason,
		moved:         decision.moved(),
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
	closeWebSocketResponse(response)
	id := account.id()
	d.reauthed[id] = true
	if d.server.refreshed(account, id) {
		return nil
	}
	if retained {
		return errRouteOwnerUnavailable
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
	if status == http.StatusTooManyRequests || usageLimit {
		account.observe(response.Header)
		if usageLimit {
			if account.markSpent() {
				d.server.log.Info("account stopped accepting new websockets", "account", id, "source", "handshake", "thread", d.thread, "status", status)
			}
			if !workspaceUsageLimitReached(response.Header) && !d.resetRetried[id] && d.server.recoverUsageLimit(d.request.Context(), account, result.sent) {
				d.resetRetried[id] = true
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
