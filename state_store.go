package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
}

type StateStore struct {
	db   *sql.DB
	path string
}

type storedAttempt struct {
	At          time.Time
	Thread      string
	Account     string
	ServiceTier string
	Transport   transport
}

type storedEvent struct {
	At          time.Time
	Kind        string
	Account     string
	Detail      string
	Duration    time.Duration
	Model       string
	ServiceTier string
	Usage       responseUsage
}

func defaultStatePath() string {
	return filepath.Join(homeDir(), ".codex-balancer", "state.db")
}

func defaultLegacyAffinityPath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("codex-balancer-%d", os.Getuid()), "affinity.json")
}

func openConfiguredState(path string) (*StateStore, error) {
	if filepath.Clean(path) == filepath.Clean(defaultStatePath()) {
		return openStateStore(path, defaultLegacyAccountsPath(), defaultLegacyAffinityPath())
	}
	return openStateStore(path, "", "")
}

func openStateStore(path, legacyAccounts, legacyAffinity string) (*StateStore, error) {
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
	created := errors.Is(statErr, fs.ErrNotExist)
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
	if created {
		if err := store.importLegacy(legacyAccounts, legacyAffinity); err != nil {
			return nil, err
		}
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
	_, err := s.db.Exec(`INSERT INTO attempts (at_ns, thread_key, account_id, service_tier, transport) VALUES (?, ?, ?, ?, ?)`,
		attempt.At.UnixNano(), attempt.Thread, attempt.Account, attempt.ServiceTier, attempt.Transport)
	return err
}

func (s *StateStore) recordEvent(event storedEvent) error {
	_, err := s.db.Exec(`INSERT INTO events (
		at_ns, kind, account_id, detail, duration_ns, model, service_tier,
		input_tokens, cached_tokens, cache_write_tokens, output_tokens
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.At.UnixNano(), event.Kind, event.Account, event.Detail, event.Duration.Nanoseconds(), event.Model, event.ServiceTier,
		event.Usage.InputTokens, event.Usage.InputDetails.CachedTokens, event.Usage.InputDetails.CacheWriteTokens, event.Usage.OutputTokens)
	return err
}

func (s *StateStore) restoreStats(stats *Stats) error {
	attempts, err := s.db.Query(`SELECT at_ns, thread_key, account_id, service_tier, transport FROM attempts ORDER BY id`)
	if err != nil {
		return err
	}
	for attempts.Next() {
		var at int64
		var thread, account, tier string
		var via transport
		if err := attempts.Scan(&at, &thread, &account, &tier, &via); err != nil {
			attempts.Close()
			return err
		}
		stats.applyRouted(time.Unix(0, at), thread, account, tier, via)
	}
	if err := attempts.Close(); err != nil {
		return err
	}
	events, err := s.db.Query(`SELECT at_ns, kind, account_id, detail, duration_ns, model, service_tier,
		input_tokens, cached_tokens, cache_write_tokens, output_tokens FROM events ORDER BY id`)
	if err != nil {
		return err
	}
	defer events.Close()
	for events.Next() {
		var event storedEvent
		var at, duration int64
		if err := events.Scan(&at, &event.Kind, &event.Account, &event.Detail, &duration, &event.Model, &event.ServiceTier,
			&event.Usage.InputTokens, &event.Usage.InputDetails.CachedTokens, &event.Usage.InputDetails.CacheWriteTokens, &event.Usage.OutputTokens); err != nil {
			return err
		}
		event.At = time.Unix(0, at)
		event.Duration = time.Duration(duration)
		switch event.Kind {
		case eventResponseAnswered:
			stats.ttfbSum += event.Duration
			stats.ttfbN++
			continue
		case eventResponseUsage:
			stats.applyUsage(event.Model, event.ServiceTier, event.Usage)
			continue
		case eventRateLimited:
			stats.limited++
			stats.account(event.Account).limited++
		case eventFailover:
			stats.failures++
		}
		stats.appendEvent(Event{At: event.At, Kind: event.Kind, Account: event.Account, Detail: event.Detail})
	}
	return events.Err()
}

func (s *StateStore) importLegacy(accountsPath, affinityPath string) error {
	accounts, accountsData, err := readLegacyAccounts(accountsPath)
	if err != nil {
		return err
	}
	affinity, affinityData, err := readLegacyAffinity(affinityPath)
	if err != nil {
		return err
	}
	if len(accountsData) == 0 && len(affinityData) == 0 {
		return nil
	}
	backupDir := filepath.Join(filepath.Dir(s.path), "legacy-backup")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(backupDir, 0o700); err != nil {
		return err
	}
	if len(accountsData) > 0 {
		if err := writeSecureFile(filepath.Join(backupDir, "accounts.json"), accountsData); err != nil {
			return err
		}
	}
	if len(affinityData) > 0 {
		if err := writeSecureFile(filepath.Join(backupDir, "affinity.json"), affinityData); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, account := range accounts {
		if err := insertAccount(tx, account); err != nil {
			return err
		}
	}
	for _, binding := range affinity {
		if _, err := tx.Exec(`INSERT INTO bindings (kind, value, account_id, created_at_ns) VALUES (?, ?, ?, ?)`,
			binding.Kind, binding.Value, binding.Account, encodeTime(binding.CreatedAt)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(accountsData) > 0 {
		os.Remove(accountsPath)
	}
	if len(affinityData) > 0 {
		os.Remove(affinityPath)
	}
	return nil
}

func writeSecureFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func readLegacyAccounts(path string) ([]*Account, []byte, error) {
	if path == "" {
		return nil, nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var accounts []*Account
	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	seen := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		id := account.id()
		if id == "" {
			return nil, nil, fmt.Errorf("parse %s: credentials carry no chatgpt_account_id", path)
		}
		if seen[id] {
			return nil, nil, fmt.Errorf("parse %s: duplicate account %q", path, id)
		}
		seen[id] = true
	}
	return accounts, data, nil
}

type legacySoftBinding struct {
	Account string    `json:"account"`
	Seen    time.Time `json:"seen"`
}

type legacyAffinityState struct {
	Soft      map[string]legacySoftBinding `json:"soft"`
	Hard      map[string]string            `json:"hard"`
	HardOrder []string                     `json:"hard_order"`
}

type legacyBinding struct {
	Kind      string
	Value     string
	Account   string
	CreatedAt time.Time
}

func readLegacyAffinity(path string) ([]legacyBinding, []byte, error) {
	if path == "" {
		return nil, nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var state legacyAffinityState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var bindings []legacyBinding
	softKeys := make([]string, 0, len(state.Soft))
	for key := range state.Soft {
		softKeys = append(softKeys, key)
	}
	slices.Sort(softKeys)
	for _, key := range softKeys {
		binding := state.Soft[key]
		kind, value, ok := strings.Cut(key, "\n")
		if !ok || kind == "" || value == "" || binding.Account == "" {
			continue
		}
		bindings = append(bindings, legacyBinding{Kind: kind, Value: value, Account: binding.Account, CreatedAt: binding.Seen})
	}
	ordered := make(map[string]bool, len(state.HardOrder))
	hardKeys := make([]string, 0, len(state.Hard))
	for _, key := range state.HardOrder {
		if !ordered[key] && state.Hard[key] != "" {
			ordered[key] = true
			hardKeys = append(hardKeys, key)
		}
	}
	var missing []string
	for key := range state.Hard {
		if !ordered[key] {
			missing = append(missing, key)
		}
	}
	slices.Sort(missing)
	hardKeys = append(hardKeys, missing...)
	for _, key := range hardKeys {
		kind, value, ok := strings.Cut(key, "\n")
		account := state.Hard[key]
		if !ok || kind == "" || value == "" || account == "" {
			continue
		}
		bindings = append(bindings, legacyBinding{Kind: kind, Value: value, Account: account})
	}
	return bindings, data, nil
}
