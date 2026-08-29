package app

import (
	"slices"
	"sync"
)

// routeClaimRegistry pins unaccepted WebSocket routes to one account. Live
// claims are process-local; invalidation barriers are mirrored to SQLite until
// a replacement route is accepted.
type routeClaimRegistry struct {
	mu       sync.Mutex
	next     uint64
	byKey    map[string]*routeClaim
	byID     map[uint64]*routeClaim
	barriers map[string]string
}

type routeClaim struct {
	id         uint64
	account    string
	priorOwner string
	reason     routingReason
	keys       []string
	refs       int
	accepted   bool
}

type routeClaimHandle struct {
	registry *routeClaimRegistry
	id       uint64
	account  string
	once     sync.Once
}

type claimedRoutingDecision struct {
	routingDecision
	claim  *routeClaimHandle
	joined bool
}

type routeClaimInvalidation struct {
	claims int
	keys   []string
}

type durableRouteOwners struct {
	thread  string
	session string
}

func (o durableRouteOwners) ordered() []string {
	owners := make([]string, 0, 2)
	owners = appendUniqueOwner(owners, o.thread)
	owners = appendUniqueOwner(owners, o.session)
	return owners
}

func (o durableRouteOwners) contains(account string) bool {
	return account != "" && (o.thread == account || o.session == account)
}

func (r *routeClaimRegistry) selectAccount(
	route websocketRoute,
	durable durableRouteOwners,
	pick func([]string) routingDecision,
) claimedRoutingDecision {
	keys := routeClaimKeys(route)
	if len(keys) == 0 {
		return claimedRoutingDecision{routingDecision: pick(durable.ordered())}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureMaps()

	claims := r.claimsForKeys(keys)
	owners := make([]string, 0, len(claims)+2)
	// An exact in-flight thread claim is strongest, followed by an invalidated
	// claim's move barrier. A durable thread owner then wins over a sibling's
	// session claim; that session claim in turn wins over a stale durable
	// session owner that was unavailable when work started.
	if route.thread != "" {
		owners = appendClaimOwner(owners, r.byKey[route.thread])
		owners = appendUniqueOwner(owners, r.barriers[route.thread])
	}
	owners = appendUniqueOwner(owners, durable.thread)
	if route.session != "" {
		owners = appendClaimOwner(owners, r.byKey[route.session])
		owners = appendUniqueOwner(owners, r.barriers[route.session])
	}
	owners = appendUniqueOwner(owners, durable.session)
	for _, claim := range claims {
		owners = appendClaimOwner(owners, claim)
	}
	for _, key := range keys {
		owners = appendUniqueOwner(owners, r.barriers[key])
	}
	decision := pick(owners)
	if decision.account == nil {
		return claimedRoutingDecision{routingDecision: decision}
	}
	selected := decision.account.id()
	for _, claim := range claims {
		if claim.account != selected {
			continue
		}
		if claimConflicts(claim, claims) {
			decision.account = nil
			decision.blocked = claim.account
			decision.reason = routingReasonProvisionalConflict
			return claimedRoutingDecision{routingDecision: decision}
		}
		r.attachKeys(claim, keys)
		claim.refs++
		decision.priorOwner = claim.priorOwner
		if claim.priorOwner != "" {
			decision.reason = claim.reason
		} else {
			decision.reason = routingReasonProvisionalClaim
		}
		return claimedRoutingDecision{
			routingDecision: decision,
			claim:           r.handle(claim),
			joined:          true,
		}
	}
	if durable.contains(selected) {
		return claimedRoutingDecision{routingDecision: decision}
	}

	// An active claim is authoritative until every connection using it either
	// fails or one of them receives response.created. Do not create a competing
	// claim while account-bound work may already be in flight.
	if len(claims) > 0 {
		decision.account = nil
		decision.blocked = claims[0].account
		decision.reason = routingReasonProvisionalConflict
		return claimedRoutingDecision{routingDecision: decision}
	}

	r.next++
	claim := &routeClaim{
		id:         r.next,
		account:    selected,
		priorOwner: decision.priorOwner,
		reason:     decision.reason,
		refs:       1,
	}
	r.byID[claim.id] = claim
	r.attachKeys(claim, keys)
	return claimedRoutingDecision{
		routingDecision: decision,
		claim:           r.handle(claim),
	}
}

func appendClaimOwner(owners []string, claim *routeClaim) []string {
	if claim == nil {
		return owners
	}
	return appendUniqueOwner(owners, claim.account)
}

func appendUniqueOwner(owners []string, owner string) []string {
	if owner != "" && !slices.Contains(owners, owner) {
		return append(owners, owner)
	}
	return owners
}

func (r *routeClaimRegistry) ensureMaps() {
	if r.byKey == nil {
		r.byKey = map[string]*routeClaim{}
	}
	if r.byID == nil {
		r.byID = map[uint64]*routeClaim{}
	}
	if r.barriers == nil {
		r.barriers = map[string]string{}
	}
}

func (r *routeClaimRegistry) claimsForKeys(keys []string) []*routeClaim {
	claims := make([]*routeClaim, 0, len(keys))
	for _, key := range keys {
		claim := r.byKey[key]
		if claim != nil && !slices.Contains(claims, claim) {
			claims = append(claims, claim)
		}
	}
	return claims
}

func claimConflicts(selected *routeClaim, claims []*routeClaim) bool {
	for _, claim := range claims {
		if claim != selected && claim.account != selected.account {
			return true
		}
	}
	return false
}

func (r *routeClaimRegistry) attachKeys(claim *routeClaim, keys []string) {
	for _, key := range keys {
		if current := r.byKey[key]; current != nil {
			continue
		}
		r.byKey[key] = claim
		claim.keys = append(claim.keys, key)
	}
}

func (r *routeClaimRegistry) handle(claim *routeClaim) *routeClaimHandle {
	return &routeClaimHandle{registry: r, id: claim.id, account: claim.account}
}

func (h *routeClaimHandle) release() {
	if h == nil || h.registry == nil {
		return
	}
	h.once.Do(func() { h.registry.release(h.id) })
}

func (h *routeClaimHandle) commit(keys []string) {
	if h == nil || h.registry == nil {
		return
	}
	h.once.Do(func() { h.registry.commit(h.id, keys) })
}

func (h *routeClaimHandle) active() bool {
	if h == nil || h.registry == nil {
		return true
	}
	h.registry.mu.Lock()
	defer h.registry.mu.Unlock()
	claim := h.registry.byID[h.id]
	return claim != nil && claim.account == h.account
}

func (h *routeClaimHandle) acceptSwitch() bool {
	if h == nil || h.registry == nil {
		return true
	}
	h.registry.mu.Lock()
	defer h.registry.mu.Unlock()
	claim := h.registry.byID[h.id]
	if claim == nil || claim.account != h.account || claim.accepted {
		return false
	}
	claim.accepted = true
	return true
}

func (r *routeClaimRegistry) release(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	claim := r.byID[id]
	if claim == nil {
		return
	}
	claim.refs--
	if claim.refs == 0 {
		r.removeLocked(claim)
	}
}

func (r *routeClaimRegistry) remove(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if claim := r.byID[id]; claim != nil {
		r.removeLocked(claim)
	}
}

func (r *routeClaimRegistry) commit(id uint64, acceptedKeys []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range acceptedKeys {
		delete(r.barriers, key)
	}
	if claim := r.byID[id]; claim != nil {
		r.removeLocked(claim)
	}
}

func (r *routeClaimRegistry) clearBarriers(keys []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		delete(r.barriers, key)
	}
}

func (r *routeClaimRegistry) invalidateAccount(account string) routeClaimInvalidation {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureMaps()
	result := routeClaimInvalidation{}
	seenKeys := map[string]bool{}
	claims := make([]*routeClaim, 0, len(r.byID))
	for _, claim := range r.byID {
		claims = append(claims, claim)
	}
	for _, claim := range claims {
		if claim.account == account {
			for _, key := range claim.keys {
				r.barriers[key] = account
				if !seenKeys[key] {
					seenKeys[key] = true
					result.keys = append(result.keys, key)
				}
			}
			r.removeLocked(claim)
			result.claims++
		}
	}
	slices.Sort(result.keys)
	return result
}

func (s *server) claimAccount(
	route websocketRoute,
	durable durableRouteOwners,
	model string,
	serviceTier string,
	skip map[string]bool,
	attempt int,
) claimedRoutingDecision {
	return s.routeClaims.selectAccount(route, durable, func(owners []string) routingDecision {
		return s.pickAccount(route.key(), owners, model, serviceTier, skip, attempt)
	})
}

func (r *routeClaimRegistry) removeLocked(claim *routeClaim) {
	delete(r.byID, claim.id)
	for _, key := range claim.keys {
		if r.byKey[key] == claim {
			delete(r.byKey, key)
		}
	}
}

func routeClaimKeys(route websocketRoute) []string {
	keys := make([]string, 0, 2)
	if route.thread != "" {
		keys = append(keys, route.thread)
	}
	if route.session != "" && route.session != route.thread {
		keys = append(keys, route.session)
	}
	return keys
}
