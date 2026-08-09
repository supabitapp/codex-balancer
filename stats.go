package main

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	eventLog     = 200
	threadLog    = 100
	activityLen  = 24
	activitySpan = 30 * time.Second

	eventRateLimited      = "rate limited"
	eventFailover         = "failover"
	eventResponseAnswered = "response answered"
	eventResponseUsage    = "response usage"
)

type transport string

const (
	transportHTTP      transport = "http"
	transportWebSocket transport = "ws"
	serviceTierFast              = "priority"
)

type Stats struct {
	mu                 sync.Mutex
	store              *StateStore
	persistFailed      func(error)
	started            time.Time
	turns              int64
	failures           int64
	limited            int64
	ttfbSum            time.Duration
	ttfbN              int64
	wsTurns            int64
	wsOpen             int64
	apiCostNanoDollars int64
	unpricedResponses  int64
	accounts           map[string]*accountStats
	threads            map[string]*threadStats
	events             []Event
}

type accountStats struct {
	turns    int64
	limited  int64
	wsOpen   int64
	activity [activityLen]int64
	bucket   int64
}

type threadStats struct {
	key         string
	account     string
	serviceTier string
	turns       int64
	last        time.Time
	via         transport
}

type Event struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Account string    `json:"account"`
	Detail  string    `json:"detail"`
}

func newStats() *Stats {
	return &Stats{
		started:  time.Now(),
		accounts: map[string]*accountStats{},
		threads:  map[string]*threadStats{},
	}
}

func newPersistentStats(store *StateStore, persistFailed func(error)) (*Stats, error) {
	stats := newStats()
	stats.store = store
	stats.persistFailed = persistFailed
	if err := store.restoreStats(stats); err != nil {
		return nil, err
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
	s.persistEvent(storedEvent{At: now, Kind: kind, Account: account, Detail: detail})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendEvent(Event{At: now, Kind: kind, Account: account, Detail: detail})
}

func (s *Stats) rateLimited(account string) {
	now := time.Now()
	s.persistEvent(storedEvent{At: now, Kind: eventRateLimited, Account: account})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limited++
	s.account(account).limited++
	s.appendEvent(Event{At: now, Kind: eventRateLimited, Account: account})
}

func (s *Stats) failedOver(account, reason string) {
	now := time.Now()
	s.persistEvent(storedEvent{At: now, Kind: eventFailover, Account: account, Detail: reason})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	s.appendEvent(Event{At: now, Kind: eventFailover, Account: account, Detail: reason})
}

func (s *Stats) routed(thread, account, serviceTier string, via transport) {
	now := time.Now()
	s.persistAttempt(storedAttempt{At: now, Thread: thread, Account: account, ServiceTier: serviceTier, Transport: via})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyRouted(now, thread, account, serviceTier, via)
}

func (s *Stats) applyRouted(now time.Time, thread, account, serviceTier string, via transport) {
	s.turns++
	if via == transportWebSocket {
		s.wsTurns++
	}
	a := s.account(account)
	a.turns++
	advanceActivity(a, now)
	a.activity[0]++

	if thread == "" {
		return
	}
	t := s.threads[thread]
	if t == nil {
		t = &threadStats{key: thread}
		s.threads[thread] = t
	}
	t.account = account
	t.serviceTier = serviceTier
	t.turns++
	t.last = now
	t.via = via
	s.trimThreads()
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

func (s *Stats) trimThreads() {
	if len(s.threads) <= threadLog {
		return
	}
	oldest, at := "", time.Now()
	for key, t := range s.threads {
		if t.last.Before(at) {
			oldest, at = key, t.last
		}
	}
	delete(s.threads, oldest)
}

func (s *Stats) answered(ttfb time.Duration) {
	s.persistEvent(storedEvent{At: time.Now(), Kind: eventResponseAnswered, Duration: ttfb})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ttfbSum += ttfb
	s.ttfbN++
}

func (s *Stats) recordUsage(model, serviceTier string, usage responseUsage) {
	if usage.empty() {
		return
	}
	s.persistEvent(storedEvent{At: time.Now(), Kind: eventResponseUsage, Model: model, ServiceTier: serviceTier, Usage: usage})
	s.applyUsage(model, serviceTier, usage)
}

func (s *Stats) applyUsage(model, serviceTier string, usage responseUsage) {
	cost, known := estimateAPIPrice(model, serviceTier, usage)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !known {
		s.unpricedResponses++
		return
	}
	s.apiCostNanoDollars += cost
}

func (s *Stats) appendEvent(event Event) {
	s.events = append(s.events, event)
	if len(s.events) > eventLog {
		s.events = s.events[len(s.events)-eventLog:]
	}
}

func (s *Stats) persistAttempt(attempt storedAttempt) {
	if s.store == nil {
		return
	}
	if err := s.store.recordAttempt(attempt); err != nil && s.persistFailed != nil {
		s.persistFailed(err)
	}
}

func (s *Stats) persistEvent(event storedEvent) {
	if s.store == nil {
		return
	}
	if err := s.store.recordEvent(event); err != nil && s.persistFailed != nil {
		s.persistFailed(err)
	}
}

type Snapshot struct {
	Uptime             time.Duration
	Turns              int64
	Failures           int64
	Limited            int64
	TTFB               time.Duration
	WSTurns            int64
	WSOpen             int64
	APICostNanoDollars int64
	UnpricedResponses  int64
	Accounts           map[string]AccountSnapshot
	Threads            []ThreadSnapshot
	Events             []Event
}

type AccountSnapshot struct {
	Turns    int64
	Limited  int64
	WSOpen   int64
	Activity []int64
}

type ThreadSnapshot struct {
	Key         string    `json:"key"`
	Account     string    `json:"account"`
	ServiceTier string    `json:"service_tier"`
	Turns       int64     `json:"turns"`
	Last        time.Time `json:"last"`
	Via         transport `json:"via"`
}

func (s *Stats) snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, account := range s.accounts {
		advanceActivity(account, now)
	}

	out := Snapshot{
		Uptime:             time.Since(s.started),
		Turns:              s.turns,
		Failures:           s.failures,
		Limited:            s.limited,
		WSTurns:            s.wsTurns,
		WSOpen:             s.wsOpen,
		APICostNanoDollars: s.apiCostNanoDollars,
		UnpricedResponses:  s.unpricedResponses,
		Accounts:           make(map[string]AccountSnapshot, len(s.accounts)),
		Threads:            make([]ThreadSnapshot, 0, len(s.threads)),
		Events:             append([]Event{}, s.events...),
	}
	if s.ttfbN > 0 {
		out.TTFB = s.ttfbSum / time.Duration(s.ttfbN)
	}
	for id, a := range s.accounts {
		out.Accounts[id] = AccountSnapshot{
			Turns:    a.turns,
			Limited:  a.limited,
			WSOpen:   a.wsOpen,
			Activity: append([]int64(nil), a.activity[:]...),
		}
	}
	for _, t := range s.threads {
		out.Threads = append(out.Threads, ThreadSnapshot{
			Key:         t.key,
			Account:     t.account,
			ServiceTier: t.serviceTier,
			Turns:       t.turns,
			Last:        t.last,
			Via:         t.via,
		})
	}
	return out
}

func advanceActivity(account *accountStats, now time.Time) {
	slot := now.UnixNano() / int64(activitySpan)
	if account.bucket == 0 {
		account.bucket = slot
		return
	}
	if slot <= account.bucket {
		return
	}
	shift := min(slot-account.bucket, activityLen)
	copy(account.activity[shift:], account.activity[:activityLen-shift])
	clear(account.activity[:shift])
	account.bucket = slot
}

type statsResponse struct {
	UptimeSeconds           float64                `json:"uptime_seconds"`
	Turns                   int64                  `json:"turns"`
	WebSocketTurns          int64                  `json:"websocket_turns"`
	OpenWebSockets          int64                  `json:"open_websockets"`
	Threads                 int                    `json:"threads"`
	Failovers               int64                  `json:"failovers"`
	RateLimits              int64                  `json:"rate_limits"`
	AverageTTFBMilliseconds float64                `json:"average_ttfb_ms"`
	Accounts                []accountStatsResponse `json:"accounts"`
}

type accountStatsResponse struct {
	ID                     string                     `json:"id"`
	Email                  string                     `json:"email,omitempty"`
	Plan                   string                     `json:"plan"`
	Status                 accountStatus              `json:"status"`
	WeeklyRemainingPercent *float64                   `json:"weekly_remaining_percent"`
	BankedResets           *int64                     `json:"banked_resets"`
	ResetCredits           []resetCreditStatsResponse `json:"reset_credits,omitempty"`
	ResetAt                *time.Time                 `json:"reset_at"`
	Turns                  int64                      `json:"turns"`
	OpenWebSockets         int64                      `json:"open_websockets"`
	RateLimits             int64                      `json:"rate_limits"`
	Activity               []int64                    `json:"activity"`
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
		WebSocketTurns:          snapshot.WSTurns,
		OpenWebSockets:          snapshot.WSOpen,
		Threads:                 len(snapshot.Threads),
		Failovers:               snapshot.Failures,
		RateLimits:              snapshot.Limited,
		AverageTTFBMilliseconds: float64(snapshot.TTFB) / float64(time.Millisecond),
		Accounts:                make([]accountStatsResponse, 0, s.pool.count()),
	}
	for _, account := range s.pool.sorted() {
		claims := account.claims()
		primary, secondary, _, _ := account.health()
		traffic := snapshot.Accounts[claims.Auth.AccountID]
		weekly := longestWindow(primary, secondary)
		var weeklyRemaining *float64
		if remaining, known := remainingPercent(weekly); known {
			weeklyRemaining = &remaining
		}
		var bankedResets *int64
		var resetCredits []resetCreditStatsResponse
		if count, credits, known := account.bankedResets(); known {
			bankedResets = &count
			for _, credit := range credits {
				if !credit.available() {
					continue
				}
				resetCredits = append(resetCredits, resetCreditStatsResponse{
					ExpiresAt: credit.ExpiresAt,
				})
			}
		}
		var resetAt *time.Time
		if reset := nextReset(now, primary, secondary); !reset.IsZero() {
			resetAt = &reset
		}
		out.Accounts = append(out.Accounts, accountStatsResponse{
			ID:                     claims.Auth.AccountID,
			Email:                  maskEmail(claims.Email),
			Plan:                   claims.Auth.Plan,
			Status:                 account.status(now),
			WeeklyRemainingPercent: weeklyRemaining,
			BankedResets:           bankedResets,
			ResetCredits:           resetCredits,
			ResetAt:                resetAt,
			Turns:                  traffic.Turns,
			OpenWebSockets:         traffic.WSOpen,
			RateLimits:             traffic.Limited,
			Activity:               append([]int64{}, traffic.Activity...),
		})
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
