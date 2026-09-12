package app

import (
	"context"
	"sync"
)

// A zero-value mutex with cancelable acquisition for bounded completion work.
// Ordinary pool/routing callers retain the same Lock/Unlock behavior.
type contextMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *contextMutex) Lock() { _ = m.LockContext(context.Background()) }

func (m *contextMutex) LockContext(ctx context.Context) error {
	m.once.Do(func() {
		m.token = make(chan struct{}, 1)
		m.token <- struct{}{}
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.token:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	}
}

func (m *contextMutex) Unlock() {
	select {
	case m.token <- struct{}{}:
	default:
		panic("unlock of unlocked contextMutex")
	}
}
