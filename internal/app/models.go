package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	modelRefreshInterval = time.Hour
	modelRetryInterval   = 5 * time.Minute
	modelFetchTimeout    = 15 * time.Second
	maxModelCatalogBody  = 16 << 20
	modelSnapshotKind    = "models"
)

type modelEntry map[string]any

type modelCatalog struct {
	mu            sync.RWMutex
	refreshMu     sync.Mutex
	accounts      map[string]map[string]modelEntry
	active        map[string]bool
	clientVersion string
	nextRefresh   time.Time
}

func newModelCatalog() *modelCatalog {
	return &modelCatalog{accounts: map[string]map[string]modelEntry{}, active: map[string]bool{}}
}

func (c *modelCatalog) replace(active []string, fresh map[string][]modelEntry, clientVersion string) {
	c.replaceAt(active, fresh, clientVersion, time.Now())
}

func (c *modelCatalog) replaceAt(active []string, fresh map[string][]modelEntry, clientVersion string, refreshedAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	activeIDs := make(map[string]bool, len(active))
	nextAccounts := make(map[string]map[string]modelEntry, len(active))
	for _, id := range active {
		activeIDs[id] = true
		if previous, ok := c.accounts[id]; ok {
			nextAccounts[id] = previous
		}
	}
	for id, entries := range fresh {
		if !activeIDs[id] {
			continue
		}
		models := make(map[string]modelEntry, len(entries))
		for _, entry := range entries {
			slug := modelSlug(entry)
			if slug == "" {
				continue
			}
			models[slug] = entry
		}
		nextAccounts[id] = models
	}
	c.accounts = nextAccounts
	c.active = activeIDs
	c.clientVersion = clientVersion
	c.nextRefresh = refreshedAt.Add(modelRefreshInterval)
	if len(fresh) != len(active) {
		c.nextRefresh = refreshedAt.Add(modelRetryInterval)
	}
}

func (c *modelCatalog) allowedAccounts(accounts []*Account, model, serviceTier string) map[string]bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	for _, account := range accounts {
		candidate := account.routingCandidate()
		if !candidate.available(now) {
			continue
		}
		if c.accounts[candidate.id] == nil {
			return nil
		}
	}

	serviceTier = canonicalServiceTier(serviceTier)
	allowed := map[string]bool{}
	for _, account := range accounts {
		id := account.id()
		entry := matchingModelEntry(c.accounts[id], model)
		if entry == nil || serviceTier != "" && !modelSupportsServiceTier(entry, serviceTier) {
			continue
		}
		allowed[id] = true
	}
	return allowed
}

func (c *modelCatalog) entries() []modelEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	accountIDs := make([]string, 0, len(c.accounts))
	for id := range c.accounts {
		accountIDs = append(accountIDs, id)
	}
	slices.Sort(accountIDs)
	merged := map[string]modelEntry{}
	for _, id := range accountIDs {
		for slug, entry := range c.accounts[id] {
			if existing := merged[slug]; existing != nil {
				mergeModelServiceTiers(existing, entry)
				continue
			}
			clone := cloneModelEntry(entry)
			delete(clone, "service_tiers")
			mergeModelServiceTiers(clone, entry)
			merged[slug] = clone
		}
	}
	entries := make([]modelEntry, 0, len(merged))
	for _, entry := range merged {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b modelEntry) int {
		return strings.Compare(modelSlug(a), modelSlug(b))
	})
	return entries
}

func (c *modelCatalog) accountModelCount(id string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.accounts[id])
}

type modelContextLimits struct {
	Window      int64
	AutoCompact int64
}

func (c *modelCatalog) contextLimits(accountID, model string) modelContextLimits {
	if c == nil {
		return modelContextLimits{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	model = strings.ToLower(strings.TrimSpace(model))
	entry := matchingModelEntry(c.accounts[accountID], model)
	if entry == nil {
		for _, entries := range c.accounts {
			if entry = matchingModelEntry(entries, model); entry != nil {
				break
			}
		}
	}
	if entry == nil {
		return modelContextLimits{}
	}
	raw := modelInteger(entry, "context_window")
	if raw == 0 {
		raw = modelInteger(entry, "max_context_window")
	}
	percent := modelInteger(entry, "effective_context_window_percent")
	if percent == 0 {
		percent = 95
	}
	limits := modelContextLimits{Window: raw * percent / 100}
	configured := modelInteger(entry, "auto_compact_token_limit")
	if raw > 0 {
		limits.AutoCompact = raw * 9 / 10
		if configured > 0 {
			limits.AutoCompact = min(configured, limits.AutoCompact)
		}
	} else {
		limits.AutoCompact = configured
	}
	return limits
}

func matchingModelEntry(entries map[string]modelEntry, model string) modelEntry {
	if entry := entries[model]; entry != nil {
		return entry
	}
	var match modelEntry
	matchLength := 0
	for slug, entry := range entries {
		if strings.HasPrefix(model, slug+"-") && len(slug) > matchLength {
			match = entry
			matchLength = len(slug)
		}
	}
	return match
}

func modelInteger(entry modelEntry, key string) int64 {
	switch value := entry[key].(type) {
	case json.Number:
		result, _ := value.Int64()
		return result
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	default:
		return 0
	}
}

func (c *modelCatalog) needsRefresh(active []string, clientVersion string, now time.Time) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if clientVersion == "" {
		return false
	}
	if c.clientVersion == "" || !now.Before(c.nextRefresh) {
		return true
	}
	for _, id := range active {
		if !c.active[id] {
			return true
		}
	}
	return len(c.active) != len(active)
}

func (c *modelCatalog) version() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.clientVersion
}

func (c *modelCatalog) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextRefresh = time.Time{}
}

func (c *modelCatalog) retryAt(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := now.Add(modelRetryInterval)
	if c.nextRefresh.IsZero() || next.Before(c.nextRefresh) {
		c.nextRefresh = next
	}
}

func (s *server) refreshModels(ctx context.Context, clientVersion string) error {
	if s.catalog == nil || clientVersion == "" {
		return nil
	}
	s.catalog.refreshMu.Lock()
	defer s.catalog.refreshMu.Unlock()

	accounts := s.pool.all()
	active := make([]*Account, 0, len(accounts))
	activeIDs := make([]string, 0, len(accounts))
	type skippedAccount struct {
		id     string
		reason string
	}
	skipped := make([]skippedAccount, 0, len(accounts))
	for _, account := range accounts {
		candidate := account.routingCandidate()
		if candidate.id == "" {
			continue
		}
		if candidate.paused || candidate.reauth != "" {
			reason := "paused"
			if candidate.reauth != "" {
				reason = "needs_reauth"
			}
			skipped = append(skipped, skippedAccount{id: candidate.id, reason: reason})
			continue
		}
		active = append(active, account)
		activeIDs = append(activeIDs, candidate.id)
	}
	if !s.catalog.needsRefresh(activeIDs, clientVersion, time.Now()) {
		return nil
	}
	for _, account := range skipped {
		s.log.Debug("model refresh skipped account",
			"account", account.id,
			"client_version", clientVersion,
			"reason", account.reason,
		)
	}

	type fetchResult struct {
		id     string
		models []modelEntry
		err    error
	}
	results := make(chan fetchResult, len(active))
	for _, account := range active {
		go func() {
			models, err := s.fetchAccountModels(ctx, account, clientVersion, true)
			results <- fetchResult{id: account.id(), models: models, err: err}
		}()
	}
	fresh := make(map[string][]modelEntry, len(active))
	errs := make([]error, 0, len(active))
	writeFailed := false
	for range active {
		result := <-results
		if result.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.id, result.err))
			s.log.Debug("model catalog retained after refresh failure",
				"account", result.id,
				"client_version", clientVersion,
				"models", s.catalog.accountModelCount(result.id),
				"retry_in", modelRetryInterval,
				"error", result.err,
			)
			continue
		}
		fresh[result.id] = result.models
		if err := s.saveModelSnapshot(result.id, clientVersion, result.models); err != nil {
			writeFailed = true
			errs = append(errs, fmt.Errorf("store %s: %w", result.id, err))
			s.log.Warn("model catalog write failed", "account", result.id, "error", err)
		}
	}
	s.catalog.replace(activeIDs, fresh, clientVersion)
	if writeFailed {
		s.catalog.retryAt(time.Now())
	}
	s.log.Debug("model catalog refreshed",
		"accounts", len(activeIDs),
		"client_version", clientVersion,
		"failures", len(errs),
		"models", len(s.catalog.entries()),
		"refresh_in", modelRefreshInterval,
	)
	return errors.Join(errs...)
}

func (s *server) fetchAccountModels(ctx context.Context, account *Account, clientVersion string, canReauth bool) ([]modelEntry, error) {
	requestContext, cancel := context.WithTimeout(ctx, modelFetchTimeout)
	defer cancel()
	endpoint, err := url.Parse(s.upstream + "/models")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("client_version", clientVersion)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	account.mu.Lock()
	token := account.AccessToken
	account.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("chatgpt-account-id", account.id())
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && canReauth {
		if !s.refreshed(account, account.id()) {
			return nil, errors.New("account needs reauth")
		}
		return s.fetchAccountModels(ctx, account, clientVersion, false)
	}
	if resp.StatusCode/100 != 2 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBody))
		return nil, fmt.Errorf("models returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	var payload struct {
		Models []modelEntry `json:"models"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxModelCatalogBody))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	models := payload.Models[:0]
	for _, model := range payload.Models {
		if modelSlug(model) != "" {
			models = append(models, model)
		}
	}
	return models, nil
}

func (s *server) saveModelSnapshot(account, version string, models []modelEntry) error {
	if s.pool == nil || s.pool.store == nil {
		return nil
	}
	payload, err := json.Marshal(models)
	if err != nil {
		return err
	}
	return s.pool.store.saveAccountSnapshot(storedAccountSnapshot{
		Account:   account,
		Kind:      modelSnapshotKind,
		FetchedAt: time.Now(),
		Version:   version,
		Payload:   payload,
	})
}

func restoreModelCatalog(store *StateStore, catalog *modelCatalog, accounts []*Account) error {
	active := make([]string, 0, len(accounts))
	activeSet := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		candidate := account.routingCandidate()
		if candidate.id == "" || candidate.paused || candidate.reauth != "" {
			continue
		}
		active = append(active, candidate.id)
		activeSet[candidate.id] = true
	}
	snapshots, err := store.readAccountSnapshots(modelSnapshotKind)
	if err != nil {
		return err
	}
	fresh := make(map[string][]modelEntry, len(snapshots))
	var version string
	var refreshedAt time.Time
	var newest time.Time
	consistent := true
	for _, snapshot := range snapshots {
		if !activeSet[snapshot.Account] {
			continue
		}
		var models []modelEntry
		decoder := json.NewDecoder(bytes.NewReader(snapshot.Payload))
		decoder.UseNumber()
		if err := decoder.Decode(&models); err != nil {
			return fmt.Errorf("restore models for %s: %w", snapshot.Account, err)
		}
		fresh[snapshot.Account] = models
		if version != "" && version != snapshot.Version {
			consistent = false
		}
		if refreshedAt.IsZero() || snapshot.FetchedAt.Before(refreshedAt) {
			refreshedAt = snapshot.FetchedAt
		}
		if newest.IsZero() || snapshot.FetchedAt.After(newest) {
			version = snapshot.Version
			newest = snapshot.FetchedAt
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	catalog.replaceAt(active, fresh, version, refreshedAt)
	if !consistent || len(fresh) != len(active) {
		catalog.invalidate()
	}
	return nil
}

func (s *server) watchModels(ctx context.Context) {
	ticker := time.NewTicker(modelRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.refreshModels(ctx, s.catalog.version()); err != nil && s.log != nil {
				s.log.Warn("model refresh failed", "error", err)
			}
		}
	}
}

func modelSlug(entry modelEntry) string {
	slug, _ := entry["slug"].(string)
	return strings.ToLower(strings.TrimSpace(slug))
}

const serviceTierPriority = "priority"

func canonicalServiceTier(serviceTier string) string {
	serviceTier = strings.ToLower(strings.TrimSpace(serviceTier))
	switch serviceTier {
	case "", "auto", "default":
		return ""
	case "fast":
		return serviceTierPriority
	default:
		return serviceTier
	}
}

func modelSupportsServiceTier(entry modelEntry, serviceTier string) bool {
	for _, tier := range modelServiceTiers(entry) {
		if tier == serviceTier {
			return true
		}
	}
	return false
}

func modelServiceTiers(entry modelEntry) []string {
	values, _ := entry["service_tiers"].([]any)
	tiers := make([]string, 0, len(values))
	for _, value := range values {
		id := modelServiceTierID(value)
		if id != "" {
			tiers = append(tiers, id)
		}
	}
	return tiers
}

func modelServiceTierID(value any) string {
	tier, _ := value.(map[string]any)
	id, _ := tier["id"].(string)
	return canonicalServiceTier(id)
}

func cloneModelEntry(entry modelEntry) modelEntry {
	clone := make(modelEntry, len(entry))
	for key, value := range entry {
		clone[key] = value
	}
	return clone
}

func mergeModelServiceTiers(target, source modelEntry) {
	seen := map[string]bool{}
	merged := []any{}
	for _, entry := range []modelEntry{target, source} {
		values, _ := entry["service_tiers"].([]any)
		for _, value := range values {
			id := modelServiceTierID(value)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			merged = append(merged, value)
		}
	}
	if len(merged) == 0 {
		delete(target, "service_tiers")
		return
	}
	target["service_tiers"] = merged
}
