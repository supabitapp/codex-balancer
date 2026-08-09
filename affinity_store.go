package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	affinityTTL                        = 12 * time.Hour
	maxEphemeralAffinityRecordsPerKind = 8192
)

type softAffinityBinding struct {
	Account string    `json:"account"`
	Seen    time.Time `json:"seen"`
}

type affinityState struct {
	Soft      map[string]softAffinityBinding `json:"soft,omitempty"`
	Hard      map[string]string              `json:"hard,omitempty"`
	HardOrder []string                       `json:"hard_order,omitempty"`
}

func (s affinityState) clone() affinityState {
	return affinityState{
		Soft:      maps.Clone(s.Soft),
		Hard:      maps.Clone(s.Hard),
		HardOrder: slices.Clone(s.HardOrder),
	}
}

type AffinityStore struct {
	mu    sync.Mutex
	path  string
	state affinityState
	now   func() time.Time
}

func defaultAffinityPath() string {
	dir := fmt.Sprintf("codex-balancer-%d", os.Getuid())
	return filepath.Join(os.TempDir(), dir, "affinity.json")
}

func newAffinityStore(path string) (*AffinityStore, error) {
	store := &AffinityStore{
		path: path,
		state: affinityState{
			Soft: map[string]softAffinityBinding{},
			Hard: map[string]string{},
		},
		now: time.Now,
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if store.state.Soft == nil {
		store.state.Soft = map[string]softAffinityBinding{}
	}
	if store.state.Hard == nil {
		store.state.Hard = map[string]string{}
	}
	sweepAffinityState(&store.state, store.now())
	return store, nil
}

func (s *AffinityStore) lookup(ref affinityRef) string {
	if s == nil || !ref.valid() {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ref.storageKey()
	if ref.hard() {
		return s.state.Hard[key]
	}
	binding, ok := s.state.Soft[key]
	if !ok {
		return ""
	}
	now := s.now()
	if now.Sub(binding.Seen) > affinityTTL {
		delete(s.state.Soft, key)
		return ""
	}
	binding.Seen = now
	s.state.Soft[key] = binding
	return binding.Account
}

func (s *AffinityStore) bind(ref affinityRef, account string) error {
	return s.bindAll([]affinityRef{ref}, account)
}

func (s *AffinityStore) bindAll(refs []affinityRef, account string) error {
	if s == nil || account == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ref := range refs {
		if !ref.valid() || !ref.hard() {
			continue
		}
		owner := s.state.Hard[ref.storageKey()]
		if owner != "" && owner != account {
			return errAffinityConflict
		}
	}
	next := s.state.clone()
	now := s.now()
	changed := false
	for _, ref := range refs {
		if !ref.valid() {
			continue
		}
		key := ref.storageKey()
		if ref.hard() {
			owner := next.Hard[key]
			if owner == "" {
				next.Hard[key] = account
				if ref.kind == affinityResponse || ref.kind == affinityTurnState {
					next.HardOrder = append(next.HardOrder, key)
				}
				changed = true
			}
			continue
		}
		binding := softAffinityBinding{Account: account, Seen: now}
		if next.Soft[key] != binding {
			next.Soft[key] = binding
			changed = true
		}
	}
	limitHardAffinityState(&next, maxEphemeralAffinityRecordsPerKind)
	if !changed {
		return nil
	}
	if err := writeJSONFile(s.path, next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *AffinityStore) resolve(request requestAffinity, pool *Pool) (affinityResolution, error) {
	resolution := affinityResolution{
		preferred: s.lookup(request.preferred),
	}
	owners := map[string]bool{}
	refOwners := map[affinityRef]string{}
	fileCount := 0
	ownedFileCount := 0
	conversationKnown := false
	turnKnown := false
	nonFileHard := false
	for _, ref := range request.hard {
		owner := s.lookup(ref)
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

func (s *AffinityStore) sweep() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.state.clone()
	sweepAffinityState(&next, s.now())
	if maps.Equal(next.Soft, s.state.Soft) && maps.Equal(next.Hard, s.state.Hard) && slices.Equal(next.HardOrder, s.state.HardOrder) {
		return nil
	}
	if err := writeJSONFile(s.path, next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func sweepAffinityState(state *affinityState, now time.Time) {
	for key, binding := range state.Soft {
		if key == "" || binding.Account == "" || now.Sub(binding.Seen) > affinityTTL {
			delete(state.Soft, key)
		}
	}
	for key, account := range state.Hard {
		if key == "" || account == "" {
			delete(state.Hard, key)
		}
	}
	limitHardAffinityState(state, maxEphemeralAffinityRecordsPerKind)
}

func limitHardAffinityState(state *affinityState, limit int) {
	seen := make(map[string]bool, len(state.Hard))
	order := make([]string, 0, len(state.Hard))
	for _, key := range state.HardOrder {
		if state.Hard[key] == "" || seen[key] || !ephemeralHardAffinityKey(key) {
			continue
		}
		seen[key] = true
		order = append(order, key)
	}
	missing := make([]string, 0, len(state.Hard)-len(order))
	for key := range state.Hard {
		if !seen[key] && ephemeralHardAffinityKey(key) {
			missing = append(missing, key)
		}
	}
	slices.Sort(missing)
	order = append(missing, order...)
	counts := map[string]int{}
	kept := make([]string, 0, len(order))
	for index := len(order) - 1; index >= 0; index-- {
		key := order[index]
		kind, _, _ := strings.Cut(key, "\n")
		if counts[kind] >= limit {
			delete(state.Hard, key)
			continue
		}
		counts[kind]++
		kept = append(kept, key)
	}
	slices.Reverse(kept)
	state.HardOrder = kept
}

func ephemeralHardAffinityKey(key string) bool {
	kind, _, _ := strings.Cut(key, "\n")
	return kind == string(affinityResponse) || kind == string(affinityTurnState)
}
