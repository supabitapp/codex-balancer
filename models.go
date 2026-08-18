package main

import (
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
	modelRefreshInterval = 5 * time.Minute
	modelFetchTimeout    = 15 * time.Second
	maxModelCatalogBody  = 16 << 20
)

type modelEntry map[string]any

type modelCatalog struct {
	mu            sync.RWMutex
	refreshMu     sync.Mutex
	accounts      map[string]map[string]modelEntry
	active        map[string]bool
	clientVersion string
	refreshedAt   time.Time
}

func newModelCatalog() *modelCatalog {
	return &modelCatalog{accounts: map[string]map[string]modelEntry{}, active: map[string]bool{}}
}

func (c *modelCatalog) replace(active []string, fresh map[string][]modelEntry, clientVersion string) {
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
	c.refreshedAt = time.Now()
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
		if !account.available(now) {
			continue
		}
		if _, ok := c.accounts[account.id()]; !ok {
			return nil
		}
	}

	serviceTier = canonicalServiceTier(serviceTier)
	allowed := map[string]bool{}
	for _, account := range accounts {
		entry, ok := c.accounts[account.id()][model]
		if !ok {
			continue
		}
		if serviceTier != "" && !modelSupportsServiceTier(entry, serviceTier) {
			continue
		}
		allowed[account.id()] = true
	}
	return allowed
}

func (c *modelCatalog) accountSupportsServiceTier(accountID, model, serviceTier string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.accounts[accountID][model]
	return ok && modelSupportsServiceTier(entry, serviceTier)
}

func (c *modelCatalog) entries() []modelEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	merged := map[string]modelEntry{}
	for _, account := range c.accounts {
		for slug, entry := range account {
			if existing, ok := merged[slug]; ok {
				mergeModelServiceTiers(existing, entry)
				continue
			}
			merged[slug] = cloneModelEntry(entry)
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
	if c.clientVersion == "" || now.Sub(c.refreshedAt) >= modelRefreshInterval {
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
	c.refreshedAt = time.Time{}
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
	for range active {
		result := <-results
		if result.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.id, result.err))
			s.log.Debug("model catalog retained after refresh failure",
				"account", result.id,
				"client_version", clientVersion,
				"models", s.catalog.accountModelCount(result.id),
				"retry_in", modelRefreshInterval,
				"error", result.err,
			)
			continue
		}
		fresh[result.id] = result.models
	}
	s.catalog.replace(activeIDs, fresh, clientVersion)
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

func (s *server) watchModels(ctx context.Context) {
	ticker := time.NewTicker(modelRefreshInterval)
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
		tier, _ := value.(map[string]any)
		id, _ := tier["id"].(string)
		id = canonicalServiceTier(id)
		if id != "" {
			tiers = append(tiers, id)
		}
	}
	return tiers
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
			tier, _ := value.(map[string]any)
			id, _ := tier["id"].(string)
			id = canonicalServiceTier(id)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			merged = append(merged, value)
		}
	}
	if len(merged) > 0 {
		target["service_tiers"] = merged
	}
}
