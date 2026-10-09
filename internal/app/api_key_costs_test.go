package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestAdminAPIKeyUsageShowsMonthWithLifetimeCost(t *testing.T) {
	srv := newTestServer(t, nil)
	enableTestAdmin(t, srv)
	now := time.Now()
	for _, name := range []string{"standard", "fast", "unpriced", "unused"} {
		if err := srv.pool.store.addAPIKey(storedAPIKey{Name: name, Secret: name + "-secret", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	base := responseUsage{InputTokens: 200_000, OutputTokens: 2_000}
	base.InputDetails.CachedTokens = 50_000
	base.InputDetails.CacheWriteTokens = 10_000
	long := responseUsage{InputTokens: 300_000, OutputTokens: 10_000}
	long.InputDetails.CachedTokens = 100_000
	long.InputDetails.CacheWriteTokens = 50_000
	small := responseUsage{InputTokens: 1_000}
	small.InputDetails.CachedTokens = 200
	small.InputDetails.CacheWriteTokens = 100
	events := []storedUsage{
		{At: calendarMonthStart(now).Add(-time.Hour), APIKeyName: "standard", Model: "gpt-5.6-sol", Usage: base},
		{At: now, APIKeyName: "standard", Model: "gpt-5.6-sol", Usage: base},
		{At: now, APIKeyName: "standard", Model: "gpt-5.6-sol", Usage: long},
		{At: now, APIKeyName: "standard", Model: "gpt-5.4", Usage: small},
		{At: now, APIKeyName: "fast", Model: "gpt-5.6-sol-2026-08-01", ServiceTier: serviceTierFast, Usage: long},
		{At: now, APIKeyName: "unpriced", Model: "gpt-5.4", Usage: small},
		{At: now, APIKeyName: "unpriced", Model: "unknown", Usage: small},
		{At: now, Model: "gpt-5.6-sol", Usage: responseUsage{InputTokens: 100_000_000}},
	}
	for _, event := range events {
		if err := srv.pool.store.recordUsage(event); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		start time.Time
		want  map[string]usageCost
	}{
		{start: time.Time{}, want: map[string]usageCost{
			"standard": {apiCostNanoDollars: 4_372_050_000},
			"fast":     {apiCostNanoDollars: 5_350_000_000},
			"unpriced": {apiCostNanoDollars: 2_050_000, unpricedResponses: 1},
			"unused":   {},
		}},
		{start: calendarMonthStart(now), want: map[string]usageCost{
			"standard": {apiCostNanoDollars: 3_524_550_000},
			"fast":     {apiCostNanoDollars: 5_350_000_000},
			"unpriced": {apiCostNanoDollars: 2_050_000, unpricedResponses: 1},
			"unused":   {},
		}},
	} {
		costs, err := srv.pool.store.apiKeyCosts(srv.prices.current(), test.start)
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range test.want {
			if got := costs[name]; got != want {
				t.Fatalf("%s cost since %v = %+v, want %+v", name, test.start, got, want)
			}
		}
		if _, found := costs[""]; found {
			t.Fatal("unattributed usage included in API key costs")
		}
	}
	handler := srv.routes()
	cookie, csrf := loginTestAdmin(t, handler)
	checkRows := func(response *httptest.ResponseRecorder, status string) {
		t.Helper()
		if response.Code != http.StatusOK {
			t.Fatalf("admin status = %d", response.Code)
		}
		body := response.Body.String()
		if !strings.Contains(body, `<th title="Estimated API cost this month at current model prices">USD burnt</th>`) ||
			!strings.Contains(body, `<th title="Estimated lifetime API cost at current model prices">Total USD burnt</th>`) {
			t.Fatal("admin table missing USD burnt columns")
		}
		if !strings.Contains(body, "usage from "+calendarMonthStart(now).Format("Jan 2")) {
			t.Fatal("admin keys summary missing usage period")
		}
		rows := make(map[string]string)
		for _, row := range regexp.MustCompile(`(?s)<tr[^>]*>\s*<td>([^<]*)</td>(.*?)</tr>`).FindAllStringSubmatch(body, -1) {
			rows[row[1]] = row[2]
		}
		for name, want := range map[string]string{
			"standard": "<td>501K</td>\n<td>150.2K</td>\n<td>12K</td>\n<td>513K</td>\n<td>$3.52</td>\n<td>$4.37</td>",
			"fast":     "<td>300K</td>\n<td>100K</td>\n<td>10K</td>\n<td>310K</td>\n<td>$5.35</td>\n<td>$5.35</td>",
			"unpriced": "<td>2K</td>\n<td>400</td>\n<td>0</td>\n<td>2K</td>\n<td>--</td>\n<td>--</td>",
			"unused":   "<td>0</td>\n<td>0</td>\n<td>0</td>\n<td>0</td>\n<td>$0.00</td>\n<td>$0.00</td>",
		} {
			if !strings.Contains(rows[name], want) {
				t.Fatalf("%s row missing %q", name, want)
			}
		}
		if !strings.Contains(rows["unused"], `<td class="dim">--</td>`) {
			t.Fatal("unused row missing last used placeholder")
		}
		for _, name := range []string{"standard", "fast", "unpriced"} {
			if !strings.Contains(rows[name], `<td class="dim">just now</td>`) {
				t.Fatalf("%s row missing last used date", name)
			}
		}
		if !strings.Contains(rows["standard"], "</span> "+strings.ToLower(status)) {
			t.Fatalf("standard row missing %s status", status)
		}
	}
	checkRows(adminRequest(handler, http.MethodGet, "/admin", nil, cookie), "Active")
	request := httptest.NewRequest(http.MethodPost, "https://balancer.test/admin/keys/revoke", strings.NewReader(url.Values{
		"name": {"standard"}, "csrf": {csrf},
	}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("HX-Request", "true")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	checkRows(response, "Revoked")
	if strings.Contains(response.Body.String(), "<!doctype") {
		t.Fatal("key revocation returned a full page instead of the keys panel")
	}
	srv.prices.install(priceSnapshot{})
	page := adminRequest(handler, http.MethodGet, "/admin", nil, cookie)
	if strings.Contains(page.Body.String(), "<td>$4.37</td>") || !strings.Contains(page.Body.String(), "<td>513K</td>\n<td>--</td>\n<td>--</td>") {
		t.Fatal("missing catalog displayed a known price")
	}
	srv.prices.install(testPriceSnapshot(t))
	checkRows(adminRequest(handler, http.MethodGet, "/admin", nil, cookie), "Revoked")
}

func TestAPIKeyCostsSurviveStoreReopen(t *testing.T) {
	statePath := t.TempDir() + "/state.db"
	store, err := openStateStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Now()
	if err := store.addAPIKey(storedAPIKey{Name: "client", Secret: "secret", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.recordUsage(storedUsage{At: now, APIKeyName: "client", Model: "gpt-5.4", Usage: responseUsage{InputTokens: 1_000}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStateStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	costs, err := store.apiKeyCosts(testPriceSnapshot(t), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := costs["client"]; got != (usageCost{apiCostNanoDollars: 2_500_000}) {
		t.Fatalf("restored cost = %+v, want 2500000 nano-dollars", got)
	}
}

func TestFormatRelativeDate(t *testing.T) {
	now := time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		at   time.Time
		want string
	}{
		{name: "now", at: now, want: "just now"},
		{name: "minute", at: now.Add(-time.Minute), want: "1 minute ago"},
		{name: "minutes", at: now.Add(-5 * time.Minute), want: "5 minutes ago"},
		{name: "hour", at: now.Add(-time.Hour), want: "1 hour ago"},
		{name: "hours", at: now.Add(-3 * time.Hour), want: "3 hours ago"},
		{name: "yesterday", at: now.Add(-24 * time.Hour), want: "yesterday"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := formatRelativeDate(now, test.at); got != test.want {
				t.Fatalf("formatRelativeDate() = %q, want %q", got, test.want)
			}
		})
	}
}
