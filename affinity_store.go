package main

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

const hardAffinityAbandonAfter = time.Hour

type AffinityStore struct {
	store *StateStore
}

type affinityBinding struct {
	account   string
	lastUsed  time.Time
	abandoned bool
}

func (s *AffinityStore) lookup(ref affinityRef) string {
	account, _ := s.owner(ref)
	return account
}

func (s *AffinityStore) owner(ref affinityRef) (string, error) {
	binding, err := s.binding(ref)
	if err != nil || binding.abandoned {
		return "", err
	}
	return binding.account, nil
}

func (s *AffinityStore) binding(ref affinityRef) (affinityBinding, error) {
	if s == nil || !ref.valid() {
		return affinityBinding{}, nil
	}
	var binding affinityBinding
	var lastUsed int64
	var abandoned sql.NullInt64
	err := s.store.db.QueryRow(
		`SELECT account_id, last_used_at_ns, abandoned_at_ns FROM bindings WHERE kind = ? AND value = ?`,
		ref.kind,
		ref.value,
	).Scan(&binding.account, &lastUsed, &abandoned)
	if errors.Is(err, sql.ErrNoRows) {
		return affinityBinding{}, nil
	}
	if err != nil {
		return affinityBinding{}, err
	}
	binding.lastUsed = time.Unix(0, lastUsed)
	binding.abandoned = abandoned.Valid
	return binding, nil
}

func (s *AffinityStore) bind(ref affinityRef, account string) error {
	return s.bindAll([]affinityRef{ref}, account)
}

func (s *AffinityStore) bindAll(refs []affinityRef, account string) error {
	return s.storeBindings(refs, account, true)
}

func (s *AffinityStore) claimAll(refs []affinityRef, account string) error {
	return s.storeBindings(refs, account, false)
}

func (s *AffinityStore) storeBindings(refs []affinityRef, account string, touch bool) error {
	if s == nil || account == "" {
		return nil
	}
	return s.store.immediate(func(conn *sql.Conn) error {
		for _, ref := range refs {
			if !ref.valid() || !ref.hard() {
				continue
			}
			var owner string
			var abandoned sql.NullInt64
			err := conn.QueryRowContext(
				context.Background(),
				`SELECT account_id, abandoned_at_ns FROM bindings WHERE kind = ? AND value = ?`,
				ref.kind,
				ref.value,
			).Scan(&owner, &abandoned)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if owner != "" && !abandoned.Valid && owner != account {
				return errAffinityConflict
			}
		}
		now := time.Now().UnixNano()
		for _, ref := range refs {
			if !ref.valid() {
				continue
			}
			_, err := conn.ExecContext(context.Background(), `INSERT INTO bindings (
					kind, value, account_id, created_at_ns, last_used_at_ns, abandoned_at_ns
			) VALUES (?, ?, ?, ?, ?, NULL) ON CONFLICT (kind, value) DO UPDATE SET
					account_id = excluded.account_id,
					last_used_at_ns = CASE
						WHEN ? OR bindings.abandoned_at_ns IS NOT NULL THEN excluded.last_used_at_ns
						ELSE bindings.last_used_at_ns
					END,
					abandoned_at_ns = NULL`, ref.kind, ref.value, account, now, now, touch)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *AffinityStore) bindingForResolution(ref affinityRef, pool *Pool, now time.Time) (affinityBinding, error) {
	binding, err := s.binding(ref)
	if err != nil || binding.account == "" || binding.abandoned || !ref.abandonable() || now.Sub(binding.lastUsed) < hardAffinityAbandonAfter {
		return binding, err
	}
	owner := pool.find(binding.account)
	if owner != nil && !affinityOwnerAbandonable(owner, now) {
		return binding, nil
	}
	result, err := s.store.db.Exec(
		`UPDATE bindings SET abandoned_at_ns = ?
		WHERE kind = ? AND value = ? AND account_id = ? AND last_used_at_ns = ? AND abandoned_at_ns IS NULL`,
		now.UnixNano(),
		ref.kind,
		ref.value,
		binding.account,
		binding.lastUsed.UnixNano(),
	)
	if err != nil {
		return affinityBinding{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return affinityBinding{}, err
	}
	if changed == 1 {
		binding.abandoned = true
		return binding, nil
	}
	return s.binding(ref)
}

func affinityOwnerAbandonable(account *Account, now time.Time) bool {
	candidate := account.routingCandidate()
	if candidate.paused || candidate.reauth != "" {
		return true
	}
	if !candidate.spent {
		return false
	}
	return !candidate.primary.resetsAt.After(now) && !candidate.secondary.resetsAt.After(now)
}

func (s *AffinityStore) resolve(request requestAffinity, pool *Pool) (affinityResolution, error) {
	preferred, err := s.owner(request.preferred)
	if err != nil {
		return affinityResolution{}, err
	}
	resolution := affinityResolution{
		preferred: preferred,
	}
	owners := map[string]bool{}
	refOwners := map[affinityRef]string{}
	fileCount := 0
	ownedFileCount := 0
	conversationKnown := false
	turnKnown := false
	conversationAbandoned := false
	unknownTurn := false
	nonFileHard := false
	now := time.Now()
	for _, ref := range request.hard {
		binding, err := s.bindingForResolution(ref, pool, now)
		if err != nil {
			return affinityResolution{}, err
		}
		owner := binding.account
		if binding.abandoned {
			owner = ""
			conversationAbandoned = conversationAbandoned || ref.kind == affinityConversation
		}
		refOwners[ref] = owner
		if ref.kind == affinityConversation && owner != "" {
			conversationKnown = true
		}
		if ref.kind == affinityTurnState && owner != "" {
			turnKnown = true
		} else if ref.kind == affinityTurnState && !binding.abandoned {
			unknownTurn = true
		}
		if ref.kind == affinityFile {
			fileCount++
			if owner != "" {
				ownedFileCount++
			}
		} else {
			nonFileHard = true
		}
		if owner != "" {
			owners[owner] = true
			continue
		}
		if ref.kind == affinityResponse {
			return affinityResolution{}, errAffinityOwnerUnavailable
		}
	}
	if ownedFileCount > 0 && ownedFileCount != fileCount {
		return affinityResolution{}, errAffinityOwnerUnavailable
	}
	if len(owners) > 1 {
		return affinityResolution{}, errAffinityConflict
	}
	for owner := range owners {
		if pool.find(owner) == nil {
			return affinityResolution{}, errAffinityOwnerUnavailable
		}
		resolution.required = owner
	}
	if unknownTurn && resolution.required == "" {
		accounts := pool.all()
		if len(accounts) != 1 {
			return affinityResolution{}, errAffinityOwnerUnavailable
		}
		resolution.required = accounts[0].id()
	}
	if request.requireUnambiguous && !conversationKnown && !turnKnown && !conversationAbandoned {
		accounts := pool.all()
		if len(accounts) != 1 {
			return affinityResolution{}, errAffinityAmbiguous
		}
		resolution.required = accounts[0].id()
	}
	resolution.hard = nonFileHard || ownedFileCount > 0 || request.requireUnambiguous
	resolution.bindings = request.bindings()
	if resolution.hard {
		resolution.bindings = hardAffinityRefs(resolution.bindings)
	}
	resolution.bindings = slices.DeleteFunc(resolution.bindings, func(ref affinityRef) bool {
		return ref.kind == affinityFile && refOwners[ref] == ""
	})
	return resolution, nil
}
