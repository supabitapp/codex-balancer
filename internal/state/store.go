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

const ApplicationID = 0x43425853

var stateMigrations = []string{
	`CREATE TABLE accounts (
		account_id TEXT PRIMARY KEY,
		id_token TEXT NOT NULL,
		access_token TEXT NOT NULL,
		refresh_token TEXT NOT NULL,
		paused INTEGER NOT NULL CHECK (paused IN (0, 1)),
		last_refresh_ns INTEGER NOT NULL
	) STRICT;
	CREATE TABLE bindings (
		id INTEGER PRIMARY KEY,
		kind TEXT NOT NULL,
		value TEXT NOT NULL,
		account_id TEXT NOT NULL,
		created_at_ns INTEGER NOT NULL,
		UNIQUE (kind, value)
	) STRICT;
	CREATE TABLE attempts (
		id INTEGER PRIMARY KEY,
		at_ns INTEGER NOT NULL,
		thread_key TEXT NOT NULL,
		account_id TEXT NOT NULL,
		service_tier TEXT NOT NULL,
		transport TEXT NOT NULL
	) STRICT;
	CREATE INDEX attempts_account_at ON attempts (account_id, at_ns);
	CREATE INDEX attempts_thread_at ON attempts (thread_key, at_ns);
	CREATE TABLE events (
		id INTEGER PRIMARY KEY,
		at_ns INTEGER NOT NULL,
		kind TEXT NOT NULL,
		account_id TEXT NOT NULL,
		detail TEXT NOT NULL,
		duration_ns INTEGER NOT NULL,
		model TEXT NOT NULL,
		service_tier TEXT NOT NULL,
		input_tokens INTEGER NOT NULL,
		cached_tokens INTEGER NOT NULL,
		cache_write_tokens INTEGER NOT NULL,
		output_tokens INTEGER NOT NULL
	) STRICT;
	CREATE INDEX events_kind_at ON events (kind, at_ns);
	CREATE INDEX events_account_at ON events (account_id, at_ns);`,
	`ALTER TABLE attempts ADD COLUMN client_ip TEXT NOT NULL DEFAULT '';`,
	`ALTER TABLE attempts DROP COLUMN client_ip;
	ALTER TABLE attempts ADD COLUMN client_id TEXT NOT NULL DEFAULT '';`,
	`CREATE TABLE client_identity (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		key BLOB NOT NULL CHECK (length(key) = 32)
	) STRICT;
	INSERT INTO client_identity (id, key) VALUES (1, randomblob(32));`,
	`UPDATE attempts SET client_id = '';`,
	`ALTER TABLE bindings ADD COLUMN last_used_at_ns INTEGER NOT NULL DEFAULT 0;
	UPDATE bindings SET last_used_at_ns = created_at_ns;
	ALTER TABLE bindings ADD COLUMN abandoned_at_ns INTEGER;`,
	`ALTER TABLE events ADD COLUMN thread_key TEXT NOT NULL DEFAULT '';`,
	`ALTER TABLE attempts ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT '';`,
	`ALTER TABLE attempts ADD COLUMN turn_metadata TEXT NOT NULL DEFAULT '';
	ALTER TABLE events ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE events ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0;`,
	`SELECT 1;`,
	`DROP TABLE IF EXISTS settings;`,
	`ALTER TABLE attempts DROP COLUMN client_id;
	ALTER TABLE attempts ADD COLUMN client_ip TEXT NOT NULL DEFAULT '';`,
	`CREATE TABLE price_catalog (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		fetched_at_ns INTEGER NOT NULL CHECK (fetched_at_ns > 0),
		payload BLOB NOT NULL CHECK (length(payload) > 0)
	) STRICT;`,
	`ALTER TABLE accounts ADD COLUMN routing_mode TEXT NOT NULL DEFAULT 'normal' CHECK (routing_mode IN ('normal', 'priority', 'draining'));`,
	`ALTER TABLE events ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT '';`,
	`ALTER TABLE attempts DROP COLUMN transport;`,
	`DROP TABLE IF EXISTS bindings;
	DELETE FROM events WHERE kind IN ('compaction switch', 'rotation reconnect', 'rotated');`,
	`ALTER TABLE accounts ADD COLUMN next_routing_mode TEXT NOT NULL DEFAULT 'normal' CHECK (next_routing_mode IN ('normal', 'priority'));
	UPDATE accounts SET next_routing_mode = CASE routing_mode WHEN 'priority' THEN 'priority' ELSE 'normal' END;
	ALTER TABLE accounts DROP COLUMN routing_mode;
	ALTER TABLE accounts RENAME COLUMN next_routing_mode TO routing_mode;`,
	`ALTER TABLE attempts ADD COLUMN session_key TEXT NOT NULL DEFAULT '';
	ALTER TABLE attempts ADD COLUMN warmup INTEGER NOT NULL DEFAULT 0 CHECK (warmup IN (0, 1));
	CREATE INDEX attempts_session_at ON attempts (session_key, at_ns);`,
	`ALTER TABLE accounts ADD COLUMN reauth TEXT NOT NULL DEFAULT '';
	CREATE TABLE account_snapshots (
		account_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		fetched_at_ns INTEGER NOT NULL CHECK (fetched_at_ns > 0),
		version TEXT NOT NULL,
		payload BLOB NOT NULL CHECK (length(payload) > 0),
		PRIMARY KEY (account_id, kind)
	) STRICT;`,
	`CREATE TABLE api_keys (
		name TEXT PRIMARY KEY CHECK (length(name) > 0),
		secret TEXT NOT NULL UNIQUE CHECK (length(secret) > 0),
		created_at_ns INTEGER NOT NULL CHECK (created_at_ns > 0),
		revoked_at_ns INTEGER
	) STRICT;`,
}

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
	Reauth       string
}

type Attempt struct {
	At          time.Time
	Session     string
	Thread      string
	ClientIP    string
	Account     string
	Effort      string
	ServiceTier string
	Metadata    string
	Warmup      bool
}

type Usage struct {
	InputTokens      int64
	CachedTokens     int64
	CacheWriteTokens int64
	OutputTokens     int64
	TotalTokens      int64
	ReasoningTokens  int64
}

type Event struct {
	At          time.Time
	Kind        string
	Account     string
	Thread      string
	Detail      string
	Duration    time.Duration
	Model       string
	Effort      string
	ServiceTier string
	Usage       Usage
}

type AccountSnapshot struct {
	Account   string
	Kind      string
	FetchedAt time.Time
	Version   string
	Payload   []byte
}

type APIKey struct {
	Name      string
	Secret    string
	CreatedAt time.Time
	RevokedAt time.Time
}

func SchemaVersion() int { return len(stateMigrations) }

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
	if err := store.migrate(); err != nil {
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

func (s *Store) migrate() error {
	var applicationID, version int
	if err := s.db.QueryRow("PRAGMA application_id").Scan(&applicationID); err != nil {
		return err
	}
	if applicationID != 0 && applicationID != ApplicationID {
		return fmt.Errorf("%s is not a codex-balancer state database", s.path)
	}
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	currentVersion := SchemaVersion()
	if version > currentVersion {
		return fmt.Errorf("state schema %d is newer than supported schema %d", version, currentVersion)
	}
	for version < currentVersion {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(stateMigrations[version]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate state to version %d: %w", version+1, err)
		}
		if applicationID == 0 {
			if _, err := tx.Exec(fmt.Sprintf("PRAGMA application_id = %d", ApplicationID)); err != nil {
				tx.Rollback()
				return err
			}
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		applicationID = ApplicationID
		version++
	}
	if applicationID == 0 {
		_, err := s.db.Exec(fmt.Sprintf("PRAGMA application_id = %d", ApplicationID))
		return err
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
	rows, err := s.db.Query(`SELECT name, secret, created_at_ns, revoked_at_ns FROM api_keys ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		var key APIKey
		var createdAt int64
		var revokedAt sql.NullInt64
		if err := rows.Scan(&key.Name, &key.Secret, &createdAt, &revokedAt); err != nil {
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

func (s *Store) LoadPriceCatalog() (time.Time, []byte, error) {
	var fetchedAt int64
	var payload []byte
	if err := s.db.QueryRow(`SELECT fetched_at_ns, payload FROM price_catalog WHERE id = 1`).Scan(&fetchedAt, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, nil, nil
		}
		return time.Time{}, nil, err
	}
	return time.Unix(0, fetchedAt), payload, nil
}

func (s *Store) SavePriceCatalog(fetchedAt time.Time, payload []byte) error {
	_, err := s.db.Exec(`INSERT INTO price_catalog (id, fetched_at_ns, payload) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET fetched_at_ns = excluded.fetched_at_ns, payload = excluded.payload`, fetchedAt.UnixNano(), payload)
	return err
}

func (s *Store) ReadAccountSnapshots(kind string) ([]AccountSnapshot, error) {
	rows, err := s.db.Query(`SELECT account_id, kind, fetched_at_ns, version, payload FROM account_snapshots WHERE kind = ? ORDER BY account_id`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []AccountSnapshot
	for rows.Next() {
		var snapshot AccountSnapshot
		var fetchedAt int64
		if err := rows.Scan(&snapshot.Account, &snapshot.Kind, &fetchedAt, &snapshot.Version, &snapshot.Payload); err != nil {
			return nil, err
		}
		snapshot.FetchedAt = decodeTime(fetchedAt)
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}

func (s *Store) SaveAccountSnapshot(snapshot AccountSnapshot) error {
	_, err := s.db.Exec(`INSERT INTO account_snapshots (account_id, kind, fetched_at_ns, version, payload) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (account_id, kind) DO UPDATE SET fetched_at_ns = excluded.fetched_at_ns, version = excluded.version, payload = excluded.payload`,
		snapshot.Account, snapshot.Kind, encodeTime(snapshot.FetchedAt), snapshot.Version, snapshot.Payload)
	return err
}

func (s *Store) ReadAccounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT account_id, id_token, access_token, refresh_token, paused, routing_mode, last_refresh_ns, reauth FROM accounts ORDER BY account_id`)
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
		var lastRefresh int64
		if err := rows.Scan(&account.ID, &account.IDToken, &account.AccessToken, &account.RefreshToken, &account.Paused, &account.RoutingMode, &lastRefresh, &account.Reauth); err != nil {
			return nil, err
		}
		account.LastRefresh = decodeTime(lastRefresh)
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (s *Store) LastUsed() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT account_id, MAX(at_ns) FROM attempts GROUP BY account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lastUsed := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at int64
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		lastUsed[id] = time.Unix(0, at)
	}
	return lastUsed, rows.Err()
}

func (s *Store) MutateAccounts(change func([]Account) ([]Account, error)) ([]Account, error) {
	var accounts []Account
	err := s.immediate(func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(context.Background(), `SELECT account_id, id_token, access_token, refresh_token, paused, routing_mode, last_refresh_ns, reauth FROM accounts ORDER BY account_id`)
		if err != nil {
			return err
		}
		accounts, err = scanAccounts(rows)
		if err != nil {
			return err
		}
		accounts, err = change(accounts)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(context.Background(), "DELETE FROM accounts"); err != nil {
			return err
		}
		for _, account := range accounts {
			if err := insertAccount(conn, account); err != nil {
				return err
			}
		}
		_, err = conn.ExecContext(context.Background(), `DELETE FROM account_snapshots WHERE account_id NOT IN (SELECT account_id FROM accounts)`)
		return err
	})
	return accounts, err
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

func insertAccount(exec sqlExecer, account Account) error {
	if account.ID == "" {
		return errors.New("account record has no ID")
	}
	_, err := exec.ExecContext(context.Background(), `INSERT INTO accounts (
		account_id, id_token, access_token, refresh_token, paused, routing_mode, last_refresh_ns, reauth
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, account.ID, account.IDToken, account.AccessToken, account.RefreshToken,
		account.Paused, account.RoutingMode, encodeTime(account.LastRefresh), account.Reauth)
	return err
}

func (s *Store) RecordAttempt(attempt Attempt) error {
	_, err := s.db.Exec(`INSERT INTO attempts (at_ns, session_key, thread_key, client_ip, account_id, reasoning_effort, service_tier, turn_metadata, warmup) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attempt.At.UnixNano(), attempt.Session, attempt.Thread, attempt.ClientIP, attempt.Account, attempt.Effort, attempt.ServiceTier, attempt.Metadata, attempt.Warmup)
	return err
}

func (s *Store) RouteOwners(thread, session string) ([]string, error) {
	lookups := []struct {
		column string
		value  string
	}{
		{column: "thread_key", value: thread},
		{column: "session_key", value: session},
	}
	owners := make([]string, 0, len(lookups))
	for _, lookup := range lookups {
		if lookup.value == "" {
			continue
		}
		var account string
		err := s.db.QueryRow(`SELECT account_id FROM attempts WHERE `+lookup.column+` = ? ORDER BY at_ns DESC, id DESC LIMIT 1`, lookup.value).Scan(&account)
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

func (s *Store) RecordEvent(event Event) error {
	_, err := s.db.Exec(`INSERT INTO events (
		at_ns, kind, account_id, thread_key, detail, duration_ns, model, reasoning_effort, service_tier,
		input_tokens, cached_tokens, cache_write_tokens, output_tokens, total_tokens, reasoning_tokens
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.At.UnixNano(), event.Kind, event.Account, event.Thread, event.Detail, event.Duration.Nanoseconds(), event.Model, event.Effort, event.ServiceTier,
		event.Usage.InputTokens, event.Usage.CachedTokens, event.Usage.CacheWriteTokens, event.Usage.OutputTokens,
		event.Usage.TotalTokens, event.Usage.ReasoningTokens)
	return err
}

func (s *Store) UsageEventsSince(kind string, start time.Time) ([]Event, error) {
	rows, err := s.db.Query(`SELECT model, service_tier, input_tokens, cached_tokens, cache_write_tokens,
		output_tokens, total_tokens, reasoning_tokens FROM events WHERE kind = ? AND at_ns >= ? ORDER BY id`, kind, start.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.Model, &event.ServiceTier, &event.Usage.InputTokens, &event.Usage.CachedTokens,
			&event.Usage.CacheWriteTokens, &event.Usage.OutputTokens, &event.Usage.TotalTokens,
			&event.Usage.ReasoningTokens); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) Restore() ([]Attempt, []Event, error) {
	attempts, err := s.readAttempts()
	if err != nil {
		return nil, nil, err
	}
	events, err := s.readEvents()
	return attempts, events, err
}

func (s *Store) readAttempts() ([]Attempt, error) {
	rows, err := s.db.Query(`SELECT at_ns, session_key, thread_key, client_ip, account_id, reasoning_effort, service_tier, turn_metadata, warmup FROM attempts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []Attempt
	for rows.Next() {
		var attempt Attempt
		var at int64
		if err := rows.Scan(&at, &attempt.Session, &attempt.Thread, &attempt.ClientIP, &attempt.Account, &attempt.Effort, &attempt.ServiceTier, &attempt.Metadata, &attempt.Warmup); err != nil {
			return nil, err
		}
		attempt.At = time.Unix(0, at)
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) readEvents() ([]Event, error) {
	rows, err := s.db.Query(`SELECT at_ns, kind, account_id, thread_key, detail, duration_ns, model, reasoning_effort, service_tier,
		input_tokens, cached_tokens, cache_write_tokens, output_tokens, total_tokens, reasoning_tokens FROM events ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var at, duration int64
		if err := rows.Scan(&at, &event.Kind, &event.Account, &event.Thread, &event.Detail, &duration, &event.Model, &event.Effort, &event.ServiceTier,
			&event.Usage.InputTokens, &event.Usage.CachedTokens, &event.Usage.CacheWriteTokens, &event.Usage.OutputTokens,
			&event.Usage.TotalTokens, &event.Usage.ReasoningTokens); err != nil {
			return nil, err
		}
		event.At = time.Unix(0, at)
		event.Duration = time.Duration(duration)
		events = append(events, event)
	}
	return events, rows.Err()
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
