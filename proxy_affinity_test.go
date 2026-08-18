package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestHTTPNewWorkUsesZeroRemainingAccountWithExpiringReset(t *testing.T) {
	now := time.Now()
	resetting := testAccount("account-resetting", 100)
	resetting.primary = window{usedPercent: 10, minutes: 300, resetsAt: now.Add(4 * time.Hour), seenAt: now}
	resetting.secondary = window{usedPercent: 100, minutes: 7 * 24 * 60, resetsAt: now.Add(6 * 24 * time.Hour), seenAt: now}
	adoptTestResetCredit(resetting, now.Add(30*time.Minute))
	roomier := testAccount("account-roomier", 10)
	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{roomier, resetting}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_resetting")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-resetting]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPNewWorkDrainsBeforeExpiringReset(t *testing.T) {
	now := time.Now()
	earlier := testAccount("account-earlier-reset", 40)
	adoptTestResetCredit(earlier, now.Add(time.Hour))
	later := testAccount("account-later-reset", 20)
	adoptTestResetCredit(later, now.Add(2*time.Hour))
	drain := testAccount("account-drain", 99)
	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{drain, later, earlier}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-drain]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPNewWorkDrainsOnlyAccountsBelowFivePercent(t *testing.T) {
	tests := []struct {
		name        string
		usedPercent float64
		want        string
	}{
		{name: "below", usedPercent: 95.01, want: "account-drain"},
		{name: "equal", usedPercent: 95, want: "account-roomier"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drain := testAccount("account-drain", tt.usedPercent)
			roomier := testAccount("account-roomier", 50)
			calls := []string{}
			server, _, closeServer := newAffinityHTTPServer(t, []*Account{roomier, drain}, func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Header.Get("chatgpt-account-id"))
				writeResponseCreated(w, "resp")
			})
			defer closeServer()

			response := serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if fmt.Sprint(calls) != fmt.Sprintf("[%s]", tt.want) {
				t.Fatalf("calls = %v", calls)
			}
		})
	}
}

func TestHTTPDrainingAccountForcesFastTier(t *testing.T) {
	tests := []struct {
		name        string
		usedPercent float64
		tiers       []string
		body        string
		want        string
	}{
		{name: "forces unset tier", usedPercent: 96, tiers: []string{"priority"}, body: `{"model":"gpt-5.6-sol","input":[]}`, want: "priority"},
		{name: "overrides explicit default", usedPercent: 96, tiers: []string{"priority"}, body: `{"model":"gpt-5.6-sol","service_tier":"default","input":[]}`, want: "priority"},
		{name: "keeps tier when not draining", usedPercent: 50, tiers: []string{"priority"}, body: `{"model":"gpt-5.6-sol","input":[]}`, want: ""},
		{name: "keeps tier when model lacks priority", usedPercent: 96, tiers: nil, body: `{"model":"gpt-5.6-sol","input":[]}`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drain := testAccount("account-drain", tt.usedPercent)
			tiers := []string{}
			server, _, closeServer := newAffinityHTTPServer(t, []*Account{drain}, func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				tiers = append(tiers, responseRequest(body).ServiceTier)
				writeResponseCreated(w, "resp")
			})
			defer closeServer()
			server.catalog.replace(
				[]string{drain.id()},
				map[string][]modelEntry{drain.id(): {testModelEntry("gpt-5.6-sol", tt.tiers...)}},
				"0.1.0",
			)

			response := serveHTTPResponse(t, server, "", "", tt.body)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if fmt.Sprint(tiers) != fmt.Sprintf("[%s]", tt.want) {
				t.Fatalf("service tiers = %v, want %q", tiers, tt.want)
			}
		})
	}
}

func TestHTTPDrainingAccountForcesFastTierInZstdBody(t *testing.T) {
	drain := testAccount("account-drain", 96)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll([]byte(`{"model":"gpt-5.6-sol","input":[]}`), nil)
	encoder.Close()
	tiers := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{drain}, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Encoding"); got != "zstd" {
			t.Errorf("content encoding = %q, want zstd", got)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		decoded, err := decodeRequestBody(r.Header, body)
		if err != nil {
			t.Error(err)
			return
		}
		tiers = append(tiers, responseRequest(decoded).ServiceTier)
		writeResponseCreated(w, "resp")
	})
	defer closeServer()
	server.catalog.replace(
		[]string{drain.id()},
		map[string][]modelEntry{drain.id(): {testModelEntry("gpt-5.6-sol", "priority")}},
		"0.1.0",
	)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressed))
	request.Header.Set("Content-Encoding", "zstd")
	response := httptest.NewRecorder()
	server.responses(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(tiers) != "[priority]" {
		t.Fatalf("service tiers = %v, want priority", tiers)
	}
}

func TestHTTPNewWorkDrainsLowestRemainingAccountFirst(t *testing.T) {
	onePercent := testAccount("account-one-percent", 99)
	oneAndHalfPercent := testAccount("account-one-half-percent", 98.5)
	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{oneAndHalfPercent, onePercent}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-one-percent]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPSoftSessionMovesFromSpentAccount(t *testing.T) {
	a := testAccount("account-a", 99)
	b := testAccount("account-b", 0)
	a.spent = true
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-b]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_b"}); got != "account-b" {
		t.Fatalf("response owner = %q, want account-b", got)
	}
}

func TestHTTPSoftSessionMovesToDrainingAccount(t *testing.T) {
	owner := testAccount("account-owner", 10)
	draining := testAccount("account-draining", 96)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{owner, draining}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_draining")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, owner.id()); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-draining]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPHardResponseStaysOnOwnerWhileDraining(t *testing.T) {
	owner := testAccount("account-owner", 10)
	draining := testAccount("account-draining", 96)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{owner, draining}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_next")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_owner"}, owner.id()); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"previous_response_id":"resp_owner","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-owner]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPSoftSessionRetriesRateLimitOnAnotherAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		if account == "account-a" {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a account-b]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
}

func TestHTTPGenericRateLimitDoesNotSpendAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	apiCalls := 0
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer usage.Close()
	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = usage.URL
	t.Cleanup(func() { accountAPIBaseURL = oldBaseURL })

	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		accountID := r.Header.Get("chatgpt-account-id")
		calls = append(calls, accountID)
		if len(calls) == 1 {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "rate_limit_exceeded"}})
			return
		}
		writeResponseCreated(w, "resp")
	})
	defer closeServer()

	for range 2 {
		response := serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	}
	if fmt.Sprint(calls) != "[account-a account-b account-a]" {
		t.Fatalf("calls = %v", calls)
	}
	if apiCalls != 0 {
		t.Fatalf("account API calls = %d", apiCalls)
	}
}

func TestHTTPUsageLimitRemovesAccountFromLaterRouting(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	resetCreditCalls := useNoResetCreditsAPI(t)
	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		if account == "account-a" {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "usage_limit_reached"}})
			return
		}
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()

	for range 2 {
		response := serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	}
	if fmt.Sprint(calls) != "[account-a account-b account-b]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := resetCreditCalls(); got != 1 {
		t.Fatalf("reset credit calls = %d", got)
	}
}

func TestHTTPHardUsageLimitNeverMovesAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	resetCreditCalls := useNoResetCreditsAPI(t)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "usage_limit_reached"}})
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "", "", `{"model":"gpt","previous_response_id":"resp_a","input":[]}`)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := resetCreditCalls(); got != 1 {
		t.Fatalf("reset credit calls = %d", got)
	}
}

func TestHTTPWorkspaceUsageLimitDoesNotRedeemReset(t *testing.T) {
	now := time.Now().UTC()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	adoptTestResetCredit(a, now.Add(time.Hour))
	apiCalls := 0
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer usage.Close()
	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = usage.URL
	t.Cleanup(func() { accountAPIBaseURL = oldBaseURL })

	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		if account == "account-a" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("x-codex-rate-limit-reached-type", "workspace_member_usage_limit_reached")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "usage_limit_reached"}})
			return
		}
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a account-b]" {
		t.Fatalf("calls = %v", calls)
	}
	if apiCalls != 0 {
		t.Fatalf("account API calls = %d", apiCalls)
	}
}

func TestHTTPUsageLimitRedeemsResetWithinTwentyFourHoursAndRetriesSameAccount(t *testing.T) {
	account := testAccount("account-a", 99)
	api := useResetAPI(t, 23*time.Hour, 0, http.StatusOK)

	upstreamCalls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls = append(upstreamCalls, r.Header.Get("chatgpt-account-id"))
		if len(upstreamCalls) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "usage_limit_reached"}})
			return
		}
		writeResponseCreated(w, "resp_a")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(upstreamCalls) != "[account-a account-a]" {
		t.Fatalf("upstream calls = %v", upstreamCalls)
	}
	wantAPICalls := []string{
		"GET /rate-limit-reset-credits",
		"POST /rate-limit-reset-credits/consume",
		"GET /usage",
	}
	if calls := api.calls(); fmt.Sprint(calls) != fmt.Sprint(wantAPICalls) {
		t.Fatalf("API calls = %v, want %v", calls, wantAPICalls)
	}
}

func TestHTTPUsageLimitResetFailureFallsBackAndKeepsAccountSpent(t *testing.T) {
	tests := []struct {
		name          string
		expiresAfter  time.Duration
		usedPercent   float64
		consumeStatus int
		wantAPICalls  []string
	}{
		{
			name:          "credit expires after twenty four hours",
			expiresAfter:  24*time.Hour + time.Minute,
			usedPercent:   0,
			consumeStatus: http.StatusOK,
			wantAPICalls:  []string{"GET /rate-limit-reset-credits"},
		},
		{
			name:          "redemption fails",
			expiresAfter:  time.Hour,
			usedPercent:   0,
			consumeStatus: http.StatusInternalServerError,
			wantAPICalls: []string{
				"GET /rate-limit-reset-credits",
				"POST /rate-limit-reset-credits/consume",
			},
		},
		{
			name:          "redemption restores no quota",
			expiresAfter:  time.Hour,
			usedPercent:   100,
			consumeStatus: http.StatusOK,
			wantAPICalls: []string{
				"GET /rate-limit-reset-credits",
				"POST /rate-limit-reset-credits/consume",
				"GET /usage",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := testAccount("account-a", 0)
			b := testAccount("account-b", 20)
			api := useResetAPI(t, test.expiresAfter, test.usedPercent, test.consumeStatus)
			upstreamCalls := []string{}
			server, _, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
				account := r.Header.Get("chatgpt-account-id")
				upstreamCalls = append(upstreamCalls, account)
				if account == "account-a" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "usage_limit_reached"}})
					return
				}
				writeResponseCreated(w, "resp_b")
			})
			defer closeServer()

			for range 2 {
				response := serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
				if response.Code != http.StatusOK {
					t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
				}
			}
			if fmt.Sprint(upstreamCalls) != "[account-a account-b account-b]" {
				t.Fatalf("upstream calls = %v", upstreamCalls)
			}
			if calls := api.calls(); fmt.Sprint(calls) != fmt.Sprint(test.wantAPICalls) {
				t.Fatalf("API calls = %v, want %v", calls, test.wantAPICalls)
			}
		})
	}
}

func TestHTTPConcurrentUsageLimitsRedeemOneReset(t *testing.T) {
	account := testAccount("account-a", 99)
	api := useResetAPI(t, time.Hour, 0, http.StatusOK)

	var upstreamMu sync.Mutex
	upstreamCalls := 0
	failures := 0
	failuresReady := make(chan struct{})
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		upstreamMu.Lock()
		upstreamCalls++
		if failures < 2 {
			failures++
			if failures == 2 {
				close(failuresReady)
			}
			upstreamMu.Unlock()
			<-failuresReady
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "usage_limit_reached"}})
			return
		}
		upstreamMu.Unlock()
		writeResponseCreated(w, "resp_a")
	})
	defer closeServer()

	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			<-start
			results <- serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
		}()
	}
	close(start)
	for range 2 {
		response := <-results
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	}
	if got := api.consumeCalls(); got != 1 {
		t.Fatalf("consume calls = %d", got)
	}
	upstreamMu.Lock()
	gotUpstreamCalls := upstreamCalls
	upstreamMu.Unlock()
	if gotUpstreamCalls != 4 {
		t.Fatalf("upstream calls = %d", gotUpstreamCalls)
	}
}

func TestHTTPLateConcurrentUsageLimitUsesCompletedReset(t *testing.T) {
	account := testAccount("account-a", 99)
	api := useResetAPI(t, time.Hour, 0, http.StatusOK)

	var upstreamMu sync.Mutex
	upstreamCalls := 0
	bothStarted := make(chan struct{})
	releaseLateFailure := make(chan struct{})
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		upstreamMu.Lock()
		upstreamCalls++
		call := upstreamCalls
		if call == 2 {
			close(bothStarted)
		}
		upstreamMu.Unlock()
		if call <= 2 {
			<-bothStarted
			if call == 2 {
				<-releaseLateFailure
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "usage_limit_reached"}})
			return
		}
		if call == 3 {
			close(releaseLateFailure)
		}
		writeResponseCreated(w, "resp_a")
	})
	defer closeServer()

	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			<-start
			results <- serveHTTPResponse(t, server, "", "", `{"model":"gpt","input":[]}`)
		}()
	}
	close(start)
	for range 2 {
		response := <-results
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	}
	if got := api.consumeCalls(); got != 1 {
		t.Fatalf("consume calls = %d", got)
	}
	upstreamMu.Lock()
	gotUpstreamCalls := upstreamCalls
	upstreamMu.Unlock()
	if gotUpstreamCalls != 4 {
		t.Fatalf("upstream calls = %d", gotUpstreamCalls)
	}
}

func TestHTTPSoftSessionRetriesServerFailureOnAnotherAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		if account == "account-a" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a account-a account-a account-a account-b]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPServerFailureRetriesSameAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	calls := 0
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeResponseCreated(w, "resp_a")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if events := server.stats.snapshot().Events; len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
}

func TestHTTPCanceledRequestDoesNotFailOver(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	started := make(chan struct{})
	release := make(chan struct{})
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})
	defer closeServer()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt","input":[]}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		server.responses(httptest.NewRecorder(), request)
		close(done)
	}()

	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled request did not return")
	}

	requireNoFailedAccounts(t, server, a, b)
}

func TestHTTPUnsupportedModelRetriesAnotherAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		if account == "account-a" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":"invalid_request_error","message":"The 'gpt-route' model is not supported when using Codex with a ChatGPT account."}}`)
			return
		}
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-route","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a account-b]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPUnsupportedModelPreservesRejectionWithoutReplacement(t *testing.T) {
	a := testAccount("account-a", 0)
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"invalid_request_error","message":"The 'gpt-route' model is not supported when using Codex with a ChatGPT account."}}`)
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-route","input":[]}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "The 'gpt-route' model is not supported") {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestHTTPModelCatalogSkipsUnsupportedSoftOwner(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()
	server.catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-5.6-terra")},
			b.id(): {testModelEntry("gpt-5.6-sol")},
		},
		"0.1.0",
	)
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, a.id()); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-sol","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-b]" {
		t.Fatalf("calls = %v, want account-b", calls)
	}
}

func TestHTTPModelCatalogFiltersPriorityTier(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()
	server.catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-5.6-sol")},
			b.id(): {testModelEntry("gpt-5.6-sol", "priority")},
		},
		"0.1.0",
	)

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-sol","service_tier":"priority","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-b]" {
		t.Fatalf("calls = %v, want account-b", calls)
	}
}

func TestHTTPModelCatalogDoesNotMoveHardOwner(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "unexpected")
	})
	defer closeServer()
	server.catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-5.6-terra")},
			b.id(): {testModelEntry("gpt-5.6-sol")},
		},
		"0.1.0",
	)
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, a.id()); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-sol","previous_response_id":"resp_a","input":[]}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %v, want none", calls)
	}
}

func TestHTTPUnsupportedModelRetriesOnlyOnce(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	c := testAccount("account-c", 40)
	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a, b, c}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		if account == c.id() {
			writeResponseCreated(w, "unexpected")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"invalid_request_error","message":"The 'gpt-route' model is not supported when using Codex with a ChatGPT account."}}`)
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-route","input":[]}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a account-b]" {
		t.Fatalf("calls = %v, want one replacement", calls)
	}
}

func TestHTTPUnsupportedModelRequiresInvalidRequestCode(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"other_error","message":"The 'gpt-route' model is not supported when using Codex with a ChatGPT account."}}`)
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-route","input":[]}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("calls = %v, want one attempt", calls)
	}
}

func TestHTTPHardPreviousResponseNeverMoves(t *testing.T) {
	a := testAccount("account-a", 99)
	b := testAccount("account-b", 0)
	a.spent = true
	calls := 0
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeResponseCreated(w, "unexpected")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"previous_response_id":"resp_a","input":[]}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestHTTPResponseTurnStateBindsOwner(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Turn-State", "turn")
		writeResponseCreated(w, "resp_a")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := store.lookup(affinityRef{kind: affinityTurnState, value: "turn"}); got != "account-a" {
		t.Fatalf("turn owner = %q, want account-a", got)
	}
}

func TestHTTPUnknownHardAffinityFailsBeforeUpstream(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "", "turn", `{"input":[]}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinityTurnState, value: "turn"}); got != "" {
		t.Fatalf("turn owner = %q, want none", got)
	}
}

func TestHTTPHardAffinityDoesNotFailOverAfterNetworkFailure(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	var mu sync.Mutex
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		mu.Lock()
		calls = append(calls, account)
		mu.Unlock()
		if account == "account-a" {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
		writeResponseCreated(w, "unexpected")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinityTurnState, value: "turn"}, "account-a"); err != nil {
		t.Fatal(err)
	}
	lastUsed := time.Now().Add(-2 * time.Hour).UnixNano()
	if _, err := store.store.db.Exec(
		`UPDATE bindings SET last_used_at_ns = ? WHERE kind = ? AND value = ?`,
		lastUsed,
		affinityTurnState,
		"turn",
	); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "", "turn", `{"input":[]}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	mu.Lock()
	gotCalls := fmt.Sprint(calls)
	mu.Unlock()
	if gotCalls != "[account-a]" {
		t.Fatalf("calls = %s", gotCalls)
	}
	if got := store.lookup(affinityRef{kind: affinityTurnState, value: "turn"}); got != "account-a" {
		t.Fatalf("turn owner = %q, want account-a", got)
	}
	var storedLastUsed int64
	if err := store.store.db.QueryRow(
		`SELECT last_used_at_ns FROM bindings WHERE kind = ? AND value = ?`,
		affinityTurnState,
		"turn",
	).Scan(&storedLastUsed); err != nil {
		t.Fatal(err)
	}
	if storedLastUsed != lastUsed {
		t.Fatalf("last used = %d, want %d", storedLastUsed, lastUsed)
	}
}

func TestHTTPPreviousResponseOverridesSoftSessionWithoutRebinding(t *testing.T) {
	a := testAccount("account-a", 90)
	b := testAccount("account-b", 0)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_next")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"previous_response_id":"resp_a","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
}

func TestHTTPPreviousResponseOverridesPromptCacheWithoutRebinding(t *testing.T) {
	a := testAccount("account-a", 90)
	b := testAccount("account-b", 0)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_next")
	})
	defer closeServer()
	cache := affinityRef{kind: affinityPromptCache, value: "cache"}
	if err := store.bind(cache, "account-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "", "", `{"prompt_cache_key":"cache","previous_response_id":"resp_a","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(cache); got != "account-b" {
		t.Fatalf("cache owner = %q, want account-b", got)
	}
}

func TestHTTPOpaqueFileCanFailOver(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		calls = append(calls, account)
		if account == "account-a" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeResponseCreated(w, "resp_b")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[{"type":"input_file","file_id":"file_unknown"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a account-a account-a account-a account-b]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinityFile, value: "file_unknown"}); got != "" {
		t.Fatalf("file owner = %q, want empty", got)
	}
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
}

func TestHTTPKnownFileOverridesSoftSessionWithoutRebinding(t *testing.T) {
	a := testAccount("account-a", 90)
	b := testAccount("account-b", 0)
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		writeResponseCreated(w, "resp_next")
	})
	defer closeServer()
	session := affinityRef{kind: affinitySession, value: "session"}
	file := affinityRef{kind: affinityFile, value: "file_owned"}
	if err := store.bind(session, "account-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(file, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "session", "", `{"input":[{"type":"input_file","file_id":"file_owned"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(session); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
}

func TestHTTPPartialFileOwnershipFailsBeforeUpstream(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 0)
	calls := 0
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(http.ResponseWriter, *http.Request) {
		calls++
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinityFile, value: "file_known"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "", "", `{"input":[{"type":"input_file","file_id":"file_known"},{"type":"input_file","file_id":"file_unknown"}]}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestHTTPZstdBodyPreservesAffinityAndForwardedBytes(t *testing.T) {
	a := testAccount("account-a", 90)
	b := testAccount("account-b", 0)
	body := []byte(`{"previous_response_id":"resp_a","input":[]}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(body, nil)
	encoder.Close()
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("chatgpt-account-id"))
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if !bytes.Equal(got, compressed) {
			t.Errorf("forwarded body changed")
			return
		}
		if got := r.Header.Get("Content-Encoding"); got != "zstd" {
			t.Errorf("content encoding = %q, want zstd", got)
			return
		}
		writeResponseCreated(w, "resp_next")
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressed))
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set("session-id", "session")
	response := httptest.NewRecorder()
	server.responses(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if fmt.Sprint(calls) != "[account-a]" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHTTPCompletedFollowUpsLeaveNoLiveThreads(t *testing.T) {
	account := testAccount("account", 0)
	calls := 0
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeResponseCreated(w, fmt.Sprintf("resp_%d", calls))
	})
	defer closeServer()

	first := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-sol","reasoning":{"effort":"medium"},"input":[]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	second := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-terra","reasoning":{"effort":"xhigh"},"previous_response_id":"resp_1","input":[]}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}

	snapshot := server.stats.snapshot()
	if snapshot.Turns != 2 {
		t.Fatalf("turns = %d, want 2", snapshot.Turns)
	}
	if len(snapshot.Threads) != 0 {
		t.Fatalf("completed HTTP threads = %+v", snapshot.Threads)
	}
}

func TestHTTPThreadLivesOnlyWhileResponseIsOpen(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseResponse := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseResponse()
	account := testAccount("account", 0)
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		writeResponseCreated(w, "resp")
		w.(http.Flusher).Flush()
		close(started)
		<-release
		io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	})
	defer closeServer()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-sol","input":[]}`)
	}()
	<-started
	deadline := time.Now().Add(time.Second)
	for {
		threads := server.stats.snapshot().Threads
		if len(threads) == 1 {
			if threads[0].Key != "session" || threads[0].Via != transportHTTP {
				t.Fatalf("live HTTP thread = %+v", threads[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("HTTP thread did not become live")
		}
		time.Sleep(time.Millisecond)
	}

	releaseResponse()
	response := <-done
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if threads := server.stats.snapshot().Threads; len(threads) != 0 {
		t.Fatalf("completed HTTP threads = %+v", threads)
	}
}

func TestHTTPConflictingHardOwnersFailBeforeUpstream(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 0)
	calls := 0
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		calls++
	})
	defer closeServer()
	if err := store.bind(affinityRef{kind: affinityTurnState, value: "turn"}, "account-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp"}, "account-b"); err != nil {
		t.Fatal(err)
	}

	response := serveHTTPResponse(t, server, "", "turn", `{"previous_response_id":"resp","input":[]}`)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestHTTPChunkedResponseRegistersHardOwner(t *testing.T) {
	a := testAccount("account-a", 10)
	b := testAccount("account-b", 0)
	var mu sync.Mutex
	calls := []string{}
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a, b}, func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		mu.Lock()
		calls = append(calls, account)
		call := len(calls)
		mu.Unlock()
		if call == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"type":"response.cre`)
			w.(http.Flusher).Flush()
			io.WriteString(w, `ated","response":{"id":"resp_a"}}`+"\n\n")
			return
		}
		writeResponseCreated(w, "resp_next")
	})
	defer closeServer()

	first := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	second := serveHTTPResponse(t, server, "other", "", `{"previous_response_id":"resp_a","input":[]}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(calls) != "[account-b account-b]" {
		t.Fatalf("calls = %v", calls)
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_a"}); got != "account-b" {
		t.Fatalf("response owner = %q, want account-b", got)
	}
}

func TestHTTPCarriageReturnStreamRegistersHardOwner(t *testing.T) {
	a := testAccount("account-a", 0)
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cr\"}}\r\r")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_cr\"}}\r\r")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_cr"}); got != "account-a" {
		t.Fatalf("response owner = %q, want account-a", got)
	}
}

func TestHTTPMultilineStreamRegistersHardOwner(t *testing.T) {
	a := testAccount("account-a", 0)
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.created\",\r")
		w.(http.Flusher).Flush()
		io.WriteString(w, "\ndata: \"response\":{\"id\":\"resp_multiline\"}}\r\n\r\n")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_multiline"}); got != "account-a" {
		t.Fatalf("response owner = %q, want account-a", got)
	}
}

func TestHTTPJSONResponseRegistersHardOwner(t *testing.T) {
	a := testAccount("account-a", 0)
	server, store, closeServer := newAffinityHTTPServer(t, []*Account{a}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_json","object":"response","status":"completed"}`)
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_json"}); got != "account-a" {
		t.Fatalf("response owner = %q, want account-a", got)
	}
}

func TestHTTPCompletedResponseTracksAPIEstimate(t *testing.T) {
	account := testAccount("account-a", 0)
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":800},"output_tokens":100}}}`+"\n\n")
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"model":"gpt-5.6-sol","input":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	snapshot := server.stats.snapshot()
	if snapshot.APICostNanoDollars != 4_400_000 || snapshot.UnpricedResponses != 0 {
		t.Fatalf("API estimate = %d with %d unpriced, want 4400000 with none", snapshot.APICostNanoDollars, snapshot.UnpricedResponses)
	}
	if len(snapshot.Threads) != 0 {
		t.Fatalf("completed HTTP threads = %+v", snapshot.Threads)
	}
}

func TestHTTPV2CompactionTracksMetadataAndUsage(t *testing.T) {
	account := testAccount("account-a", 0)
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{account}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"model":"gpt-5.6-sol","usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":100},"output_tokens":100,"total_tokens":1100,"output_tokens_details":{"reasoning_tokens":60}}}}`+"\n\n")
	})
	defer closeServer()
	logs := &testLogBuffer{}
	server.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	metadata := turnMetadata{RequestKind: "compaction", ThreadID: "codex-thread", TurnID: "codex-turn", WindowID: "codex-window:1", SubagentKind: "compact"}
	body, err := json.Marshal(map[string]any{
		"model":           "gpt-5.6-sol",
		"client_metadata": map[string]string{codexTurnMetadataKey: encodeTurnMetadata(metadata)},
		"input":           []any{map[string]any{"type": "compaction_trigger"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := serveHTTPResponse(t, server, "session", "", string(body))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if threads := server.stats.snapshot().Threads; len(threads) != 0 {
		t.Fatalf("completed HTTP threads = %+v", threads)
	}
	requireLogRecord(t, logs.records(t), "response usage", map[string]any{
		"transport":          "http",
		"thread":             "session",
		"turn":               "codex-turn",
		"request_kind":       "compaction",
		"account":            "account-a",
		"rotation_source":    "",
		"compaction_replay":  false,
		"model":              "gpt-5.6-sol",
		"service_tier":       "",
		"input_tokens":       float64(1_000),
		"cached_tokens":      float64(800),
		"cache_write_tokens": float64(100),
		"output_tokens":      float64(100),
		"reasoning_tokens":   float64(60),
	})
}

func TestHTTPMissingPreviousResponseFailsBeforeUpstream(t *testing.T) {
	a := testAccount("account-a", 0)
	calls := 0
	server, _, closeServer := newAffinityHTTPServer(t, []*Account{a}, func(w http.ResponseWriter, r *http.Request) {
		calls++
	})
	defer closeServer()

	response := serveHTTPResponse(t, server, "session", "", `{"previous_response_id":"missing","input":[]}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func newAffinityHTTPServer(
	t *testing.T,
	accounts []*Account,
	handler http.HandlerFunc,
) (*server, *AffinityStore, func()) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	state, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	pool, err := loadPool(state)
	if err != nil {
		state.Close()
		upstream.Close()
		t.Fatal(err)
	}
	for _, account := range accounts {
		if err := pool.add(account); err != nil {
			state.Close()
			upstream.Close()
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { state.Close() })
	store := &AffinityStore{store: state}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &server{
		ctx:          context.Background(),
		pool:         pool,
		catalog:      newModelCatalog(),
		affinity:     store,
		stats:        newStatsWithPrices(testPriceSnapshot(t)),
		upstream:     upstream.URL,
		client:       newProxyClient(),
		log:          log,
		retryBackoff: func(int) time.Duration { return 0 },
	}
	return server, store, upstream.Close
}

func serveHTTPResponse(t *testing.T, server *server, session, turnState, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	if session != "" {
		request.Header.Set("session-id", session)
	}
	if turnState != "" {
		request.Header.Set("x-codex-turn-state", turnState)
	}
	response := httptest.NewRecorder()
	server.responses(response, request)
	return response
}

func writeResponseCreated(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":%q}}\n\n", id)
}

func requireNoFailedAccounts(t *testing.T, server *server, accounts ...*Account) {
	t.Helper()
	if events := server.stats.snapshot().Events; len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
	for _, account := range accounts {
		_, _, cooldown, _ := account.health()
		if !cooldown.IsZero() {
			t.Fatalf("account %s cooldown = %s, want none", account.id(), cooldown)
		}
	}
}

func useNoResetCreditsAPI(t *testing.T) func() int {
	t.Helper()
	api := useResetAPI(t, 0, 100, http.StatusOK)
	return func() int { return len(api.calls()) }
}

type resetAPI struct {
	mu            sync.Mutex
	requests      []string
	expiresAfter  time.Duration
	usedPercent   float64
	consumeStatus int
}

func useResetAPI(t *testing.T, expiresAfter time.Duration, usedPercent float64, consumeStatus int) *resetAPI {
	t.Helper()
	api := &resetAPI{
		expiresAfter:  expiresAfter,
		usedPercent:   usedPercent,
		consumeStatus: consumeStatus,
	}
	server := httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	oldBaseURL := accountAPIBaseURL
	accountAPIBaseURL = server.URL
	t.Cleanup(func() {
		accountAPIBaseURL = oldBaseURL
		server.Close()
	})
	return api
}

func (a *resetAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	request := r.Method + " " + r.URL.Path
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.mu.Unlock()
	switch request {
	case "GET /rate-limit-reset-credits":
		if a.expiresAfter <= 0 {
			json.NewEncoder(w).Encode(map[string]any{"available_count": 0, "credits": []any{}})
			return
		}
		expiresAt := time.Now().UTC().Add(a.expiresAfter)
		json.NewEncoder(w).Encode(map[string]any{
			"available_count": 1,
			"credits": []map[string]any{{
				"id":         "credit-a",
				"reset_type": "codex_rate_limits",
				"status":     "available",
				"expires_at": expiresAt.Format(time.RFC3339),
			}},
		})
	case "POST /rate-limit-reset-credits/consume":
		if a.consumeStatus != http.StatusOK {
			http.Error(w, "reset failed", a.consumeStatus)
			return
		}
		json.NewEncoder(w).Encode(consumeResetCreditResponse{Code: "reset", WindowsReset: 2})
	case "GET /usage":
		json.NewEncoder(w).Encode(map[string]any{
			"rate_limit": map[string]any{
				"primary_window":   map[string]any{"used_percent": a.usedPercent, "limit_window_seconds": 300},
				"secondary_window": map[string]any{"used_percent": a.usedPercent, "limit_window_seconds": 604800},
			},
			"rate_limit_reset_credits": map[string]any{"available_count": 0},
		})
	default:
		http.NotFound(w, r)
	}
}

func (a *resetAPI) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

func (a *resetAPI) consumeCalls() int {
	calls := 0
	for _, request := range a.calls() {
		if request == "POST /rate-limit-reset-credits/consume" {
			calls++
		}
	}
	return calls
}
