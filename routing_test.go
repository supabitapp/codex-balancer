package main

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

	if got := p.route(nil).account; got != b {
		t.Fatalf("account = %s, want account-b", got.id())
	}
}

func TestPoolRouteHonorsSkip(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 20)
	p := &Pool{accounts: []*Account{a, b}}

	if got := p.route(map[string]bool{a.id(): true}).account; got != b {
		t.Fatalf("account = %s, want account-b", got.id())
	}
	if got := p.route(map[string]bool{a.id(): true, b.id(): true}).account; got != nil {
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
		{name: "reauth", mutate: func(account *Account) { account.dead = "reauth required" }},
		{name: "checking", mutate: func(account *Account) { account.primary = window{}; account.secondary = window{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a.Paused = false
			a.spent = false
			a.cooldown = time.Time{}
			a.dead = ""
			a.primary = window{usedPercent: 10, seenAt: time.Now()}
			a.secondary = window{usedPercent: 10, seenAt: time.Now()}
			test.mutate(a)
			if got := p.route(nil).account; got != b {
				t.Fatalf("account = %s, want account-b", got.id())
			}
		})
	}
}

func TestPoolRouteDrainingOverridesRoomierAccount(t *testing.T) {
	roomier := testAccount("account-roomier", 10)
	draining := testAccount("account-draining", 96)
	p := &Pool{accounts: []*Account{roomier, draining}}

	if got := p.route(nil).account; got != draining {
		t.Fatalf("account = %s, want account-draining", got.id())
	}
}

func TestPoolRouteStartsDrainingBelowFivePercent(t *testing.T) {
	roomier := testAccount("account-roomier", 10)
	boundary := testAccount("account-boundary", 95)
	p := &Pool{accounts: []*Account{roomier, boundary}}

	if got := p.route(nil).account; got != roomier {
		t.Fatalf("account at five percent = %s, want account-roomier", got.id())
	}
	setTestAccountUsage(boundary, 95.01)
	if got := p.route(nil).account; got != boundary {
		t.Fatalf("account below five percent = %s, want account-boundary", got.id())
	}
}

func TestPoolRouteDrainingOrderUsesPressureResetAndID(t *testing.T) {
	now := time.Now()
	pressure := testAccount("account-pressure", 99)
	pressure.primary.resetsAt = now.Add(2 * time.Hour)
	pressure.secondary.resetsAt = now.Add(2 * time.Hour)
	reset := testAccount("account-reset", 96)
	reset.primary.resetsAt = now.Add(time.Hour)
	reset.secondary.resetsAt = now.Add(time.Hour)
	p := &Pool{accounts: []*Account{reset, pressure}}
	if got := p.route(nil).account; got != pressure {
		t.Fatalf("account = %s, want account-pressure", got.id())
	}

	setTestAccountUsage(pressure, 96)
	if got := p.route(nil).account; got != reset {
		t.Fatalf("account = %s, want account-reset", got.id())
	}

	reset.primary.resetsAt = now.Add(2 * time.Hour)
	reset.secondary.resetsAt = now.Add(2 * time.Hour)
	reset.IDToken = testAccount("account-z", 96).IDToken
	pressure.IDToken = testAccount("account-a", 96).IDToken
	if got := p.route(nil).account; got != pressure {
		t.Fatalf("account = %s, want account-a", got.id())
	}
	reset.IDToken = testAccount("account-a", 96).IDToken
	pressure.IDToken = testAccount("account-b", 96).IDToken
	if got := p.route(nil).account; got != reset {
		t.Fatalf("account = %s, want account-a", got.id())
	}
}

func TestPoolRouteManualDrainingWins(t *testing.T) {
	manual := testAccount("account-manual", 20)
	manual.RoutingMode = routingModeDraining
	automatic := testAccount("account-automatic", 99)
	p := &Pool{accounts: []*Account{automatic, manual}}

	if got := p.route(nil).account; got != manual {
		t.Fatalf("account = %s, want account-manual", got.id())
	}
}

func TestPoolRouteManualPriorityWins(t *testing.T) {
	roomier := testAccount("account-roomier", 10)
	priority := testAccount("account-priority", 80)
	priority.RoutingMode = routingModePriority
	p := &Pool{accounts: []*Account{roomier, priority}}

	if got := p.route(nil).account; got != priority {
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

	if got := p.route(nil).account; got != resetting {
		t.Fatalf("account = %s, want account-resetting", got.id())
	}
	adoptTestResetCredit(resetting, now.Add(25*time.Hour))
	if got := p.route(nil).account; got != roomier {
		t.Fatalf("account = %s, want account-roomier", got.id())
	}
}

func TestPoolRouteBreaksEqualPressureTiesByLastUsedAndID(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 10)
	a.lastUsed = time.Time{}
	b.lastUsed = time.Time{}
	p := &Pool{accounts: []*Account{b, a}}

	if got := p.route(nil).account; got != a {
		t.Fatalf("account = %s, want account-a", got.id())
	}
	a.lastUsed = time.Now()
	b.lastUsed = time.Time{}
	if got := p.route(nil).account; got != b {
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
	account.dead = "reauth required"
	if got := account.status(now); got != accountNeedsReauth {
		t.Fatalf("status = %s, want needs reauth", got)
	}
}

func TestAccountStatusShowsPriorityAndDraining(t *testing.T) {
	account := testAccount("account-a", 20)
	account.RoutingMode = routingModePriority
	if got := account.status(time.Now()); got != accountPriority {
		t.Fatalf("priority status = %s", got)
	}
	account.RoutingMode = routingModeDraining
	if got := account.status(time.Now()); got != accountDraining {
		t.Fatalf("draining status = %s", got)
	}
	account.RoutingMode = routingModeNormal
	setTestAccountUsage(account, 96)
	if got := account.status(time.Now()); got != accountDraining {
		t.Fatalf("automatic draining status = %s", got)
	}
}

func TestAccountPriorityStatusRequiresAvailableReset(t *testing.T) {
	now := time.Now()
	expiresAt := now.Add(30 * time.Minute)
	account := testAccount("account-a", 20)
	account.secondary = window{usedPercent: 20, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
	account.adoptResetCredits(1, []resetCredit{{ResetType: "other", Status: "available", ExpiresAt: &expiresAt}})
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
	account.adoptResetCredits(1, []resetCredit{{
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
