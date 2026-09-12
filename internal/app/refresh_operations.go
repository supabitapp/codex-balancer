package app

import (
	"context"
	"errors"
	"sync"
	"time"
)

const refreshCompletionTimeout = 30 * time.Second

// Registration and stopping share a lock: shutdown cannot observe zero and
// close storage while a new refresh operation is about to start. Counts cover
// exchange, persistence, publication and completion callbacks, not waiters.
type refreshOperations struct {
	mu         sync.Mutex
	active     int
	stopping   bool
	idle       chan struct{}
	completion context.Context
	cancel     context.CancelFunc
	failure    error // first completion failure after shutdown registration closes
}

func (g *refreshOperations) init() {
	if g.idle == nil {
		g.idle = make(chan struct{})
		close(g.idle)
		g.completion, g.cancel = context.WithCancel(context.Background())
	}
}

func (g *refreshOperations) start() (context.Context, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	if g.stopping {
		return nil, false
	}
	if g.active == 0 {
		g.idle = make(chan struct{})
	}
	g.active++
	return g.completion, true
}

func (g *refreshOperations) finish(completionErr error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping && g.failure == nil && completionErr != nil {
		g.failure = completionErr
	}
	g.active--
	if g.active == 0 {
		close(g.idle)
	}
}

func (g *refreshOperations) stop() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	g.stopping = true
	return g.idle
}

func (g *refreshOperations) wait(ctx context.Context) error {
	idle := g.stop()
	select {
	case <-idle:
		g.cancel()
		return g.completionFailure()
	case <-ctx.Done():
		// Owned pool-lock, SQL and notification-lock waits honor completion.
		// Cancel those waits, then join before permitting storage to close.
		g.cancel()
		<-idle
		return errors.Join(ctx.Err(), g.completionFailure())
	}
}

func (g *refreshOperations) completionFailure() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failure
}
