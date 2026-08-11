package main

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

func TestPoolRouteHonorsHardAndSoftAffinity(t *testing.T) {
	now := time.Now()
	a := testAccount("account-a", 80)
	b := testAccount("account-b", 10)
	a.lastUsed = now.Add(-time.Hour)
	b.lastUsed = now.Add(-time.Minute)
	pool := &Pool{accounts: []*Account{a, b}}

	tests := []struct {
		name      string
		required  string
		preferred string
		mutate    func()
		want      string
	}{
		{name: "fresh chooses roomiest", want: "account-b"},
		{name: "hard owner wins", required: "account-a", want: "account-a"},
		{name: "soft owner wins while available", preferred: "account-a", want: "account-a"},
		{
			name:      "soft owner spills when spent",
			preferred: "account-a",
			mutate:    func() { a.spent = true },
			want:      "account-b",
		},
		{
			name:     "hard owner fails when spent",
			required: "account-a",
			mutate:   func() { a.spent = true },
		},
		{
			name:      "soft owner spills while cooling",
			preferred: "account-a",
			mutate:    func() { a.cooldown = time.Now().Add(time.Hour) },
			want:      "account-b",
		},
		{
			name:     "hard owner fails while cooling",
			required: "account-a",
			mutate:   func() { a.cooldown = time.Now().Add(time.Hour) },
		},
		{
			name:      "soft owner spills while paused",
			preferred: "account-a",
			mutate:    func() { a.Paused = true },
			want:      "account-b",
		},
		{
			name:     "hard owner fails while paused",
			required: "account-a",
			mutate:   func() { a.Paused = true },
		},
		{
			name:      "soft owner spills when credentials fail",
			preferred: "account-a",
			mutate:    func() { a.dead = "reauth required" },
			want:      "account-b",
		},
		{
			name:     "hard owner fails when credentials fail",
			required: "account-a",
			mutate:   func() { a.dead = "reauth required" },
		},
		{name: "unknown soft owner falls back", preferred: "missing", want: "account-b"},
		{name: "unknown hard owner fails", required: "missing"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a.spent = false
			a.cooldown = time.Time{}
			a.Paused = false
			a.dead = ""
			if test.mutate != nil {
				test.mutate()
			}
			got := pool.route(test.required, test.preferred, nil, nil).account
			if got == nil {
				if test.want != "" {
					t.Fatalf("account = nil, want %s", test.want)
				}
				return
			}
			if got.id() != test.want {
				t.Fatalf("account = %s, want %s", got.id(), test.want)
			}
		})
	}
}

func TestPoolRouteDoesNotRetryExcludedOwner(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 20)
	pool := &Pool{accounts: []*Account{a, b}}
	if got := pool.route("account-a", "", map[string]bool{"account-a": true}, nil).account; got != nil {
		t.Fatalf("hard route selected %s after exclusion", got.id())
	}
	if got := pool.route("", "account-a", map[string]bool{"account-a": true}, nil).account; got == nil || got.id() != "account-b" {
		t.Fatalf("soft route selected %v, want account-b", got)
	}
}

func TestPoolRouteBreaksTiesByAccountID(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 10)
	a.lastUsed = time.Time{}
	b.lastUsed = time.Time{}
	pool := &Pool{accounts: []*Account{b, a}}
	if got := pool.route("", "", nil, nil).account; got != a {
		t.Fatalf("account = %s, want account-a", got.id())
	}
}

func TestPoolRoutePrioritizesExpiringBankedResetsAfterAffinity(t *testing.T) {
	now := time.Now()
	resetting := testAccount("account-resetting", 80)
	resetting.secondary = window{usedPercent: 80, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
	adoptTestResetCredit(resetting, now.Add(30*time.Minute))
	roomier := testAccount("account-roomier", 10)
	roomier.secondary = window{usedPercent: 10, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
	pool := &Pool{accounts: []*Account{roomier, resetting}}

	if got := pool.route("", "", nil, nil).account; got != resetting {
		t.Fatalf("fresh route selected %s, want expiring-reset account", got.id())
	}
	if got := pool.route("", roomier.id(), nil, nil).account; got != roomier {
		t.Fatalf("soft route selected %s, want affinity owner", got.id())
	}
	if got := pool.route(roomier.id(), "", nil, nil).account; got != roomier {
		t.Fatalf("hard route selected %s, want affinity owner", got.id())
	}
	adoptTestResetCredit(resetting, now.Add(25*time.Hour))
	if got := pool.route("", "", nil, nil).account; got != roomier {
		t.Fatalf("route outside lead selected %s, want roomier account", got.id())
	}
}

func TestPoolRoutePrioritizesEarliestBankedResetExpiry(t *testing.T) {
	now := time.Now()
	earlier := testAccount("account-earlier", 80)
	earlier.secondary = window{usedPercent: 80, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
	adoptTestResetCredit(earlier, now.Add(15*time.Minute))
	later := testAccount("account-later", 10)
	later.secondary = window{usedPercent: 10, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
	adoptTestResetCredit(later, now.Add(45*time.Minute))
	pool := &Pool{accounts: []*Account{later, earlier}}

	if got := pool.route("", "", nil, nil).account; got != earlier {
		t.Fatalf("account = %s, want earliest banked reset expiry", got.id())
	}
}

func TestPoolRouteRequiresTwentyPercentWeeklyRemainingForResetPriority(t *testing.T) {
	now := time.Now()
	resetting := testAccount("account-resetting", 80.01)
	resetting.primary = window{usedPercent: 10, minutes: 300, resetsAt: now.Add(4 * time.Hour), seenAt: now}
	resetting.secondary = window{usedPercent: 80.01, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
	adoptTestResetCredit(resetting, now.Add(30*time.Minute))
	roomier := testAccount("account-roomier", 10)
	roomier.secondary = window{usedPercent: 10, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
	pool := &Pool{accounts: []*Account{resetting, roomier}}

	if got := pool.route("", "", nil, nil).account; got != roomier {
		t.Fatalf("account = %s, want roomier account below priority threshold", got.id())
	}
	if got := resetting.status(now); got != accountLive {
		t.Fatalf("status = %s, want live below priority threshold", got)
	}
	resetting.secondary.usedPercent = 80
	if got := pool.route("", "", nil, nil).account; got != resetting {
		t.Fatalf("account = %s, want reset account at priority threshold", got.id())
	}
	if got := resetting.status(now); got != accountPriority {
		t.Fatalf("status = %s, want priority at threshold", got)
	}
}

func TestAccountPriorityStatusUsesBankedResetExpiryWindow(t *testing.T) {
	now := time.Now()
	account := testAccount("account-a", 20)
	account.secondary = window{usedPercent: 20, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
	adoptTestResetCredit(account, now.Add(30*time.Minute))

	if got := account.status(now); got != accountPriority {
		t.Fatalf("status = %s, want priority", got)
	}
	account.Paused = true
	if got := account.status(now); got != accountPaused {
		t.Fatalf("paused status = %s, want paused", got)
	}
	account.Paused = false
	account.spent = true
	if got := account.status(now); got != accountCooling {
		t.Fatalf("spent status = %s, want cooling", got)
	}
	account.spent = false
	adoptTestResetCredit(account, now.Add(24*time.Hour))
	if got := account.status(now); got != accountPriority {
		t.Fatalf("status at lead boundary = %s, want priority", got)
	}
	adoptTestResetCredit(account, now.Add(24*time.Hour+time.Second))
	if got := account.status(now); got != accountLive {
		t.Fatalf("status = %s outside lead, want live", got)
	}
}

func TestAccountPriorityStatusRequiresAvailableBankedReset(t *testing.T) {
	now := time.Now()
	expiresAt := now.Add(30 * time.Minute)
	account := testAccount("account-a", 20)
	account.secondary = window{usedPercent: 20, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}

	tests := []resetCredit{
		{ResetType: "other", Status: "available", ExpiresAt: &expiresAt},
		{ResetType: "codex_rate_limits", Status: "redeemed", ExpiresAt: &expiresAt},
		{ResetType: "codex_rate_limits", Status: "available"},
	}
	for _, credit := range tests {
		account.adoptResetCredits(1, []resetCredit{credit})
		if got := account.status(now); got != accountLive {
			t.Fatalf("status for credit %+v = %s, want live", credit, got)
		}
	}
}

func testAccount(id string, used float64) *Account {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"email":"%s@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"%s","chatgpt_plan_type":"pro"}}`, id, id)))
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
