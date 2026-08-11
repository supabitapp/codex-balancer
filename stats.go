package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	eventLog           = 200
	threadActiveWindow = 5 * time.Minute
	activityLen        = 24
	activitySpan       = 30 * time.Second

	eventRateLimited       = "rate limited"
	eventFailover          = "failover"
	eventResponseAnswered  = "response answered"
	eventResponseCompleted = "response completed"
	eventResponseUsage     = "response usage"
	eventCompactionSwitch  = "compaction switch"
	eventLegacyReconnect   = "rotation reconnect"
	eventLegacyRotated     = "rotated"
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
	usageMonth         int
	monthlyUsage       responseUsage
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
	key              string
	clientIP         string
	account          string
	model            string
	effort           string
	serviceTier      string
	metadata         turnMetadata
	turns            int64
	compactions      int64
	usage            responseUsage
	latestUsage      responseUsage
	ttfb             time.Duration
	latency          time.Duration
	createdAt        time.Time
	segmentStartedAt time.Time
	last             time.Time
	via              transport
}

type Event struct {
	At            time.Time `json:"at"`
	Kind          string    `json:"kind"`
	Account       string    `json:"account"`
	SourceAccount string    `json:"source_account,omitempty"`
	Thread        string    `json:"thread,omitempty"`
	Detail        string    `json:"detail"`
}

func newStats() *Stats {
	now := time.Now()
	return &Stats{
		started:    now,
		usageMonth: calendarMonth(now),
		accounts:   map[string]*accountStats{},
		threads:    map[string]*threadStats{},
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

func (s *Stats) compactionSwitched(thread, source, target string) {
	now := time.Now()
	s.persistEvent(storedEvent{At: now, Kind: eventCompactionSwitch, Account: target, Thread: thread, Detail: source})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendEvent(Event{At: now, Kind: eventCompactionSwitch, Account: target, SourceAccount: source, Thread: thread})
}

func eventText(event Event, names map[string]string) (string, string, bool) {
	switch event.Kind {
	case eventLegacyReconnect, eventLegacyRotated:
		return "", "", false
	case eventCompactionSwitch:
		return eventAccountName(names, event.SourceAccount) + " → " + eventAccountName(names, event.Account), shortKey(event.Thread), true
	}
	return eventAccountName(names, event.Account), event.Detail, true
}

func eventAccountName(names map[string]string, account string) string {
	if name := names[account]; name != "" {
		return name
	}
	return shortKey(account)
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

func (s *Stats) routed(thread, clientIP, account, model, effort, serviceTier string, via transport, metadata turnMetadata) {
	now := time.Now()
	s.persistAttempt(storedAttempt{At: now, Thread: thread, ClientIP: clientIP, Account: account, Effort: effort, ServiceTier: serviceTier, Transport: via, Metadata: encodeTurnMetadata(metadata)})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyRouted(now, thread, clientIP, account, model, effort, serviceTier, via, metadata)
}

func (s *Stats) applyRouted(now time.Time, thread, clientIP, account, model, effort, serviceTier string, via transport, metadata turnMetadata) {
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
	s.pruneInactiveThreads(now)
	t := s.threads[thread]
	if t == nil {
		t = &threadStats{key: thread, createdAt: now, segmentStartedAt: now}
		s.threads[thread] = t
	} else if t.account != "" && t.account != account {
		t.turns = 0
		t.usage = responseUsage{}
		t.latestUsage = responseUsage{}
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
	t.via = via
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

func (s *Stats) pruneInactiveThreads(now time.Time) {
	cutoff := now.Add(-threadActiveWindow)
	for key, t := range s.threads {
		if !t.last.After(cutoff) {
			delete(s.threads, key)
		}
	}
}

func (s *Stats) answered(thread, account string, ttfb time.Duration) {
	now := time.Now()
	s.persistEvent(storedEvent{At: now, Kind: eventResponseAnswered, Account: account, Thread: thread, Duration: ttfb})
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
	s.persistEvent(storedEvent{At: now, Kind: eventResponseCompleted, Account: account, Thread: thread, Detail: metadata.RequestKind, Duration: latency})
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

func (s *Stats) recordUsage(thread, account, model, serviceTier string, usage responseUsage) {
	if usage.empty() {
		return
	}
	now := time.Now()
	s.persistEvent(storedEvent{At: now, Kind: eventResponseUsage, Account: account, Thread: thread, Model: model, ServiceTier: serviceTier, Usage: usage})
	s.applyUsageAt(now, thread, account, model, serviceTier, usage)
}

func (s *Stats) applyUsageAt(at time.Time, thread, account, model, serviceTier string, usage responseUsage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.threads[thread]; current != nil && current.account == account && !at.Before(current.segmentStartedAt) {
		current.model = model
		current.usage.add(usage)
		current.latestUsage = usage
	}
	month := calendarMonth(at)
	if month < s.usageMonth {
		return
	}
	s.syncUsageMonth(month)
	s.monthlyUsage.add(usage)
	cost, known := estimateAPIPrice(model, serviceTier, usage)
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
	MonthlyUsage       responseUsage
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
	Key         string `json:"key"`
	ClientIP    string `json:"-"`
	Account     string `json:"account"`
	Model       string `json:"model"`
	Effort      string `json:"reasoning_effort"`
	ServiceTier string `json:"service_tier"`
	Metadata    turnMetadata
	Turns       int64 `json:"turns"`
	Compactions int64 `json:"compactions"`
	Usage       responseUsage
	LatestUsage responseUsage
	TTFB        time.Duration
	Latency     time.Duration
	Last        time.Time `json:"last"`
	Via         transport `json:"via"`
}

func (s *Stats) snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.syncUsageMonth(calendarMonth(now))
	s.pruneInactiveThreads(now)
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
		MonthlyUsage:       s.monthlyUsage,
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
			ClientIP:    t.clientIP,
			Account:     t.account,
			Model:       t.model,
			Effort:      t.effort,
			ServiceTier: t.serviceTier,
			Metadata:    t.metadata,
			Turns:       t.turns,
			Compactions: t.compactions,
			Usage:       t.usage,
			LatestUsage: t.latestUsage,
			TTFB:        t.ttfb,
			Latency:     t.latency,
			Last:        t.last,
			Via:         t.via,
		})
	}
	slices.SortFunc(out.Threads, func(left, right ThreadSnapshot) int {
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
	ID                     string                        `json:"id"`
	Email                  string                        `json:"email,omitempty"`
	Plan                   string                        `json:"plan"`
	Status                 accountStatus                 `json:"status"`
	RoutingPriority        *routingPriorityStatsResponse `json:"routing_priority,omitempty"`
	WeeklyRemainingPercent *float64                      `json:"weekly_remaining_percent"`
	BankedResets           *int64                        `json:"banked_resets"`
	ResetCredits           []resetCreditStatsResponse    `json:"reset_credits,omitempty"`
	ResetAt                *time.Time                    `json:"reset_at"`
	Turns                  int64                         `json:"turns"`
	OpenWebSockets         int64                         `json:"open_websockets"`
	RateLimits             int64                         `json:"rate_limits"`
	Activity               []int64                       `json:"activity"`
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
		if reset := nextReset(now, primary, secondary); !reset.IsZero() {
			resetAt = &reset
		}
		var routingPriority *routingPriorityStatsResponse
		status := candidate.status(now)
		if priority, ok := candidate.routingPriority(now); ok && status == accountPriority {
			routingPriority = &routingPriorityStatsResponse{
				ExpiresAt:        priority.expiresAt,
				RemainingPercent: priority.remainingPercent,
			}
		}
		out.Accounts = append(out.Accounts, accountStatsResponse{
			ID:                     claims.Auth.AccountID,
			Email:                  maskEmail(claims.Email),
			Plan:                   claims.Auth.Plan,
			Status:                 status,
			RoutingPriority:        routingPriority,
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

func clientIDForIP(ip string, key []byte) string {
	if ip == "" || len(key) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("codex-balancer-client\x00"))
	mac.Write([]byte(ip))
	return hex.EncodeToString(mac.Sum(nil)[:4])
}
