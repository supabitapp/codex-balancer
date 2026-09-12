package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Signals consumption, not merely receipt, of a synthetic OAuth response.
// Tests then block the real pool/SQL persistence path, not a substitute writer.
type completionConsumedBody struct {
	io.Reader
	consumed chan struct{}
	once     sync.Once
}

func (b *completionConsumedBody) Close() error { b.once.Do(func() { close(b.consumed) }); return nil }

func consumedRefreshServer(t *testing.T, status int, body string) (*server, *Account, <-chan struct{}) {
	t.Helper()
	account := testAccount("a", 0)
	srv := newTestServer(t, []*Account{account})
	consumed := make(chan struct{})
	srv.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: make(http.Header), Request: r,
			Body: &completionConsumedBody{Reader: strings.NewReader(body), consumed: consumed}}, nil
	})}
	return srv, account, consumed
}

func TestRefreshCompletionInvalidatesAfterLastWaiterLeaves(t *testing.T) {
	for _, followers := range []int{0, 2} {
		t.Run(fmt.Sprint(followers), func(t *testing.T) {
			srv, account, consumed := consumedRefreshServer(t, 401, `{"error":"invalid_grant"}`)
			logs := captureTestLogs(srv)
			var notifications atomic.Int64
			activeID := srv.activeWebSockets.add(account.id(), func(string, string) {
				// Completion must not call us under either persistence or routing
				// ownership locks. Contexts turn a regression into a bounded failure.
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := srv.pool.storageMu.LockContext(ctx); err != nil {
					t.Error(err)
				} else {
					srv.pool.storageMu.Unlock()
				}
				if err := srv.routeOwnership.LockContext(ctx); err != nil {
					t.Error(err)
				} else {
					srv.routeOwnership.Unlock()
				}
				notifications.Add(1)
			})
			defer srv.activeWebSockets.remove(activeID, account.id())
			claim := srv.claimAccount(websocketRoute{session: "existing-session"}, durableRouteOwners{}, "", "", nil, 0).claim
			if claim == nil {
				t.Fatal("missing provisional claim")
			}
			defer claim.release()
			srv.pool.storageMu.Lock()
			var release sync.Once
			unlock := func() { release.Do(srv.pool.storageMu.Unlock) }
			defer unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := make(chan bool, 1)
			go func() { first <- srv.refreshedContext(ctx, account, account.id()) }()
			waitHTTPSignal(t, consumed)
			account.mu.Lock()
			operation := account.inflight
			account.mu.Unlock()
			if operation == nil {
				t.Fatal("operation ended before persistence")
			}
			results := make(chan error, followers)
			cancels := []context.CancelFunc{}
			for range followers {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				cancels = append(cancels, cancel)
				waiter := &refreshWaitContext{Context: ctx, joined: make(chan struct{})}
				go func() { results <- account.refresh(waiter, srv.client, srv.pool.persistAccountState) }()
				waitHTTPSignal(t, waiter.joined)
			}
			cancel()
			if <-first {
				t.Fatal("canceled request succeeded")
			}
			for _, cancel := range cancels {
				cancel()
			}
			for range followers {
				if err := <-results; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			if account.needsReauth() {
				t.Fatal("fixture did not block publication")
			}
			unlock()
			waitHTTPSignal(t, operation.done)
			if !account.needsReauth() || notifications.Load() != 1 || claim.active() {
				t.Fatalf("reauth=%t notifications=%d claim active=%t", account.needsReauth(), notifications.Load(), claim.active())
			}
			owners, err := srv.pool.store.routeOwners("", "existing-session")
			if err != nil || len(owners) != 1 || owners[0] != "a" {
				t.Fatalf("barrier=%v error=%v", owners, err)
			}
			change, err := srv.pool.reload()
			if err != nil || len(change.unavailable) != 0 {
				t.Fatalf("completion wrongly depends on later watcher transition: %+v %v", change, err)
			}
			if strings.Count(logs.String(), `"msg":"refresh failed"`) != 1 {
				t.Fatal("refresh outcome not reported exactly once:", logs.String())
			}
		})
	}
}

func TestRefreshCompletionShutdownJoinsBeforeStorageClose(t *testing.T) {
	srv, account, consumed := consumedRefreshServer(t, 200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh"}`)
	lifetime, cancelRuntime := context.WithCancel(context.Background())
	defer cancelRuntime()
	srv.ctx = lifetime
	srv.admission = newAdmissionGate(1)
	if !srv.admission.acquire() {
		t.Fatal("admission")
	}
	srv.pool.storageMu.Lock()
	var release sync.Once
	unlock := func() { release.Do(srv.pool.storageMu.Unlock) }
	defer unlock()
	ctx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	request := make(chan bool, 1)
	go func() { defer srv.admission.release(); request <- srv.refreshedContext(ctx, account, account.id()) }()
	waitHTTPSignal(t, consumed)
	account.mu.Lock()
	operation := account.inflight
	account.mu.Unlock()
	if operation == nil {
		t.Fatal("operation ended before persistence")
	}
	cancelRequest()
	if <-request {
		t.Fatal("canceled request succeeded")
	}
	grace, cancelGrace := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelGrace()
	if err := srv.admission.wait(grace); err != nil {
		t.Fatal(err)
	}
	// This is the completion-join API used by both drainServer and serverCmd's
	// deferred cleanup. Done() synchronizes entry into its actual wait select.
	waiting := &refreshWaitContext{Context: grace, joined: make(chan struct{})}
	closed := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		err := srv.stopRefreshesContext(waiting, cancelRuntime)
		if err == nil {
			err = srv.pool.store.Close()
		}
		close(closed)
		result <- err
	}()
	waitHTTPSignal(t, waiting.joined)
	if lifetime.Err() == nil {
		t.Fatal("network work was not canceled before completion join")
	}
	select {
	case <-closed:
		t.Fatal("storage closed before pending publication")
	default:
	}
	if _, accepted := srv.refreshes.start(); accepted {
		srv.refreshes.finish(nil)
		t.Fatal("shutdown accepted a new operation")
	}
	unlock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	waitHTTPSignal(t, operation.done)
	if operation.err != nil || account.persisted().AccessToken != "rotated-access" {
		t.Fatalf("publication failed: %v", operation.err)
	}
	reopened, err := openStateStore(srv.pool.store.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	accounts, err := reopened.readAccounts()
	if err != nil || len(accounts) != 1 || accounts[0].persisted().RefreshToken != "rotated-refresh" {
		t.Fatalf("replacement credentials were not durable: %v", err)
	}
}

func TestRefreshCompletionShutdownBudgetCancelsBlockedPersistence(t *testing.T) {
	for _, block := range []string{"pool mutex", "SQL connection"} {
		t.Run(block, func(t *testing.T) {
			srv, account, consumed := consumedRefreshServer(t, 200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh"}`)
			lifetime, cancelRuntime := context.WithCancel(context.Background())
			defer cancelRuntime()
			srv.ctx = lifetime
			var unblock func()
			if block == "pool mutex" {
				srv.pool.storageMu.Lock()
				unblock = srv.pool.storageMu.Unlock
			} else {
				conn, err := srv.pool.store.db.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				unblock = func() { conn.Close() }
			}
			var once sync.Once
			release := func() { once.Do(unblock) }
			defer release()
			ctx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			request := make(chan bool, 1)
			go func() { request <- srv.refreshedContext(ctx, account, account.id()) }()
			waitHTTPSignal(t, consumed)
			account.mu.Lock()
			operation := account.inflight
			account.mu.Unlock()
			cancelRequest()
			<-request
			grace, expire := context.WithCancel(context.Background())
			defer expire()
			waiting := &refreshWaitContext{Context: grace, joined: make(chan struct{})}
			result := make(chan error, 1)
			go func() { result <- srv.stopRefreshesContext(waiting, cancelRuntime) }()
			waitHTTPSignal(t, waiting.joined)
			expire() // deterministic shutdown-budget exhaustion, not a 30s sleep
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("shutdown failure=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown left a blocked completion worker")
			}
			waitHTTPSignal(t, operation.done)
			if !errors.Is(operation.err, context.Canceled) {
				t.Fatalf("completion=%v", operation.err)
			}
			srv.refreshes.mu.Lock()
			active := srv.refreshes.active
			srv.refreshes.mu.Unlock()
			if active != 0 {
				t.Fatalf("workers after shutdown=%d", active)
			}
			release()
			if account.persisted().AccessToken == "rotated-access" {
				t.Fatal("canceled persistence resumed after shutdown")
			}
			if err := srv.pool.store.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRefreshCompletionShutdownReportsPersistenceFailure(t *testing.T) {
	srv, account, consumed := consumedRefreshServer(t, 200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh"}`)
	if _, err := srv.pool.store.db.Exec(`CREATE TRIGGER reject_refresh BEFORE UPDATE ON accounts BEGIN SELECT RAISE(FAIL, 'test refresh persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	srv.pool.storageMu.Lock()
	var release sync.Once
	unlock := func() { release.Do(srv.pool.storageMu.Unlock) }
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := make(chan bool, 1)
	go func() { request <- srv.refreshedContext(ctx, account, account.id()) }()
	waitHTTPSignal(t, consumed)
	cancel()
	<-request
	grace, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	waiting := &refreshWaitContext{Context: grace, joined: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- srv.stopRefreshesContext(waiting, func() {}) }()
	waitHTTPSignal(t, waiting.joined)
	unlock()
	if err := <-result; err == nil || !strings.Contains(err.Error(), "test refresh persistence failure") {
		t.Fatalf("shutdown hid pending persistence failure: %v", err)
	}
	if grace.Err() != nil {
		t.Fatal("test only exercised grace expiry, not an earlier completion error")
	}
	if account.persisted().AccessToken == "rotated-access" {
		t.Fatal("failed persistence published replacement credentials")
	}
}

func TestRefreshOperationsStopSerializesRegistration(t *testing.T) {
	var operations refreshOperations
	var workers sync.WaitGroup
	release := make(chan struct{})
	for range 100 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, accepted := operations.start(); accepted {
				<-release
				operations.finish(nil)
			}
		}()
	}
	idle := operations.stop()
	close(release)
	workers.Wait()
	waitHTTPSignal(t, idle)
	if _, accepted := operations.start(); accepted {
		operations.finish(nil)
		t.Fatal("registered work after stop")
	}
	if err := operations.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
