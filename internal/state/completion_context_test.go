package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMutateAccountsContextRollsBackCanceledTransaction(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	account := Account{ID: "a", IDToken: "id", AccessToken: "access", RefreshToken: "refresh", RoutingMode: "normal"}
	_, err = store.MutateAccountsContext(ctx, func([]Account) ([]Account, error) {
		cancel() // BEGIN has succeeded, but writes must not commit.
		return []Account{account}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mutation=%v", err)
	}
	accounts, err := store.ReadAccounts()
	if err != nil || len(accounts) != 0 {
		t.Fatalf("canceled mutation persisted data: %v %v", accounts, err)
	}
	// The manual transaction must be rolled back before connection reuse.
	if _, err := store.MutateAccounts(func([]Account) ([]Account, error) { return []Account{account}, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshContextSQLWaitIsCancelable(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn, err := store.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, operation := range []string{"credentials", "owner barriers"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var err error
			if operation == "credentials" {
				_, err = store.MutateAccountsContext(ctx, func(accounts []Account) ([]Account, error) {
					t.Error("mutation ran after cancellation")
					return accounts, nil
				})
			} else {
				err = store.PreserveRouteOwnersContext(ctx, time.Now(), "a", []string{"session"})
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("SQL wait=%v", err)
			}
		})
	}
}
