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
	"strings"
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

func (p *Pool) resolve(query string) (*Account, error) {
	if a := p.find(query); a != nil {
		return a, nil
	}
	var matched []*Account
	for _, a := range p.accounts {
		if strings.EqualFold(a.email(), query) {
			matched = append(matched, a)
		}
	}
	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return nil, fmt.Errorf("no account %q", query)
	default:
		return nil, fmt.Errorf("%q matches %d accounts; name one by id, which `accounts list -json` prints", query, len(matched))
	}
}

func (p *Pool) remove(a *Account) error {
	i := p.indexOf(a.id())
	if i < 0 {
		return fmt.Errorf("no account %q", a.id())
	}
	p.accounts = slices.Delete(p.accounts, i, i+1)
	return p.save()
}

func (p *Pool) pick(pinned string, skip map[string]bool) *Account {
	if pinned != "" {
		if a := p.find(pinned); a != nil && !skip[pinned] {
			return a
		}
		return nil
	}

	now := time.Now()
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
	return a.pressure(), a.lastUsed
}

func (p *Pool) sorted() []*Account {
	out := slices.Clone(p.accounts)
	slices.SortFunc(out, func(x, y *Account) int {
		return cmp.Or(cmp.Compare(x.email(), y.email()), cmp.Compare(x.id(), y.id()))
	})
	return out
}

func readWindow(h http.Header, prefix string) window {
	used, err := strconv.ParseFloat(h.Get(prefix+"-used-percent"), 64)
	if err != nil {
		return window{}
	}
	w := window{usedPercent: used, seenAt: time.Now()}
	if minutes, err := strconv.Atoi(h.Get(prefix + "-window-minutes")); err == nil {
		w.minutes = minutes
	}
	if secs, err := strconv.ParseInt(h.Get(prefix+"-reset-at"), 10, 64); err == nil {
		w.resetsAt = time.Unix(secs, 0)
	}
	return w
}

func (a *Account) observe(h http.Header) {
	primary := readWindow(h, "x-codex-primary")
	secondary := readWindow(h, "x-codex-secondary-primary")

	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastUsed = time.Now()
	if primary.known() {
		a.primary = primary
	}
	if secondary.known() {
		a.secondary = secondary
	}
}

func (a *Account) rateLimited(h http.Header, attempt int) {
	until := time.Now().Add(backoff(attempt))
	if reset := resetHeader(h); !reset.IsZero() {
		until = reset
	}
	a.mu.Lock()
	defer a.mu.Unlock()
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
