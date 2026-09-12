package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// Done is evaluated after refresh has registered this waiter under the account
// lock. This avoids timing/sleep assumptions about a follower joining a refresh.
type refreshWaitContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *refreshWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

type testRefreshExchange struct {
	release chan struct{}
	closed  chan struct{}
}

func blockedRefreshServer(t *testing.T) (*server, *Account, <-chan testRefreshExchange, *atomic.Int64) {
	t.Helper()
	exchanges := make(chan testRefreshExchange, 4)
	calls := &atomic.Int64{}
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		exchange := testRefreshExchange{release: make(chan struct{}), closed: make(chan struct{})}
		exchanges <- exchange
		defer close(exchange.closed)
		select {
		case <-exchange.release:
			io.WriteString(w, `{"access_token":"refreshed-access","refresh_token":"refreshed-refresh"}`)
		case <-r.Context().Done():
		}
	}))
	previous := oauthEndpoint
	oauthEndpoint = oauth.URL
	t.Cleanup(func() { oauthEndpoint = previous; oauth.Close() })
	account := testAccount("a", 0)
	srv := newTestServer(t, []*Account{account})
	return srv, account, exchanges, calls
}

func TestSharedRefreshFirstWaiterCancellation(t *testing.T) {
	srv, account, exchanges, calls := blockedRefreshServer(t)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first := make(chan bool, 1)
	go func() { first <- srv.refreshedContext(firstCtx, account, account.id()) }()
	exchange := <-exchanges
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	waiter := &refreshWaitContext{Context: secondCtx, joined: make(chan struct{})}
	second := make(chan error, 1)
	go func() { second <- account.refresh(waiter, srv.client, srv.pool.persistAccountState) }()
	waitHTTPSignal(t, waiter.joined)
	cancelFirst()
	if <-first {
		t.Fatal("canceled initiating request reported success")
	}
	select {
	case <-exchange.closed:
		t.Fatal("first waiter canceled the live follower's exchange")
	default:
	}
	close(exchange.release)
	if err := <-second; err != nil {
		t.Fatal("live follower inherited cancellation:", err)
	}
	if secondCtx.Err() != nil || calls.Load() != 1 {
		t.Fatalf("follower context=%v exchanges=%d", secondCtx.Err(), calls.Load())
	}
	if account.persisted().AccessToken != "refreshed-access" {
		t.Fatal("remaining waiter did not publish credentials")
	}
	account.mu.Lock()
	inflight := account.inflight
	account.mu.Unlock()
	if inflight != nil {
		t.Fatal("completed shared refresh retained inflight state")
	}
}

func TestSharedRefreshFollowerCancellation(t *testing.T) {
	srv, account, exchanges, calls := blockedRefreshServer(t)
	first := make(chan error, 1)
	go func() { first <- account.refresh(context.Background(), srv.client, srv.pool.persistAccountState) }()
	exchange := <-exchanges
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter := &refreshWaitContext{Context: ctx, joined: make(chan struct{})}
	second := make(chan error, 1)
	go func() { second <- account.refresh(waiter, srv.client, srv.pool.persistAccountState) }()
	waitHTTPSignal(t, waiter.joined)
	cancel()
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled follower=%v", err)
	}
	select {
	case <-exchange.closed:
		t.Fatal("follower canceled the initiating waiter's exchange")
	default:
	}
	close(exchange.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("exchanges=%d", calls.Load())
	}
}

func TestSharedRefreshNoWaitersCancelsAndAllowsLaterRefresh(t *testing.T) {
	srv, account, exchanges, calls := blockedRefreshServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan bool, 1)
	go func() { first <- srv.refreshedContext(ctx, account, account.id()) }()
	exchange := <-exchanges
	account.mu.Lock()
	operation := account.inflight
	account.mu.Unlock()
	cancel()
	if <-first {
		t.Fatal("canceled caller succeeded")
	}
	waitHTTPSignal(t, exchange.closed)
	waitHTTPSignal(t, operation.done)
	account.mu.Lock()
	inflight := account.inflight
	account.mu.Unlock()
	if inflight != nil || operation.waiters != 0 {
		t.Fatal("abandoned exchange retained state")
	}
	second := make(chan bool, 1)
	go func() { second <- srv.refreshedContext(context.Background(), account, account.id()) }()
	next := <-exchanges
	close(next.release)
	if !<-second || calls.Load() != 2 {
		t.Fatalf("later refresh failed, exchanges=%d", calls.Load())
	}
}

func TestSharedRefreshShutdownCancelsAllWaiters(t *testing.T) {
	srv, account, exchanges, calls := blockedRefreshServer(t)
	lifetime, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	srv.ctx = lifetime
	first := make(chan bool, 1)
	go func() { first <- srv.refreshedContext(context.Background(), account, account.id()) }()
	exchange := <-exchanges
	waiter := &refreshWaitContext{Context: context.Background(), joined: make(chan struct{})}
	second := make(chan error, 1)
	go func() { second <- account.refresh(waiter, srv.client, srv.pool.persistAccountState) }()
	waitHTTPSignal(t, waiter.joined)
	account.mu.Lock()
	operation := account.inflight
	account.mu.Unlock()
	shutdown()
	if <-first {
		t.Fatal("shutdown refresh succeeded")
	}
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown follower=%v", err)
	}
	waitHTTPSignal(t, exchange.closed)
	waitHTTPSignal(t, operation.done)
	if calls.Load() != 1 {
		t.Fatalf("exchanges=%d", calls.Load())
	}
	if srv.refreshedContext(context.Background(), account, account.id()) || calls.Load() != 1 {
		t.Fatal("refresh started after shutdown")
	}
}

func TestSharedRefreshNewCallerWaitsForAbandonedOperation(t *testing.T) {
	calls := useOAuthRefreshServer(t)
	account := testAccount("a", 0)
	entered, release := make(chan struct{}), make(chan struct{})
	var persists atomic.Int64
	persist := func(state accountState) (accountState, error) {
		if persists.Add(1) == 1 {
			close(entered)
			<-release
		}
		return state, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { first <- account.refresh(ctx, http.DefaultClient, persist) }()
	waitHTTPSignal(t, entered)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller blocked behind shared persistence: %v", err)
	}
	waiter := &refreshWaitContext{Context: context.Background(), joined: make(chan struct{})}
	second := make(chan error, 1)
	go func() { second <- account.refresh(waiter, http.DefaultClient, persist) }()
	waitHTTPSignal(t, waiter.joined)
	if calls() != 1 {
		t.Fatal("started a second exchange before abandoned operation finished")
	}
	close(release)
	if err := <-second; err != nil {
		t.Fatal("new caller inherited the abandoned operation's result:", err)
	}
	if calls() != 2 || persists.Load() != 2 {
		t.Fatalf("exchanges=%d persists=%d", calls(), persists.Load())
	}
}

func TestSharedRefreshCanceledCallerDoesNotStartWork(t *testing.T) {
	srv, account, _, calls := blockedRefreshServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := account.refresh(ctx, srv.client, srv.pool.persistAccountState); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("canceled caller started an exchange")
	}
}
