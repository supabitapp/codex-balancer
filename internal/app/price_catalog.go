package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	modelsDevPriceURL       = "https://models.dev/api.json"
	priceRefreshInterval    = 24 * time.Hour
	priceRefreshRetry       = time.Hour
	priceRefreshTimeout     = 30 * time.Second
	maxPriceCatalogResponse = 8 << 20
)

type modelsDevRate struct {
	Input      json.Number `json:"input"`
	Output     json.Number `json:"output"`
	CacheRead  json.Number `json:"cache_read"`
	CacheWrite json.Number `json:"cache_write"`
}

type modelsDevTier struct {
	modelsDevRate
	Tier struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
	} `json:"tier"`
}

type modelsDevCost struct {
	modelsDevRate
	Tiers []modelsDevTier `json:"tiers"`
}

type modelsDevModel struct {
	Cost         *modelsDevCost `json:"cost"`
	Experimental struct {
		Modes struct {
			Fast *struct {
				Cost *modelsDevCost `json:"cost"`
			} `json:"fast"`
		} `json:"modes"`
	} `json:"experimental"`
}

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type priceCatalog struct {
	mu       sync.RWMutex
	client   *http.Client
	endpoint string
	now      func() time.Time
	snapshot priceSnapshot
}

func newPriceCatalog() *priceCatalog {
	return &priceCatalog{
		client:   &http.Client{Timeout: priceRefreshTimeout},
		endpoint: modelsDevPriceURL,
		now:      time.Now,
	}
}

func (c *priceCatalog) current() priceSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot
}

func (c *priceCatalog) install(snapshot priceSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = snapshot
}

func (c *priceCatalog) refreshIn(now time.Time) time.Duration {
	fetchedAt := c.current().fetchedAt
	if fetchedAt.IsZero() {
		return 0
	}
	return max(fetchedAt.Add(priceRefreshInterval).Sub(now), 0)
}

func (c *priceCatalog) refresh(ctx context.Context) (priceSnapshot, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return priceSnapshot{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return priceSnapshot{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return priceSnapshot{}, fmt.Errorf("models.dev returned %s", response.Status)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxPriceCatalogResponse+1))
	if err != nil {
		return priceSnapshot{}, err
	}
	if len(payload) > maxPriceCatalogResponse {
		return priceSnapshot{}, fmt.Errorf("models.dev response exceeds %d bytes", maxPriceCatalogResponse)
	}
	fetchedAt := c.now()
	snapshot, err := parseModelsDevPriceCatalog(payload, fetchedAt)
	if err != nil {
		return priceSnapshot{}, err
	}
	return snapshot, nil
}

func (s *server) refreshPriceCatalog(ctx context.Context) error {
	snapshot, err := s.prices.refresh(ctx)
	if err != nil {
		return err
	}
	if err := s.stats.reprice(snapshot); err != nil {
		return err
	}
	s.prices.install(snapshot)
	return nil
}

func (s *server) watchPriceCatalog(ctx context.Context) {
	delay := s.prices.refreshIn(time.Now())
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := s.refreshPriceCatalog(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("price refresh failed", "error", err)
			delay = priceRefreshRetry
			continue
		}
		s.log.Info("prices updated", "source", modelsDevPriceURL, "models", len(s.prices.current().models))
		delay = s.prices.refreshIn(time.Now())
	}
}

func parseModelsDevPriceCatalog(payload []byte, fetchedAt time.Time) (priceSnapshot, error) {
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(payload, &providers); err != nil {
		return priceSnapshot{}, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	openAI := providers["openai"]
	if len(openAI) == 0 {
		return priceSnapshot{}, fmt.Errorf("models.dev catalog has no OpenAI provider")
	}
	snapshot, err := parseOpenAIPriceCatalog(openAI, fetchedAt)
	if err != nil {
		return priceSnapshot{}, err
	}
	return snapshot, nil
}

func parseOpenAIPriceCatalog(payload []byte, fetchedAt time.Time) (priceSnapshot, error) {
	var provider modelsDevProvider
	if err := json.Unmarshal(payload, &provider); err != nil {
		return priceSnapshot{}, fmt.Errorf("decode OpenAI prices: %w", err)
	}
	models := make(map[string]modelPrice)
	for id, model := range provider.Models {
		if strings.TrimSpace(id) == "" || model.Cost == nil || model.Cost.Input == "" || model.Cost.Output == "" {
			continue
		}
		standard, err := parsePriceTiers(*model.Cost)
		if err != nil {
			return priceSnapshot{}, fmt.Errorf("price %s: %w", id, err)
		}
		var fast []priceTier
		if mode := model.Experimental.Modes.Fast; mode != nil && mode.Cost != nil {
			fast, err = parsePriceTiers(*mode.Cost)
			if err != nil {
				return priceSnapshot{}, fmt.Errorf("fast price %s: %w", id, err)
			}
		}
		models[id] = modelPrice{standard: standard, fast: fast}
	}
	if len(models) == 0 {
		return priceSnapshot{}, fmt.Errorf("OpenAI price catalog has no priced models")
	}
	return newPriceSnapshot(models, fetchedAt), nil
}

func parsePriceTiers(cost modelsDevCost) ([]priceTier, error) {
	base, err := parseTokenRates(cost.modelsDevRate, tokenRates{}, true)
	if err != nil {
		return nil, err
	}
	tiers := []priceTier{{rates: base}}
	for _, source := range cost.Tiers {
		if source.Tier.Type != "context" || source.Tier.Size <= 0 {
			return nil, fmt.Errorf("invalid price tier")
		}
		rates, err := parseTokenRates(source.modelsDevRate, base, false)
		if err != nil {
			return nil, err
		}
		tiers = append(tiers, priceTier{minInput: source.Tier.Size, rates: rates})
	}
	slices.SortFunc(tiers, func(left, right priceTier) int {
		if left.minInput < right.minInput {
			return -1
		}
		if left.minInput > right.minInput {
			return 1
		}
		return 0
	})
	for i := 1; i < len(tiers); i++ {
		if tiers[i-1].minInput == tiers[i].minInput {
			return nil, fmt.Errorf("duplicate price tier %d", tiers[i].minInput)
		}
	}
	return tiers, nil
}

func parseTokenRates(source modelsDevRate, fallback tokenRates, requireBase bool) (tokenRates, error) {
	input, err := parseTokenRate(source.Input, fallback.input, requireBase)
	if err != nil {
		return tokenRates{}, fmt.Errorf("input: %w", err)
	}
	output, err := parseTokenRate(source.Output, fallback.output, requireBase)
	if err != nil {
		return tokenRates{}, fmt.Errorf("output: %w", err)
	}
	cached, err := parseTokenRate(source.CacheRead, input, false)
	if err != nil {
		return tokenRates{}, fmt.Errorf("cache read: %w", err)
	}
	cacheWrite, err := parseTokenRate(source.CacheWrite, input, false)
	if err != nil {
		return tokenRates{}, fmt.Errorf("cache write: %w", err)
	}
	return tokenRates{input: input, cached: cached, cacheWrite: cacheWrite, output: output}, nil
}

func parseTokenRate(source json.Number, fallback int64, required bool) (int64, error) {
	if source == "" {
		if required {
			return 0, fmt.Errorf("missing rate")
		}
		return fallback, nil
	}
	value, err := strconv.ParseFloat(source.String(), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, fmt.Errorf("invalid rate %q", source)
	}
	nanoDollars := value * 1_000
	rounded := math.Round(nanoDollars)
	if math.Abs(nanoDollars-rounded) > 1e-9 || rounded > math.MaxInt64 {
		return 0, fmt.Errorf("rate %q cannot use nano-dollar units", source)
	}
	return int64(rounded), nil
}
