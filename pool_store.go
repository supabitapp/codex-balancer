package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"github.com/gofrs/flock"
)

type poolChange struct {
	added   int
	removed int
	updated int
}

func (c poolChange) changed() bool {
	return c.added+c.removed+c.updated > 0
}

func loadPool(path string) (*Pool, error) {
	accounts, err := readAccounts(path)
	if err != nil {
		return nil, err
	}
	return &Pool{path: path, accounts: accounts}, nil
}

func readAccounts(path string) ([]*Account, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var accounts []*Account
	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	seen := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		id := account.id()
		if id == "" {
			return nil, fmt.Errorf("parse %s: credentials carry no chatgpt_account_id", path)
		}
		if seen[id] {
			return nil, fmt.Errorf("parse %s: duplicate account %q", path, id)
		}
		seen[id] = true
	}
	return accounts, nil
}

func writeAccounts(path string, accounts []*Account) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(accounts, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".accounts-*")
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
	return os.Rename(tmp.Name(), path)
}

func (p *Pool) mutate(change func([]*Account) ([]*Account, error)) error {
	p.storageMu.Lock()
	defer p.storageMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	lock := flock.New(p.path + ".lock")
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()
	accounts, err := readAccounts(p.path)
	if err != nil {
		return err
	}
	accounts, err = change(accounts)
	if err != nil {
		return err
	}
	if err := writeAccounts(p.path, accounts); err != nil {
		return err
	}
	p.reconcile(accounts)
	return nil
}

func (p *Pool) reload() (poolChange, error) {
	p.storageMu.Lock()
	defer p.storageMu.Unlock()
	accounts, err := readAccounts(p.path)
	if err != nil {
		return poolChange{}, err
	}
	return p.reconcile(accounts), nil
}

func (p *Pool) reconcile(accounts []*Account) poolChange {
	p.mu.Lock()
	defer p.mu.Unlock()
	current := make(map[string]*Account, len(p.accounts))
	for _, account := range p.accounts {
		current[account.id()] = account
	}
	next := make([]*Account, 0, len(accounts))
	change := poolChange{}
	for _, account := range accounts {
		id := account.id()
		existing := current[id]
		if existing == nil {
			next = append(next, account)
			change.added++
			continue
		}
		delete(current, id)
		if existing != account && existing.applyPersisted(account.persisted()) {
			change.updated++
		}
		next = append(next, existing)
	}
	change.removed = len(current)
	p.accounts = next
	return change
}

func (p *Pool) watch(ctx context.Context, changed func(poolChange), failed func(error)) error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := watcher.Add(filepath.Dir(p.path)); err != nil {
		watcher.Close()
		return err
	}
	path := filepath.Clean(p.path)
	go func() {
		defer watcher.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if filepath.Clean(event.Name) != path ||
					event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
					continue
				}
				change, err := p.reload()
				if err != nil {
					failed(err)
					continue
				}
				if change.changed() {
					changed(change)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				failed(err)
			}
		}
	}()
	return nil
}
