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
	wantTables := []string{"accounts", "api_keys", "client_identity", "response_usage", "routes"}
	if !reflect.DeepEqual(tables, wantTables) {
		t.Fatalf("tables = %v, want %v", tables, wantTables)
	}

	wantColumns := map[string][]string{
		"accounts":        {"account_id", "id_token", "access_token", "refresh_token", "paused", "routing_mode", "last_refresh_ns", "last_used_at_ns", "reauth"},
		"api_keys":        {"name", "secret", "created_at_ns", "revoked_at_ns", "input_tokens", "cached_tokens", "cache_write_tokens", "output_tokens", "reasoning_tokens"},
		"client_identity": {"id", "key"},
		"response_usage":  {"id", "at_ns", "model", "service_tier", "input_tokens", "cached_tokens", "cache_write_tokens", "output_tokens", "reasoning_tokens"},
		"routes":          {"kind", "key", "account_id", "updated_at_ns"},
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
	owners, err := store.routeOwners("thread", "session")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(owners, []string{"account-b"}) {
		t.Fatalf("owners = %v, want account-b", owners)
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

func TestStateStoreTracksCumulativeUsageOnAPIKey(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	created := time.Now().Add(-time.Hour)
	if err := store.addAPIKey(storedAPIKey{Name: "legacy", Secret: "secret", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}

	first := responseUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 999}
	first.InputDetails.CachedTokens = 30
	first.InputDetails.CacheWriteTokens = 4
	first.OutputDetails.ReasoningTokens = 7
	second := responseUsage{InputTokens: 9, OutputTokens: 3}
	if err := store.recordUsage(storedUsage{At: time.Now().Add(-time.Second), APIKeyName: "legacy", Model: "gpt-5.4", Usage: first}); err != nil {
		t.Fatal(err)
	}
	if err := store.recordUsage(storedUsage{At: time.Now(), APIKeyName: "legacy", Model: "gpt-5.4", ServiceTier: "priority", Usage: second}); err != nil {
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
	var serviceTier string
	if err := store.db.QueryRow(`SELECT service_tier FROM response_usage ORDER BY id LIMIT 1`).Scan(&serviceTier); err != nil {
		t.Fatal(err)
	}
	if serviceTier != "default" {
		t.Fatalf("default service tier stored as %q", serviceTier)
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

func TestPersistentStatsRestoresOnlyCurrentMonthUsage(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prices := testPriceSnapshot(t)
	now := time.Now()
	old := storedUsage{At: calendarMonthStart(now).Add(-time.Second), Model: "gpt-5.4", Usage: responseUsage{InputTokens: 10_000}}
	current := storedUsage{At: now, Model: "gpt-5.4", ServiceTier: "default", Usage: responseUsage{InputTokens: 1_000, OutputTokens: 100}}
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
	var rows int
	if err := store.db.QueryRow(`SELECT count(*) FROM response_usage`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("retained usage rows = %d, want 1", rows)
	}
}

func TestStateStoreClientIdentitySurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := store.clientIDKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.clientIDKey()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || len(got) != 32 {
		t.Fatalf("client identity key changed or has length %d", len(got))
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
