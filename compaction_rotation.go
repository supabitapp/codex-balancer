package main

import (
	"context"
	"log/slog"
	"sync"
)

type pendingCompactionRotation struct {
	session      string
	thread       string
	account      string
	turn         string
	reconnecting bool
}

type compactionRotation struct {
	store   *StateStore
	log     *slog.Logger
	mu      sync.Mutex
	enabled bool
	pending map[string]pendingCompactionRotation
}

func newCompactionRotation(store *StateStore, log *slog.Logger) (*compactionRotation, error) {
	enabled, err := store.rotateAfterCompaction()
	if err != nil {
		return nil, err
	}
	return &compactionRotation{
		store:   store,
		log:     log,
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
		r.log.Warn("compaction rotation toggle failed", "enabled", enabled, "error", err)
		return r.enabled, err
	}
	cleared := 0
	r.enabled = enabled
	if !enabled {
		cleared = len(r.pending)
		clear(r.pending)
	}
	r.log.Info("compaction rotation toggled", "enabled", enabled, "cleared_pending", cleared)
	return enabled, nil
}

func (r *compactionRotation) arm(session, account string, metadata turnMetadata) {
	if r == nil || metadata.RequestKind != "compaction" {
		return
	}
	if session == "" || metadata.ThreadID == "" || account == "" {
		r.log.Warn("compaction rotation not armed",
			"reason", "missing identity",
			"session", session,
			"thread", metadata.ThreadID,
			"account", account,
			"compaction_turn", metadata.TurnID,
		)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled {
		r.log.Debug("compaction rotation not armed",
			"reason", "disabled",
			"session", session,
			"thread", metadata.ThreadID,
			"account", account,
			"compaction_turn", metadata.TurnID,
		)
		return
	}
	previous, replaced := r.pending[metadata.ThreadID]
	r.pending[metadata.ThreadID] = pendingCompactionRotation{
		session: session,
		thread:  metadata.ThreadID,
		account: account,
		turn:    metadata.TurnID,
	}
	r.log.Info("compaction rotation armed",
		"session", session,
		"thread", metadata.ThreadID,
		"source_account", account,
		"compaction_turn", metadata.TurnID,
		"replaced", replaced,
		"previous_source_account", previous.account,
		"previous_compaction_turn", previous.turn,
	)
}

func (r *compactionRotation) shouldReconnect(session, account string, metadata turnMetadata, hard bool, activeTurns int, alternate bool) bool {
	if r == nil || metadata.ThreadID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.pending[metadata.ThreadID]
	if !r.enabled || !ok {
		return false
	}
	decision := "restart"
	level := slog.LevelInfo
	switch {
	case pending.session != session:
		decision = "wait_session"
		level = slog.LevelDebug
	case r.otherReconnectingThread(session, metadata.ThreadID) != "":
		decision = "wait_session_reconnecting"
		level = slog.LevelDebug
	case pending.reconnecting:
		decision = "wait_reconnecting"
		level = slog.LevelDebug
	case pending.account != account:
		decision = "wait_source_account"
		level = slog.LevelDebug
	case activeTurns > 0:
		decision = "wait_active_turn"
		level = slog.LevelDebug
	case hard:
		decision = "wait_hard_affinity"
		level = slog.LevelDebug
	case sameCompactionTurn(pending, metadata):
		decision = "wait_same_turn"
		level = slog.LevelDebug
	case !alternate:
		decision = "cancel_no_alternate"
		level = slog.LevelWarn
	}
	r.log.Log(context.Background(), level, "compaction rotation decision",
		"decision", decision,
		"session", session,
		"thread", metadata.ThreadID,
		"source_account", pending.account,
		"current_account", account,
		"compaction_turn", pending.turn,
		"request_turn", metadata.TurnID,
		"request_kind", metadata.RequestKind,
		"hard_affinity", hard,
		"active_turns", activeTurns,
		"alternate_available", alternate,
	)
	if decision != "restart" {
		if decision == "cancel_no_alternate" {
			delete(r.pending, metadata.ThreadID)
		}
		return false
	}
	pending.reconnecting = true
	r.pending[metadata.ThreadID] = pending
	return true
}

func (r *compactionRotation) handshakeSkip(session string, hard bool) (map[string]bool, pendingCompactionRotation, bool) {
	if r == nil {
		return nil, pendingCompactionRotation{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.reconnectingForSession(session)
	if !r.enabled || !ok {
		return nil, pendingCompactionRotation{}, false
	}
	decision := "exclude_source"
	if hard {
		decision = "keep_hard_affinity"
	}
	r.log.Debug("compaction rotation handshake",
		"decision", decision,
		"session", session,
		"thread", pending.thread,
		"source_account", pending.account,
		"compaction_turn", pending.turn,
		"hard_affinity", hard,
	)
	if hard {
		return nil, pending, false
	}
	return map[string]bool{pending.account: true}, pending, true
}

func (r *compactionRotation) routeSource(session, account string, metadata turnMetadata, hard bool) string {
	if r == nil || hard || metadata.ThreadID == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.pending[metadata.ThreadID]
	if !r.enabled || !ok || pending.session != session || !pending.reconnecting || sameCompactionTurn(pending, metadata) {
		return ""
	}
	r.log.Info("compaction rotation request routed",
		"session", session,
		"thread", metadata.ThreadID,
		"source_account", pending.account,
		"current_account", account,
		"compaction_turn", pending.turn,
		"request_turn", metadata.TurnID,
		"request_kind", metadata.RequestKind,
	)
	return pending.account
}

func (r *compactionRotation) finish(thread, outcome, account string) {
	if r == nil || thread == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.pending[thread]
	if !ok {
		return
	}
	delete(r.pending, thread)
	r.log.Info("compaction rotation finished",
		"outcome", outcome,
		"session", pending.session,
		"thread", thread,
		"source_account", pending.account,
		"account", account,
		"compaction_turn", pending.turn,
		"reconnecting", pending.reconnecting,
	)
}

func (r *compactionRotation) reconnectingForSession(session string) (pendingCompactionRotation, bool) {
	for _, pending := range r.pending {
		if pending.session == session && pending.reconnecting {
			return pending, true
		}
	}
	return pendingCompactionRotation{}, false
}

func (r *compactionRotation) otherReconnectingThread(session, thread string) string {
	for _, pending := range r.pending {
		if pending.session == session && pending.thread != thread && pending.reconnecting {
			return pending.thread
		}
	}
	return ""
}

func sameCompactionTurn(pending pendingCompactionRotation, metadata turnMetadata) bool {
	return pending.turn != "" && metadata.TurnID != "" && pending.turn == metadata.TurnID
}
