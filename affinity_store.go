package main

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

type AffinityStore struct {
	store *StateStore
}

func (s *AffinityStore) lookup(ref affinityRef) string {
	account, _ := s.owner(ref)
	return account
}

func (s *AffinityStore) owner(ref affinityRef) (string, error) {
	if s == nil || !ref.valid() {
		return "", nil
	}
	var account string
	err := s.store.db.QueryRow(`SELECT account_id FROM bindings WHERE kind = ? AND value = ?`, ref.kind, ref.value).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return account, nil
}

func (s *AffinityStore) bind(ref affinityRef, account string) error {
	return s.bindAll([]affinityRef{ref}, account)
}

func (s *AffinityStore) bindAll(refs []affinityRef, account string) error {
	if s == nil || account == "" {
		return nil
	}
	return s.store.immediate(func(conn *sql.Conn) error {
		for _, ref := range refs {
			if !ref.valid() || !ref.hard() {
				continue
			}
			var owner string
			err := conn.QueryRowContext(context.Background(), `SELECT account_id FROM bindings WHERE kind = ? AND value = ?`, ref.kind, ref.value).Scan(&owner)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if owner != "" && owner != account {
				return errAffinityConflict
			}
		}
		now := time.Now().UnixNano()
		for _, ref := range refs {
			if !ref.valid() {
				continue
			}
			var err error
			if ref.hard() {
				_, err = conn.ExecContext(context.Background(), `INSERT INTO bindings (kind, value, account_id, created_at_ns)
					VALUES (?, ?, ?, ?) ON CONFLICT (kind, value) DO NOTHING`, ref.kind, ref.value, account, now)
			} else {
				_, err = conn.ExecContext(context.Background(), `INSERT INTO bindings (kind, value, account_id, created_at_ns)
					VALUES (?, ?, ?, ?) ON CONFLICT (kind, value) DO UPDATE SET account_id = excluded.account_id`, ref.kind, ref.value, account, now)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
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
	nonFileHard := false
	for _, ref := range request.hard {
		owner, err := s.owner(ref)
		if err != nil {
			return affinityResolution{}, err
		}
		refOwners[ref] = owner
		if ref.kind == affinityConversation && owner != "" {
			conversationKnown = true
		}
		if ref.kind == affinityTurnState && owner != "" {
			turnKnown = true
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
	if request.requireUnambiguous && !conversationKnown && !turnKnown {
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
