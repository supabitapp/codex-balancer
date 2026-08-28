package app

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"path/filepath"
	"time"

	statepkg "github.com/supabitapp/codex-balancer/internal/state"
)

const stateApplicationID = statepkg.ApplicationID

var stateSchemaVersion = statepkg.SchemaVersion()

type StateStore struct {
	raw  *statepkg.Store
	db   *sql.DB
	path string
}

type storedAttempt = statepkg.Attempt
type storedAccountSnapshot = statepkg.AccountSnapshot
type storedAPIKey = statepkg.APIKey

type storedEvent struct {
	At          time.Time
	APIKeyName  string
	Kind        string
	Account     string
	Thread      string
	Detail      string
	Duration    time.Duration
	Model       string
	Effort      string
	ServiceTier string
	Usage       responseUsage
}

func defaultStatePath() string {
	return filepath.Join(homeDir(), ".codex-balancer", "state.db")
}

func openStateStore(path string) (*StateStore, error) {
	raw, err := statepkg.Open(path)
	if err != nil {
		return nil, err
	}
	return &StateStore{raw: raw, db: raw.DB(), path: raw.Path()}, nil
}

func (s *StateStore) Close() error {
	return s.raw.Close()
}

func (s *StateStore) clientIDKey() ([]byte, error) {
	return s.raw.ClientIDKey()
}

func (s *StateStore) readAPIKeys() ([]storedAPIKey, error) {
	return s.raw.ReadAPIKeys()
}

func (s *StateStore) addAPIKey(key storedAPIKey) error {
	return s.raw.AddAPIKey(key)
}

func (s *StateStore) revokeAPIKey(name string, at time.Time) (bool, error) {
	return s.raw.RevokeAPIKey(name, at)
}

func (s *StateStore) validAPIKey(presented string) (bool, error) {
	_, valid, err := s.apiKeyName(presented)
	return valid, err
}

func (s *StateStore) apiKeyName(presented string) (string, bool, error) {
	keys, err := s.readAPIKeys()
	if err != nil {
		return "", false, err
	}
	valid := 0
	name := ""
	for _, key := range keys {
		if key.RevokedAt.IsZero() {
			matched := subtle.ConstantTimeCompare([]byte(presented), []byte(key.Secret))
			valid |= matched
			if matched == 1 {
				name = key.Name
			}
		}
	}
	return name, valid == 1, nil
}

func (s *StateStore) activeAPIKeyCount() (int, error) {
	keys, err := s.readAPIKeys()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, key := range keys {
		if key.RevokedAt.IsZero() {
			count++
		}
	}
	return count, nil
}

func (s *StateStore) loadPriceCatalog() (time.Time, []byte, error) {
	return s.raw.LoadPriceCatalog()
}

func (s *StateStore) savePriceCatalog(fetchedAt time.Time, payload []byte) error {
	return s.raw.SavePriceCatalog(fetchedAt, payload)
}

func (s *StateStore) readAccountSnapshots(kind string) ([]storedAccountSnapshot, error) {
	return s.raw.ReadAccountSnapshots(kind)
}

func (s *StateStore) saveAccountSnapshot(snapshot storedAccountSnapshot) error {
	return s.raw.SaveAccountSnapshot(snapshot)
}

func (s *StateStore) readAccounts() ([]*Account, error) {
	records, err := s.raw.ReadAccounts()
	if err != nil {
		return nil, err
	}
	return accountsFromRecords(records), nil
}

func accountsFromRecords(records []statepkg.Account) []*Account {
	accounts := make([]*Account, 0, len(records))
	for _, record := range records {
		accounts = append(accounts, accountFromState(accountState{
			IDToken:      record.IDToken,
			AccessToken:  record.AccessToken,
			RefreshToken: record.RefreshToken,
			Paused:       record.Paused,
			RoutingMode:  routingMode(record.RoutingMode).normalized(),
			LastRefresh:  record.LastRefresh,
			Reauth:       record.Reauth,
		}))
	}
	return accounts
}

func accountRecord(account *Account) (statepkg.Account, error) {
	persisted := account.persisted()
	id := claimsFromToken(persisted.IDToken).Auth.AccountID
	if id == "" {
		return statepkg.Account{}, errors.New("credentials carry no chatgpt_account_id")
	}
	return statepkg.Account{
		ID:           id,
		IDToken:      persisted.IDToken,
		AccessToken:  persisted.AccessToken,
		RefreshToken: persisted.RefreshToken,
		Paused:       persisted.Paused,
		RoutingMode:  string(persisted.RoutingMode.normalized()),
		LastRefresh:  persisted.LastRefresh,
		Reauth:       persisted.Reauth,
	}, nil
}

func (s *StateStore) restoreLastUsed(accounts []*Account) error {
	lastUsed, err := s.raw.LastUsed()
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if at, ok := lastUsed[account.id()]; ok {
			account.mu.Lock()
			account.lastUsed = at
			account.mu.Unlock()
		}
	}
	return nil
}

func (s *StateStore) mutateAccounts(change func([]*Account) ([]*Account, error)) ([]*Account, error) {
	var updated []*Account
	_, err := s.raw.MutateAccounts(func(records []statepkg.Account) ([]statepkg.Account, error) {
		accounts, err := change(accountsFromRecords(records))
		if err != nil {
			return nil, err
		}
		updated = accounts
		next := make([]statepkg.Account, 0, len(accounts))
		for _, account := range accounts {
			record, err := accountRecord(account)
			if err != nil {
				return nil, err
			}
			next = append(next, record)
		}
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *StateStore) recordAttempt(attempt storedAttempt) error {
	return s.raw.RecordAttempt(attempt)
}

func (s *StateStore) routeOwners(thread, session string) ([]string, error) {
	return s.raw.RouteOwners(thread, session)
}

func (s *StateStore) recordEvent(event storedEvent) error {
	return s.raw.RecordEvent(stateEvent(event))
}

func (s *StateStore) recordUsage(event storedEvent) error {
	return s.raw.RecordUsage(stateUsageEvent(event))
}

func (s *StateStore) usageEventsSince(start time.Time) ([]storedEvent, error) {
	events, err := s.raw.UsageEventsSince(start)
	return storedUsageEvents(events), err
}

func (s *StateStore) threadUsageEventsSince(start time.Time) ([]storedEvent, error) {
	events, err := s.raw.ThreadUsageEventsSince(start)
	return storedUsageEvents(events), err
}

func (s *StateStore) apiKeyUsage() (map[string]responseUsage, error) {
	records, err := s.raw.APIKeyUsage()
	if err != nil {
		return nil, err
	}
	usage := make(map[string]responseUsage, len(records))
	for name, record := range records {
		usage[name] = responseUsageFromState(record)
	}
	return usage, nil
}

func stateEvent(event storedEvent) statepkg.Event {
	return statepkg.Event{
		At:       event.At,
		Kind:     event.Kind,
		Account:  event.Account,
		Thread:   event.Thread,
		Detail:   event.Detail,
		Duration: event.Duration,
	}
}

func stateUsageEvent(event storedEvent) statepkg.UsageEvent {
	return statepkg.UsageEvent{
		At:          event.At,
		APIKeyName:  event.APIKeyName,
		Account:     event.Account,
		Thread:      event.Thread,
		Model:       event.Model,
		Effort:      event.Effort,
		ServiceTier: event.ServiceTier,
		Usage: statepkg.Usage{
			InputTokens:      event.Usage.InputTokens,
			CachedTokens:     event.Usage.InputDetails.CachedTokens,
			CacheWriteTokens: event.Usage.InputDetails.CacheWriteTokens,
			OutputTokens:     event.Usage.OutputTokens,
			TotalTokens:      event.Usage.TotalTokens,
			ReasoningTokens:  event.Usage.OutputDetails.ReasoningTokens,
		},
	}
}

func storedEvents(events []statepkg.Event) []storedEvent {
	stored := make([]storedEvent, 0, len(events))
	for _, event := range events {
		stored = append(stored, storedEvent{
			At:       event.At,
			Kind:     event.Kind,
			Account:  event.Account,
			Thread:   event.Thread,
			Detail:   event.Detail,
			Duration: event.Duration,
		})
	}
	return stored
}

func storedUsageEvents(events []statepkg.UsageEvent) []storedEvent {
	stored := make([]storedEvent, 0, len(events))
	for _, event := range events {
		stored = append(stored, storedEvent{
			At:          event.At,
			APIKeyName:  event.APIKeyName,
			Account:     event.Account,
			Thread:      event.Thread,
			Model:       event.Model,
			Effort:      event.Effort,
			ServiceTier: event.ServiceTier,
			Usage:       responseUsageFromState(event.Usage),
		})
	}
	return stored
}

func responseUsageFromState(usage statepkg.Usage) responseUsage {
	value := responseUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens}
	value.InputDetails.CachedTokens = usage.CachedTokens
	value.InputDetails.CacheWriteTokens = usage.CacheWriteTokens
	value.OutputDetails.ReasoningTokens = usage.ReasoningTokens
	return value
}

func (s *StateStore) restoreStats(stats *Stats) error {
	attempts, events, usageEvents, err := s.raw.Restore()
	if err != nil {
		return err
	}
	for _, attempt := range attempts {
		if attempt.Warmup {
			continue
		}
		metadata := decodeTurnMetadata(attempt.Metadata)
		stats.applyRouted(attempt.At, statsThreadKey(attempt.Thread, metadata), attempt.ClientIP, attempt.Account, "", attempt.Effort, attempt.ServiceTier, metadata)
	}
	for _, event := range storedEvents(events) {
		switch event.Kind {
		case eventResponseAnswered:
			stats.applyAnswered(event.At, event.Thread, event.Account, event.Duration)
			continue
		case eventResponseCompleted:
			stats.applyCompleted(event.At, event.Thread, event.Account, event.Detail, event.Duration)
			continue
		case eventRateLimited:
			stats.applyRateLimited(event.At, event.Account)
		case eventFailover:
			stats.failures++
		}
		stats.appendEvent(Event{At: event.At, Kind: event.Kind, Account: event.Account, Detail: event.Detail})
	}
	for _, event := range storedUsageEvents(usageEvents) {
		stats.applyUsageAt(event.At, event.Thread, event.Account, event.Model, event.Effort, event.ServiceTier, event.Usage)
	}
	return nil
}
