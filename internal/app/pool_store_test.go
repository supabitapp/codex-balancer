package app

import (
	"fmt"
	"testing"
	"time"
)

func TestPoolReconcileReportsAccountLifecycleInvalidations(t *testing.T) {
	tests := []struct {
		name   string
		change func(accountState) accountState
		want   routingReason
	}{
		{
			name: "pause",
			change: func(state accountState) accountState {
				state.Paused = true
				return state
			},
			want: routingReasonOwnerPaused,
		},
		{
			name: "permanent sign out",
			change: func(state accountState) accountState {
				state.Reauth = "refresh token invalidated"
				state.LastRefresh = state.LastRefresh.Add(time.Second)
				return state
			},
			want: routingReasonOwnerSignedOut,
		},
		{
			name: "managed workspace",
			change: func(state accountState) accountState {
				replacement := testAccountWithPlan("account-a", 10, "business").persisted()
				replacement.LastRefresh = state.LastRefresh.Add(time.Second)
				return replacement
			},
			want: routingReasonOwnerNotRoutable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := testAccount("account-a", 10)
			pool := &Pool{accounts: []*Account{current}}
			next := accountFromState(test.change(current.persisted()))
			change := pool.reconcile([]*Account{next})
			if got := fmt.Sprint(change.unavailable); got != fmt.Sprintf("[{account-a %s}]", test.want) {
				t.Fatalf("unavailable = %s, want account-a/%s", got, test.want)
			}
		})
	}
}

func TestPoolReconcileReportsRemovedAccountsButNotSafeUpdates(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 10)
	pool := &Pool{accounts: []*Account{a, b}}
	updated := a.persisted()
	updated.RoutingMode = routingModePriority

	change := pool.reconcile([]*Account{accountFromState(updated)})
	if change.removed != 1 || len(change.unavailable) != 1 || change.unavailable[0] != (accountUnavailable{id: b.id(), reason: routingReasonOwnerRemoved}) {
		t.Fatalf("change = %+v, want only account-b removed", change)
	}
	for _, unavailable := range change.unavailable {
		if unavailable.id == a.id() {
			t.Fatal("routing-mode-only update invalidated account-a")
		}
	}
}
