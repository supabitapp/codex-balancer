package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAdmissionGateLimitsAndReleasesWork(t *testing.T) {
	gate := newAdmissionGate(1)
	if !gate.acquire() {
		t.Fatal("first request rejected")
	}
	if gate.acquire() {
		t.Fatal("request admitted above limit")
	}
	gate.release()
	if !gate.acquire() {
		t.Fatal("request rejected after release")
	}
	gate.release()
}

func TestAdmissionGateDrainsActiveWork(t *testing.T) {
	gate := newAdmissionGate(1)
	if !gate.acquire() {
		t.Fatal("request rejected")
	}
	gate.beginDrain()
	if gate.acquire() {
		t.Fatal("request admitted during drain")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- gate.wait(ctx)
	}()
	select {
	case <-done:
		t.Fatal("drain completed while work remained")
	case <-time.After(10 * time.Millisecond):
	}

	gate.release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionGateDrainHonorsDeadline(t *testing.T) {
	gate := newAdmissionGate(1)
	if !gate.acquire() {
		t.Fatal("request rejected")
	}
	gate.beginDrain()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
	gate.release()
}

func TestAdmittedHandlerRejectsWhenFull(t *testing.T) {
	gate := newAdmissionGate(1)
	if !gate.acquire() {
		t.Fatal("request rejected")
	}
	server := &server{admission: gate}
	handler := server.admitted(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	gate.release()

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if got := response.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
}

func TestDrainServerWaitsBeforeCancelingActiveWork(t *testing.T) {
	gate := newAdmissionGate(1)
	if !gate.acquire() {
		t.Fatal("request rejected")
	}
	runtimeCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		drainServer(&http.Server{}, gate, cancel)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		gate.mu.Lock()
		draining := gate.draining
		gate.mu.Unlock()
		if draining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not begin")
		}
		time.Sleep(time.Millisecond)
	}

	select {
	case <-runtimeCtx.Done():
		t.Fatal("runtime canceled while work remained")
	case <-time.After(10 * time.Millisecond):
	}
	if gate.acquire() {
		t.Fatal("request admitted after shutdown began")
	}

	gate.release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
	if runtimeCtx.Err() == nil {
		t.Fatal("runtime remained active after drain")
	}
}
