package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Each operation owns its result and lifetime; callers own only their wait.
// An abandoned exchange must finish before a later caller starts another one.
type accountRefresh struct {
	done      chan struct{}
	cancel    context.CancelFunc
	waiters   int
	abandoned bool
	err       error // published by closing done
}

type refreshOwner struct {
	lifetime   context.Context
	client     *http.Client
	operations *refreshOperations
	persist    func(context.Context, accountState) (accountState, error)
	completed  func(context.Context, error) error
}

func (a *Account) refresh(ctx context.Context, hc *http.Client, persist func(accountState) (accountState, error)) error {
	owner := refreshOwner{lifetime: context.Background(), client: hc}
	if persist != nil {
		owner.persist = func(_ context.Context, state accountState) (accountState, error) { return persist(state) }
	}
	return a.refreshOwned(ctx, owner)
}

func (a *Account) refreshOwned(wait context.Context, owner refreshOwner) error {
	for {
		if err := wait.Err(); err != nil {
			return err
		}
		a.mu.Lock()
		operation := a.inflight
		if operation != nil && operation.abandoned {
			a.mu.Unlock()
			select {
			case <-wait.Done():
				return wait.Err()
			case <-operation.done:
				continue
			}
		}
		if operation == nil {
			if err := owner.lifetime.Err(); err != nil {
				a.mu.Unlock()
				return err
			}
			if a.Reauth != "" {
				err := fmt.Errorf("account %s needs reauth: %s", claimsFromToken(a.IDToken).Auth.AccountID, a.Reauth)
				a.mu.Unlock()
				return err
			}
			completionParent := context.Background()
			if owner.operations != nil {
				var registered bool
				completionParent, registered = owner.operations.start()
				if !registered {
					a.mu.Unlock()
					return context.Canceled
				}
			}
			ctx, cancel := context.WithTimeout(owner.lifetime, refreshTimeout)
			operation = &accountRefresh{done: make(chan struct{}), cancel: cancel}
			a.inflight = operation
			state := a.accountState
			go func() {
				var completionErr error
				if owner.operations != nil {
					defer func() { owner.operations.finish(completionErr) }()
				}
				tokens, permanent, err := exchangeRefreshToken(ctx, owner.client, state.RefreshToken)
				cancel()
				// Decoded results must survive waiter/runtime cancellation. Only
				// the completion budget (or its shutdown grace) can interrupt this
				// phase, including persistence and unavailable-owner notification.
				completion, stop := context.WithTimeout(completionParent, refreshCompletionTimeout)
				err, completionErr = a.finishRefresh(completion, owner.persist, state, tokens, permanent, err)
				if owner.completed != nil {
					notifyErr := owner.completed(completion, err)
					err = errors.Join(err, notifyErr)
					completionErr = errors.Join(completionErr, notifyErr)
				}
				stop()
				a.mu.Lock()
				operation.err = err
				a.inflight = nil
				close(operation.done)
				a.mu.Unlock()
			}()
		}
		operation.waiters++
		a.mu.Unlock()

		var err error
		select {
		case <-wait.Done():
			err = wait.Err()
		case <-operation.done:
			err = operation.err
		}
		a.mu.Lock()
		operation.waiters--
		if operation.waiters == 0 && a.inflight == operation {
			operation.abandoned = true
			operation.cancel()
		}
		a.mu.Unlock()
		return err
	}
}
