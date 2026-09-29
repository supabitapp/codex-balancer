package app

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSubscriptionClaimsReachStatsAndPlanTooltip(t *testing.T) {
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		plan        string
		currentPlan string
		until       any
		checked     any
		wantDate    string
		wantChecked bool
	}{
		{"paid period", "pro", "pro", "2026-10-09T12:31:59+00:00", "2026-09-22T23:40:01.566030+00:00", "Subscription period ends: 9 October 2026, 12:31 UTC", true},
		{"managed workspace", "business", "business", "2026-10-09T12:31:59Z", nil, "Subscription period ends: 9 October 2026, 12:31 UTC", false},
		{"past period", "pro", "pro", "2026-09-09T12:31:59Z", nil, "Last reported subscription period ended: 9 September 2026, 12:31 UTC", false},
		{"missing fields", "pro", "pro", nil, nil, "", false},
		{"invalid date", "pro", "pro", "not a date", nil, "", false},
		{"invalid checked date", "pro", "pro", "2026-10-09T12:31:59Z", "not a date", "Subscription period ends: 9 October 2026, 12:31 UTC", false},
		{"changed plan", "pro", "plus", "2026-10-09T12:31:59Z", nil, "", false},
		{"free plan", "free", "free", "2026-10-09T12:31:59Z", nil, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{
				"https://api.openai.com/auth": map[string]any{
					"chatgpt_account_id":                "account-a",
					"chatgpt_plan_type":                 test.plan,
					"chatgpt_subscription_active_until": test.until,
					"chatgpt_subscription_last_checked": test.checked,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			account := accountFromState(accountState{IDToken: "x." + base64.RawURLEncoding.EncodeToString(payload) + ".x"})
			account.planType = test.currentPlan
			srv := &server{pool: &Pool{accounts: []*Account{account}}, stats: newStatsWithPrices(priceSnapshot{})}
			stats := srv.currentStats(now)
			if len(stats.Accounts) != 1 || stats.Accounts[0].ID != "account-a" || stats.Accounts[0].Plan != test.currentPlan {
				t.Fatalf("subscription claims broke account identity: %+v", stats.Accounts)
			}
			subscription := stats.Accounts[0].Subscription
			if test.wantDate == "" {
				if subscription != nil {
					t.Fatalf("unexpected subscription: %+v", subscription)
				}
			} else if subscription == nil || (subscription.LastChecked != nil) != test.wantChecked {
				t.Fatalf("subscription = %+v", subscription)
			}
			view := srv.currentDashboard(now)
			fragment := "accounts-update"
			if !routablePlan(test.currentPlan) {
				fragment = "workspace-section-update"
			}
			html, err := renderDashboard(fragment, view)
			if err != nil {
				t.Fatal(err)
			}
			body := string(html)
			if test.wantDate == "" {
				if !strings.Contains(body, `<td class="dim">`+dashboardPlan(test.currentPlan)+`</td>`) {
					t.Fatalf("plan without a period end should have no tooltip: %s", body)
				}
			} else if !strings.Contains(body, `data-tooltip="`+test.wantDate+`"`) || !strings.Contains(body, `aria-describedby="dashboard-tooltip" tabindex="0">`+dashboardPlan(test.currentPlan)+`</span>`) {
				t.Fatalf("missing accessible plan tooltip: %s", body)
			}
		})
	}
}

func TestPlanTooltipUpdatesWhenSignInClaimsRefresh(t *testing.T) {
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	account := testAccount("account-a", 20)
	srv := &server{pool: &Pool{accounts: []*Account{account}}, stats: newStatsWithPrices(priceSnapshot{})}
	previous := make(map[string][]byte)
	if _, err := renderDashboardChanges(srv.currentDashboard(now), previous); err != nil {
		t.Fatal(err)
	}
	next := account.persisted()
	next.IDToken = "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-a","chatgpt_plan_type":"pro","chatgpt_subscription_active_until":"2026-10-09T12:31:59Z"}}`)) + ".x"
	next.LastRefresh = next.LastRefresh.Add(time.Minute)
	account.applyPersisted(next)
	update, err := renderDashboardChanges(srv.currentDashboard(now), previous)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(update), "Subscription period ends: 9 October 2026, 12:31 UTC") {
		t.Fatalf("subscription refresh missing from live update: %s", update)
	}
}
