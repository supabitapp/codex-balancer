package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
)

func TestStatsEndpointPubliclyReturnsDashboardAccountState(t *testing.T) {
	now := time.Now()
	account := accountFromState(accountState{
		IDToken: jwtForEmail("khoi.nguyen@example.com", "acct-a"),
	})
	banked := int64(3)
	primaryReset := now.Add(2 * time.Hour).Truncate(time.Second)
	account.adopt(
		window{usedPercent: 25, minutes: 300, resetsAt: primaryReset, seenAt: now},
		window{usedPercent: 64.125, minutes: 10080, resetsAt: now.Add(4 * 24 * time.Hour), seenAt: now},
		&banked,
		false,
	)
	account.rateLimited(http.Header{"Retry-After": []string{"60"}}, 0)
	stats := newStats()
	stats.routed("thread-a", "acct-a", "", transportWebSocket)
	stats.websocketOpened("acct-a")
	stats.rateLimited("acct-a")
	stats.answered(1500 * time.Microsecond)
	s := &server{
		pool:  &Pool{accounts: []*Account{account}},
		stats: stats,
		key:   "secret",
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	if strings.Contains(rec.Body.String(), "khoi.nguyen@example.com") {
		t.Fatalf("response leaks the email: %s", rec.Body)
	}

	var got statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Turns != 1 || got.WebSocketTurns != 1 || got.OpenWebSockets != 1 || got.Threads != 1 {
		t.Fatalf("totals = %+v", got)
	}
	if got.RateLimits != 1 || got.AverageTTFBMilliseconds != 1.5 || got.UptimeSeconds <= 0 {
		t.Fatalf("rates = %+v", got)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(got.Accounts))
	}
	gotAccount := got.Accounts[0]
	if gotAccount.ID != "acct-a" || gotAccount.Email != "k***n@example.com" || gotAccount.Plan != "pro" {
		t.Fatalf("account identity = %+v", gotAccount)
	}
	if gotAccount.Status != accountCooling || gotAccount.Turns != 1 || gotAccount.OpenWebSockets != 1 || gotAccount.RateLimits != 1 {
		t.Fatalf("account status = %+v", gotAccount)
	}
	if gotAccount.WeeklyRemainingPercent == nil || *gotAccount.WeeklyRemainingPercent != 35.875 {
		t.Fatalf("weekly remaining = %v, want 35.875", gotAccount.WeeklyRemainingPercent)
	}
	if gotAccount.BankedResets == nil || *gotAccount.BankedResets != 3 {
		t.Fatalf("banked resets = %v, want 3", gotAccount.BankedResets)
	}
	if gotAccount.ResetAt == nil || !gotAccount.ResetAt.Equal(primaryReset) {
		t.Fatalf("reset at = %v, want %s", gotAccount.ResetAt, primaryReset)
	}
}

func TestMaskEmailHidesTheLocalPart(t *testing.T) {
	for email, want := range map[string]string{
		"khoi@example.com": "k***i@example.com",
		"ab@example.com":   "a***@example.com",
		"a@example.com":    "***@example.com",
		"not-an-email":     "***",
		"":                 "",
	} {
		if got := maskEmail(email); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", email, got, want)
		}
	}
}

func TestWindowsReadBothRateLimitHeaders(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Unix()
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "42.5")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-at", strconv.FormatInt(reset, 10))
	h.Set("x-codex-secondary-primary-used-percent", "91")

	a := accountFor("acct-a")
	a.observe(h)

	primary, secondary, _, _ := a.health()
	if primary.usedPercent != 42.5 || primary.minutes != 300 || primary.resetsAt.Unix() != reset {
		t.Fatalf("primary = %+v", primary)
	}
	if secondary.usedPercent != 91 {
		t.Fatalf("secondary = %+v", secondary)
	}
	if got := a.pressure(); got != 91 {
		t.Fatalf("pressure = %v, want the fuller of the two windows", got)
	}
}

func TestActivityRingShiftsWithTime(t *testing.T) {
	s := newStats()
	s.routed("thread", "acct-a", "", transportHTTP)
	s.routed("thread", "acct-a", "", transportHTTP)

	s.mu.Lock()
	s.accounts["acct-a"].bucket -= 3
	s.mu.Unlock()
	s.routed("thread", "acct-a", "", transportHTTP)

	got := s.snapshot().Accounts["acct-a"].Activity
	if got[0] != 1 {
		t.Fatalf("newest bucket = %d, want the fresh turn", got[0])
	}
	if got[3] != 2 {
		t.Fatalf("activity = %v, want the older pair shifted three slots back", got[:6])
	}
}

func TestRelayForwardsBytesUntouched(t *testing.T) {
	stream := "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}))
	defer upstream.Close()

	s := testServer(t, upstream.URL, "acct-a")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	s.responses(rec, req)

	if rec.Body.String() != stream {
		t.Fatalf("relayed body differs from upstream:\n got %q\nwant %q", rec.Body, stream)
	}
	if s.stats.snapshot().TTFB <= 0 {
		t.Fatal("time to first byte not recorded")
	}
}
func TestAccountsResolveByEmailOrID(t *testing.T) {
	pool := &Pool{path: t.TempDir() + "/accounts.json"}
	for _, id := range []string{"acct-a", "acct-b"} {
		pool.accounts = append(pool.accounts, accountFor(id))
	}

	for _, query := range []string{"acct-a@example.com", "ACCT-A@EXAMPLE.COM", "acct-a"} {
		got, err := pool.resolve(query)
		if err != nil {
			t.Fatalf("resolve(%q): %v", query, err)
		}
		if got.id() != "acct-a" {
			t.Fatalf("resolve(%q) = %s, want acct-a", query, got.id())
		}
	}

	if _, err := pool.resolve("nobody@example.com"); err == nil {
		t.Fatal("resolving an unknown email should fail")
	}
}

func TestRemoveByEmailDropsOnlyThatAccount(t *testing.T) {
	pool := &Pool{path: t.TempDir() + "/accounts.json"}
	for _, id := range []string{"acct-a", "acct-b"} {
		if err := pool.add(accountFor(id)); err != nil {
			t.Fatal(err)
		}
	}

	account, err := pool.resolve("acct-a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.remove(account); err != nil {
		t.Fatal(err)
	}
	if len(pool.accounts) != 1 || pool.accounts[0].id() != "acct-b" {
		t.Fatalf("pool = %v, want only acct-b left", pool.accounts)
	}
}

func TestAmbiguousEmailAsksForAnID(t *testing.T) {
	pool := &Pool{path: t.TempDir() + "/accounts.json"}
	pool.accounts = append(pool.accounts,
		accountFromState(accountState{IDToken: jwtForEmail("acct-a@example.com", "workspace-one")}),
		accountFromState(accountState{IDToken: jwtForEmail("acct-a@example.com", "workspace-two")}))

	_, err := pool.resolve("acct-a@example.com")
	if err == nil || !strings.Contains(err.Error(), "matches 2 accounts") {
		t.Fatalf("err = %v, want a request to name one by id", err)
	}
	if _, err := pool.resolve("workspace-two"); err != nil {
		t.Fatalf("the id must still resolve: %v", err)
	}
}

func TestCooldownWaitsForTheExhaustedWindow(t *testing.T) {
	fiveHours := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	weekly := time.Now().Add(4*24*time.Hour + 7*time.Hour).Truncate(time.Second)

	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "40")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-at", strconv.FormatInt(fiveHours.Unix(), 10))
	h.Set("x-codex-secondary-primary-used-percent", "100")
	h.Set("x-codex-secondary-primary-window-minutes", "10080")
	h.Set("x-codex-secondary-primary-reset-at", strconv.FormatInt(weekly.Unix(), 10))

	a := accountFor("acct-a")
	a.observe(h)
	a.rateLimited(h, 0)

	_, _, cooldown, _ := a.health()
	if !cooldown.Equal(weekly) {
		t.Fatalf("cooldown = %s, want the exhausted weekly window at %s; waking on the 5h reset earns an instant 429",
			cooldown, weekly)
	}
	if a.available(fiveHours.Add(time.Minute)) {
		t.Fatal("account came back when only the unexhausted window had reset")
	}
	if !a.available(weekly.Add(time.Minute)) {
		t.Fatal("account never came back after its weekly window reset")
	}
}

func withUsageEndpoint(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	original := usageEndpoint
	usageEndpoint = srv.URL
	t.Cleanup(func() {
		usageEndpoint = original
		srv.Close()
	})
}

const usageBody = `{"plan_type":"pro","rate_limit":{
  "primary_window":{"used_percent":12.5,"reset_at":%d,"limit_window_seconds":18000},
  "secondary_window":{"used_percent":64,"reset_at":%d,"limit_window_seconds":604800}},
  "rate_limit_reset_credits":{"available_count":3}}`

func TestUsagePollFillsBothWindowsAndBankedResets(t *testing.T) {
	fiveHours := time.Now().Add(3 * time.Hour).Unix()
	weekly := time.Now().Add(96 * time.Hour).Unix()
	var auth, account string
	withUsageEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		auth, account = r.Header.Get("Authorization"), r.Header.Get("chatgpt-account-id")
		fmt.Fprintf(w, usageBody, fiveHours, weekly)
	})

	s := testServer(t, "http://unused", "acct-a")
	if err := s.pollUsage(t.Context(), s.pool.accounts[0]); err != nil {
		t.Fatal(err)
	}

	if auth != "Bearer token-acct-a" || account != "acct-a" {
		t.Fatalf("upstream saw auth=%q account=%q", auth, account)
	}
	primary, secondary, _, _ := s.pool.accounts[0].health()
	if primary.usedPercent != 12.5 || primary.minutes != 300 {
		t.Fatalf("primary = %+v, want 12.5%% over a 300 minute window", primary)
	}
	if secondary.usedPercent != 64 || secondary.minutes != 10080 {
		t.Fatalf("secondary = %+v, want 64%% over a weekly window", secondary)
	}
	if banked, known := s.pool.accounts[0].bankedResets(); !known || banked != 3 {
		t.Fatalf("banked resets = %d, %t, want 3, true", banked, known)
	}
}

func TestUsagePollRetriesAfterRefreshingTheAccount(t *testing.T) {
	withTokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tokenResponse{
			IDToken:      jwtFor("acct-a"),
			AccessToken:  "AT-new",
			RefreshToken: "RT-new",
		})
	})
	var calls int
	withUsageEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") == "Bearer AT-old" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, usageBody, time.Now().Add(3*time.Hour).Unix(), time.Now().Add(96*time.Hour).Unix())
	})

	s := testServer(t, "http://unused", "acct-a")
	a := s.pool.accounts[0]
	a.AccessToken = "AT-old"
	a.RefreshToken = "RT-old"
	if err := s.pollUsage(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || a.AccessToken != "AT-new" {
		t.Fatalf("usage calls = %d, token = %q; want a refresh and one retry", calls, a.AccessToken)
	}
}

func TestNextResetPicksTheSoonestFutureWindow(t *testing.T) {
	now := time.Now()
	soon := now.Add(3 * time.Hour)
	later := now.Add(4 * 24 * time.Hour)
	past := now.Add(-time.Minute)

	if got := nextReset(now, window{resetsAt: later}, window{resetsAt: soon}); !got.Equal(soon) {
		t.Fatalf("next reset = %s, want %s", got, soon)
	}
	if got := nextReset(now, window{resetsAt: past}); !got.IsZero() {
		t.Fatalf("past reset returned as next: %s", got)
	}
}

func TestDashboardShowsBankedResetsAndNextReset(t *testing.T) {
	account := accountFor("acct-a")
	banked := int64(3)
	account.adopt(
		window{usedPercent: 25, minutes: 300, resetsAt: time.Now().Add(90 * time.Minute), seenAt: time.Now()},
		window{usedPercent: 64, minutes: 10080, resetsAt: time.Now().Add(4 * 24 * time.Hour), seenAt: time.Now()},
		&banked,
		false,
	)
	account.cooldown = time.Now().Add(83 * time.Hour)
	d := dashboard{pool: &Pool{accounts: []*Account{account}}, stats: newStats(), width: 80}

	rendered := d.accounts(len(d.pool.accounts))
	for _, want := range []string{"cooling", "Weekly", "36%", "Banked", "Reset in", "3", "1h29m"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("dashboard does not contain %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "3d10h") {
		t.Fatalf("status contains a duplicate reset timer:\n%s", rendered)
	}
	for _, line := range strings.Split(rendered, "\n") {
		if width := lipgloss.Width(line); width > d.width {
			t.Fatalf("dashboard row is %d columns wide, want at most %d:\n%s", width, d.width, line)
		}
	}
}

func TestUsagePollLiftsACooldownOnceTheWindowHasRoom(t *testing.T) {
	withUsageEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, usageBody, time.Now().Add(3*time.Hour).Unix(), time.Now().Add(96*time.Hour).Unix())
	})

	s := testServer(t, "http://unused", "acct-a")
	a := s.pool.accounts[0]

	limited := http.Header{}
	limited.Set("x-codex-secondary-primary-used-percent", "100")
	limited.Set("x-codex-secondary-primary-reset-at", strconv.FormatInt(time.Now().Add(96*time.Hour).Unix(), 10))
	a.rateLimited(limited, 0)
	if a.available(time.Now()) {
		t.Fatal("account should be cooling after a 429")
	}

	if err := s.pollUsage(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if !a.available(time.Now()) {
		t.Fatal("upstream reported room but the account stayed parked")
	}
}

func TestUsagePollLeavesAnExhaustedAccountParked(t *testing.T) {
	withUsageEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"rate_limit":{"secondary_window":{"used_percent":100,"reset_at":%d,"limit_window_seconds":604800}}}`,
			time.Now().Add(96*time.Hour).Unix())
	})

	s := testServer(t, "http://unused", "acct-a")
	a := s.pool.accounts[0]

	if err := s.pollUsage(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if a.available(time.Now()) {
		t.Fatal("account still at 100% must stay parked")
	}
}

func TestUsagePollTrustsTheLimitReachedFlag(t *testing.T) {
	withUsageEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"rate_limit":{"limit_reached":true,"primary_window":
			{"used_percent":4,"reset_at":%d,"limit_window_seconds":604800}}}`,
			time.Now().Add(96*time.Hour).Unix())
	})

	s := testServer(t, "http://unused", "acct-a")
	a := s.pool.accounts[0]

	if err := s.pollUsage(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if a.available(time.Now()) {
		t.Fatal("upstream said the limit is reached; a low percentage must not release the account")
	}
}
