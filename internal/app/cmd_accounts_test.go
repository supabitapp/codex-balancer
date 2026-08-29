package app

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountsModeSetsAndPersistsRoutingMode(t *testing.T) {
	path := accountStatePath(t)

	for _, mode := range []routingMode{routingModePriority, routingModeNormal} {
		if err := accountsCmd([]string{"mode", "-state", path, "account-a@example.com", string(mode)}); err != nil {
			t.Fatal(err)
		}
		if got := persistedRoutingMode(t, path, "account-a"); got != mode {
			t.Fatalf("routing mode = %q, want %q", got, mode)
		}
	}
}

func TestAccountsModeRejectsUnknownMode(t *testing.T) {
	path := accountStatePath(t)
	err := accountsCmd([]string{"mode", "-state", path, "account-a", "fast"})
	if err == nil || !strings.Contains(err.Error(), "use normal or priority") {
		t.Fatalf("error = %v", err)
	}
	if got := persistedRoutingMode(t, path, "account-a"); got != routingModeNormal {
		t.Fatalf("routing mode = %q, want %q", got, routingModeNormal)
	}
}

func accountStatePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.add(testAccount("account-a", 20)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func persistedRoutingMode(t *testing.T, path, id string) routingMode {
	t.Helper()
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	account := pool.find(id)
	if account == nil {
		t.Fatalf("account %q not found", id)
	}
	return account.routingCandidate().mode
}
