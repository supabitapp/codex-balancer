package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

const (
	minCooldown = 30 * time.Second
	maxCooldown = time.Hour
)

type Pool struct {
	path     string
	accounts []*Account
}

func loadPool(path string) (*Pool, error) {
	p := &Pool{path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &p.accounts); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return p, nil
}

func (p *Pool) save() error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p.accounts, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.path), ".accounts-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p.path)
}

func (p *Pool) find(id string) *Account {
	for _, a := range p.accounts {
		if a.ID() == id {
			return a
		}
	}
	return nil
}

func (p *Pool) add(a *Account) error {
	if a.ID() == "" {
		return fmt.Errorf("credentials carry no chatgpt_account_id")
	}
	if existing := p.find(a.ID()); existing != nil {
		existing.IDToken = a.IDToken
		existing.AccessToken = a.AccessToken
		existing.RefreshToken = a.RefreshToken
		existing.AccountID = a.AccountID
		existing.LastRefresh = a.LastRefresh
		existing.dead = ""
		existing.cooldown = time.Time{}
		return p.save()
	}
	p.accounts = append(p.accounts, a)
	return p.save()
}

func (p *Pool) remove(id string) error {
	for i, a := range p.accounts {
		if a.ID() == id {
			p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
			return p.save()
		}
	}
	return fmt.Errorf("no account %q", id)
}

func (p *Pool) pick(prefer string, skip map[string]bool) *Account {
	now := time.Now()
	if a := p.find(prefer); a != nil && !skip[prefer] && a.available(now) {
		return a
	}

	var best *Account
	for _, a := range p.accounts {
		if skip[a.ID()] || !a.available(now) {
			continue
		}
		if best == nil || less(a, best) {
			best = a
		}
	}
	return best
}

func less(a, b *Account) bool {
	a.mu.Lock()
	ap, at := a.pressure, a.lastUsed
	a.mu.Unlock()
	b.mu.Lock()
	bp, bt := b.pressure, b.lastUsed
	b.mu.Unlock()

	if math.Abs(ap-bp) > 1 {
		return ap < bp
	}
	return at.Before(bt)
}

func (p *Pool) sorted() []*Account {
	out := append([]*Account(nil), p.accounts...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

func (a *Account) observe(h http.Header) {
	pressure := math.Max(
		percentHeader(h, "x-codex-primary-used-percent"),
		percentHeader(h, "x-codex-secondary-primary-used-percent"),
	)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastUsed = time.Now()
	if pressure >= 0 {
		a.pressure = pressure
	}
}

func percentHeader(h http.Header, name string) float64 {
	v, err := strconv.ParseFloat(h.Get(name), 64)
	if err != nil {
		return -1
	}
	return v
}

func (a *Account) rateLimited(h http.Header, attempt int) {
	until := time.Now().Add(backoff(attempt))
	if reset := resetHeader(h); !reset.IsZero() {
		until = reset
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pressure = 100
	a.cooldown = until
}

func resetHeader(h http.Header) time.Time {
	for _, name := range []string{"x-codex-primary-reset-at", "x-codex-secondary-primary-reset-at"} {
		if secs, err := strconv.ParseInt(h.Get(name), 10, 64); err == nil {
			return time.Unix(secs, 0)
		}
	}
	if secs, err := strconv.Atoi(h.Get("retry-after")); err == nil {
		return time.Now().Add(time.Duration(secs) * time.Second)
	}
	return time.Time{}
}

func (a *Account) failed(attempt int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cooldown = time.Now().Add(backoff(attempt))
}

func backoff(attempt int) time.Duration {
	d := minCooldown << attempt
	if d > maxCooldown {
		return maxCooldown
	}
	return d
}
