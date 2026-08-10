package main

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

const stateApplicationID = 0x43425853

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
}

type StateStore struct {
	db   *sql.DB
	path string
}

type storedAttempt struct {
	At          time.Time
	Thread      string
	ClientIP    string
	Account     string
	Effort      string
	ServiceTier string
	Transport   transport
	Metadata    string
}

type storedEvent struct {
	At          time.Time
	Kind        string
	Account     string
	Thread      string
	Detail      string
	Duration    time.Duration
	Model       string
	ServiceTier string
	Usage       responseUsage
}

func defaultStatePath() string {
	return filepath.Join(homeDir(), ".codex-balancer", "state.db")
}

func (s *StateStore) clientIDKey() ([]byte, error) {
	var key []byte
	if err := s.db.QueryRow(`SELECT key FROM client_identity WHERE id = 1`).Scan(&key); err != nil {
		return nil, err
	}
	return key, nil
}

func openStateStore(path string) (*StateStore, error) {
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
	store := &StateStore{db: db, path: path}
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

func (s *StateStore) configure() error {
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

func (s *StateStore) migrate() error {
	var applicationID, version int
	if err := s.db.QueryRow("PRAGMA application_id").Scan(&applicationID); err != nil {
		return err
	}
	if applicationID != 0 && applicationID != stateApplicationID {
		return fmt.Errorf("%s is not a codex-balancer state database", s.path)
	}
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	currentVersion := len(stateMigrations)
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
			if _, err := tx.Exec(fmt.Sprintf("PRAGMA application_id = %d", stateApplicationID)); err != nil {
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
		applicationID = stateApplicationID
		version++
	}
	if applicationID == 0 {
		_, err := s.db.Exec(fmt.Sprintf("PRAGMA application_id = %d", stateApplicationID))
		return err
	}
	return nil
}

func (s *StateStore) Close() error {
	return s.db.Close()
}

func (s *StateStore) readAccounts() ([]*Account, error) {
	rows, err := s.db.Query(`SELECT id_token, access_token, refresh_token, paused, last_refresh_ns FROM accounts ORDER BY account_id`)
	if err != nil {
		return nil, err
	}
	return scanAccounts(rows)
}

func scanAccounts(rows *sql.Rows) ([]*Account, error) {
	defer rows.Close()
	var accounts []*Account
	for rows.Next() {
		var state accountState
		var paused int
		var lastRefresh int64
		if err := rows.Scan(&state.IDToken, &state.AccessToken, &state.RefreshToken, &paused, &lastRefresh); err != nil {
			return nil, err
		}
		state.Paused = paused != 0
		state.LastRefresh = decodeTime(lastRefresh)
		accounts = append(accounts, accountFromState(state))
	}
	return accounts, rows.Err()
}

func (s *StateStore) restoreLastUsed(accounts []*Account) error {
	rows, err := s.db.Query(`SELECT account_id, MAX(at_ns) FROM attempts GROUP BY account_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]*Account, len(accounts))
	for _, account := range accounts {
		byID[account.id()] = account
	}
	for rows.Next() {
		var id string
		var at int64
		if err := rows.Scan(&id, &at); err != nil {
			return err
		}
		if account := byID[id]; account != nil {
			account.mu.Lock()
			account.lastUsed = time.Unix(0, at)
			account.mu.Unlock()
		}
	}
	return rows.Err()
}

func (s *StateStore) mutateAccounts(change func([]*Account) ([]*Account, error)) ([]*Account, error) {
	var accounts []*Account
	err := s.immediate(func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(context.Background(), `SELECT id_token, access_token, refresh_token, paused, last_refresh_ns FROM accounts ORDER BY account_id`)
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
		return nil
	})
	return accounts, err
}

func (s *StateStore) immediate(run func(*sql.Conn) error) error {
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

func insertAccount(exec sqlExecer, account *Account) error {
	state := account.persisted()
	id := claimsFromToken(state.IDToken).Auth.AccountID
	if id == "" {
		return errors.New("credentials carry no chatgpt_account_id")
	}
	_, err := exec.ExecContext(context.Background(), `INSERT INTO accounts (
		account_id, id_token, access_token, refresh_token, paused, last_refresh_ns
	) VALUES (?, ?, ?, ?, ?, ?)`, id, state.IDToken, state.AccessToken, state.RefreshToken, state.Paused, encodeTime(state.LastRefresh))
	return err
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

func (s *StateStore) recordAttempt(attempt storedAttempt) error {
	_, err := s.db.Exec(`INSERT INTO attempts (at_ns, thread_key, client_ip, account_id, reasoning_effort, service_tier, transport, turn_metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		attempt.At.UnixNano(), attempt.Thread, attempt.ClientIP, attempt.Account, attempt.Effort, attempt.ServiceTier, attempt.Transport, attempt.Metadata)
	return err
}

func (s *StateStore) recordEvent(event storedEvent) error {
	_, err := s.db.Exec(`INSERT INTO events (
		at_ns, kind, account_id, thread_key, detail, duration_ns, model, service_tier,
		input_tokens, cached_tokens, cache_write_tokens, output_tokens, total_tokens, reasoning_tokens
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.At.UnixNano(), event.Kind, event.Account, event.Thread, event.Detail, event.Duration.Nanoseconds(), event.Model, event.ServiceTier,
		event.Usage.InputTokens, event.Usage.InputDetails.CachedTokens, event.Usage.InputDetails.CacheWriteTokens, event.Usage.OutputTokens,
		event.Usage.TotalTokens, event.Usage.OutputDetails.ReasoningTokens)
	return err
}

func (s *StateStore) restoreStats(stats *Stats) error {
	attempts, err := s.db.Query(`SELECT at_ns, thread_key, client_ip, account_id, reasoning_effort, service_tier, transport, turn_metadata FROM attempts ORDER BY id`)
	if err != nil {
		return err
	}
	for attempts.Next() {
		var at int64
		var thread, clientIP, account, effort, tier, metadata string
		var via transport
		if err := attempts.Scan(&at, &thread, &clientIP, &account, &effort, &tier, &via, &metadata); err != nil {
			attempts.Close()
			return err
		}
		stats.applyRouted(time.Unix(0, at), thread, clientIP, account, "", effort, tier, via, decodeTurnMetadata(metadata))
	}
	if err := attempts.Close(); err != nil {
		return err
	}
	events, err := s.db.Query(`SELECT at_ns, kind, account_id, thread_key, detail, duration_ns, model, service_tier,
		input_tokens, cached_tokens, cache_write_tokens, output_tokens, total_tokens, reasoning_tokens FROM events ORDER BY id`)
	if err != nil {
		return err
	}
	defer events.Close()
	for events.Next() {
		var event storedEvent
		var at, duration int64
		if err := events.Scan(&at, &event.Kind, &event.Account, &event.Thread, &event.Detail, &duration, &event.Model, &event.ServiceTier,
			&event.Usage.InputTokens, &event.Usage.InputDetails.CachedTokens, &event.Usage.InputDetails.CacheWriteTokens, &event.Usage.OutputTokens,
			&event.Usage.TotalTokens, &event.Usage.OutputDetails.ReasoningTokens); err != nil {
			return err
		}
		event.At = time.Unix(0, at)
		event.Duration = time.Duration(duration)
		switch event.Kind {
		case eventResponseAnswered:
			stats.applyAnswered(event.At, event.Thread, event.Duration)
			continue
		case eventResponseCompleted:
			stats.applyCompleted(event.At, event.Thread, event.Detail, event.Duration)
			continue
		case eventResponseUsage:
			stats.applyUsageAt(event.At, event.Thread, event.Model, event.ServiceTier, event.Usage)
			continue
		case eventRateLimited:
			stats.limited++
			stats.account(event.Account).limited++
		case eventFailover:
			stats.failures++
		}
		if event.Kind == eventCompactionSwitch {
			stats.appendEvent(Event{At: event.At, Kind: event.Kind, Account: event.Account, SourceAccount: event.Detail, Thread: event.Thread})
			continue
		}
		stats.appendEvent(Event{At: event.At, Kind: event.Kind, Account: event.Account, Detail: event.Detail})
	}
	return events.Err()
}
