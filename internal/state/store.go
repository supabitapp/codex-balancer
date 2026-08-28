package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	ApplicationID = 0x43425853
	schemaVersion = 1
)

const currentSchema = `CREATE TABLE accounts (
	account_id TEXT PRIMARY KEY,
	id_token TEXT NOT NULL,
	access_token TEXT NOT NULL,
	refresh_token TEXT NOT NULL,
	paused INTEGER NOT NULL CHECK (paused IN (0, 1)),
	routing_mode TEXT NOT NULL CHECK (routing_mode IN ('normal', 'priority')),
	last_refresh_ns INTEGER NOT NULL,
	last_used_at_ns INTEGER NOT NULL,
	reauth TEXT NOT NULL
) STRICT;
CREATE TABLE api_keys (
	name TEXT PRIMARY KEY CHECK (length(name) > 0),
	secret TEXT NOT NULL UNIQUE CHECK (length(secret) > 0),
	created_at_ns INTEGER NOT NULL CHECK (created_at_ns > 0),
	revoked_at_ns INTEGER CHECK (revoked_at_ns IS NULL OR revoked_at_ns > 0),
	input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
	cached_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cached_tokens >= 0),
	cache_write_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
	output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
	reasoning_tokens INTEGER NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0)
) STRICT;
CREATE TABLE routes (
	kind TEXT NOT NULL CHECK (kind IN ('thread', 'session')),
	key TEXT NOT NULL CHECK (length(key) > 0),
	account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
	updated_at_ns INTEGER NOT NULL CHECK (updated_at_ns > 0),
	PRIMARY KEY (kind, key)
) STRICT, WITHOUT ROWID;
CREATE INDEX routes_account ON routes (account_id);
CREATE TABLE response_usage (
	id INTEGER PRIMARY KEY,
	at_ns INTEGER NOT NULL CHECK (at_ns > 0),
	model TEXT NOT NULL,
	service_tier TEXT NOT NULL,
	input_tokens INTEGER NOT NULL CHECK (input_tokens >= 0),
	cached_tokens INTEGER NOT NULL CHECK (cached_tokens >= 0),
	cache_write_tokens INTEGER NOT NULL CHECK (cache_write_tokens >= 0),
	output_tokens INTEGER NOT NULL CHECK (output_tokens >= 0),
	reasoning_tokens INTEGER NOT NULL CHECK (reasoning_tokens >= 0)
) STRICT;
CREATE INDEX response_usage_at ON response_usage (at_ns);
CREATE TABLE client_identity (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	key BLOB NOT NULL CHECK (length(key) = 32)
) STRICT;
INSERT INTO client_identity (id, key) VALUES (1, randomblob(32));`

type Store struct {
	db   *sql.DB
	path string
}

type Account struct {
	ID           string
	IDToken      string
	AccessToken  string
	RefreshToken string
	Paused       bool
	RoutingMode  string
	LastRefresh  time.Time
	LastUsed     time.Time
	Reauth       string
}

type Usage struct {
	InputTokens      int64
	CachedTokens     int64
	CacheWriteTokens int64
	OutputTokens     int64
	ReasoningTokens  int64
}

type APIKey struct {
	Name      string
	Secret    string
	CreatedAt time.Time
	RevokedAt time.Time
	Usage     Usage
}

type Route struct {
	At      time.Time
	Session string
	Thread  string
	Account string
}

type UsageEvent struct {
	At          time.Time
	APIKeyName  string
	Model       string
	ServiceTier string
	Usage       Usage
}

func SchemaVersion() int { return schemaVersion }

func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if dir != "." {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return nil, statErr
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, path: path}
	failed := true
	defer func() {
		if !failed {
			return
		}
		db.Close()
		if created {
			os.Remove(path)
			os.Remove(path + "-wal")
			os.Remove(path + "-shm")
		}
	}()
	if err := store.configure(); err != nil {
		return nil, err
	}
	if err := store.initialize(); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	failed = false
	return store, nil
}

func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Path() string { return s.path }
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) configure() error {
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := s.db.Exec(pragma); err != nil {
			return err
		}
	}
	return s.db.Ping()
}

func (s *Store) initialize() error {
	var applicationID, version int
	if err := s.db.QueryRow("PRAGMA application_id").Scan(&applicationID); err != nil {
		return err
	}
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if applicationID == 0 && version == 0 {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(currentSchema); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA application_id = %d", ApplicationID)); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	if applicationID != ApplicationID {
		return fmt.Errorf("%s is not a codex-balancer state database", s.path)
	}
	if version != schemaVersion {
		return fmt.Errorf("state schema %d is unsupported; expected %d", version, schemaVersion)
	}
	return nil
}

func (s *Store) ClientIDKey() ([]byte, error) {
	var key []byte
	if err := s.db.QueryRow(`SELECT key FROM client_identity WHERE id = 1`).Scan(&key); err != nil {
		return nil, err
	}
	return key, nil
}

func (s *Store) ReadAPIKeys() ([]APIKey, error) {
	rows, err := s.db.Query(`SELECT name, secret, created_at_ns, revoked_at_ns, input_tokens, cached_tokens,
		cache_write_tokens, output_tokens, reasoning_tokens FROM api_keys ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		var key APIKey
		var createdAt int64
		var revokedAt sql.NullInt64
		if err := rows.Scan(&key.Name, &key.Secret, &createdAt, &revokedAt, &key.Usage.InputTokens,
			&key.Usage.CachedTokens, &key.Usage.CacheWriteTokens, &key.Usage.OutputTokens,
			&key.Usage.ReasoningTokens); err != nil {
			return nil, err
		}
		key.CreatedAt = decodeTime(createdAt)
		if revokedAt.Valid {
			key.RevokedAt = decodeTime(revokedAt.Int64)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) AddAPIKey(key APIKey) error {
	if key.Name == "" {
		return errors.New("API key has no name")
	}
	if key.Secret == "" {
		return errors.New("API key has no secret")
	}
	if key.CreatedAt.IsZero() {
		return errors.New("API key has no creation time")
	}
	result, err := s.db.Exec(`INSERT INTO api_keys (name, secret, created_at_ns) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET secret = excluded.secret, created_at_ns = excluded.created_at_ns, revoked_at_ns = NULL
		WHERE api_keys.revoked_at_ns IS NOT NULL`, key.Name, key.Secret, encodeTime(key.CreatedAt))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("active API key %q already exists", key.Name)
	}
	return nil
}

func (s *Store) RevokeAPIKey(name string, at time.Time) (bool, error) {
	result, err := s.db.Exec(`UPDATE api_keys SET revoked_at_ns = ? WHERE name = ? AND revoked_at_ns IS NULL`, encodeTime(at), name)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed != 0, err
}

func (s *Store) ReadAccounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT account_id, id_token, access_token, refresh_token, paused, routing_mode,
		last_refresh_ns, last_used_at_ns, reauth FROM accounts ORDER BY account_id`)
	if err != nil {
		return nil, err
	}
	return scanAccounts(rows)
}

func scanAccounts(rows *sql.Rows) ([]Account, error) {
	defer rows.Close()
	var accounts []Account
	for rows.Next() {
		var account Account
		var lastRefresh, lastUsed int64
		if err := rows.Scan(&account.ID, &account.IDToken, &account.AccessToken, &account.RefreshToken,
			&account.Paused, &account.RoutingMode, &lastRefresh, &lastUsed, &account.Reauth); err != nil {
			return nil, err
		}
		account.LastRefresh = decodeTime(lastRefresh)
		account.LastUsed = decodeTime(lastUsed)
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (s *Store) MutateAccounts(change func([]Account) ([]Account, error)) ([]Account, error) {
	var accounts []Account
	err := s.immediate(func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(context.Background(), `SELECT account_id, id_token, access_token, refresh_token,
			paused, routing_mode, last_refresh_ns, last_used_at_ns, reauth FROM accounts ORDER BY account_id`)
		if err != nil {
			return err
		}
		current, err := scanAccounts(rows)
		if err != nil {
			return err
		}
		accounts, err = change(current)
		if err != nil {
			return err
		}
		next := make(map[string]bool, len(accounts))
		for _, account := range accounts {
			if err := upsertAccount(conn, account); err != nil {
				return err
			}
			next[account.ID] = true
		}
		for _, account := range current {
			if next[account.ID] {
				continue
			}
			if _, err := conn.ExecContext(context.Background(), `DELETE FROM accounts WHERE account_id = ?`, account.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return accounts, err
}

func upsertAccount(exec sqlExecer, account Account) error {
	if account.ID == "" {
		return errors.New("account record has no ID")
	}
	_, err := exec.ExecContext(context.Background(), `INSERT INTO accounts (
		account_id, id_token, access_token, refresh_token, paused, routing_mode, last_refresh_ns, last_used_at_ns, reauth
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (account_id) DO UPDATE SET id_token = excluded.id_token, access_token = excluded.access_token,
		refresh_token = excluded.refresh_token, paused = excluded.paused, routing_mode = excluded.routing_mode,
		last_refresh_ns = excluded.last_refresh_ns, last_used_at_ns = excluded.last_used_at_ns, reauth = excluded.reauth`,
		account.ID, account.IDToken, account.AccessToken, account.RefreshToken, account.Paused, account.RoutingMode,
		encodeTime(account.LastRefresh), encodeTime(account.LastUsed), account.Reauth)
	return err
}

func (s *Store) RecordRoute(route Route) error {
	if route.At.IsZero() || route.Account == "" {
		return errors.New("route has no time or account")
	}
	return s.immediate(func(conn *sql.Conn) error {
		result, err := conn.ExecContext(context.Background(), `UPDATE accounts SET last_used_at_ns = max(last_used_at_ns, ?)
			WHERE account_id = ?`, encodeTime(route.At), route.Account)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("route account %q does not exist", route.Account)
		}
		for _, item := range []struct {
			kind string
			key  string
		}{{"thread", route.Thread}, {"session", route.Session}} {
			if item.key == "" {
				continue
			}
			if _, err := conn.ExecContext(context.Background(), `INSERT INTO routes (kind, key, account_id, updated_at_ns)
				VALUES (?, ?, ?, ?) ON CONFLICT (kind, key) DO UPDATE SET account_id = excluded.account_id,
				updated_at_ns = excluded.updated_at_ns WHERE excluded.updated_at_ns >= routes.updated_at_ns`,
				item.kind, item.key, route.Account, encodeTime(route.At)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) RouteOwners(thread, session string) ([]string, error) {
	owners := make([]string, 0, 2)
	for _, item := range []struct {
		kind string
		key  string
	}{{"thread", thread}, {"session", session}} {
		if item.key == "" {
			continue
		}
		var account string
		err := s.db.QueryRow(`SELECT account_id FROM routes WHERE kind = ? AND key = ?`, item.kind, item.key).Scan(&account)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(owners) == 0 || owners[0] != account {
			owners = append(owners, account)
		}
	}
	return owners, nil
}

func (s *Store) RecordUsage(event UsageEvent) error {
	if event.At.IsZero() {
		return errors.New("usage has no time")
	}
	serviceTier := event.ServiceTier
	if serviceTier == "" {
		serviceTier = "default"
	}
	return s.immediate(func(conn *sql.Conn) error {
		if event.APIKeyName != "" {
			result, err := conn.ExecContext(context.Background(), `UPDATE api_keys SET
				input_tokens = input_tokens + ?, cached_tokens = cached_tokens + ?,
				cache_write_tokens = cache_write_tokens + ?, output_tokens = output_tokens + ?,
				reasoning_tokens = reasoning_tokens + ? WHERE name = ?`, event.Usage.InputTokens,
				event.Usage.CachedTokens, event.Usage.CacheWriteTokens, event.Usage.OutputTokens,
				event.Usage.ReasoningTokens, event.APIKeyName)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed == 0 {
				return fmt.Errorf("API key %q does not exist", event.APIKeyName)
			}
		}
		_, err := conn.ExecContext(context.Background(), `INSERT INTO response_usage (
			at_ns, model, service_tier, input_tokens, cached_tokens, cache_write_tokens,
			output_tokens, reasoning_tokens
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, encodeTime(event.At), event.Model,
			serviceTier, event.Usage.InputTokens, event.Usage.CachedTokens, event.Usage.CacheWriteTokens,
			event.Usage.OutputTokens, event.Usage.ReasoningTokens)
		return err
	})
}

func (s *Store) UsageEventsSince(start time.Time) ([]UsageEvent, error) {
	rows, err := s.db.Query(`SELECT at_ns, model, service_tier, input_tokens, cached_tokens,
		cache_write_tokens, output_tokens, reasoning_tokens FROM response_usage WHERE at_ns >= ? ORDER BY id`, encodeTime(start))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []UsageEvent
	for rows.Next() {
		var event UsageEvent
		var at int64
		if err := rows.Scan(&at, &event.Model, &event.ServiceTier, &event.Usage.InputTokens,
			&event.Usage.CachedTokens, &event.Usage.CacheWriteTokens, &event.Usage.OutputTokens,
			&event.Usage.ReasoningTokens); err != nil {
			return nil, err
		}
		event.At = decodeTime(at)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) PruneUsageBefore(cutoff time.Time) error {
	_, err := s.db.Exec(`DELETE FROM response_usage WHERE at_ns < ?`, encodeTime(cutoff))
	return err
}

func (s *Store) APIKeyUsage() (map[string]Usage, error) {
	keys, err := s.ReadAPIKeys()
	if err != nil {
		return nil, err
	}
	usage := make(map[string]Usage, len(keys))
	for _, key := range keys {
		usage[key.Name] = key.Usage
	}
	return usage, nil
}

func (s *Store) immediate(run func(*sql.Conn) error) error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	if err := run(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func encodeTime(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func decodeTime(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value)
}
