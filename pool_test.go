package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPoolWatchCreatesTheAccountDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "accounts.json")
	pool, err := loadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.watch(t.Context(), func(poolChange) {}, func(error) {}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("account directory mode = %o, want 700", info.Mode().Perm())
	}
}

func TestConcurrentPoolMutationsKeepEveryAccount(t *testing.T) {
	path := t.TempDir() + "/accounts.json"
	const total = 12
	errs := make(chan error, total)
	var workers sync.WaitGroup
	for i := range total {
		workers.Add(1)
		go func() {
			defer workers.Done()
			pool := &Pool{path: path}
			errs <- pool.add(accountFor(fmt.Sprintf("acct-%d", i)))
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	pool, err := loadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	if pool.count() != total {
		t.Fatalf("accounts = %d, want %d", pool.count(), total)
	}
}

func TestPoolWatchesNewAccountsWithoutLosingRuntimeState(t *testing.T) {
	path := t.TempDir() + "/accounts.json"
	seed := &Pool{path: path}
	if err := seed.add(accountFromState(accountState{
		IDToken:     jwtFor("acct-a"),
		AccessToken: "AT-a",
		LastRefresh: time.Now(),
	})); err != nil {
		t.Fatal(err)
	}

	pool, err := loadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	original := pool.find("acct-a")
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "37")
	original.observe(headers)

	changes := make(chan poolChange, 1)
	failures := make(chan error, 1)
	if err := pool.watch(t.Context(), func(change poolChange) {
		changes <- change
	}, func(err error) {
		failures <- err
	}); err != nil {
		t.Fatal(err)
	}

	external, err := loadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := external.add(accountFromState(accountState{
		IDToken:     jwtFor("acct-b"),
		AccessToken: "AT-b",
		LastRefresh: time.Now(),
	})); err != nil {
		t.Fatal(err)
	}

	select {
	case change := <-changes:
		if change.added != 1 || change.removed != 0 {
			t.Fatalf("change = %+v, want one added account", change)
		}
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("account file change was not loaded")
	}

	if pool.count() != 2 || pool.find("acct-b") == nil {
		t.Fatalf("accounts = %v, want acct-a and acct-b", pool.all())
	}
	if pool.find("acct-a") != original {
		t.Fatal("reload replaced the live account")
	}
	primary, _, _, _ := original.health()
	if primary.usedPercent != 37 {
		t.Fatalf("usage = %v, want runtime state kept", primary.usedPercent)
	}
}

func TestStalePoolMutationPreservesAccountsAddedElsewhere(t *testing.T) {
	path := t.TempDir() + "/accounts.json"
	seed := &Pool{path: path}
	if err := seed.add(accountFor("acct-a")); err != nil {
		t.Fatal(err)
	}

	stale, err := loadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	external, err := loadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := external.add(accountFor("acct-b")); err != nil {
		t.Fatal(err)
	}
	if paused, err := stale.togglePause(stale.find("acct-a")); err != nil {
		t.Fatal(err)
	} else if !paused {
		t.Fatal("account was resumed instead of paused")
	}

	reloaded, err := loadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.count() != 2 || reloaded.find("acct-b") == nil {
		t.Fatalf("accounts = %v, want the new account kept", reloaded.all())
	}
	if !reloaded.find("acct-a").paused() {
		t.Fatal("pause was not saved")
	}
}

func TestTokenPersistenceKeepsTheLatestPause(t *testing.T) {
	path := t.TempDir() + "/accounts.json"
	seed := &Pool{path: path}
	oldRefresh := time.Now().Add(-time.Hour)
	if err := seed.add(accountFromState(accountState{
		IDToken:      jwtFor("acct-a"),
		AccessToken:  "AT-old",
		RefreshToken: "RT-old",
		LastRefresh:  oldRefresh,
	})); err != nil {
		t.Fatal(err)
	}

	server, err := loadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	external, err := loadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	if paused, err := external.togglePause(external.find("acct-a")); err != nil {
		t.Fatal(err)
	} else if !paused {
		t.Fatal("account was resumed instead of paused")
	}

	account := server.find("acct-a")
	account.mu.Lock()
	account.AccessToken = "AT-new"
	account.RefreshToken = "RT-new"
	account.LastRefresh = time.Now()
	account.mu.Unlock()
	if err := server.persistTokens(account); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.find("acct-a")
	if !got.paused() || got.AccessToken != "AT-new" || got.RefreshToken != "RT-new" {
		t.Fatalf("account = %+v, want new tokens and the external pause", got.persisted())
	}
}
