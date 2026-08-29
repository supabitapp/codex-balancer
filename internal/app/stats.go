package app

import (
	"cmp"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	eventLog            = 200
	threadHistoryWindow = 5 * time.Minute
	accountStatsWindow  = 24 * time.Hour
	activityLen         = 24
	activitySpan        = accountStatsWindow / activityLen

	eventRateLimited       = "rate limited"
	eventFailover          = "failover"
	eventResponseAnswered  = "response answered"
	eventResponseCompleted = "response completed"
)

const serviceTierFast = "priority"

type Stats struct {
	mu                 sync.Mutex
	pricingMu          sync.Mutex
	store              *StateStore
	persistFailed      func(error)
	prices             priceSnapshot
	started            time.Time
	turns              int64
	failures           int64
	limited            int64
	ttfbSum            time.Duration
	ttfbN              int64
	wsOpen             int64
	usageMonth         int
	monthlyUsage       responseUsage
	apiCostNanoDollars int64
	unpricedResponses  int64
	monthlyModelCosts  map[string]modelCost
	accounts           map[string]*accountStats
	threads            map[string]*threadStats
	liveThreads        map[string]int
	events             []Event
}

type accountStats struct {
	turns    int64
	limited  rollingCounter
	wsOpen   int64
	activity rollingCounter
}

type rollingCounter struct {
	events []time.Time
	start  int
}

type threadStats struct {
	key                string
	clientIP           string
	apiKeySuffix       string
	account            string
	model              string
	models             []threadModel
	effort             string
	serviceTier        string
	metadata           turnMetadata
	turns              int64
	compactions        int64
	usage              responseUsage
	latestUsage        responseUsage
	apiCostNanoDollars int64
	unpricedResponses  int64
	ttfb               time.Duration
	latency            time.Duration
	createdAt          time.Time
	segmentStartedAt   time.Time
	last               time.Time
}

type threadModel struct {
	name    string
	efforts []string
}

type modelCost struct {
	apiCostNanoDollars int64
	unpricedResponses  int64
}

type Event struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Account string    `json:"account"`
	Detail  string    `json:"detail"`
}

func newStatsWithPrices(prices priceSnapshot) *Stats {
	now := time.Now()
	return &Stats{
		started:           now,
		usageMonth:        calendarMonth(now),
		prices:            prices,
		monthlyModelCosts: make(map[string]modelCost),
		accounts:          map[string]*accountStats{},
		threads:           map[string]*threadStats{},
		liveThreads:       map[string]int{},
	}
}

func newPersistentStats(store *StateStore, prices priceSnapshot, persistFailed func(error)) (*Stats, error) {
	stats := newStatsWithPrices(prices)
	stats.store = store
	stats.persistFailed = persistFailed
	monthStart := calendarMonthStart(time.Now())
	events, err := store.usageEventsSince(monthStart)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		stats.applyUsageAt(event.At, "", "", event.Model, "", event.ServiceTier, event.Usage)
	}
	return stats, nil
}

func (s *Stats) account(id string) *accountStats {
	a := s.accounts[id]
	if a == nil {
		a = &accountStats{}
		s.accounts[id] = a
	}
	return a
}

func (s *Stats) note(kind, account, detail string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendEvent(Event{At: now, Kind: kind, Account: account, Detail: detail})
}

func eventAccountName(names map[string]string, account string) string {
	if name := names[account]; name != "" {
		return name
	}
	return shortKey(account)
}

func (s *Stats) rateLimited(account string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyRateLimited(now, account)
	s.appendEvent(Event{At: now, Kind: eventRateLimited, Account: account})
}

func (s *Stats) applyRateLimited(now time.Time, account string) {
	s.limited++
	s.account(account).limited.add(now)
}

func (s *Stats) failedOver(account, reason string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	s.appendEvent(Event{At: now, Kind: eventFailover, Account: account, Detail: reason})
}

func (s *Stats) accepted(session, routeThread, statsThread, clientIP, apiKeySuffix, account, model, effort, serviceTier string, metadata turnMetadata, counted bool) bool {
	now := time.Now()
	persisted := s.persistRoute(storedRoute{At: now, Session: session, Thread: routeThread, Account: account})
	if !counted {
		return persisted
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyRouted(now, statsThread, clientIP, account, model, effort, serviceTier, metadata)
	if thread := s.threads[statsThread]; thread != nil {
		thread.apiKeySuffix = apiKeySuffix
	}
	return persisted
}

func (s *Stats) applyRouted(now time.Time, thread, clientIP, account, model, effort, serviceTier string, metadata turnMetadata) {
	s.turns++
	a := s.account(account)
	a.turns++
	a.activity.add(now)

	if thread == "" {
		return
	}
	s.pruneThreadHistory(now)
	t := s.threads[thread]
	if t == nil {
		t = &threadStats{key: thread, createdAt: now, segmentStartedAt: now}
		s.threads[thread] = t
	} else if t.account != "" && t.account != account {
		t.turns = 0
		t.usage = responseUsage{}
		t.latestUsage = responseUsage{}
		t.models = nil
		t.apiCostNanoDollars = 0
		t.unpricedResponses = 0
		t.ttfb = 0
		t.latency = 0
		t.segmentStartedAt = now
	}
	t.clientIP = clientIP
	t.account = account
	t.model = model
	t.effort = effort
	t.serviceTier = serviceTier
	t.metadata = metadata
	t.turns++
	t.last = now
}

func (s *Stats) activateThread(thread string) {
	if thread == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveThreads[thread] == 0 {
		if history := s.threads[thread]; history != nil && !history.last.After(time.Now().Add(-threadHistoryWindow)) {
			delete(s.threads, thread)
		}
	}
	s.liveThreads[thread]++
}

func (s *Stats) deactivateThread(thread string) {
	if thread == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	refs := s.liveThreads[thread]
	if refs > 1 {
		s.liveThreads[thread] = refs - 1
		return
	}
	if refs == 1 {
		delete(s.liveThreads, thread)
	}
}

func (s *Stats) pruneThreadHistory(now time.Time) {
	cutoff := now.Add(-threadHistoryWindow)
	for key, thread := range s.threads {
		if s.liveThreads[key] == 0 && !thread.last.After(cutoff) {
			delete(s.threads, key)
		}
	}
}

func (s *Stats) websocketOpened(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wsOpen++
	s.account(account).wsOpen++
}

func (s *Stats) websocketClosed(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wsOpen--
	s.account(account).wsOpen--
}

func (s *Stats) answered(thread, account string, ttfb time.Duration) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyAnswered(now, thread, account, ttfb)
}

func (s *Stats) applyAnswered(at time.Time, thread, account string, ttfb time.Duration) {
	s.ttfbSum += ttfb
	s.ttfbN++
	if current := s.threads[thread]; current != nil && current.account == account && !at.Before(current.segmentStartedAt) {
		current.ttfb = ttfb
	}
}

func (s *Stats) completed(thread, account string, metadata turnMetadata, latency time.Duration) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyCompleted(now, thread, account, metadata.RequestKind, latency)
}

func (s *Stats) applyCompleted(at time.Time, thread, account, requestKind string, latency time.Duration) {
	if current := s.threads[thread]; current != nil && !at.Before(current.createdAt) {
		if requestKind == "compaction" {
			current.compactions++
		}
		if current.account != account || at.Before(current.segmentStartedAt) {
			return
		}
		current.latency = latency
	}
}

func (s *Stats) recordUsage(thread, account, model, effort, serviceTier string, usage responseUsage) {
	s.recordAPIKeyUsage("", thread, account, model, effort, serviceTier, usage)
}

func (s *Stats) recordAPIKeyUsage(apiKeyName, thread, account, model, effort, serviceTier string, usage responseUsage) {
	if usage.empty() {
		return
	}
	s.pricingMu.Lock()
	defer s.pricingMu.Unlock()
	now := time.Now()
	s.persistUsage(storedUsage{At: now, APIKeyName: apiKeyName, Model: model, ServiceTier: serviceTier, Usage: usage})
	s.applyUsageAt(now, thread, account, model, effort, serviceTier, usage)
}

func (s *Stats) applyUsageAt(at time.Time, thread, account, model, effort, serviceTier string, usage responseUsage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cost, known := s.prices.estimate(model, serviceTier, usage)
	if current := s.threads[thread]; current != nil && current.account == account && !at.Before(current.segmentStartedAt) {
		current.model = model
		if model != "" {
			index := slices.IndexFunc(current.models, func(item threadModel) bool { return item.name == model })
			if index < 0 {
				current.models = append(current.models, threadModel{name: model})
				index = len(current.models) - 1
			}
			if effort != "" && !slices.Contains(current.models[index].efforts, effort) {
				current.models[index].efforts = append(current.models[index].efforts, effort)
			}
		}
		current.usage.add(usage)
		current.latestUsage = usage
		if known {
			current.apiCostNanoDollars += cost
		} else {
			current.unpricedResponses++
		}
	}
	month := calendarMonth(at)
	if month < s.usageMonth {
		return
	}
	s.syncUsageMonth(month)
	s.monthlyUsage.add(usage)
	modelCost := s.monthlyModelCosts[model]
	if !known {
		s.unpricedResponses++
		modelCost.unpricedResponses++
		s.monthlyModelCosts[model] = modelCost
		return
	}
	s.apiCostNanoDollars += cost
	modelCost.apiCostNanoDollars += cost
	s.monthlyModelCosts[model] = modelCost
}

func (s *Stats) reprice(prices priceSnapshot) error {
	if s.store == nil {
		return errors.New("price refresh needs state store")
	}
	s.pricingMu.Lock()
	defer s.pricingMu.Unlock()
	now := time.Now()
	monthStart := calendarMonthStart(now)
	events, err := s.store.usageEventsSince(monthStart)
	if err != nil {
		return err
	}
	var usage responseUsage
	var cost, unpriced int64
	modelCosts := make(map[string]modelCost)
	for _, event := range events {
		usage.add(event.Usage)
		price, known := prices.estimate(event.Model, event.ServiceTier, event.Usage)
		modelCost := modelCosts[event.Model]
		if !known {
			unpriced++
			modelCost.unpricedResponses++
			modelCosts[event.Model] = modelCost
			continue
		}
		cost += price
		modelCost.apiCostNanoDollars += price
		modelCosts[event.Model] = modelCost
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usageMonth = calendarMonth(now)
	s.monthlyUsage = usage
	s.apiCostNanoDollars = cost
	s.unpricedResponses = unpriced
	s.monthlyModelCosts = modelCosts
	s.prices = prices
	return nil
}

func (s *Stats) appendEvent(event Event) {
	s.events = append(s.events, event)
	if len(s.events) > eventLog {
		s.events = s.events[len(s.events)-eventLog:]
	}
}

func (s *Stats) persistRoute(route storedRoute) bool {
	if s.store == nil {
		return true
	}
	if err := s.store.recordRoute(route); err != nil {
		if s.persistFailed != nil {
			s.persistFailed(err)
		}
		return false
	}
	return true
}

func (s *Stats) persistUsage(event storedUsage) {
	if s.store == nil {
		return
	}
	if err := s.store.recordUsage(event); err != nil && s.persistFailed != nil {
		s.persistFailed(err)
	}
}

type Snapshot struct {
	Uptime             time.Duration
	Turns              int64
	Failures           int64
	Limited            int64
	TTFB               time.Duration
	WSOpen             int64
	MonthlyUsage       responseUsage
	APICostNanoDollars int64
	UnpricedResponses  int64
	ModelCosts         []ModelCostSnapshot
	PriceFetchedAt     time.Time
	Accounts           map[string]AccountSnapshot
	Threads            []ThreadSnapshot
	Events             []Event
}

type ModelCostSnapshot struct {
	Model              string
	APICostNanoDollars int64
	UnpricedResponses  int64
}

type AccountSnapshot struct {
	Turns    int64
	Limited  int64
	WSOpen   int64
	Activity []int64
}

type ThreadSnapshot struct {
	Key                string `json:"key"`
	ClientIP           string `json:"-"`
	APIKeySuffix       string `json:"-"`
	Account            string `json:"account"`
	Model              string `json:"model"`
	models             []threadModel
	Effort             string `json:"reasoning_effort"`
	ServiceTier        string `json:"service_tier"`
	Metadata           turnMetadata
	Turns              int64 `json:"turns"`
	Compactions        int64 `json:"compactions"`
	Usage              responseUsage
	LatestUsage        responseUsage
	apiCostNanoDollars int64
	unpricedResponses  int64
	TTFB               time.Duration
	Latency            time.Duration
	Last               time.Time `json:"last"`
}

func (s *Stats) snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.syncUsageMonth(calendarMonth(now))
	s.pruneThreadHistory(now)

	out := Snapshot{
		Uptime:             time.Since(s.started),
		Turns:              s.turns,
		Failures:           s.failures,
		Limited:            s.limited,
		WSOpen:             s.wsOpen,
		MonthlyUsage:       s.monthlyUsage,
		APICostNanoDollars: s.apiCostNanoDollars,
		UnpricedResponses:  s.unpricedResponses,
		ModelCosts:         make([]ModelCostSnapshot, 0, len(s.monthlyModelCosts)),
		PriceFetchedAt:     s.prices.fetchedAt,
		Accounts:           make(map[string]AccountSnapshot, len(s.accounts)),
		Threads:            make([]ThreadSnapshot, 0, len(s.threads)),
		Events:             append([]Event{}, s.events...),
	}
	for model, cost := range s.monthlyModelCosts {
		out.ModelCosts = append(out.ModelCosts, ModelCostSnapshot{
			Model:              model,
			APICostNanoDollars: cost.apiCostNanoDollars,
			UnpricedResponses:  cost.unpricedResponses,
		})
	}
	slices.SortFunc(out.ModelCosts, func(left, right ModelCostSnapshot) int {
		if left.APICostNanoDollars != right.APICostNanoDollars {
			return cmp.Compare(right.APICostNanoDollars, left.APICostNanoDollars)
		}
		return strings.Compare(left.Model, right.Model)
	})
	if s.ttfbN > 0 {
		out.TTFB = s.ttfbSum / time.Duration(s.ttfbN)
	}
	for id, a := range s.accounts {
		activity := a.activity.recent(now)
		limited := a.limited.recent(now)
		out.Accounts[id] = AccountSnapshot{
			Turns:    a.turns,
			Limited:  int64(len(limited)),
			WSOpen:   a.wsOpen,
			Activity: accountActivity(now, activity),
		}
	}
	for _, t := range s.threads {
		if s.liveThreads[t.key] == 0 {
			continue
		}
		models := make([]threadModel, len(t.models))
		for index, model := range t.models {
			models[index] = threadModel{name: model.name, efforts: append([]string(nil), model.efforts...)}
		}
		out.Threads = append(out.Threads, ThreadSnapshot{
			Key:                t.key,
			ClientIP:           t.clientIP,
			APIKeySuffix:       t.apiKeySuffix,
			Account:            t.account,
			Model:              t.model,
			models:             models,
			Effort:             t.effort,
			ServiceTier:        t.serviceTier,
			Metadata:           t.metadata,
			Turns:              t.turns,
			Compactions:        t.compactions,
			Usage:              t.usage,
			LatestUsage:        t.latestUsage,
			apiCostNanoDollars: t.apiCostNanoDollars,
			unpricedResponses:  t.unpricedResponses,
			TTFB:               t.ttfb,
			Latency:            t.latency,
			Last:               t.last,
		})
	}
	slices.SortFunc(out.Threads, func(left, right ThreadSnapshot) int {
		if left.Last.After(right.Last) {
			return -1
		}
		if left.Last.Before(right.Last) {
			return 1
		}
		return strings.Compare(left.Key, right.Key)
	})
	return out
}

func calendarMonth(value time.Time) int {
	start := calendarMonthStart(value)
	return start.Year()*12 + int(start.Month()) - 1
}

func calendarMonthStart(value time.Time) time.Time {
	year, month, _ := value.Date()
	return time.Date(year, month, 1, 0, 0, 0, 0, value.Location())
}

func (s *Stats) syncUsageMonth(month int) {
	if month == s.usageMonth {
		return
	}
	s.usageMonth = month
	s.monthlyUsage = responseUsage{}
	s.apiCostNanoDollars = 0
	s.unpricedResponses = 0
	s.monthlyModelCosts = make(map[string]modelCost)
}

func (c *rollingCounter) add(at time.Time) {
	c.events = append(c.events, at)
	c.recent(at)
}

func (c *rollingCounter) recent(now time.Time) []time.Time {
	cutoff := now.Add(-accountStatsWindow)
	for c.start < len(c.events) && !c.events[c.start].After(cutoff) {
		c.start++
	}
	if c.start == len(c.events) {
		c.events = c.events[:0]
		c.start = 0
		return c.events
	}
	recent := c.events[c.start:]
	if c.start >= 1024 && c.start*2 >= len(c.events) {
		copy(c.events, recent)
		c.events = c.events[:len(recent)]
		c.start = 0
		return c.events
	}
	return recent
}

func accountActivity(now time.Time, events []time.Time) []int64 {
	activity := make([]int64, activityLen)
	for _, at := range events {
		age := max(now.Sub(at), 0)
		bucket := min(int(age/activitySpan), activityLen-1)
		activity[bucket]++
	}
	return activity
}

func activityTotal(activity []int64) int64 {
	var total int64
	for _, count := range activity {
		total += count
	}
	return total
}

type statsResponse struct {
	UptimeSeconds           float64                `json:"uptime_seconds"`
	Turns                   int64                  `json:"turns"`
	OpenWebSockets          int64                  `json:"open_websockets"`
	Threads                 int                    `json:"threads"`
	Failovers               int64                  `json:"failovers"`
	RateLimits              int64                  `json:"rate_limits"`
	AverageTTFBMilliseconds float64                `json:"average_ttfb_ms"`
	Accounts                []accountStatsResponse `json:"accounts"`
}

type accountStatsResponse struct {
	ID                     string                        `json:"id"`
	Email                  string                        `json:"email,omitempty"`
	Plan                   string                        `json:"plan"`
	Status                 accountStatus                 `json:"status"`
	RoutingMode            routingMode                   `json:"routing_mode"`
	RoutingPriority        *routingPriorityStatsResponse `json:"routing_priority,omitempty"`
	WeeklyRemainingPercent *float64                      `json:"weekly_remaining_percent"`
	BankedResets           *int64                        `json:"banked_resets"`
	ResetCredits           []resetCreditStatsResponse    `json:"reset_credits,omitempty"`
	ResetAt                *time.Time                    `json:"reset_at"`
	CreditBurn             *float64                      `json:"credit_burn,omitempty"`
	CreditBurnSince        *time.Time                    `json:"credit_burn_since,omitempty"`
	SpendControl           *spendControlStatsResponse    `json:"spend_control,omitempty"`
	Turns                  int64                         `json:"turns"`
	OpenWebSockets         int64                         `json:"open_websockets"`
	RateLimits             int64                         `json:"rate_limits"`
	Activity               []int64                       `json:"activity"`
}

type spendControlStatsResponse struct {
	Reached          bool       `json:"reached"`
	Source           string     `json:"source,omitempty"`
	Limit            string     `json:"limit,omitempty"`
	Used             string     `json:"used,omitempty"`
	Remaining        string     `json:"remaining,omitempty"`
	UsedPercent      *float64   `json:"used_percent,omitempty"`
	RemainingPercent *float64   `json:"remaining_percent,omitempty"`
	ResetAt          *time.Time `json:"reset_at,omitempty"`
}

type routingPriorityStatsResponse struct {
	ExpiresAt        time.Time `json:"expires_at"`
	RemainingPercent float64   `json:"remaining_percent"`
}

type resetCreditStatsResponse struct {
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (s *server) statsJSON(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.currentStats(time.Now()))
}

func (s *server) currentStats(now time.Time) statsResponse {
	return s.statsResponseAt(now, s.stats.snapshot())
}

func (s *server) statsResponseAt(now time.Time, snapshot Snapshot) statsResponse {
	out := statsResponse{
		UptimeSeconds:           snapshot.Uptime.Seconds(),
		Turns:                   snapshot.Turns,
		OpenWebSockets:          snapshot.WSOpen,
		Threads:                 len(snapshot.Threads),
		Failovers:               snapshot.Failures,
		RateLimits:              snapshot.Limited,
		AverageTTFBMilliseconds: float64(snapshot.TTFB) / float64(time.Millisecond),
		Accounts:                make([]accountStatsResponse, 0, s.pool.count()),
	}
	for _, account := range s.pool.sorted() {
		claims := account.claims()
		plan := account.plan()
		candidate := account.routingCandidate()
		primary, secondary := candidate.primary, candidate.secondary
		traffic := snapshot.Accounts[claims.Auth.AccountID]
		weekly := longestWindow(primary, secondary)
		var weeklyRemaining *float64
		if remaining, known := remainingPercent(weekly); known {
			weeklyRemaining = &remaining
		}
		var bankedResets *int64
		var resetCredits []resetCreditStatsResponse
		if candidate.resetCredits.known {
			bankedResets = &candidate.resetCredits.count
			for _, credit := range candidate.resetCredits.details {
				if !credit.available() {
					continue
				}
				resetCredits = append(resetCredits, resetCreditStatsResponse{
					ExpiresAt: credit.ExpiresAt,
				})
			}
		}
		var resetAt *time.Time
		if reset := weekly.resetsAt; reset.After(now) {
			resetAt = &reset
		}
		var creditBurn *float64
		var creditBurnSince *time.Time
		if burn, since, known := account.creditBurnSinceReset(now); known {
			creditBurn = &burn
			creditBurnSince = &since
		}
		var routingPriority *routingPriorityStatsResponse
		status := candidate.status(now)
		if priority, ok := candidate.routingPriority(now); ok && status == accountPriority {
			routingPriority = &routingPriorityStatsResponse{
				ExpiresAt:        priority.expiresAt,
				RemainingPercent: priority.remainingPercent,
			}
		}
		spendControl := spendControlStats(candidate.spendControl)
		out.Accounts = append(out.Accounts, accountStatsResponse{
			ID:                     claims.Auth.AccountID,
			Email:                  maskEmail(claims.Email),
			Plan:                   plan,
			Status:                 status,
			RoutingMode:            candidate.mode,
			RoutingPriority:        routingPriority,
			WeeklyRemainingPercent: weeklyRemaining,
			BankedResets:           bankedResets,
			ResetCredits:           resetCredits,
			ResetAt:                resetAt,
			CreditBurn:             creditBurn,
			CreditBurnSince:        creditBurnSince,
			SpendControl:           spendControl,
			Turns:                  traffic.Turns,
			OpenWebSockets:         traffic.WSOpen,
			RateLimits:             traffic.Limited,
			Activity:               append([]int64{}, traffic.Activity...),
		})
	}
	return out
}

func spendControlStats(control *spendControlPayload) *spendControlStatsResponse {
	if control == nil {
		return nil
	}
	out := &spendControlStatsResponse{Reached: control.Reached}
	if limit := control.IndividualLimit; limit != nil {
		out.Source = limit.Source
		out.Limit = limit.Limit
		out.Used = limit.Used
		out.Remaining = limit.Remaining
		out.UsedPercent = limit.UsedPercent
		out.RemainingPercent = limit.RemainingPercent
		if limit.ResetAt > 0 {
			resetAt := time.Unix(limit.ResetAt, 0)
			out.ResetAt = &resetAt
		}
	}
	return out
}

func maskEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		if email == "" {
			return ""
		}
		return "***"
	}
	local := []rune(email[:at])
	domain := email[at+1:]
	suffix := ""
	if dot := strings.LastIndexByte(domain, '.'); dot > 0 && dot < len(domain)-1 {
		suffix = domain[dot:]
	}
	maskedDomain := "@***" + suffix
	switch len(local) {
	case 1:
		return "***" + maskedDomain
	case 2:
		return string(local[0]) + "***" + maskedDomain
	default:
		return string(local[0]) + "***" + string(local[len(local)-1]) + maskedDomain
	}
}

func requestIP(r *http.Request) string {
	forwarded := r.Header.Values("X-Forwarded-For")
	for i := len(forwarded) - 1; i >= 0; i-- {
		addresses := strings.Split(forwarded[i], ",")
		for j := len(addresses) - 1; j >= 0; j-- {
			if address, err := netip.ParseAddr(strings.TrimSpace(addresses[j])); err == nil {
				return address.Unmap().String()
			}
		}
	}
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		address = r.RemoteAddr
	}
	parsed, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil {
		return ""
	}
	return parsed.Unmap().String()
}
