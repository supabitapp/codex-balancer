package app

import (
	"fmt"
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
	if applicationID != stateApplicationID || version != stateSchemaVersion {
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
	want := []string{"account_snapshots", "accounts", "api_keys", "attempts", "client_identity", "events", "price_catalog", "response_usage"}
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
	var clientIP, clientID, sessionKey, warmup int
	if err := store.db.QueryRow(`SELECT count(*) FROM pragma_table_info('attempts') WHERE name = 'client_ip'`).Scan(&clientIP); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM pragma_table_info('attempts') WHERE name = 'client_id'`).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM pragma_table_info('attempts') WHERE name = 'session_key'`).Scan(&sessionKey); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM pragma_table_info('attempts') WHERE name = 'warmup'`).Scan(&warmup); err != nil {
		t.Fatal(err)
	}
	if clientIP != 1 || clientID != 0 || sessionKey != 1 || warmup != 1 {
		t.Fatalf("attempt columns: client_ip = %d, client_id = %d, session_key = %d, warmup = %d", clientIP, clientID, sessionKey, warmup)
	}
}

func TestStateStoreMovesLegacyUsageEventsIntoDedicatedTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.addAPIKey(storedAPIKey{Name: "client", Secret: "secret", CreatedAt: time.Unix(0, 1)}); err != nil {
		t.Fatal(err)
	}
	removeUsageMigration(t, store)
	if _, err := store.db.Exec(`INSERT INTO events (
		at_ns, kind, account_id, thread_key, detail, duration_ns, model, reasoning_effort, service_tier,
		input_tokens, cached_tokens, cache_write_tokens, output_tokens, total_tokens, reasoning_tokens
	) VALUES (100, 'response usage', 'account', 'thread', '', 0, 'model', 'high', 'priority', 10, 8, 2, 4, 14, 3);
	PRAGMA user_version = ` + fmt.Sprint(stateSchemaVersion-1)); err != nil {
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
	var key, model, effort, tier string
	var input, cached, cacheWrite, output, total, reasoning int64
	if err := reopened.db.QueryRow(`SELECT api_key_name, model, reasoning_effort, service_tier, input_tokens,
		cached_tokens, cache_write_tokens, output_tokens, total_tokens, reasoning_tokens FROM response_usage`).Scan(
		&key, &model, &effort, &tier, &input, &cached, &cacheWrite, &output, &total, &reasoning); err != nil {
		t.Fatal(err)
	}
	if key != "client" || model != "model" || effort != "high" || tier != "priority" || input != 10 || cached != 8 || cacheWrite != 2 || output != 4 || total != 14 || reasoning != 3 {
		t.Fatalf("migrated usage = key %q, model %q, effort %q, tier %q, tokens %d/%d/%d/%d/%d/%d", key, model, effort, tier, input, cached, cacheWrite, output, total, reasoning)
	}
	var legacyEvents, legacyColumns, accountIndex int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM events WHERE kind = 'response usage'`).Scan(&legacyEvents); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow(`SELECT count(*) FROM pragma_table_info('events') WHERE name IN ('model', 'input_tokens', 'output_tokens')`).Scan(&legacyColumns); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'events_account_at'`).Scan(&accountIndex); err != nil {
		t.Fatal(err)
	}
	if legacyEvents != 0 || legacyColumns != 0 || accountIndex != 0 {
		t.Fatalf("legacy storage remains: events = %d, columns = %d, account index = %d", legacyEvents, legacyColumns, accountIndex)
	}
}

func TestStateStoreRouteOwnersPreferThreadSuccessOverNewerSessionSuccess(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.recordAttempt(storedAttempt{At: time.Unix(0, 1), Session: "session", Thread: "thread-a", Account: "account-a"}); err != nil {
		t.Fatal(err)
	}
	if err := store.recordAttempt(storedAttempt{At: time.Unix(0, 2), Session: "session", Thread: "thread-b", Account: "account-b"}); err != nil {
		t.Fatal(err)
	}
	owners, err := store.routeOwners("thread-a", "session")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(owners); got != "[account-a account-b]" {
		t.Fatalf("route owners = %s, want the thread owner before the newer session owner", got)
	}
}

func TestStateStorePersistsAndRevokesMultipleAPIKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Unix(100, 200)
	for _, key := range []storedAPIKey{
		{Name: "alice", Secret: "secret-a", CreatedAt: createdAt},
		{Name: "bob", Secret: "secret-b", CreatedAt: createdAt.Add(time.Second)},
	} {
		if err := store.addAPIKey(key); err != nil {
			t.Fatal(err)
		}
	}
	if name, valid, err := store.apiKeyName("secret-a"); err != nil || !valid || name != "alice" {
		t.Fatalf("first key = %q, valid = %t, error = %v", name, valid, err)
	}
	if name, valid, err := store.apiKeyName("secret-b"); err != nil || !valid || name != "bob" {
		t.Fatalf("second key = %q, valid = %t, error = %v", name, valid, err)
	}
	if name, valid, err := store.apiKeyName("wrong"); err != nil || valid || name != "" {
		t.Fatalf("wrong key = %q, valid = %t, error = %v", name, valid, err)
	}
	revokedAt := createdAt.Add(2 * time.Second)
	revoked, err := store.revokeAPIKey("alice", revokedAt)
	if err != nil || !revoked {
		t.Fatalf("revoke = %t, error = %v", revoked, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if valid, err := reopened.validAPIKey("secret-a"); err != nil || valid {
		t.Fatalf("revoked key valid = %t, error = %v", valid, err)
	}
	if valid, err := reopened.validAPIKey("secret-b"); err != nil || !valid {
		t.Fatalf("remaining key valid = %t, error = %v", valid, err)
	}
	keys, err := reopened.readAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].Name != "alice" || !keys[0].RevokedAt.Equal(revokedAt) || keys[1].Name != "bob" || !keys[1].RevokedAt.IsZero() {
		t.Fatalf("keys = %+v", keys)
	}
}

func TestStateStoreReprovisionsARevokedAPIKeyName(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	createdAt := time.Unix(100, 0)
	if err := store.addAPIKey(storedAPIKey{Name: "client", Secret: "old-secret", CreatedAt: createdAt}); err != nil {
		t.Fatal(err)
	}
	if err := store.addAPIKey(storedAPIKey{Name: "client", Secret: "duplicate", CreatedAt: createdAt}); err == nil {
		t.Fatal("reprovisioned an active API key name")
	}
	if revoked, err := store.revokeAPIKey("client", createdAt.Add(time.Second)); err != nil || !revoked {
		t.Fatalf("revoke = %t, error = %v", revoked, err)
	}
	if err := store.addAPIKey(storedAPIKey{Name: "client", Secret: "new-secret", CreatedAt: createdAt.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if valid, err := store.validAPIKey("old-secret"); err != nil || valid {
		t.Fatalf("old key valid = %t, error = %v", valid, err)
	}
	if valid, err := store.validAPIKey("new-secret"); err != nil || !valid {
		t.Fatalf("new key valid = %t, error = %v", valid, err)
	}
}

func TestStatsPersistsAcceptedWarmupRouteWithoutCountingATurn(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stats, err := newPersistentStats(store, testPriceSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	stats.accepted("session", "thread", "thread", "203.0.113.1", "account-a", "gpt-5.6-sol", "high", "default", turnMetadata{}, false)
	owners, err := store.routeOwners("thread", "session")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(owners); got != "[account-a]" {
		t.Fatalf("route owners = %s, want the account that accepted the warmup", got)
	}
	if got := stats.snapshot().Turns; got != 0 {
		t.Fatalf("turns = %d, want warmup acceptance excluded from usage stats", got)
	}
}

func TestStatsRestoreKeepsWarmupRouteWithoutTurningItIntoUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := newPersistentStats(store, testPriceSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	stats.accepted("session", "thread", "thread", "203.0.113.1", "account-a", "gpt-5.6-sol", "high", "default", turnMetadata{}, false)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := newPersistentStats(reopened, testPriceSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	owners, err := reopened.routeOwners("thread", "session")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(owners); got != "[account-a]" {
		t.Fatalf("route owners after restart = %s, want the accepted warmup owner", got)
	}
	if got := restored.snapshot().Turns; got != 0 {
		t.Fatalf("restored turns = %d, want warmup route facts excluded after restart", got)
	}
}

func TestStatsRestoreKeepsCountedUsageWhileTheLatestWarmupOwnsRouting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := newPersistentStats(store, testPriceSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	stats.accepted("session", "thread", "thread", "203.0.113.1", "account-a", "gpt-5.6-sol", "high", "default", turnMetadata{}, true)
	stats.accepted("session", "thread", "thread", "203.0.113.1", "account-b", "gpt-5.6-sol", "high", "default", turnMetadata{}, false)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := newPersistentStats(reopened, testPriceSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	owners, err := reopened.routeOwners("thread", "session")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := restored.snapshot()
	if got := fmt.Sprint(owners); got != "[account-b]" {
		t.Fatalf("route owners after restart = %s, want the latest accepted warmup account", got)
	}
	if snapshot.Turns != 1 || snapshot.Accounts["account-a"].Turns != 1 || snapshot.Accounts["account-b"].Turns != 0 {
		t.Fatalf("restored stats = %+v, want warmup routing without counted usage", snapshot.Accounts)
	}
}

func TestStateStoreRouteOwnersKeepOnlyTheLatestSuccessfulMoveAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.recordAttempt(storedAttempt{At: time.Unix(0, 1), Session: "session", Thread: "thread", Account: "account-a"}); err != nil {
		t.Fatal(err)
	}
	if err := store.recordAttempt(storedAttempt{At: time.Unix(0, 2), Session: "session", Thread: "thread", Account: "account-b"}); err != nil {
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
	owners, err := reopened.routeOwners("thread", "session")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(owners); got != "[account-b]" {
		t.Fatalf("route owners after restart = %s, want only the latest accepted account so recovered quota cannot cause a bounce", got)
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
	for _, want := range []routingMode{routingModePriority, routingModeNormal, routingModePriority} {
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
	if len(reloaded) != 1 || reloaded[0].routingCandidate().mode != routingModePriority {
		t.Fatalf("reloaded accounts = %+v", reloaded)
	}
}

func TestStateStoreRemovesLegacyDrainingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.add(testAccount("account-a", 20)); err != nil {
		t.Fatal(err)
	}
	removeRouteFactMigration(t, store)
	if _, err := store.db.Exec(fmt.Sprintf(`ALTER TABLE accounts ADD COLUMN legacy_routing_mode TEXT NOT NULL DEFAULT 'normal' CHECK (legacy_routing_mode IN ('normal', 'priority', 'draining'));
		UPDATE accounts SET legacy_routing_mode = 'draining';
		ALTER TABLE accounts DROP COLUMN routing_mode;
		ALTER TABLE accounts RENAME COLUMN legacy_routing_mode TO routing_mode;
		PRAGMA user_version = %d;`, stateSchemaVersion-5)); err != nil {
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
	accounts, err := reopened.readAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].routingCandidate().mode != routingModeNormal {
		t.Fatalf("accounts = %+v", accounts)
	}
	if _, err := reopened.db.Exec(`UPDATE accounts SET routing_mode = 'draining'`); err == nil {
		t.Fatal("legacy routing mode accepted")
	}
}

func TestStateStoreRemovesCompactionRotationSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	removeRouteFactMigration(t, store)
	if _, err := store.db.Exec(`CREATE TABLE settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		rotate_after_compaction INTEGER NOT NULL CHECK (rotate_after_compaction IN (0, 1))
	) STRICT;
	INSERT INTO settings (id, rotate_after_compaction) VALUES (1, 0);
	DROP TABLE price_catalog;
	ALTER TABLE accounts DROP COLUMN routing_mode;
	ALTER TABLE events DROP COLUMN reasoning_effort;
	ALTER TABLE attempts ADD COLUMN transport TEXT NOT NULL DEFAULT 'ws';
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
	removeRouteFactMigration(t, store)
	if _, err := store.db.Exec(`DROP TABLE price_catalog;
		ALTER TABLE accounts DROP COLUMN routing_mode;
	ALTER TABLE events DROP COLUMN reasoning_effort;
	ALTER TABLE attempts ADD COLUMN transport TEXT NOT NULL DEFAULT 'ws';
	ALTER TABLE attempts DROP COLUMN client_ip;
		ALTER TABLE attempts ADD COLUMN client_id TEXT NOT NULL DEFAULT '';
		INSERT INTO attempts (at_ns, thread_key, account_id, service_tier, transport, reasoning_effort, turn_metadata, client_id)
		VALUES (1, 'thread', 'account', '', 'ws', '', '', '52f3c1d8');
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
	owners, err := reopened.routeOwners("thread", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(owners); got != "[account]" {
		t.Fatalf("route owners after legacy migration = %s, want old successful attempts to seed thread retention", got)
	}
}

func TestStateStoreRemovesLegacyAffinityAndRotationData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	removeRouteFactMigration(t, store)
	if _, err := store.db.Exec(`CREATE TABLE bindings (
		id INTEGER PRIMARY KEY,
		kind TEXT NOT NULL,
		value TEXT NOT NULL,
		account_id TEXT NOT NULL,
		created_at_ns INTEGER NOT NULL,
		last_used_at_ns INTEGER NOT NULL,
		abandoned_at_ns INTEGER,
		UNIQUE (kind, value)
	) STRICT;
	INSERT INTO bindings (kind, value, account_id, created_at_ns, last_used_at_ns, abandoned_at_ns)
		VALUES ('session', 'legacy-session', 'account-a', 1, 1, NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, event := range []storedEvent{
		{At: time.Unix(0, 2), Kind: "compaction switch", Account: "account-a"},
		{At: time.Unix(0, 3), Kind: "rotation reconnect", Account: "account-a"},
		{At: time.Unix(0, 4), Kind: "rotated", Account: "account-a"},
		{At: time.Unix(0, 5), Kind: eventResponseCompleted, Account: "account-a", Thread: "thread", Detail: "compaction"},
		{At: time.Unix(0, 6), Kind: eventFailover, Account: "account-a", Detail: "kept"},
	} {
		if err := store.recordEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", stateSchemaVersion-6)); err != nil {
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
	var bindings int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'bindings'`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatal("bindings table remains")
	}
	var legacy int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM events WHERE kind IN ('compaction switch', 'rotation reconnect', 'rotated')`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Fatalf("legacy rotation events = %d, want 0", legacy)
	}
	var normal, failover int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM events WHERE kind = ? AND detail = 'compaction'`, eventResponseCompleted).Scan(&normal); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow(`SELECT count(*) FROM events WHERE kind = ? AND detail = 'kept'`, eventFailover).Scan(&failover); err != nil {
		t.Fatal(err)
	}
	if normal != 1 || failover != 1 {
		t.Fatalf("preserved events = compaction %d, failover %d", normal, failover)
	}
}

func TestStateStoreDoesNotTreatLegacyClientIDsAsIPs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	removeRouteFactMigration(t, store)
	if _, err := store.db.Exec(`DROP TABLE price_catalog;
	ALTER TABLE accounts DROP COLUMN routing_mode;
	CREATE TABLE bindings (
		id INTEGER PRIMARY KEY,
		kind TEXT NOT NULL,
		value TEXT NOT NULL,
		account_id TEXT NOT NULL,
		created_at_ns INTEGER NOT NULL,
		last_used_at_ns INTEGER NOT NULL,
		abandoned_at_ns INTEGER,
		UNIQUE (kind, value)
	) STRICT;
	ALTER TABLE attempts ADD COLUMN transport TEXT NOT NULL DEFAULT 'ws';
	ALTER TABLE attempts DROP COLUMN client_ip;
		ALTER TABLE attempts ADD COLUMN client_id TEXT NOT NULL DEFAULT '';
		INSERT INTO attempts (
		at_ns, thread_key, account_id, service_tier, transport, client_id
	) VALUES (1, 'thread', 'account', '', 'ws', 'legacy')`); err != nil {
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

func removeRouteFactMigration(t *testing.T, store *StateStore) {
	t.Helper()
	removeUsageMigration(t, store)
	if _, err := store.db.Exec(`DROP TABLE api_keys;
		DROP TABLE account_snapshots;
		ALTER TABLE accounts DROP COLUMN reauth;
		DROP INDEX attempts_session_at;
		ALTER TABLE attempts DROP COLUMN session_key;
		ALTER TABLE attempts DROP COLUMN warmup;`); err != nil {
		t.Fatal(err)
	}
}

func removeUsageMigration(t *testing.T, store *StateStore) {
	t.Helper()
	if _, err := store.db.Exec(`DROP TABLE response_usage;
		ALTER TABLE events ADD COLUMN model TEXT NOT NULL DEFAULT '';
		ALTER TABLE events ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT '';
		ALTER TABLE events ADD COLUMN service_tier TEXT NOT NULL DEFAULT '';
		ALTER TABLE events ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE events ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE events ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE events ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE events ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE events ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0;
		CREATE INDEX events_account_at ON events (account_id, at_ns);
		DROP INDEX attempts_session_at;
		CREATE INDEX attempts_session_at ON attempts (session_key, at_ns);`); err != nil {
		t.Fatal(err)
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
	statsThread := statsThreadKey("thread", metadata)
	stats.accepted("", "thread", statsThread, "203.0.113.41", "account-a", "gpt-5.6-sol", "high", serviceTierFast, metadata, true)
	stats.accepted("", "thread", statsThread, "203.0.113.42", "account-a", "gpt-5.6-sol", "xhigh", serviceTierFast, metadata, true)
	stats.failedOver("account-a", "unreachable")
	stats.rateLimited("account-a")
	stats.answered(statsThread, "account-a", 2*time.Second)
	stats.completed(statsThread, "account-a", metadata, 3*time.Second)
	usage := responseUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}
	usage.OutputDetails.ReasoningTokens = 5
	stats.recordUsage(statsThread, "account-a", "gpt-5.6-sol", "xhigh", serviceTierFast, usage)
	var effort string
	if err := store.db.QueryRow(`SELECT reasoning_effort FROM response_usage ORDER BY id DESC LIMIT 1`).Scan(&effort); err != nil {
		t.Fatal(err)
	}
	if effort != "xhigh" {
		t.Fatalf("stored reasoning effort = %q, want xhigh", effort)
	}
	stats.note("account added", "account-a", "")
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
	if snapshot.Turns != 2 || snapshot.Failures != 1 || snapshot.Limited != 1 {
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
	if len(snapshot.Events) != 3 {
		t.Fatalf("events = %+v", snapshot.Events)
	}
	if snapshot.Events[2].Kind != "account added" || snapshot.Events[2].Account != "account-a" {
		t.Fatalf("account event = %+v", snapshot.Events[2])
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
	statsThread := statsThreadKey("thread", compaction)
	sourceUsage := responseUsage{InputTokens: 100, OutputTokens: 10}
	sourceUsage.InputDetails.CachedTokens = 90
	stats.accepted("", "thread", statsThread, "client", "account-a", "gpt-5.6-sol", "xhigh", "default", compaction, true)
	stats.answered(statsThread, "account-a", 100*time.Millisecond)
	stats.completed(statsThread, "account-a", compaction, time.Second)
	stats.recordUsage(statsThread, "account-a", "gpt-5.6-sol", "xhigh", "default", sourceUsage)

	target := turnMetadata{RequestKind: "normal", ThreadID: "codex-thread", TurnID: "next-turn"}
	targetUsage := responseUsage{InputTokens: 100, OutputTokens: 20}
	stats.accepted("", "thread", statsThread, "client", "account-b", "gpt-5.6-sol", "xhigh", "default", target, true)
	stats.answered(statsThread, "account-b", 200*time.Millisecond)
	stats.completed(statsThread, "account-b", target, 2*time.Second)
	stats.recordUsage(statsThread, "account-b", "gpt-5.6-sol", "xhigh", "default", targetUsage)
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
		{At: month.Add(-time.Second), Model: "gpt-5.6-sol", Usage: previousUsage},
		{At: month.Add(time.Second), Model: "gpt-5.6-sol", Usage: currentUsage},
	} {
		if err := store.recordUsage(event); err != nil {
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
	if err := store.recordAttempt(storedAttempt{At: want, Account: "account-a"}); err != nil {
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
