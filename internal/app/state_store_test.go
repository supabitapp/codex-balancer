package app

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStateStoreCreatesOnlyMinimalSchema(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var applicationID, version int
	if err := store.db.QueryRow("PRAGMA application_id").Scan(&applicationID); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if applicationID != stateApplicationID || version != stateSchemaVersion {
		t.Fatalf("application/version = %d/%d, want %d/%d", applicationID, version, stateApplicationID, stateSchemaVersion)
	}

	rows, err := store.db.Query(`SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	wantTables := []string{"accounts", "api_keys", "response_usage", "routes"}
	if !reflect.DeepEqual(tables, wantTables) {
		t.Fatalf("tables = %v, want %v", tables, wantTables)
	}

	wantColumns := map[string][]string{
		"accounts":       {"account_id", "id_token", "access_token", "refresh_token", "paused", "routing_mode", "last_refresh_ns", "last_used_at_ns", "reauth"},
		"api_keys":       {"name", "secret", "created_at_ns", "revoked_at_ns"},
		"response_usage": {"id", "api_key_name", "at_ns", "model", "service_tier", "input_tokens", "cached_tokens", "cache_write_tokens", "output_tokens", "reasoning_tokens", "account_id"},
		"routes":         {"key", "account_id", "updated_at_ns"},
	}
	for table, want := range wantColumns {
		got := tableColumns(t, store, table)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s columns = %v, want %v", table, got, want)
		}
	}
}

func tableColumns(t *testing.T, store *StateStore, table string) []string {
	t.Helper()
	rows, err := store.db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, column)
	}
	return columns
}

func TestStateStorePersistsLatestRoutesAndLastUsed(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.add(testAccount("account-a", 0)); err != nil {
		t.Fatal(err)
	}
	if err := pool.add(testAccount("account-b", 0)); err != nil {
		t.Fatal(err)
	}

	older := time.Now().Add(-time.Minute)
	newer := time.Now()
	if err := store.recordRoute(storedRoute{At: newer, Session: "session", Thread: "thread", Account: "account-b"}); err != nil {
		t.Fatal(err)
	}
	if err := store.recordRoute(storedRoute{At: older, Session: "session", Thread: "thread", Account: "account-a"}); err != nil {
		t.Fatal(err)
	}
	if err := store.recordRoute(storedRoute{At: newer, Session: "shared", Thread: "shared", Account: "account-b"}); err != nil {
		t.Fatal(err)
	}
	var routeCount int
	if err := store.db.QueryRow(`SELECT count(*) FROM routes`).Scan(&routeCount); err != nil {
		t.Fatal(err)
	}
	if routeCount != 3 {
		t.Fatalf("routes = %d, want distinct thread, session, and shared keys", routeCount)
	}
	owners, err := store.routeOwners("thread", "session")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(owners, []string{"account-b"}) {
		t.Fatalf("owners = %v, want account-b", owners)
	}
	owners, err = store.routeOwners("shared", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(owners, []string{"account-b"}) {
		t.Fatalf("shared owners = %v, want one account-b", owners)
	}
	accounts, err := store.readAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if account.id() == "account-b" && !account.lastUsed.Equal(newer) {
			t.Fatalf("last used = %s, want %s", account.lastUsed, newer)
		}
	}

	if err := pool.remove(testAccount("account-b", 0)); err != nil {
		t.Fatal(err)
	}
	owners, err = store.routeOwners("thread", "session")
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 0 {
		t.Fatalf("owners after account removal = %v", owners)
	}
}

func TestStateStoreDerivesCumulativeUsageFromResponseRows(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	created := time.Now().Add(-time.Hour)
	if err := store.addAPIKey(storedAPIKey{Name: "legacy", Secret: "secret", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.add(testAccount("account-a", 0)); err != nil {
		t.Fatal(err)
	}

	first := responseUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 999}
	first.InputDetails.CachedTokens = 30
	first.InputDetails.CacheWriteTokens = 4
	first.OutputDetails.ReasoningTokens = 7
	second := responseUsage{InputTokens: 9, OutputTokens: 3}
	if err := store.recordUsage(storedUsage{At: time.Now().Add(-time.Second), APIKeyName: "legacy", Account: "account-a", Model: "gpt-5.4", Usage: first}); err != nil {
		t.Fatal(err)
	}
	if err := store.recordUsage(storedUsage{At: time.Now(), APIKeyName: "legacy", Account: "account-a", Model: "gpt-5.4", ServiceTier: "priority", Usage: second}); err != nil {
		t.Fatal(err)
	}

	usage, err := store.apiKeyUsage()
	if err != nil {
		t.Fatal(err)
	}
	want := responseUsage{InputTokens: 109, OutputTokens: 23, TotalTokens: 132}
	want.InputDetails.CachedTokens = 30
	want.InputDetails.CacheWriteTokens = 4
	want.OutputDetails.ReasoningTokens = 7
	if usage["legacy"] != want {
		t.Fatalf("usage = %+v, want %+v", usage["legacy"], want)
	}
	var apiKeyName, serviceTier, account string
	if err := store.db.QueryRow(`SELECT api_key_name, service_tier, account_id FROM response_usage ORDER BY id LIMIT 1`).Scan(&apiKeyName, &serviceTier, &account); err != nil {
		t.Fatal(err)
	}
	if apiKeyName != "legacy" || serviceTier != "default" || account != "account-a" {
		t.Fatalf("usage identity/tier/account = %q/%q/%q, want legacy/default/account-a", apiKeyName, serviceTier, account)
	}
	events, err := store.usageEventsSince(created)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].APIKeyName != "legacy" || events[1].APIKeyName != "legacy" || events[0].Account != "account-a" || events[1].Account != "account-a" {
		t.Fatalf("usage events = %+v, want two attributed to legacy", events)
	}
	if err := store.recordUsage(storedUsage{At: time.Now(), APIKeyName: "missing", Model: "gpt-5.4", Usage: second}); err == nil {
		t.Fatal("usage for missing API key succeeded")
	}
	if err := store.recordUsage(storedUsage{At: time.Now(), Account: "missing", Model: "gpt-5.4", Usage: second}); err == nil {
		t.Fatal("usage for missing account succeeded")
	}

	revokedAt := time.Now()
	revoked, err := store.revokeAPIKey("legacy", revokedAt)
	if err != nil || !revoked {
		t.Fatalf("revoke = %t, %v", revoked, err)
	}
	if err := store.addAPIKey(storedAPIKey{Name: "legacy", Secret: "replacement", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	usage, err = store.apiKeyUsage()
	if err != nil {
		t.Fatal(err)
	}
	if usage["legacy"] != want {
		t.Fatalf("usage after reprovision = %+v, want %+v", usage["legacy"], want)
	}
}

func TestPersistentStatsRestoresCurrentMonthWithoutPruningHistory(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.add(testAccount("account-a", 0)); err != nil {
		t.Fatal(err)
	}
	prices := testPriceSnapshot(t)
	now := time.Now()
	old := storedUsage{At: calendarMonthStart(now).Add(-time.Second), Model: "gpt-5.4", Usage: responseUsage{InputTokens: 10_000}}
	current := storedUsage{At: now, Account: "account-a", Model: "gpt-5.4", ServiceTier: "default", Usage: responseUsage{InputTokens: 1_000, OutputTokens: 100}}
	if err := store.recordUsage(old); err != nil {
		t.Fatal(err)
	}
	if err := store.recordUsage(current); err != nil {
		t.Fatal(err)
	}

	stats, err := newPersistentStats(store, prices, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := stats.snapshot()
	wantUsage := current.Usage
	wantUsage.TotalTokens = wantUsage.InputTokens + wantUsage.OutputTokens
	if snapshot.MonthlyUsage != wantUsage {
		t.Fatalf("monthly usage = %+v, want %+v", snapshot.MonthlyUsage, wantUsage)
	}
	wantCost, _ := prices.estimate(current.Model, current.ServiceTier, current.Usage)
	if snapshot.APICostNanoDollars != wantCost {
		t.Fatalf("monthly cost = %d, want %d", snapshot.APICostNanoDollars, wantCost)
	}
	credits, _, known := stats.routedCreditsSince("account-a", now.Add(-time.Hour))
	if !known || credits != 0.1 {
		t.Fatalf("restored routed credits = %v, %t, want 0.1", credits, known)
	}
	if err := stats.reprice(prices); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := store.db.QueryRow(`SELECT count(*) FROM response_usage`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("retained usage rows = %d, want 2", rows)
	}
}

func TestStateStoreRejectsUnsupportedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("PRAGMA user_version = 22"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = openStateStore(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("open error = %v, want unsupported schema", err)
	}
}

func TestStateStoreMigratesUsageAccountAttribution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO response_usage (
		at_ns, model, service_tier, input_tokens, cached_tokens, cache_write_tokens, output_tokens, reasoning_tokens
	) VALUES (?, 'gpt-5.4', 'default', 100, 50, 0, 10, 0)`, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`ALTER TABLE response_usage DROP COLUMN account_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	columns := tableColumns(t, store, "response_usage")
	if columns[len(columns)-1] != "account_id" {
		t.Fatalf("response usage columns = %v, want account attribution", columns)
	}
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != stateSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, stateSchemaVersion)
	}
	var usageRows int
	if err := store.db.QueryRow(`SELECT count(*) FROM response_usage WHERE input_tokens = 100`).Scan(&usageRows); err != nil {
		t.Fatal(err)
	}
	if usageRows != 1 {
		t.Fatalf("migrated usage rows = %d, want 1", usageRows)
	}
}

func TestStateStoreRejectsAnotherApplication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("PRAGMA application_id = 1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = openStateStore(path)
	if err == nil || !strings.Contains(err.Error(), "not a codex-balancer") {
		t.Fatalf("open error = %v, want application mismatch", err)
	}
}

func TestStateStoreRouteRequiresExistingAccount(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.recordRoute(storedRoute{At: time.Now(), Thread: "thread", Account: "missing"})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", "missing")) {
		t.Fatalf("record route error = %v", err)
	}
}
