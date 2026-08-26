package app

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

func TestPoolRouteChoosesRoomierAccount(t *testing.T) {
	a := testAccount("account-a", 80)
	b := testAccount("account-b", 10)
	a.lastUsed = time.Now().Add(-time.Hour)
	b.lastUsed = time.Now().Add(-time.Minute)
	p := &Pool{accounts: []*Account{a, b}}

	if got := p.route(nil, nil).account; got != b {
		t.Fatalf("account = %s, want account-b", got.id())
	}
}

func TestPoolRouteRetainsHealthyOwnerAheadOfFreshPlacementRules(t *testing.T) {
	owner := testAccount("account-owner", 90)
	fresh := testAccount("account-fresh", 0)
	fresh.RoutingMode = routingModePriority
	p := &Pool{accounts: []*Account{fresh, owner}}

	if got := p.route([]string{owner.id()}, nil).account; got != owner {
		t.Fatalf("account = %s, want the healthy owner despite fresh-placement priority", got.id())
	}
}

func TestPoolRouteBlocksInsteadOfSpillingACoolingOwner(t *testing.T) {
	owner := testAccount("account-owner", 90)
	owner.cooldown = time.Now().Add(time.Minute)
	fresh := testAccount("account-fresh", 0)
	p := &Pool{accounts: []*Account{fresh, owner}}

	decision := p.route([]string{owner.id()}, nil)
	if decision.account != nil || decision.blocked != owner.id() {
		t.Fatalf("decision = %+v, want a retry on the cooling owner without fresh placement", decision)
	}
}

func TestPoolRouteMarksFreshPlacementAfterSpentOwnerAsAMove(t *testing.T) {
	owner := testAccount("account-owner", 90)
	owner.spent = true
	fresh := testAccount("account-fresh", 0)
	p := &Pool{accounts: []*Account{fresh, owner}}

	decision := p.route([]string{owner.id()}, nil)
	if decision.account != fresh || !decision.moved() {
		t.Fatalf("decision = %+v, want fresh placement marked as an account move", decision)
	}
}

func TestPoolRouteFallsBackOnlyWhenTheOwnerCannotContinue(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Account)
		removed bool
	}{
		{name: "spent owner cannot serve this quota window", mutate: func(account *Account) { account.spent = true }},
		{name: "paused owner was removed from routing by the operator", mutate: func(account *Account) { account.Paused = true }},
		{name: "signed-out owner cannot authenticate", mutate: func(account *Account) { account.Reauth = "reauth required" }},
		{name: "removed owner no longer exists", removed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			owner := testAccount("account-owner", 90)
			fresh := testAccount("account-fresh", 0)
			accounts := []*Account{fresh}
			if !test.removed {
				test.mutate(owner)
				accounts = append(accounts, owner)
			}
			decision := (&Pool{accounts: accounts}).route([]string{owner.id()}, nil)
			if decision.account != fresh || !decision.moved() || decision.blocked != "" {
				t.Fatalf("decision = %+v, want a marked fresh placement because the owner cannot continue", decision)
			}
		})
	}
}

func TestPoolRouteBlocksOwnerWhileQuotaStateIsUnknown(t *testing.T) {
	owner := testAccount("account-owner", 90)
	owner.primary = window{}
	owner.secondary = window{}
	fresh := testAccount("account-fresh", 0)
	decision := (&Pool{accounts: []*Account{fresh, owner}}).route([]string{owner.id()}, nil)
	if decision.account != nil || decision.blocked != owner.id() {
		t.Fatalf("decision = %+v, want retry until the owner quota check can prove whether it may continue", decision)
	}
}

func TestPoolRouteHonorsSkip(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 20)
	p := &Pool{accounts: []*Account{a, b}}

	if got := p.route(nil, map[string]bool{a.id(): true}).account; got != b {
		t.Fatalf("account = %s, want account-b", got.id())
	}
	if got := p.route(nil, map[string]bool{a.id(): true, b.id(): true}).account; got != nil {
		t.Fatalf("account = %s, want no account", got.id())
	}
}

func TestPoolRouteSkipsUnavailableAccounts(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 20)
	p := &Pool{accounts: []*Account{a, b}}

	tests := []struct {
		name   string
		mutate func(*Account)
	}{
		{name: "paused", mutate: func(account *Account) { account.Paused = true }},
		{name: "spent", mutate: func(account *Account) { account.spent = true }},
		{name: "cooling", mutate: func(account *Account) { account.cooldown = time.Now().Add(time.Hour) }},
		{name: "reauth", mutate: func(account *Account) { account.Reauth = "reauth required" }},
		{name: "checking", mutate: func(account *Account) { account.primary = window{}; account.secondary = window{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a.Paused = false
			a.spent = false
			a.cooldown = time.Time{}
			a.Reauth = ""
			a.primary = window{usedPercent: 10, seenAt: time.Now()}
			a.secondary = window{usedPercent: 10, seenAt: time.Now()}
			test.mutate(a)
			if got := p.route(nil, nil).account; got != b {
				t.Fatalf("account = %s, want account-b", got.id())
			}
		})
	}
}

func TestPoolRouteChoosesRoomierAccountWhenAnotherIsNearlySpent(t *testing.T) {
	roomier := testAccount("account-roomier", 10)
	nearlySpent := testAccount("account-nearly-spent", 99)
	p := &Pool{accounts: []*Account{roomier, nearlySpent}}

	if got := p.route(nil, nil).account; got != roomier {
		t.Fatalf("account = %s, want account-roomier", got.id())
	}
}

func TestPoolRouteManualPriorityWins(t *testing.T) {
	roomier := testAccount("account-roomier", 10)
	priority := testAccount("account-priority", 80)
	priority.RoutingMode = routingModePriority
	p := &Pool{accounts: []*Account{roomier, priority}}

	if got := p.route(nil, nil).account; got != priority {
		t.Fatalf("account = %s, want account-priority", got.id())
	}
}

func TestPoolRoutePrioritizesExpiringReset(t *testing.T) {
	now := time.Now()
	resetting := testAccount("account-resetting", 80)
	resetting.secondary = window{usedPercent: 80, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
	adoptTestResetCredit(resetting, now.Add(30*time.Minute))
	roomier := testAccount("account-roomier", 10)
	p := &Pool{accounts: []*Account{roomier, resetting}}

	if got := p.route(nil, nil).account; got != resetting {
		t.Fatalf("account = %s, want account-resetting", got.id())
	}
	adoptTestResetCredit(resetting, now.Add(25*time.Hour))
	if got := p.route(nil, nil).account; got != roomier {
		t.Fatalf("account = %s, want account-roomier", got.id())
	}
}

func TestPoolRouteBreaksEqualPressureTiesByLastUsedAndID(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 10)
	a.lastUsed = time.Time{}
	b.lastUsed = time.Time{}
	p := &Pool{accounts: []*Account{b, a}}

	if got := p.route(nil, nil).account; got != a {
		t.Fatalf("account = %s, want account-a", got.id())
	}
	a.lastUsed = time.Now()
	b.lastUsed = time.Time{}
	if got := p.route(nil, nil).account; got != b {
		t.Fatalf("account = %s, want account-b", got.id())
	}
}

func TestAccountStatusPreservesRoutingStates(t *testing.T) {
	now := time.Now()
	account := testAccount("account-a", 20)
	if got := account.status(now); got != accountLive {
		t.Fatalf("status = %s, want live", got)
	}
	account.Paused = true
	if got := account.status(now); got != accountPaused {
		t.Fatalf("status = %s, want paused", got)
	}
	account.Paused = false
	account.spent = true
	if got := account.status(now); got != accountCooling {
		t.Fatalf("status = %s, want cooling", got)
	}
	account.spent = false
	account.cooldown = now.Add(time.Hour)
	if got := account.status(now); got != accountCooling {
		t.Fatalf("status = %s, want cooling", got)
	}
	account.cooldown = time.Time{}
	account.primary = window{}
	account.secondary = window{}
	if got := account.status(now); got != accountChecking {
		t.Fatalf("status = %s, want checking", got)
	}
	account.primary = window{usedPercent: 20, seenAt: now}
	account.secondary = window{usedPercent: 20, seenAt: now}
	account.Reauth = "reauth required"
	if got := account.status(now); got != accountNeedsReauth {
		t.Fatalf("status = %s, want needs reauth", got)
	}
}

func TestAccountStatusShowsPriority(t *testing.T) {
	account := testAccount("account-a", 20)
	account.RoutingMode = routingModePriority
	if got := account.status(time.Now()); got != accountPriority {
		t.Fatalf("priority status = %s", got)
	}
	account.RoutingMode = routingModeNormal
	if got := account.status(time.Now()); got != accountLive {
		t.Fatalf("normal status = %s", got)
	}
}

func TestAccountPriorityStatusRequiresAvailableReset(t *testing.T) {
	now := time.Now()
	expiresAt := now.Add(30 * time.Minute)
	account := testAccount("account-a", 20)
	account.secondary = window{usedPercent: 20, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
	account.adoptResetCredits(now, 1, []resetCredit{{ResetType: "other", Status: "available", ExpiresAt: &expiresAt}})
	if got := account.status(now); got != accountLive {
		t.Fatalf("status = %s, want live", got)
	}
	adoptTestResetCredit(account, expiresAt)
	if got := account.status(now); got != accountPriority {
		t.Fatalf("status = %s, want priority", got)
	}
}

func testAccount(id string, used float64) *Account {
	return testAccountWithPlan(id, used, "pro")
}

func testAccountWithPlan(id string, used float64, plan string) *Account {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"email":"%s@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"%s","chatgpt_plan_type":"%s"}}`, id, id, plan)))
	account := accountFromState(accountState{
		IDToken:      "x." + payload + ".x",
		AccessToken:  "token-" + id,
		RefreshToken: "refresh-" + id,
		LastRefresh:  time.Now(),
	})
	account.primary = window{usedPercent: used, seenAt: time.Now()}
	account.secondary = window{usedPercent: used, seenAt: time.Now()}
	return account
}

func adoptTestResetCredit(account *Account, expiresAt time.Time) {
	account.adoptResetCredits(time.Now(), 1, []resetCredit{{
		ID:        "credit-" + account.id(),
		ResetType: "codex_rate_limits",
		Status:    "available",
		ExpiresAt: &expiresAt,
	}})
}

func setTestAccountUsage(account *Account, used float64) {
	account.mu.Lock()
	defer account.mu.Unlock()
	account.primary.usedPercent = used
	account.secondary.usedPercent = used
}

func setTestAccountSpent(account *Account, spent bool) {
	account.mu.Lock()
	defer account.mu.Unlock()
	account.spent = spent
}
