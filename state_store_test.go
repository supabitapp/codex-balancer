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
	want := []string{"accounts", "attempts", "bindings", "client_identity", "events", "price_catalog"}
	if !slices.Equal(tables, want) {
		t.Fatalf("tables = %v, want %v", tables, want)
	}
	key, err := store.clientIDKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 {
		t.Fatalf("client ID key length = %d, want 32", len(key))
	}
	var clientIP, clientID int
	if err := store.db.QueryRow(`SELECT count(*) FROM pragma_table_info('attempts') WHERE name = 'client_ip'`).Scan(&clientIP); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM pragma_table_info('attempts') WHERE name = 'client_id'`).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	if clientIP != 1 || clientID != 0 {
		t.Fatalf("attempt client columns: client_ip = %d, client_id = %d", clientIP, clientID)
	}
}

func TestPoolCyclesRoutingModeAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	account := testAccount("account-a", 20)
	if err := pool.add(account); err != nil {
		t.Fatal(err)
	}
	for _, want := range []routingMode{routingModePriority, routingModeDraining} {
		got, err := pool.cycleRoutingMode(account)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("mode = %q, want %q", got, want)
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
	reloaded, err := reopened.readAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded) != 1 || reloaded[0].routingMode() != routingModeDraining {
		t.Fatalf("reloaded accounts = %+v", reloaded)
	}
}

func TestStateStoreRemovesCompactionRotationSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TABLE settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		rotate_after_compaction INTEGER NOT NULL CHECK (rotate_after_compaction IN (0, 1))
	) STRICT;
	INSERT INTO settings (id, rotate_after_compaction) VALUES (1, 0);
	DROP TABLE price_catalog;
	ALTER TABLE accounts DROP COLUMN routing_mode;
	ALTER TABLE events DROP COLUMN reasoning_effort;
	ALTER TABLE attempts DROP COLUMN client_ip;
	ALTER TABLE attempts ADD COLUMN client_id TEXT NOT NULL DEFAULT '';
	PRAGMA user_version = 10;`); err != nil {
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
	var settings int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'settings'`).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	if settings != 0 {
		t.Fatal("settings table remains")
	}
}

func TestStateStoreMigratesClientIDsToIPs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE price_catalog;
		ALTER TABLE accounts DROP COLUMN routing_mode;
		ALTER TABLE events DROP COLUMN reasoning_effort;
		ALTER TABLE attempts DROP COLUMN client_ip;
		ALTER TABLE attempts ADD COLUMN client_id TEXT NOT NULL DEFAULT '';
		INSERT INTO attempts (at_ns, thread_key, account_id, service_tier, transport, reasoning_effort, turn_metadata, client_id)
		VALUES (1, 'thread', 'account', '', 'http', '', '', '52f3c1d8');
		PRAGMA user_version = 11;`); err != nil {
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
	var clientIP string
	if err := reopened.db.QueryRow(`SELECT client_ip FROM attempts WHERE thread_key = 'thread'`).Scan(&clientIP); err != nil {
		t.Fatal(err)
	}
	if clientIP != "" {
		t.Fatalf("client IP = %q, want empty", clientIP)
	}
	var clientID int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM pragma_table_info('attempts') WHERE name = 'client_id'`).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	if clientID != 0 {
		t.Fatal("client_id column remains")
	}
}

func TestStateStoreAddsAffinityLifecycleToVersionFive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE price_catalog;
		ALTER TABLE accounts DROP COLUMN routing_mode;
		ALTER TABLE bindings DROP COLUMN abandoned_at_ns;
		ALTER TABLE bindings DROP COLUMN last_used_at_ns;
		ALTER TABLE events DROP COLUMN thread_key;
		ALTER TABLE events DROP COLUMN total_tokens;
		ALTER TABLE events DROP COLUMN reasoning_tokens;
		ALTER TABLE events DROP COLUMN reasoning_effort;
		ALTER TABLE attempts DROP COLUMN reasoning_effort;
		ALTER TABLE attempts DROP COLUMN turn_metadata;
		ALTER TABLE attempts DROP COLUMN client_ip;
		ALTER TABLE attempts ADD COLUMN client_id TEXT NOT NULL DEFAULT '';
		PRAGMA user_version = 5;`); err != nil {
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
	var columns int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM pragma_table_info('bindings')
		WHERE name IN ('last_used_at_ns', 'abandoned_at_ns')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 2 {
		t.Fatalf("affinity lifecycle columns = %d, want 2", columns)
	}
}

func TestStateStoreDoesNotTreatLegacyClientIDsAsIPs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE price_catalog;
		ALTER TABLE accounts DROP COLUMN routing_mode;
		ALTER TABLE attempts DROP COLUMN client_ip;
		ALTER TABLE attempts ADD COLUMN client_id TEXT NOT NULL DEFAULT '';
		INSERT INTO attempts (
		at_ns, thread_key, account_id, service_tier, transport, client_id
	) VALUES (1, 'thread', 'account', '', 'http', 'legacy')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`ALTER TABLE bindings DROP COLUMN abandoned_at_ns;
		ALTER TABLE bindings DROP COLUMN last_used_at_ns;
		ALTER TABLE events DROP COLUMN thread_key;
		ALTER TABLE events DROP COLUMN total_tokens;
		ALTER TABLE events DROP COLUMN reasoning_tokens;
		ALTER TABLE events DROP COLUMN reasoning_effort;
		ALTER TABLE attempts DROP COLUMN reasoning_effort;
		ALTER TABLE attempts DROP COLUMN turn_metadata;
		PRAGMA user_version = 4;`); err != nil {
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
	var clientIP string
	if err := reopened.db.QueryRow(`SELECT client_ip FROM attempts WHERE thread_key = 'thread'`).Scan(&clientIP); err != nil {
		t.Fatal(err)
	}
	if clientIP != "" {
		t.Fatalf("client IP = %q, want empty", clientIP)
	}
}

func TestStateStoreRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("PRAGMA user_version = 999"); err != nil {
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
	prices := testPriceSnapshot(t)
	stats, err := newPersistentStats(store, prices, nil)
	if err != nil {
		t.Fatal(err)
	}
	metadata := turnMetadata{RequestKind: "compaction", ThreadID: "codex-thread", TurnID: "codex-turn", SubagentKind: "compact"}
	stats.routed("thread", "203.0.113.41", "account-a", "gpt-5.6-sol", "high", serviceTierFast, transportHTTP, metadata)
	stats.routed("thread", "203.0.113.42", "account-a", "gpt-5.6-sol", "xhigh", serviceTierFast, transportWebSocket, metadata)
	stats.failedOver("account-a", "unreachable")
	stats.rateLimited("account-a")
	stats.answered("thread", "account-a", 2*time.Second)
	stats.completed("thread", "account-a", metadata, 3*time.Second)
	usage := responseUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}
	usage.OutputDetails.ReasoningTokens = 5
	stats.recordUsage("thread", "account-a", "gpt-5.6-sol", "xhigh", serviceTierFast, usage)
	var effort string
	if err := store.db.QueryRow(`SELECT reasoning_effort FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1`, eventResponseUsage).Scan(&effort); err != nil {
		t.Fatal(err)
	}
	if effort != "xhigh" {
		t.Fatalf("stored reasoning effort = %q, want xhigh", effort)
	}
	stats.note("account added", "account-a", "")
	stats.compactionSwitched("codex-thread", "account-a", "account-b")
	stats.websocketOpened("account-a")
	wantCost, _ := prices.estimate("gpt-5.6-sol", serviceTierFast, usage)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := newPersistentStats(reopened, prices, nil)
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
	account := snapshot.Accounts["account-a"]
	if account.Turns != 2 || account.Limited != 1 || activityTotal(account.Activity) != 2 {
		t.Fatalf("account stats = %+v", account)
	}
	if len(snapshot.Threads) != 0 {
		t.Fatalf("restored inactive threads = %+v", snapshot.Threads)
	}
	if len(snapshot.Events) != 4 {
		t.Fatalf("events = %+v", snapshot.Events)
	}
	switchEvent := snapshot.Events[3]
	if switchEvent.Kind != eventCompactionSwitch || switchEvent.Thread != "codex-thread" || switchEvent.SourceAccount != "account-a" || switchEvent.Account != "account-b" {
		t.Fatalf("compaction switch = %+v", switchEvent)
	}
}

func TestStatsRestoreDoesNotMarkHistoricalThreadsLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	prices := testPriceSnapshot(t)
	stats, err := newPersistentStats(store, prices, nil)
	if err != nil {
		t.Fatal(err)
	}
	compaction := turnMetadata{RequestKind: "compaction", ThreadID: "codex-thread", TurnID: "compact-turn"}
	sourceUsage := responseUsage{InputTokens: 100, OutputTokens: 10}
	sourceUsage.InputDetails.CachedTokens = 90
	stats.routed("thread", "client", "account-a", "gpt-5.6-sol", "xhigh", "default", transportWebSocket, compaction)
	stats.answered("thread", "account-a", 100*time.Millisecond)
	stats.completed("thread", "account-a", compaction, time.Second)
	stats.recordUsage("thread", "account-a", "gpt-5.6-sol", "xhigh", "default", sourceUsage)

	target := turnMetadata{RequestKind: "normal", ThreadID: "codex-thread", TurnID: "next-turn"}
	targetUsage := responseUsage{InputTokens: 100, OutputTokens: 20}
	stats.routed("thread", "client", "account-b", "gpt-5.6-sol", "xhigh", "default", transportWebSocket, target)
	stats.compactionSwitched("codex-thread", "account-a", "account-b")
	stats.answered("thread", "account-b", 200*time.Millisecond)
	stats.completed("thread", "account-b", target, 2*time.Second)
	stats.recordUsage("thread", "account-b", "gpt-5.6-sol", "xhigh", "default", targetUsage)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := newPersistentStats(reopened, prices, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := restored.snapshot()
	if len(snapshot.Threads) != 0 {
		t.Fatalf("restored inactive threads = %+v", snapshot.Threads)
	}
	wantMonthly := sourceUsage
	wantMonthly.add(targetUsage)
	if snapshot.MonthlyUsage != wantMonthly {
		t.Fatalf("monthly usage = %+v, want %+v", snapshot.MonthlyUsage, wantMonthly)
	}
}

func TestStatsRestoreUsesCurrentMonthUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	month := time.Now()
	month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, month.Location())
	previousUsage := responseUsage{InputTokens: 1_000}
	currentUsage := responseUsage{InputTokens: 2_000, OutputTokens: 1_000}
	currentUsage.InputDetails.CachedTokens = 1_500
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
	prices := testPriceSnapshot(t)
	stats, err := newPersistentStats(reopened, prices, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := prices.estimate("gpt-5.6-sol", "", currentUsage)
	snapshot := stats.snapshot()
	if snapshot.MonthlyUsage != currentUsage {
		t.Fatalf("monthly usage = %+v, want %+v", snapshot.MonthlyUsage, currentUsage)
	}
	if snapshot.APICostNanoDollars != want || snapshot.UnpricedResponses != 0 {
		t.Fatalf("API estimate = %d with %d unpriced, want %d with none", snapshot.APICostNanoDollars, snapshot.UnpricedResponses, want)
	}
}

func TestStatsRepricesCurrentMonthAfterCatalogRefresh(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stats, err := newPersistentStats(store, priceSnapshot{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	usage := responseUsage{InputTokens: 1_000, OutputTokens: 100}
	stats.recordUsage("thread", "account", "gpt-5.4", "", "default", usage)
	before := stats.snapshot()
	if before.UnpricedResponses != 1 || before.APICostNanoDollars != 0 {
		t.Fatalf("estimate before refresh = %d with %d unpriced", before.APICostNanoDollars, before.UnpricedResponses)
	}
	prices := testPriceSnapshot(t)
	if err := stats.reprice(prices); err != nil {
		t.Fatal(err)
	}
	after := stats.snapshot()
	if after.APICostNanoDollars != 4_000_000 || after.UnpricedResponses != 0 {
		t.Fatalf("estimate after refresh = %d with %d unpriced, want 4000000 with none", after.APICostNanoDollars, after.UnpricedResponses)
	}
	if after.MonthlyUsage != usage || !after.PriceFetchedAt.Equal(prices.fetchedAt) {
		t.Fatalf("refreshed snapshot = %+v", after)
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
