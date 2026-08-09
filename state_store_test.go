package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestStateStoreCreatesCurrentSchema(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"), "", "")
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
	if applicationID != stateApplicationID || version != len(stateMigrations) {
		t.Fatalf("application = %x, version = %d", applicationID, version)
	}
	rows, err := store.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
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
	want := []string{"accounts", "attempts", "bindings", "events"}
	if !slices.Equal(tables, want) {
		t.Fatalf("tables = %v, want %v", tables, want)
	}
}

func TestStateStoreRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openStateStore(path, "", ""); err == nil {
		t.Fatal("newer schema opened")
	}
}

func TestStateStoreRejectsAnotherApplication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("PRAGMA application_id = 1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openStateStore(path, "", ""); err == nil {
		t.Fatal("unrelated database opened")
	}
}

func TestStateStoreImportsLegacyJSONOnce(t *testing.T) {
	dir := t.TempDir()
	accountsPath := filepath.Join(dir, "accounts.json")
	affinityPath := filepath.Join(dir, "affinity.json")
	account := testAccount("account-a", 10)
	account.Paused = true
	writeTestJSON(t, accountsPath, []*Account{account})
	soft := affinityRef{kind: affinitySession, value: "session"}
	hard := affinityRef{kind: affinityResponse, value: "response"}
	writeTestJSON(t, affinityPath, legacyAffinityState{
		Soft: map[string]legacySoftBinding{
			soft.storageKey(): {Account: "account-a", Seen: time.Now()},
		},
		Hard:      map[string]string{hard.storageKey(): "account-a"},
		HardOrder: []string{hard.storageKey()},
	})
	path := filepath.Join(dir, "state.db")
	store, err := openStateStore(path, accountsPath, affinityPath)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := store.readAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].id() != "account-a" || !accounts[0].paused() {
		t.Fatalf("accounts = %+v", accounts)
	}
	affinity := &AffinityStore{store: store}
	if affinity.lookup(soft) != "account-a" || affinity.lookup(hard) != "account-a" {
		t.Fatal("bindings were not imported")
	}
	for _, legacy := range []string{accountsPath, affinityPath} {
		if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy file remains: %s", legacy)
		}
	}
	for _, backup := range []string{"accounts.json", "affinity.json"} {
		if _, err := os.Stat(filepath.Join(dir, "legacy-backup", backup)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, accountsPath, []*Account{testAccount("account-b", 20)})
	reopened, err := openStateStore(path, accountsPath, affinityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	accounts, err = reopened.readAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].id() != "account-a" {
		t.Fatalf("legacy data imported twice: %+v", accounts)
	}
}

func TestStateStoreRemovesNewDatabaseAfterFailedImport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	legacy := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(legacy, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openStateStore(path, legacy, ""); err == nil {
		t.Fatal("invalid import succeeded")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed database remains: %v", err)
	}
}

func TestStatsRestoreFromRawFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	stats, err := newPersistentStats(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats.routed("thread", "account-a", serviceTierFast, transportHTTP)
	stats.routed("thread", "account-a", serviceTierFast, transportWebSocket)
	stats.failedOver("account-a", "unreachable")
	stats.rateLimited("account-a")
	stats.answered(2 * time.Second)
	usage := responseUsage{InputTokens: 10, OutputTokens: 20}
	stats.recordUsage("gpt-5.6-sol", serviceTierFast, usage)
	stats.note("account added", "account-a", "")
	stats.websocketOpened("account-a")
	wantCost, _ := estimateAPIPrice("gpt-5.6-sol", serviceTierFast, usage)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStateStore(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := newPersistentStats(reopened, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := restored.snapshot()
	if snapshot.Turns != 2 || snapshot.WSTurns != 1 || snapshot.Failures != 1 || snapshot.Limited != 1 {
		t.Fatalf("totals = %+v", snapshot)
	}
	if snapshot.TTFB != 2*time.Second || snapshot.APICostNanoDollars != wantCost || snapshot.UnpricedResponses != 0 {
		t.Fatalf("derived values = %+v", snapshot)
	}
	if snapshot.WSOpen != 0 || snapshot.Accounts["account-a"].WSOpen != 0 {
		t.Fatalf("live sockets survived restart: %+v", snapshot)
	}
	if len(snapshot.Threads) != 1 || snapshot.Threads[0].Turns != 2 || snapshot.Threads[0].Via != transportWebSocket {
		t.Fatalf("threads = %+v", snapshot.Threads)
	}
	if len(snapshot.Events) != 3 {
		t.Fatalf("events = %+v", snapshot.Events)
	}
}

func TestPoolDerivesLastUsedFromAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.add(testAccount("account-a", 10)); err != nil {
		t.Fatal(err)
	}
	want := time.Now().Add(-time.Minute).Round(0)
	if err := store.recordAttempt(storedAttempt{At: want, Account: "account-a", Transport: transportHTTP}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStateStore(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	pool, err = loadPool(reopened)
	if err != nil {
		t.Fatal(err)
	}
	account := pool.find("account-a")
	account.mu.Lock()
	got := account.lastUsed
	account.mu.Unlock()
	if !got.Equal(want) {
		t.Fatalf("last used = %s, want %s", got, want)
	}
}

func TestPoolReloadsExternalChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	serverStore, err := openStateStore(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer serverStore.Close()
	serverPool, err := loadPool(serverStore)
	if err != nil {
		t.Fatal(err)
	}
	commandStore, err := openStateStore(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer commandStore.Close()
	commandPool, err := loadPool(commandStore)
	if err != nil {
		t.Fatal(err)
	}
	if err := commandPool.add(testAccount("account-a", 10)); err != nil {
		t.Fatal(err)
	}
	change, err := serverPool.reload()
	if err != nil {
		t.Fatal(err)
	}
	if change.added != 1 || serverPool.find("account-a") == nil {
		t.Fatalf("add change = %+v", change)
	}
	if _, err := commandPool.togglePause(commandPool.find("account-a")); err != nil {
		t.Fatal(err)
	}
	change, err = serverPool.reload()
	if err != nil {
		t.Fatal(err)
	}
	if change.updated != 1 || !serverPool.find("account-a").paused() {
		t.Fatalf("pause change = %+v", change)
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
