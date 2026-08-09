package main

import (
	"context"
	"net/http"
	"sync"
)

const maxActiveProxyRequests = 128

type admissionGate struct {
	mu       sync.Mutex
	active   int
	limit    int
	draining bool
	idle     chan struct{}
}

func newAdmissionGate(limit int) *admissionGate {
	idle := make(chan struct{})
	close(idle)
	return &admissionGate{limit: limit, idle: idle}
}

func (g *admissionGate) acquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining || g.active >= g.limit {
		return false
	}
	if g.active == 0 {
		g.idle = make(chan struct{})
	}
	g.active++
	return true
}

func (g *admissionGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.active == 0 {
		close(g.idle)
	}
}

func (g *admissionGate) beginDrain() {
	g.mu.Lock()
	g.draining = true
	g.mu.Unlock()
}

func (g *admissionGate) wait(ctx context.Context) error {
	g.mu.Lock()
	idle := g.idle
	g.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *server) admitted(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.admission == nil {
			next(w, r)
			return
		}
		if !s.admission.acquire() {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, "server busy")
			return
		}
		defer s.admission.release()
		next(w, r)
	})
}
