package main

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestStateStoreCreatesCurrentSchema(t *testing.T) {
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
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openStateStore(path); err == nil {
		t.Fatal("newer schema opened")
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
	if _, err := openStateStore(path); err == nil {
		t.Fatal("unrelated database opened")
	}
}

func TestStatsRestoreFromRawFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
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
	reopened, err := openStateStore(path)
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

func TestStatsRestoreUsesCurrentMonthAPIUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	month := time.Now()
	month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, month.Location())
	previousUsage := responseUsage{InputTokens: 1_000}
	currentUsage := responseUsage{OutputTokens: 1_000}
	for _, event := range []storedEvent{
		{At: month.Add(-time.Second), Kind: eventResponseUsage, Model: "gpt-5.6-sol", Usage: previousUsage},
		{At: month.Add(time.Second), Kind: eventResponseUsage, Model: "gpt-5.6-sol", Usage: currentUsage},
	} {
		if err := store.recordEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stats, err := newPersistentStats(reopened, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := estimateAPIPrice("gpt-5.6-sol", "", currentUsage)
	snapshot := stats.snapshot()
	if snapshot.APICostNanoDollars != want || snapshot.UnpricedResponses != 0 {
		t.Fatalf("API estimate = %d with %d unpriced, want %d with none", snapshot.APICostNanoDollars, snapshot.UnpricedResponses, want)
	}
}

func TestPoolDerivesLastUsedFromAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
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
	reopened, err := openStateStore(path)
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
	serverStore, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer serverStore.Close()
	serverPool, err := loadPool(serverStore)
	if err != nil {
		t.Fatal(err)
	}
	commandStore, err := openStateStore(path)
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
