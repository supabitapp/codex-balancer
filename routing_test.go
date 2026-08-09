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

func setTestAccountUsage(account *Account, used float64) {
	account.mu.Lock()
	defer account.mu.Unlock()
	account.primary.usedPercent = used
	account.secondary.usedPercent = used
}
