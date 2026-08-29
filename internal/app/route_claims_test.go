package app

import "testing"

func TestRouteClaimsKeepConcurrentConnectionsOnTheFirstAccount(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 10)
	pool := &Pool{accounts: []*Account{a, b}}
	registry := routeClaimRegistry{}
	pick := func(owners []string) routingDecision { return pool.route(owners, nil) }
	route := websocketRoute{session: "session", thread: "thread"}

	first := registry.selectAccount(route, durableRouteOwners{}, pick)
	if first.account != a || first.claim == nil || first.joined {
		t.Fatalf("first selection = %+v, want a new claim on account-a", first)
	}
	b.RoutingMode = routingModePriority
	second := registry.selectAccount(route, durableRouteOwners{}, pick)
	if second.account != a || second.claim == nil || !second.joined {
		t.Fatalf("second selection = %+v, want the existing account-a claim", second)
	}
	if second.reason != routingReasonProvisionalClaim {
		t.Fatalf("routing reason = %q, want %q", second.reason, routingReasonProvisionalClaim)
	}

	first.claim.release()
	third := registry.selectAccount(route, durableRouteOwners{}, pick)
	if third.account != a || !third.joined {
		t.Fatalf("selection after one release = %+v, want the remaining account-a claim", third)
	}
	second.claim.release()
	third.claim.release()
	fresh := registry.selectAccount(route, durableRouteOwners{}, pick)
	if fresh.account != b || fresh.joined {
		t.Fatalf("selection after every release = %+v, want a fresh account-b claim", fresh)
	}
	if fresh.claim != nil {
		fresh.claim.release()
	}
}

func TestRouteClaimsLinkSiblingThreadsThroughTheSession(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 10)
	pool := &Pool{accounts: []*Account{a, b}}
	registry := routeClaimRegistry{}
	pick := func(owners []string) routingDecision { return pool.route(owners, nil) }

	root := registry.selectAccount(websocketRoute{session: "session", thread: "root"}, durableRouteOwners{}, pick)
	b.RoutingMode = routingModePriority
	child := registry.selectAccount(websocketRoute{session: "session", thread: "child"}, durableRouteOwners{}, pick)
	if child.account != a || !child.joined {
		t.Fatalf("child selection = %+v, want it to join the root's provisional account", child)
	}
	root.claim.release()
	child.claim.release()
}

func TestRouteClaimsPreferADurableThreadOwnerOverASessionClaim(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 10)
	pool := &Pool{accounts: []*Account{a, b}}
	registry := routeClaimRegistry{}
	pick := func(owners []string) routingDecision { return pool.route(owners, nil) }

	root := registry.selectAccount(websocketRoute{session: "session", thread: "root"}, durableRouteOwners{}, pick)
	child := registry.selectAccount(websocketRoute{session: "session", thread: "child"}, durableRouteOwners{thread: b.id()}, pick)
	if child.account != b || child.claim != nil {
		t.Fatalf("child selection = %+v, want durable thread owner account-b without a claim", child)
	}
	root.claim.release()
}

func TestRouteClaimsKeepInFlightReplacementAheadOfRecoveredStaleOwners(t *testing.T) {
	oldOwner := testAccount("account-old", 10)
	newOwner := testAccount("account-new", 20)
	pool := &Pool{accounts: []*Account{oldOwner, newOwner}}
	registry := routeClaimRegistry{}
	pick := func(owners []string) routingDecision {
		decision := pool.route(owners, nil)
		decision.reason = routingDecisionReason(decision, nil, nil)
		return decision
	}

	oldOwner.spent = true
	rootRoute := websocketRoute{session: "session", thread: "root"}
	root := registry.selectAccount(rootRoute, durableRouteOwners{thread: oldOwner.id(), session: oldOwner.id()}, pick)
	if root.account != newOwner || root.priorOwner != oldOwner.id() || root.reason != routingReasonOwnerSpent {
		t.Fatalf("replacement = %+v, want account-new after the old owner was spent", root)
	}
	oldOwner.spent = false
	oldOwner.RoutingMode = routingModePriority

	reconnected := registry.selectAccount(rootRoute, durableRouteOwners{thread: oldOwner.id(), session: oldOwner.id()}, pick)
	if reconnected.account != newOwner || !reconnected.joined || !reconnected.moved() || reconnected.reason != routingReasonOwnerSpent {
		t.Fatalf("reconnected root = %+v, want the in-flight replacement and original move reason", reconnected)
	}
	child := registry.selectAccount(websocketRoute{session: "session", thread: "child"}, durableRouteOwners{session: oldOwner.id()}, pick)
	if child.account != newOwner || !child.joined || !child.moved() {
		t.Fatalf("sibling = %+v, want the session claim ahead of the recovered stale session owner", child)
	}

	root.claim.release()
	reconnected.claim.release()
	child.claim.release()
}

func TestRouteClaimsBlockACompetingAccountWhileClaimedAccountIsUnavailable(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 10)
	pool := &Pool{accounts: []*Account{a, b}}
	registry := routeClaimRegistry{}
	pick := func(owners []string) routingDecision { return pool.route(owners, nil) }
	route := websocketRoute{session: "session", thread: "thread"}

	first := registry.selectAccount(route, durableRouteOwners{}, pick)
	a.Paused = true
	blocked := registry.selectAccount(route, durableRouteOwners{}, pick)
	if blocked.account != nil || blocked.blocked != a.id() {
		t.Fatalf("selection = %+v, want the provisional owner to block account-b", blocked)
	}
	if blocked.reason != routingReasonProvisionalConflict {
		t.Fatalf("routing reason = %q, want %q", blocked.reason, routingReasonProvisionalConflict)
	}
	first.claim.release()
}

func TestRouteClaimCommitAndInvalidationRemoveEveryLinkedKey(t *testing.T) {
	a := testAccount("account-a", 10)
	pool := &Pool{accounts: []*Account{a}}
	pick := func(owners []string) routingDecision { return pool.route(owners, nil) }

	t.Run("commit", func(t *testing.T) {
		registry := routeClaimRegistry{}
		first := registry.selectAccount(websocketRoute{session: "session", thread: "root"}, durableRouteOwners{}, pick)
		joined := registry.selectAccount(websocketRoute{session: "session", thread: "child"}, durableRouteOwners{}, pick)
		if !first.claim.acceptSwitch() || joined.claim.acceptSwitch() {
			t.Fatal("joined claim did not deduplicate its accepted switch signal")
		}
		joined.claim.commit()
		if first.claim.active() || joined.claim.active() {
			t.Fatal("claim remained active after response.created committed a joined connection")
		}
		first.claim.release()
		if len(registry.byKey) != 0 || len(registry.byID) != 0 {
			t.Fatalf("registry after commit = keys:%d ids:%d, want empty", len(registry.byKey), len(registry.byID))
		}
	})

	t.Run("account invalidation", func(t *testing.T) {
		registry := routeClaimRegistry{}
		claim := registry.selectAccount(websocketRoute{session: "session", thread: "thread"}, durableRouteOwners{}, pick)
		if removed := registry.invalidateAccount(a.id()); removed != 1 {
			t.Fatalf("removed claims = %d, want 1", removed)
		}
		if claim.claim.active() {
			t.Fatal("claim remained active after account invalidation")
		}
		claim.claim.release()
	})
}

func TestRouteClaimsDoNotClaimAnonymousSockets(t *testing.T) {
	a := testAccount("account-a", 10)
	registry := routeClaimRegistry{}
	selection := registry.selectAccount(websocketRoute{}, durableRouteOwners{}, func([]string) routingDecision {
		return routingDecision{account: a}
	})
	if selection.claim != nil || len(registry.byID) != 0 {
		t.Fatalf("anonymous selection = %+v, want no affinity claim", selection)
	}
}
