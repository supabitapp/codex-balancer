package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

func (p *Pool) indexOf(id string) int {
	return slices.IndexFunc(p.accounts, func(a *Account) bool { return a.id() == id })
}

func (p *Pool) find(id string) *Account {
	if i := p.indexOf(id); i >= 0 {
		return p.accounts[i]
	}
	return nil
}

func (p *Pool) add(a *Account) error {
	if a.id() == "" {
		return errors.New("credentials carry no chatgpt_account_id")
	}
	if i := p.indexOf(a.id()); i >= 0 {
		p.accounts[i] = a
	} else {
		p.accounts = append(p.accounts, a)
	}
	return p.save()
}

func (p *Pool) remove(id string) error {
	i := p.indexOf(id)
	if i < 0 {
		return fmt.Errorf("no account %q", id)
	}
	p.accounts = slices.Delete(p.accounts, i, i+1)
	return p.save()
}

func (p *Pool) pick(prefer string, skip map[string]bool) *Account {
	now := time.Now()
	if a := p.find(prefer); a != nil && !skip[prefer] && a.available(now) {
		return a
	}

	var best *Account
	for _, a := range p.accounts {
		if skip[a.id()] || !a.available(now) {
			continue
		}
		if best == nil || a.roomierThan(best) {
			best = a
		}
	}
	return best
}

func (a *Account) roomierThan(b *Account) bool {
	ap, at := a.load()
	bp, bt := b.load()
	if math.Abs(ap-bp) > 1 {
		return ap < bp
	}
	return at.Before(bt)
}

func (a *Account) load() (pressure float64, lastUsed time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pressure, a.lastUsed
}

func (p *Pool) sorted() []*Account {
	out := slices.Clone(p.accounts)
	slices.SortFunc(out, func(x, y *Account) int { return cmp.Compare(x.id(), y.id()) })
	return out
}

var usedPercentHeaders = []string{
	"x-codex-primary-used-percent",
	"x-codex-secondary-primary-used-percent",
}

func (a *Account) observe(h http.Header) {
	pressure, seen := 0.0, false
	for _, name := range usedPercentHeaders {
		if v, err := strconv.ParseFloat(h.Get(name), 64); err == nil {
			pressure, seen = math.Max(pressure, v), true
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastUsed = time.Now()
	if seen {
		a.pressure = pressure
	}
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
