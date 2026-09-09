package app

import (
	"testing"
	"time"
)

func TestPoolAutomaticDrainThresholdUsesWorstWindow(t *testing.T) {
	roomier := testAccount("roomier", 10)
	draining := testAccount("draining", 20)
	draining.secondary.usedPercent = 95
	pool := &Pool{accounts: []*Account{roomier, draining}}
	if got := pool.route(nil, nil).account; got != roomier {
		t.Fatal("exactly five percent remaining must not trigger automatic draining")
	}
	draining.secondary.usedPercent = 95.01
	if got := pool.route(nil, nil).account; got != draining {
		t.Fatal("less than five percent in either window must take fresh placement")
	}
	if got := draining.status(time.Now()); got != accountDraining {
		t.Fatalf("status = %s, want draining", got)
	}
	if draining.routingCandidate().mode != routingModeNormal {
		t.Fatal("automatic draining must not change the saved mode")
	}
	draining.secondary.usedPercent = 94
	if got := draining.status(time.Now()); got != accountLive {
		t.Fatalf("recovered status = %s, want live", got)
	}
}

func TestPoolDrainOrdering(t *testing.T) {
	now := time.Now()
	manual := testAccount("manual", 20)
	manual.RoutingMode = routingModeDraining
	automatic := testAccount("automatic", 99)
	priority := testAccount("priority", 90)
	priority.RoutingMode = routingModePriority
	credit := testAccount("credit", 30)
	adoptTestResetCredit(credit, now.Add(time.Minute))
	pool := &Pool{accounts: []*Account{priority, credit, automatic, manual}}
	if got := pool.route(nil, nil).account; got != manual {
		t.Fatal("manual drain must precede automatic drain, priority, and reset credits")
	}
	manual.Paused = true
	if got := pool.route(nil, nil).account; got != automatic {
		t.Fatal("automatic drain must precede priority and reset credits")
	}
	automatic.RoutingMode = routingModePriority
	if got := automatic.status(now); got != accountPriority {
		t.Fatalf("explicit priority must suppress automatic drain; status = %s", got)
	}
	if got := pool.route(nil, nil).account; got != priority {
		t.Fatal("without drain, normal priority and roomiest-first rules must apply")
	}
}

func TestPoolDrainTiesUsePressureResetAndID(t *testing.T) {
	now := time.Now()
	a := testAccount("a", 99)
	b := testAccount("b", 96)
	a.primary.resetsAt, a.secondary.resetsAt = now.Add(2*time.Hour), now.Add(2*time.Hour)
	b.primary.resetsAt, b.secondary.resetsAt = now.Add(time.Hour), now.Add(time.Hour)
	pool := &Pool{accounts: []*Account{b, a}}
	if got := pool.route(nil, nil).account; got != a {
		t.Fatal("higher pressure must drain first outside the one-point tie band")
	}
	setTestAccountUsage(a, 96.5)
	if got := pool.route(nil, nil).account; got != b {
		t.Fatal("the earlier reset must drain first within the tie band")
	}
	b.primary.resetsAt, b.secondary.resetsAt = time.Time{}, time.Time{}
	if got := pool.route(nil, nil).account; got != a {
		t.Fatal("a known reset must precede an unknown reset within the tie band")
	}
	b.primary.resetsAt, b.secondary.resetsAt = a.primary.resetsAt, a.secondary.resetsAt
	a.lastUsed = now
	if got := pool.route(nil, nil).account; got != a {
		t.Fatal("equal drain resets must break ties by ID, not last-used time")
	}
}

func TestPoolDrainTieBandIsIndependentOfAccountOrder(t *testing.T) {
	for _, mode := range []routingMode{routingModeNormal, routingModeDraining} {
		t.Run(string(mode), func(t *testing.T) {
			now := time.Now()
			a, b, c := testAccount("a", 99), testAccount("b", 98), testAccount("c", 97)
			other := testAccount("other", 99)
			if mode == routingModeNormal {
				other.RoutingMode = routingModePriority
			}
			for i, account := range []*Account{a, b, c} {
				account.RoutingMode = mode
				if mode == routingModeDraining {
					setTestAccountUsage(account, float64(50-i))
				}
				reset := now.Add(time.Duration(3-i) * time.Hour)
				account.primary.resetsAt, account.secondary.resetsAt = reset, reset
			}
			for _, order := range [][]*Account{
				{a, b, c}, {a, c, b}, {b, a, c},
				{b, c, a}, {c, a, b}, {c, b, a},
			} {
				t.Run(order[0].id()+order[1].id()+order[2].id(), func(t *testing.T) {
					pool := &Pool{accounts: append(order, other)}
					if got := pool.route(nil, nil).account; got != b {
						t.Fatalf("account = %s, want b: earliest reset within one point of the maximum usage", got.id())
					}
				})
			}
		})
	}
}

func TestPoolDrainTieBandExcludesUnavailableAndSkippedAccounts(t *testing.T) {
	for _, skipped := range []bool{false, true} {
		now := time.Now()
		highest, a, b := testAccount("highest", 99), testAccount("a", 97), testAccount("b", 96)
		highest.Paused = !skipped
		a.primary.resetsAt, a.secondary.resetsAt = now.Add(2*time.Hour), now.Add(2*time.Hour)
		b.primary.resetsAt, b.secondary.resetsAt = now.Add(time.Hour), now.Add(time.Hour)
		pool := &Pool{accounts: []*Account{highest, a, b}}
		if got := pool.route(nil, map[string]bool{highest.id(): skipped}).account; got != b {
			t.Fatalf("skipped=%t: account = %s, want b within the eligible maximum's tie band", skipped, got.id())
		}
	}
}

func TestDrainingPreservesRetainedOwners(t *testing.T) {
	for _, mode := range []routingMode{routingModeNormal, routingModeDraining} {
		t.Run(string(mode), func(t *testing.T) {
			owner := testAccount("owner", 20)
			draining := testAccount("draining", 99)
			draining.RoutingMode = mode
			pool := &Pool{accounts: []*Account{draining, owner}}
			if got := pool.route(nil, nil).account; got != draining {
				t.Fatal("fresh placement must use the draining account")
			}
			if got := pool.route([]string{owner.id()}, nil).account; got != owner {
				t.Fatal("draining must not displace a healthy retained owner")
			}
			owner.cooldown = time.Now().Add(time.Minute)
			if got := pool.route([]string{owner.id()}, nil); got.account != nil || got.blocked != owner.id() {
				t.Fatal("draining must not bypass a retained owner's temporary cooldown")
			}
			owner.cooldown = time.Time{}
			owner.primary, owner.secondary = window{}, window{}
			if got := pool.route([]string{owner.id()}, nil); got.account != nil || got.blocked != owner.id() {
				t.Fatal("draining must not bypass a retained owner's unknown quota")
			}
		})
	}
}

func TestDrainingDoesNotOverrideEligibility(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*Account)
		status accountStatus
	}{
		{"paused", func(a *Account) { a.Paused = true }, accountPaused},
		{"spent", func(a *Account) { a.spent = true }, accountCooling},
		{"spend limit", func(a *Account) { a.spendControl = &spendControlPayload{Reached: true} }, accountCooling},
		{"cooling", func(a *Account) { a.cooldown = time.Now().Add(time.Hour) }, accountCooling},
		{"reauth", func(a *Account) { a.Reauth = "signed out" }, accountNeedsReauth},
		{"unknown quota", func(a *Account) { a.primary, a.secondary = window{}, window{} }, accountChecking},
		{"workspace", func(a *Account) { a.planType = "business" }, accountNotRouted},
	} {
		for _, mode := range []routingMode{routingModeNormal, routingModeDraining} {
			t.Run(scenario.name+"/"+string(mode), func(t *testing.T) {
				a := testAccount("draining", 99)
				a.RoutingMode = mode
				scenario.change(a)
				b := testAccount("other", 30)
				pool := &Pool{accounts: []*Account{a, b}}
				if got := pool.route(nil, nil).account; got != b {
					t.Fatal("unavailable draining account received work")
				}
				if got := a.status(time.Now()); got != scenario.status {
					t.Fatalf("status = %s, want %s", got, scenario.status)
				}
			})
		}
	}
}

func TestPoolSkipsDrainingAccountsWhenRequested(t *testing.T) {
	draining := testAccount("draining", 99)
	other := testAccount("other", 30)
	pool := &Pool{accounts: []*Account{draining, other}}
	if got := pool.route(nil, map[string]bool{draining.id(): true}).account; got != other {
		t.Fatal("skip filters must apply before drain ranking")
	}
}
