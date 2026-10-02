package app

import (
	"net/http"
	"slices"
	"strings"
	"time"
)

type responseAccount struct {
	account       *Account
	accessToken   authorizationRevision
	claim         *routeClaimHandle
	priorOwner    string
	routingReason routingReason
	moved         bool
}

type routeAcceptance struct {
	allowed   bool
	persisted bool
	logSwitch bool
}

func (d *responseAccount) releaseClaim() {
	if d != nil && d.claim != nil {
		d.claim.release()
		d.claim = nil
	}
}

func (d *responseAccount) commitClaim(keys []string) {
	if d != nil && d.claim != nil {
		d.claim.commit(keys)
		d.claim = nil
	}
}

func (d *responseAccount) acceptSwitch() bool {
	if d == nil || !d.moved {
		return false
	}
	return d.claim.acceptSwitch()
}

func (s *server) acceptResponseRoute(dial *responseAccount, route storedRoute) routeAcceptance {
	result := routeAcceptance{}
	if dial == nil || dial.account == nil {
		return result
	}
	s.routeOwnership.Lock()
	defer s.routeOwnership.Unlock()
	if !s.accountRoutable(dial.account.id()) || !dial.claim.active() {
		return result
	}
	result.allowed = true
	dial.account.accepted(route.At)
	result.persisted = s.stats.persistRoute(route)
	result.logSwitch = dial.acceptSwitch()
	if result.persisted {
		keys := routeClaimKeys(websocketRoute{session: route.Session, thread: route.Thread})
		if dial.claim != nil {
			dial.commitClaim(keys)
		} else {
			s.routeClaims.clearBarriers(keys)
		}
	}
	return result
}

func (s *server) preserveResponseOwner(dial *responseAccount) {
	if dial == nil || dial.claim == nil {
		return
	}
	s.routeOwnership.Lock()
	defer s.routeOwnership.Unlock()
	at := time.Now()
	keys := dial.claim.preserve()
	dial.claim = nil
	if len(keys) == 0 || s.pool == nil || s.pool.store == nil {
		return
	}
	if err := s.pool.store.preserveRouteOwners(at, dial.account.id(), keys); err != nil {
		s.log.Warn("websocket retry owner preservation failed", "account", dial.account.id(), "routes", keys, "error", err)
	}
}

type responseAccountRouter struct {
	server       *server
	request      *http.Request
	model        string
	serviceTier  string
	route        websocketRoute
	thread       string
	durable      durableRouteOwners
	owners       []string
	skip         map[string]bool
	reauthed     map[string]bool
	usageRetried map[string]bool
}

func newResponseAccountRouter(s *server, request *http.Request, route websocketRoute, model, serviceTier string) (*responseAccountRouter, error) {
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
	return &responseAccountRouter{
		server:       s,
		request:      request,
		model:        model,
		serviceTier:  serviceTier,
		route:        route,
		thread:       route.key(),
		durable:      durable,
		owners:       durable.ordered(),
		skip:         map[string]bool{},
		reauthed:     map[string]bool{},
		usageRetried: map[string]bool{},
	}, nil
}

func (d *responseAccountRouter) refreshBeforeDial(account *Account, retained bool) (bool, error) {
	id := account.id()
	if !account.refreshDue(time.Now()) || d.reauthed[id] {
		return false, nil
	}
	d.reauthed[id] = true
	if d.server.refreshedContext(d.request.Context(), account, id) {
		return false, nil
	}
	if retained {
		return false, errRouteOwnerUnavailable
	}
	d.skip[id] = true
	return true, nil
}

func (d *responseAccountRouter) selectHTTPAccount(event websocketEnvelope) (*responseAccount, error) {
	for attempt := 0; ; attempt++ {
		if err := d.request.Context().Err(); err != nil {
			return nil, err
		}
		selection := d.server.claimAccount(d.route, d.durable, d.model, d.serviceTier, d.skip, attempt)
		observation(d.request.Context()).selection(d.request.Context(), selection, attempt)
		if selection.blocked != "" {
			return nil, selection.unavailable(errRouteOwnerUnavailable)
		}
		account := selection.account
		if account == nil {
			return nil, selection.unavailable(errNoAccountAvailable)
		}
		if selection.moved() && (!websocketRequestPortable(event) || strings.TrimSpace(d.request.Header.Get(codexTurnStateKey)) != "") {
			selection.claim.release()
			return nil, errAccountBoundTurn
		}
		skip, err := d.refreshBeforeDial(account, slices.Contains(d.owners, account.id()) || selection.joined)
		if err != nil || skip {
			selection.claim.release()
			if err != nil {
				return nil, err
			}
			continue
		}
		if len(routeClaimKeys(d.route)) == 0 {
			account.accepted(time.Now())
		}
		return &responseAccount{account: account, claim: selection.claim, priorOwner: selection.priorOwner, routingReason: selection.reason, moved: selection.moved()}, nil
	}
}
