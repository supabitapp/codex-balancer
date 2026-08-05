package main

import (
	"net/http"
	"sync"
	"time"
)

const stickyTTL = 12 * time.Hour

var stickyHeaders = []string{
	"session-id",
	"thread-id",
}

func stickyKey(h http.Header) string {
	for _, name := range stickyHeaders {
		if v := h.Get(name); v != "" {
			return v
		}
	}
	return ""
}

type Sticky struct {
	mu sync.Mutex
	m  map[string]stickyBinding
}

type stickyBinding struct {
	account string
	seen    time.Time
}

func newSticky() *Sticky {
	return &Sticky{m: map[string]stickyBinding{}}
}

func (s *Sticky) get(key string) string {
	if key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[key]
	if !ok {
		return ""
	}
	if time.Since(b.seen) > stickyTTL {
		delete(s.m, key)
		return ""
	}
	b.seen = time.Now()
	s.m[key] = b
	return b.account
}

func (s *Sticky) bind(key, account string) {
	if key == "" || account == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = stickyBinding{account: account, seen: time.Now()}
}

func (s *Sticky) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, b := range s.m {
		if time.Since(b.seen) > stickyTTL {
			delete(s.m, k)
		}
	}
}
