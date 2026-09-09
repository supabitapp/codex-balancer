package state

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMigrateDrainingModePreservesState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	legacySchema := strings.Replace(currentSchema, "'normal', 'priority', 'draining'", "'normal', 'priority'", 1)
	if strings.Contains(legacySchema, "'draining'") {
		t.Fatal("legacy fixture must exclude draining")
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA application_id = %d; PRAGMA user_version = 5;", ApplicationID) + legacySchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO accounts VALUES
		('a', 'id-a', 'access-a', 'refresh-a', 1, 'normal', 11, 12, 'reauth-a'),
		('b', 'id-b', 'access-b', 'refresh-b', 0, 'priority', 21, 22, '');
		INSERT INTO api_keys VALUES ('client', 'secret', 1, NULL);
		INSERT INTO routes VALUES ('thread', 'a', 31), ('removed-owner', 'removed', 32);
		INSERT INTO response_usage VALUES (1, 'client', 41, 'model', 'default', 10, 2, 1, 4, 1, 'a');
		UPDATE settings SET fast_mode = 'off', admin_password_hash = 'existing-verifier';`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE accounts SET routing_mode = 'draining' WHERE account_id = 'a'`); err == nil {
		t.Fatal("schema 5 must reject draining")
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
		t.Fatalf("account state changed during migration: error = %v", err)
	}
	keys, err := store.ReadAPIKeys()
	if err != nil || !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("API key state changed during migration: error = %v", err)
	}
	var version, routeCount, usageCount int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 6 {
		t.Fatalf("version = %d, error = %v", version, err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM routes WHERE (key = 'thread' AND account_id = 'a' AND updated_at_ns = 31)
		OR (key = 'removed-owner' AND account_id = 'removed' AND updated_at_ns = 32)`).Scan(&routeCount); err != nil || routeCount != 2 {
		t.Fatalf("routes or tombstones changed: count = %d, error = %v", routeCount, err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM response_usage WHERE id = 1 AND account_id = 'a' AND api_key_name = 'client'
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
	if _, err := store.db.Exec(`UPDATE accounts SET routing_mode = 'draining' WHERE account_id = 'a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE accounts SET routing_mode = 'invalid' WHERE account_id = 'b'`); err == nil {
		t.Fatal("migration must preserve the mode constraint")
	}
	rows, err := store.db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("migration introduced a foreign-key violation")
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
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
	if err != nil || len(accounts) != 2 || accounts[0].RoutingMode != "draining" || accounts[1].RoutingMode != "priority" {
		t.Fatalf("draining mode did not survive reopening: error = %v", err)
	}
}
