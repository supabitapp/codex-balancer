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

type storedAPIKey = statepkg.APIKey
type storedRoute = statepkg.Route

type storedUsage struct {
	At          time.Time
	APIKeyName  string
	Model       string
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
		account := accountFromState(accountState{
			IDToken:      record.IDToken,
			AccessToken:  record.AccessToken,
			RefreshToken: record.RefreshToken,
			Paused:       record.Paused,
			RoutingMode:  routingMode(record.RoutingMode).normalized(),
			LastRefresh:  record.LastRefresh,
			Reauth:       record.Reauth,
		})
		account.lastUsed = record.LastUsed
		accounts = append(accounts, account)
	}
	return accounts
}

func accountRecord(account *Account) (statepkg.Account, error) {
	account.mu.Lock()
	persisted := account.accountState
	lastUsed := account.lastUsed
	account.mu.Unlock()
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
		LastUsed:     lastUsed,
		Reauth:       persisted.Reauth,
	}, nil
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

func (s *StateStore) recordRoute(route storedRoute) error {
	return s.raw.RecordRoute(route)
}

func (s *StateStore) routeOwners(thread, session string) ([]string, error) {
	return s.raw.RouteOwners(thread, session)
}

func (s *StateStore) recordUsage(event storedUsage) error {
	return s.raw.RecordUsage(stateUsageEvent(event))
}

func (s *StateStore) usageEventsSince(start time.Time) ([]storedUsage, error) {
	events, err := s.raw.UsageEventsSince(start)
	return storedUsageEvents(events), err
}

func (s *StateStore) pruneUsageBefore(cutoff time.Time) error {
	return s.raw.PruneUsageBefore(cutoff)
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

func stateUsageEvent(event storedUsage) statepkg.UsageEvent {
	return statepkg.UsageEvent{
		At:          event.At,
		APIKeyName:  event.APIKeyName,
		Model:       event.Model,
		ServiceTier: event.ServiceTier,
		Usage: statepkg.Usage{
			InputTokens:      event.Usage.InputTokens,
			CachedTokens:     event.Usage.InputDetails.CachedTokens,
			CacheWriteTokens: event.Usage.InputDetails.CacheWriteTokens,
			OutputTokens:     event.Usage.OutputTokens,
			ReasoningTokens:  event.Usage.OutputDetails.ReasoningTokens,
		},
	}
}

func storedUsageEvents(events []statepkg.UsageEvent) []storedUsage {
	stored := make([]storedUsage, 0, len(events))
	for _, event := range events {
		stored = append(stored, storedUsage{
			At:          event.At,
			APIKeyName:  event.APIKeyName,
			Model:       event.Model,
			ServiceTier: event.ServiceTier,
			Usage:       responseUsageFromState(event.Usage),
		})
	}
	return stored
}

func responseUsageFromState(usage statepkg.Usage) responseUsage {
	value := responseUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.InputTokens + usage.OutputTokens,
	}
	value.InputDetails.CachedTokens = usage.CachedTokens
	value.InputDetails.CacheWriteTokens = usage.CacheWriteTokens
	value.OutputDetails.ReasoningTokens = usage.ReasoningTokens
	return value
}
