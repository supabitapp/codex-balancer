package app

import "sync"

// activeWebSocketRegistry lets account lifecycle changes terminate connections
// that were authenticated as that account. Callbacks are invoked without the
// registry lock so closing a socket cannot block unrelated registration.
type activeWebSocketRegistry struct {
	mu        sync.Mutex
	next      uint64
	byAccount map[string]map[uint64]func(string)
}

func (r *activeWebSocketRegistry) add(account string, closeSocket func(string)) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byAccount == nil {
		r.byAccount = map[string]map[uint64]func(string){}
	}
	r.next++
	if r.byAccount[account] == nil {
		r.byAccount[account] = map[uint64]func(string){}
	}
	r.byAccount[account][r.next] = closeSocket
	return r.next
}

func (r *activeWebSocketRegistry) move(id uint64, from, to string) {
	if id == 0 || from == to {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	accounts := r.byAccount[from]
	closeSocket := accounts[id]
	if closeSocket == nil {
		return
	}
	delete(accounts, id)
	if len(accounts) == 0 {
		delete(r.byAccount, from)
	}
	if r.byAccount[to] == nil {
		r.byAccount[to] = map[uint64]func(string){}
	}
	r.byAccount[to][id] = closeSocket
}

func (r *activeWebSocketRegistry) remove(id uint64, account string) {
	if id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	accounts := r.byAccount[account]
	delete(accounts, id)
	if len(accounts) == 0 {
		delete(r.byAccount, account)
	}
}

func (r *activeWebSocketRegistry) closeAccount(account, reason string) int {
	r.mu.Lock()
	accounts := r.byAccount[account]
	delete(r.byAccount, account)
	callbacks := make([]func(string), 0, len(accounts))
	for _, closeSocket := range accounts {
		callbacks = append(callbacks, closeSocket)
	}
	r.mu.Unlock()

	for _, closeSocket := range callbacks {
		closeSocket(reason)
	}
	return len(callbacks)
}
