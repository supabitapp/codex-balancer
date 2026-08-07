package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const stickyTTL = 12 * time.Hour

var stickyHeaders = []string{
	"session-id",
	"session_id",
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
	mu   sync.Mutex
	path string
	m    map[string]stickyBinding
}

type stickyBinding struct {
	Account string    `json:"account"`
	Seen    time.Time `json:"seen"`
}

func defaultStickyPath() string {
	dir := fmt.Sprintf("codex-balancer-%d", os.Getuid())
	return filepath.Join(os.TempDir(), dir, "sticky.json")
}

func newSticky(path string) (*Sticky, error) {
	s := &Sticky{path: path, m: map[string]stickyBinding{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	s.sweepExpired(time.Now())
	return s, nil
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
	if time.Since(b.Seen) > stickyTTL {
		delete(s.m, key)
		return ""
	}
	b.Seen = time.Now()
	s.m[key] = b
	return b.Account
}

func (s *Sticky) bind(key, account string) error {
	if key == "" || account == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = stickyBinding{Account: account, Seen: time.Now()}
	return writeJSONFile(s.path, s.m)
}

func (s *Sticky) sweep() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.m)
	s.sweepExpired(time.Now())
	if len(s.m) == before {
		return nil
	}
	return writeJSONFile(s.path, s.m)
}

func (s *Sticky) sweepExpired(now time.Time) {
	for k, b := range s.m {
		if now.Sub(b.Seen) > stickyTTL || k == "" || b.Account == "" {
			delete(s.m, k)
		}
	}
}
