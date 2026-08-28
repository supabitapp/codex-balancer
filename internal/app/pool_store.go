package app

import (
	"context"
	"time"
)

type poolChange struct {
	added   int
	removed int
	updated int
}

func (c poolChange) changed() bool {
	return c.added+c.removed+c.updated > 0
}

func loadPool(store *StateStore) (*Pool, error) {
	accounts, err := store.readAccounts()
	if err != nil {
		return nil, err
	}
	return &Pool{store: store, accounts: accounts}, nil
}

func (p *Pool) mutate(change func([]*Account) ([]*Account, error)) error {
	p.storageMu.Lock()
	defer p.storageMu.Unlock()
	accounts, err := p.store.mutateAccounts(change)
	if err != nil {
		return err
	}
	p.reconcile(accounts)
	return nil
}

func (p *Pool) reload() (poolChange, error) {
	p.storageMu.Lock()
	defer p.storageMu.Unlock()
	accounts, err := p.store.readAccounts()
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

func (p *Pool) watch(ctx context.Context, changed func(poolChange), failed func(error)) {
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				change, err := p.reload()
				if err != nil {
					failed(err)
					continue
				}
				if change.changed() {
					changed(change)
				}
			}
		}
	}()
}
