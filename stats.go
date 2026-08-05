package main

import (
	"sync"
	"time"
)

const (
	eventLog     = 200
	threadLog    = 100
	activityLen  = 24
	activitySpan = 30 * time.Second
)

type Stats struct {
	mu       sync.Mutex
	started  time.Time
	turns    int64
	failures int64
	limited  int64
	ttfbSum  time.Duration
	ttfbN    int64
	accounts map[string]*accountStats
	threads  map[string]*threadStats
	events   []Event
}

type accountStats struct {
	turns    int64
	limited  int64
	activity [activityLen]int64
	bucket   int64
}

type threadStats struct {
	key     string
	account string
	turns   int64
	last    time.Time
}

type Event struct {
	At      time.Time
	Kind    string
	Account string
	Detail  string
}

func newStats() *Stats {
	return &Stats{
		started:  time.Now(),
		accounts: map[string]*accountStats{},
		threads:  map[string]*threadStats{},
	}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, Event{At: time.Now(), Kind: kind, Account: account, Detail: detail})
	if len(s.events) > eventLog {
		s.events = s.events[len(s.events)-eventLog:]
	}
}

func (s *Stats) rateLimited(account string) {
	s.mu.Lock()
	s.limited++
	s.account(account).limited++
	s.mu.Unlock()
	s.note("rate limited", account, "")
}

func (s *Stats) failedOver(account, reason string) {
	s.mu.Lock()
	s.failures++
	s.mu.Unlock()
	s.note("failover", account, reason)
}

func (s *Stats) routed(thread, account string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.turns++
	a := s.account(account)
	a.turns++
	slot := now.UnixNano() / int64(activitySpan)
	if slot != a.bucket {
		shift := min(slot-a.bucket, activityLen)
		copy(a.activity[shift:], a.activity[:activityLen-shift])
		clear(a.activity[:shift])
		a.bucket = slot
	}
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
	t.turns++
	t.last = now
	s.trimThreads()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ttfbSum += ttfb
	s.ttfbN++
}

type Snapshot struct {
	Uptime   time.Duration
	Turns    int64
	Failures int64
	Limited  int64
	TTFB     time.Duration
	Accounts map[string]AccountSnapshot
	Threads  []ThreadSnapshot
	Events   []Event
}

type AccountSnapshot struct {
	Turns    int64
	Limited  int64
	Activity []int64
}

type ThreadSnapshot struct {
	Key     string
	Account string
	Turns   int64
	Last    time.Time
}

func (s *Stats) snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := Snapshot{
		Uptime:   time.Since(s.started),
		Turns:    s.turns,
		Failures: s.failures,
		Limited:  s.limited,
		Accounts: make(map[string]AccountSnapshot, len(s.accounts)),
		Threads:  make([]ThreadSnapshot, 0, len(s.threads)),
		Events:   append([]Event(nil), s.events...),
	}
	if s.ttfbN > 0 {
		out.TTFB = s.ttfbSum / time.Duration(s.ttfbN)
	}
	for id, a := range s.accounts {
		out.Accounts[id] = AccountSnapshot{
			Turns:    a.turns,
			Limited:  a.limited,
			Activity: append([]int64(nil), a.activity[:]...),
		}
	}
	for _, t := range s.threads {
		out.Threads = append(out.Threads, ThreadSnapshot{
			Key:     t.key,
			Account: t.account,
			Turns:   t.turns,
			Last:    t.last,
		})
	}
	return out
}
