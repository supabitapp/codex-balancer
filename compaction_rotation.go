package main

import "sync"

type pendingCompactionRotation struct {
	account      string
	turn         string
	reconnecting bool
}

type compactionRotation struct {
	store   *StateStore
	mu      sync.Mutex
	enabled bool
	pending map[string]pendingCompactionRotation
}

func newCompactionRotation(store *StateStore) (*compactionRotation, error) {
	enabled, err := store.rotateAfterCompaction()
	if err != nil {
		return nil, err
	}
	return &compactionRotation{
		store:   store,
		enabled: enabled,
		pending: map[string]pendingCompactionRotation{},
	}, nil
}

func (r *compactionRotation) isEnabled() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enabled
}

func (r *compactionRotation) toggle() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	enabled := !r.enabled
	if err := r.store.setRotateAfterCompaction(enabled); err != nil {
		return r.enabled, err
	}
	r.enabled = enabled
	if !enabled {
		clear(r.pending)
	}
	return enabled, nil
}

func (r *compactionRotation) arm(thread, account string, metadata turnMetadata) {
	if r == nil || thread == "" || account == "" || metadata.RequestKind != "compaction" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled {
		return
	}
	r.pending[thread] = pendingCompactionRotation{account: account, turn: metadata.TurnID}
}

func (r *compactionRotation) shouldReconnect(thread, account string, metadata turnMetadata, hard, alternate bool) bool {
	if r == nil || hard {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.pending[thread]
	if !r.enabled || !ok || pending.reconnecting || pending.account != account || sameCompactionTurn(pending, metadata) {
		return false
	}
	if !alternate {
		delete(r.pending, thread)
		return false
	}
	pending.reconnecting = true
	r.pending[thread] = pending
	return true
}

func (r *compactionRotation) handshakeSkip(thread string, hard bool) (map[string]bool, bool) {
	if r == nil || hard {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.pending[thread]
	if !r.enabled || !ok || !pending.reconnecting {
		return nil, false
	}
	return map[string]bool{pending.account: true}, true
}

func (r *compactionRotation) routeSource(thread, account string, metadata turnMetadata, hard bool) string {
	if r == nil || hard {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.pending[thread]
	if !r.enabled || !ok || !pending.reconnecting || pending.account == account || sameCompactionTurn(pending, metadata) {
		return ""
	}
	return pending.account
}

func (r *compactionRotation) finish(thread string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, thread)
}

func sameCompactionTurn(pending pendingCompactionRotation, metadata turnMetadata) bool {
	return pending.turn != "" && metadata.TurnID != "" && pending.turn == metadata.TurnID
}
