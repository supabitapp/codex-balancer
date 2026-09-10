package state

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMigrateRoutingModesPreservesState(t *testing.T) {
	for _, version := range []int{5, 6} {
		t.Run(fmt.Sprintf("schema%d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			defer db.Close()
			if _, err := db.Exec(fmt.Sprintf("PRAGMA application_id = %d; PRAGMA user_version = %d;", ApplicationID, version) + currentSchema); err != nil {
				t.Fatal(err)
			}
			if version == 6 {
				// Reproduce schema 6's upgrade, including moving routing_mode to
				// the end of the table instead of using the fresh schema's order.
				if _, err := db.Exec(`ALTER TABLE accounts ADD COLUMN next_routing_mode TEXT NOT NULL DEFAULT 'normal'
					CHECK (next_routing_mode IN ('normal', 'priority', 'draining'));
					UPDATE accounts SET next_routing_mode = routing_mode;
					ALTER TABLE accounts DROP COLUMN routing_mode;
					ALTER TABLE accounts RENAME COLUMN next_routing_mode TO routing_mode;`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`INSERT INTO accounts
				(account_id, id_token, access_token, refresh_token, paused, routing_mode, last_refresh_ns, last_used_at_ns, reauth) VALUES
				('a', 'id-a', 'access-a', 'refresh-a', 1, 'normal', 11, 12, 'reauth-a'),
				('b', 'id-b', 'access-b', 'refresh-b', 0, 'priority', 21, 22, ''),
				('c', 'id-c', 'access-c', 'refresh-c', 0, 'normal', 23, 24, '');
				INSERT INTO api_keys VALUES ('client', 'secret', 1, NULL);
				INSERT INTO routes VALUES ('thread', 'c', 31), ('removed-owner', 'removed', 32);
				INSERT INTO response_usage VALUES (1, 'client', 41, 'model', 'default', 10, 2, 1, 4, 1, 'c');
				UPDATE settings SET fast_mode = 'off', admin_password_hash = 'existing-verifier';`); err != nil {
				t.Fatal(err)
			}
			before := &Store{db: db, path: path}
			wantAccounts, err := before.ReadAccounts()
			if err != nil {
				t.Fatal(err)
			}
			wantKeys, err := before.ReadAPIKeys()
			if err != nil {
				t.Fatal(err)
			}
			if version == 6 {
				if _, err := db.Exec(`UPDATE accounts SET routing_mode = 'draining' WHERE account_id = 'c'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			accounts, err := store.ReadAccounts()
			if err != nil || !reflect.DeepEqual(accounts, wantAccounts) {
				t.Fatalf("migration must preserve accounts and map draining to normal: error = %v", err)
			}
			keys, err := store.ReadAPIKeys()
			if err != nil || !reflect.DeepEqual(keys, wantKeys) {
				t.Fatalf("API key state changed during migration: error = %v", err)
			}
			var migratedVersion, routeCount, usageCount int
			if err := store.db.QueryRow("PRAGMA user_version").Scan(&migratedVersion); err != nil || migratedVersion != schemaVersion {
				t.Fatalf("version = %d, error = %v", migratedVersion, err)
			}
			if err := store.db.QueryRow(`SELECT count(*) FROM routes WHERE (key = 'thread' AND account_id = 'c' AND updated_at_ns = 31)
				OR (key = 'removed-owner' AND account_id = 'removed' AND updated_at_ns = 32)`).Scan(&routeCount); err != nil || routeCount != 2 {
				t.Fatalf("routes or tombstones changed: count = %d, error = %v", routeCount, err)
			}
			if err := store.db.QueryRow(`SELECT count(*) FROM response_usage WHERE id = 1 AND account_id = 'c' AND api_key_name = 'client'
				AND at_ns = 41 AND model = 'model' AND service_tier = 'default' AND input_tokens = 10 AND cached_tokens = 2
				AND cache_write_tokens = 1 AND output_tokens = 4 AND reasoning_tokens = 1`).Scan(&usageCount); err != nil || usageCount != 1 {
				t.Fatalf("usage or account attribution changed: count = %d, error = %v", usageCount, err)
			}
			if mode, err := store.FastMode(); err != nil || mode != "off" {
				t.Fatalf("fast mode = %q, error = %v", mode, err)
			}
			if hash, err := store.AdminPasswordHash(); err != nil || hash != "existing-verifier" {
				t.Fatalf("admin verifier changed: error = %v", err)
			}
			for _, mode := range []string{"draining", "invalid"} {
				if _, err := store.db.Exec(`UPDATE accounts SET routing_mode = ? WHERE account_id = 'c'`, mode); err == nil {
					t.Fatalf("migration must reject routing mode %q", mode)
				}
			}
			var violations int
			if err := store.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
				t.Fatalf("foreign-key violations = %d, error = %v", violations, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			accounts, err = reopened.ReadAccounts()
			if err != nil || !reflect.DeepEqual(accounts, wantAccounts) {
				t.Fatalf("migrated accounts changed after reopening: error = %v", err)
			}
		})
	}
}
