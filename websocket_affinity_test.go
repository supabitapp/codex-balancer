package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestWebSocketSoftRateLimitReplaysOnAnotherAccount(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-a" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":   "error",
				"status": http.StatusTooManyRequests,
				"error":  map[string]any{"code": "rate_limit_exceeded"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_b"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
	if got := store.lookup(affinityRef{kind: affinityResponse, value: "resp_b"}); got != "account-b" {
		t.Fatalf("response owner = %q, want account-b", got)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketPrecreatedUsageLimitReplaysOnAnotherAccount(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-a" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type": "response.failed",
				"response": map[string]any{
					"id":    "resp_limited",
					"error": map[string]any{"code": "usage_limit_reached"},
				},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_b"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketPreviousResponseSwitchesBeforeSend(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_next"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 90)
	b := testAccount("account-b", 0)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.bind(affinityRef{kind: affinityResponse, value: "resp_a"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": "resp_a",
		"input":                []any{},
	})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
}

func TestWebSocketFollowUpsKeepStableSessionStats(t *testing.T) {
	calls := 0
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		calls++
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": fmt.Sprintf("resp_%d", calls)},
		})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	account := testAccount("account", 0)
	server, _, closeUnusedUpstream := newAffinityHTTPServer(t, []*Account{account}, func(http.ResponseWriter, *http.Request) {})
	closeUnusedUpstream()
	server.upstream = upstream.URL
	proxy := httptest.NewServer(server.routes())
	defer proxy.Close()

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "model": "gpt-5.6-sol", "reasoning": map[string]any{"effort": "medium"}, "input": []any{}})
	readWebSocketEvent(t, conn)
	readWebSocketEvent(t, conn)
	writeWebSocketEvent(t, conn, map[string]any{
		"type":                 "response.create",
		"model":                "gpt-5.6-terra",
		"reasoning":            map[string]any{"effort": "xhigh"},
		"previous_response_id": "resp_1",
		"input":                []any{},
	})
	readWebSocketEvent(t, conn)
	readWebSocketEvent(t, conn)

	snapshot := server.stats.snapshot()
	if snapshot.Turns != 2 {
		t.Fatalf("turns = %d, want 2", snapshot.Turns)
	}
	if len(snapshot.Threads) != 1 {
		t.Fatalf("threads = %+v, want one session", snapshot.Threads)
	}
	thread := snapshot.Threads[0]
	if thread.Key != "session" || thread.Model != "gpt-5.6-terra" || thread.Effort != "xhigh" || thread.Turns != 2 || thread.Via != transportWebSocket {
		t.Fatalf("thread = %+v, want session with two WebSocket turns", thread)
	}
}

func TestWebSocketCompletedResponseTracksAPIEstimate(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp"},
		})
		writeWebSocketEvent(t, conn, map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"model": "gpt-5.6-sol",
				"usage": map[string]any{
					"input_tokens":  1_000,
					"output_tokens": 100,
				},
			},
		})
	})
	defer upstream.Close()
	account := testAccount("account", 0)
	server, _, closeUnusedUpstream := newAffinityHTTPServer(t, []*Account{account}, func(http.ResponseWriter, *http.Request) {})
	closeUnusedUpstream()
	server.upstream = upstream.URL
	proxy := httptest.NewServer(server.routes())
	defer proxy.Close()

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	metadata := turnMetadata{RequestKind: "compaction", ThreadID: "codex-thread", TurnID: "codex-turn"}
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "model": "gpt-5.6-sol", "service_tier": serviceTierFast, "client_metadata": map[string]string{codexTurnMetadataKey: encodeTurnMetadata(metadata)}, "input": []any{}})
	readWebSocketEvent(t, conn)
	readWebSocketEvent(t, conn)

	snapshot := server.stats.snapshot()
	if snapshot.APICostNanoDollars != 16_000_000 || snapshot.UnpricedResponses != 0 {
		t.Fatalf("API estimate = %d with %d unpriced, want 16000000 with none", snapshot.APICostNanoDollars, snapshot.UnpricedResponses)
	}
	wantUsage := responseUsage{InputTokens: 1_000, OutputTokens: 100}
	if len(snapshot.Threads) != 1 || snapshot.Threads[0].Usage != wantUsage {
		t.Fatalf("thread usage = %+v, want %+v", snapshot.Threads, wantUsage)
	}
	if snapshot.Threads[0].Metadata != metadata || snapshot.Threads[0].Compactions != 1 || snapshot.Threads[0].Latency <= 0 || snapshot.Threads[0].TTFB <= 0 {
		t.Fatalf("compaction thread = %+v", snapshot.Threads[0])
	}
}

func TestWebSocketHardRateLimitDoesNotReplay(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "error",
			"status": http.StatusTooManyRequests,
			"error":  map[string]any{"code": "rate_limit_exceeded"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinityTurnState, value: "turn"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"X-Codex-Turn-State": {"turn"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "error" || event.Status != http.StatusTooManyRequests {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketSoftServerFailureReplaysOnAnotherAccount(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-a" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":   "error",
				"status": http.StatusBadGateway,
				"error":  map[string]any{"code": "upstream_error"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_b"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketUnsupportedModelReplaysOnAnotherAccount(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-a" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":   "error",
				"status": http.StatusBadRequest,
				"error": map[string]any{
					"code":    "invalid_request_error",
					"message": "The 'gpt-route' model is not supported when using Codex with a ChatGPT account.",
				},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_b"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "model": "gpt-route", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketModelCatalogRoutesBeforeSendingTurn(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_b"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	server, store, closeUnusedUpstream := newAffinityHTTPServer(t, []*Account{a, b}, func(http.ResponseWriter, *http.Request) {})
	closeUnusedUpstream()
	server.upstream = upstream.URL
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
	proxy := httptest.NewServer(server.routes())
	defer proxy.Close()

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "model": "gpt-5.6-sol", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-b]" {
		t.Fatalf("request accounts = %v, want account-b", upstream.RequestAccounts())
	}
}

func TestWebSocketUnsupportedModelRetriesOnlyOnce(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, _ websocketEnvelope) {
		if account == "account-c" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":     "response.created",
				"response": map[string]any{"id": "unexpected"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "error",
			"status": http.StatusBadRequest,
			"error": map[string]any{
				"code":    "invalid_request_error",
				"message": "The 'gpt-route' model is not supported when using Codex with a ChatGPT account.",
			},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	c := testAccount("account-c", 40)
	proxy, _, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b, c})
	defer closeProxy()

	conn := dialAffinityWebSocket(t, proxy.URL, nil)
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "model": "gpt-route", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "error" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-b]" {
		t.Fatalf("request accounts = %v, want one replacement", upstream.RequestAccounts())
	}
}

func TestWebSocketSoftDisconnectReplaysOnAnotherAccount(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-a" {
			conn.CloseNow()
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_b"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketHardServerFailureDoesNotReplay(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "error",
			"status": http.StatusBadGateway,
			"error":  map[string]any{"code": "upstream_error"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinityTurnState, value: "turn"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"X-Codex-Turn-State": {"turn"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "error" || event.Status != http.StatusBadGateway {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketFailureAfterCreatedDoesNotReplay(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_a"},
		})
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "error",
			"status": http.StatusBadGateway,
			"error":  map[string]any{"code": "upstream_error"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	created := readWebSocketEvent(t, conn)
	failed := readWebSocketEvent(t, conn)
	if created.Type != "response.created" || failed.Type != "error" {
		t.Fatalf("events = %+v, %+v", created, failed)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketFailureAfterResponseEventDoesNotReplay(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-b" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":     "response.created",
				"response": map[string]any{"id": "resp_b"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":  "response.output_text.delta",
			"delta": "visible",
		})
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "error",
			"status": http.StatusBadGateway,
			"error":  map[string]any{"code": "upstream_error"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	visible := readWebSocketEvent(t, conn)
	failed := readWebSocketEvent(t, conn)
	if visible.Type != "response.output_text.delta" || failed.Type != "error" {
		t.Fatalf("events = %+v, %+v", visible, failed)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketIDBearingAuthFailureDoesNotReplay(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-b" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":     "response.created",
				"response": map[string]any{"id": "resp_b"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "response.failed",
			"status": http.StatusUnauthorized,
			"response": map[string]any{
				"id":    "resp_a",
				"error": map[string]any{"code": "invalid_api_key"},
			},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.failed" || event.Response.ID != "resp_a" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	recovered := readWebSocketEvent(t, conn)
	if recovered.Type != "response.created" || recovered.Response.ID != "resp_b" {
		t.Fatalf("recovered event = %+v", recovered)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketReplaySafetyRequiresNoResponseIdentity(t *testing.T) {
	nested := websocketEnvelope{}
	nested.Response.ID = "nested"
	tests := []struct {
		name  string
		event websocketEnvelope
		want  bool
	}{
		{name: "anonymous", want: true},
		{name: "event id", event: websocketEnvelope{ID: "event"}},
		{name: "response id", event: websocketEnvelope{ResponseID: "response"}},
		{name: "nested response id", event: nested},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := websocketReplaySafe(test.event); got != test.want {
				t.Fatalf("replay safe = %t, want %t", got, test.want)
			}
		})
	}
}

func TestWebSocketRateLimitClassification(t *testing.T) {
	topLevel := websocketEnvelope{}
	topLevel.Error.Code = "usage_limit_reached"
	nested := websocketEnvelope{}
	nested.Response.Error.Code = "rate_limit_exceeded"
	tests := []struct {
		name  string
		event websocketEnvelope
		want  bool
	}{
		{name: "status", event: websocketEnvelope{Status: http.StatusTooManyRequests}, want: true},
		{name: "status code", event: websocketEnvelope{StatusCode: http.StatusTooManyRequests}, want: true},
		{name: "top-level usage code", event: topLevel, want: true},
		{name: "nested rate code", event: nested, want: true},
		{name: "server failure", event: websocketEnvelope{Status: http.StatusBadGateway}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := websocketRateLimited(test.event); got != test.want {
				t.Fatalf("rate limited = %t, want %t", got, test.want)
			}
		})
	}
}

func TestResponsesWebSocketHeadersSanitizeTransport(t *testing.T) {
	account := testAccount("account-a", 0)
	inbound := http.Header{}
	inbound.Set("Accept", "text/event-stream")
	inbound.Set("Accept-Encoding", "gzip")
	inbound.Set("Authorization", "Bearer inbound")
	inbound.Set("Content-Type", "application/json")
	inbound.Set("Cookie", "secret=value")
	inbound.Set("OpenAI-Beta", "responses=experimental, custom=1")
	inbound.Set("Session-Id", "session")
	inbound.Set("X-Custom", "preserved")
	headers := responsesWebSocketHeaders(inbound, account)
	for _, name := range []string{"Accept", "Accept-Encoding", "Content-Type", "Cookie"} {
		if value := headers.Get(name); value != "" {
			t.Fatalf("%s = %q, want empty", name, value)
		}
	}
	if got := headers.Get("Authorization"); got != "Bearer token-account-a" {
		t.Fatalf("authorization = %q", got)
	}
	if got := headers.Get("chatgpt-account-id"); got != "account-a" {
		t.Fatalf("account = %q", got)
	}
	if got := headers.Get("Session-Id"); got != "session" {
		t.Fatalf("session = %q", got)
	}
	if got := headers.Get("X-Custom"); got != "preserved" {
		t.Fatalf("custom = %q", got)
	}
	if got := headers.Get("OpenAI-Beta"); got != "custom=1, "+responsesWebSocketBeta {
		t.Fatalf("beta = %q", got)
	}
}

func TestWebSocketOpaqueFileCanFailOver(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-a" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":   "error",
				"status": http.StatusTooManyRequests,
				"error":  map[string]any{"code": "rate_limit_exceeded"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_b"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{
		"type":  "response.create",
		"input": []any{map[string]any{"type": "input_file", "file_id": "file_unknown"}},
	})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
	if got := store.lookup(affinityRef{kind: affinityFile, value: "file_unknown"}); got != "" {
		t.Fatalf("file owner = %q, want empty", got)
	}
	if got := store.lookup(affinityRef{kind: affinitySession, value: "session"}); got != "account-b" {
		t.Fatalf("session owner = %q, want account-b", got)
	}
}

func TestWebSocketRevalidatesConversationEachTurn(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_a"},
		})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, _, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	readWebSocketEvent(t, conn)
	readWebSocketEvent(t, conn)
	writeWebSocketEvent(t, conn, map[string]any{
		"type":         "response.create",
		"conversation": "conversation",
		"input":        []any{},
	})
	event := readWebSocketEvent(t, conn)
	if event.Type != "error" || event.Status != http.StatusServiceUnavailable || websocketErrorCode(event) != "affinity_error" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketStatusCodeRateLimitReplaysOnAnotherAccount(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-a" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":        "error",
				"status_code": http.StatusTooManyRequests,
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_b"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketUnauthorizedRefreshesOnceThenFailsOver(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		if account == "account-a" {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":   "error",
				"status": http.StatusUnauthorized,
				"error":  map[string]any{"code": "invalid_token"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_b"},
		})
	})
	defer upstream.Close()
	oauthCalls := useOAuthRefreshServer(t)

	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy := newRefreshableAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer proxy.Close()

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-a account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
	if got := oauthCalls(); got != 1 {
		t.Fatalf("oauth calls = %d, want 1", got)
	}
}

func TestWebSocketUnauthorizedRefreshBudgetIsPerAccount(t *testing.T) {
	var attemptsMu sync.Mutex
	attempts := map[string]int{}
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		attemptsMu.Lock()
		attempts[account]++
		attempt := attempts[account]
		attemptsMu.Unlock()
		if account == "account-b" && attempt == 2 {
			writeWebSocketEvent(t, conn, map[string]any{
				"type":     "response.created",
				"response": map[string]any{"id": "resp_b"},
			})
			return
		}
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "error",
			"status": http.StatusUnauthorized,
			"error":  map[string]any{"code": "invalid_token"},
		})
	})
	defer upstream.Close()
	oauthCalls := useOAuthRefreshServer(t)
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy := newRefreshableAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer proxy.Close()

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" || event.Response.ID != "resp_b" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-a account-b account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
	if got := oauthCalls(); got != 2 {
		t.Fatalf("oauth calls = %d, want 2", got)
	}
}

func TestWebSocketHardOwnerUnauthorizedNeverFailsOver(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "error",
			"status": http.StatusUnauthorized,
			"error":  map[string]any{"code": "invalid_token"},
		})
	})
	defer upstream.Close()
	oauthCalls := useOAuthRefreshServer(t)
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	server, store, closeUnusedUpstream := newAffinityHTTPServer(t, []*Account{a, b}, func(http.ResponseWriter, *http.Request) {})
	closeUnusedUpstream()
	server.upstream = upstream.URL
	if err := store.bind(affinityRef{kind: affinityTurnState, value: "turn"}, "account-a"); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.routes())
	defer proxy.Close()

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"X-Codex-Turn-State": {"turn"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "error" || event.Status != http.StatusUnauthorized {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-a account-a]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
	if got := oauthCalls(); got != 1 {
		t.Fatalf("oauth calls = %d, want 1", got)
	}
}

func TestWebSocketResponseTurnStateBindsOwner(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":    "response.created",
			"headers": map[string]any{"x-codex-turn-state": "turn"},
			"response": map[string]any{
				"id": "resp_a",
			},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" {
		t.Fatalf("event = %+v", event)
	}
	if got := store.lookup(affinityRef{kind: affinityTurnState, value: "turn"}); got != "account-a" {
		t.Fatalf("turn owner = %q, want account-a", got)
	}
}

func TestWebSocketUnknownHardAffinityFailsBeforeUpstream(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{
			"type":   "error",
			"status": http.StatusTooManyRequests,
			"error":  map[string]any{"code": "rate_limit_exceeded"},
		})
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(
		ctx,
		"ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses",
		&websocket.DialOptions{HTTPHeader: http.Header{"X-Codex-Turn-State": {"turn"}}},
	)
	if conn != nil {
		conn.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial error = %v, response = %+v", err, response)
	}
	if got := store.lookup(affinityRef{kind: affinityTurnState, value: "turn"}); got != "" {
		t.Fatalf("turn owner = %q, want none", got)
	}
	if len(upstream.RequestAccounts()) != 0 {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketMissingPreviousResponseFailsBeforeSend(t *testing.T) {
	upstream := newAffinityWebSocketUpstream(t, func(account string, conn *websocket.Conn, request websocketEnvelope) {
		t.Error("unexpected upstream request")
	})
	defer upstream.Close()
	a := testAccount("account-a", 0)
	proxy, _, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a})
	defer closeProxy()

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": "missing",
		"input":                []any{},
	})
	event := readWebSocketEvent(t, conn)
	if event.Type != "error" || event.Status != http.StatusServiceUnavailable {
		t.Fatalf("event = %+v", event)
	}
	if len(upstream.RequestAccounts()) != 0 {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketSoftHandshakeRateLimitUsesAnotherAccount(t *testing.T) {
	upstream := newAffinityWebSocketHandshakeUpstream(t, "account-a", http.StatusTooManyRequests)
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinitySession, value: "session"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	conn := dialAffinityWebSocket(t, proxy.URL, http.Header{"Session-Id": {"session"}})
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" {
		t.Fatalf("event = %+v", event)
	}
	if fmt.Sprint(upstream.ConnectionAccounts()) != "[account-a account-b]" {
		t.Fatalf("connection accounts = %v", upstream.ConnectionAccounts())
	}
	if fmt.Sprint(upstream.RequestAccounts()) != "[account-b]" {
		t.Fatalf("request accounts = %v", upstream.RequestAccounts())
	}
}

func TestWebSocketHardHandshakeRateLimitFailsClosed(t *testing.T) {
	upstream := newAffinityWebSocketHandshakeUpstream(t, "account-a", http.StatusTooManyRequests)
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, store, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()
	if err := store.bind(affinityRef{kind: affinityTurnState, value: "turn"}, "account-a"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Codex-Turn-State": {"turn"}},
	})
	if err == nil {
		t.Fatal("expected websocket dial failure")
	}
	if response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("response = %v, error = %v", response, err)
	}
	if fmt.Sprint(upstream.ConnectionAccounts()) != "[account-a]" {
		t.Fatalf("connection accounts = %v", upstream.ConnectionAccounts())
	}
}

func TestWebSocketHandshakeServerFailureRetriesThenUsesAnotherAccount(t *testing.T) {
	upstream := newAffinityWebSocketHandshakeUpstream(t, "account-a", http.StatusServiceUnavailable)
	defer upstream.Close()
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	proxy, _, closeProxy := newAffinityProxyWebSocketServer(t, upstream.URL, []*Account{a, b})
	defer closeProxy()

	conn := dialAffinityWebSocket(t, proxy.URL, nil)
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "input": []any{}})
	event := readWebSocketEvent(t, conn)
	if event.Type != "response.created" {
		t.Fatalf("event = %+v", event)
	}
	if got := fmt.Sprint(upstream.ConnectionAccounts()); got != "[account-a account-a account-a account-a account-b]" {
		t.Fatalf("connection accounts = %s", got)
	}
	if got := fmt.Sprint(upstream.RequestAccounts()); got != "[account-b]" {
		t.Fatalf("request accounts = %s", got)
	}
}

type affinityWebSocketUpstream struct {
	*httptest.Server
	mu          sync.Mutex
	connections []string
	requests    []string
}

func newAffinityWebSocketUpstream(
	t *testing.T,
	respond func(string, *websocket.Conn, websocketEnvelope),
) *affinityWebSocketUpstream {
	t.Helper()
	upstream := &affinityWebSocketUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		account := r.Header.Get("chatgpt-account-id")
		upstream.mu.Lock()
		upstream.connections = append(upstream.connections, account)
		upstream.mu.Unlock()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request websocketEnvelope
			if err := json.Unmarshal(data, &request); err != nil {
				t.Error(err)
				return
			}
			if request.Type != "response.create" {
				continue
			}
			upstream.mu.Lock()
			upstream.requests = append(upstream.requests, account)
			upstream.mu.Unlock()
			respond(account, conn, request)
		}
	}))
	return upstream
}

func newAffinityWebSocketHandshakeUpstream(t *testing.T, rejected string, status int) *affinityWebSocketUpstream {
	t.Helper()
	upstream := &affinityWebSocketUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("chatgpt-account-id")
		upstream.mu.Lock()
		upstream.connections = append(upstream.connections, account)
		upstream.mu.Unlock()
		if account == rejected {
			w.WriteHeader(status)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request websocketEnvelope
			if err := json.Unmarshal(data, &request); err != nil {
				t.Error(err)
				return
			}
			if request.Type != "response.create" {
				continue
			}
			upstream.mu.Lock()
			upstream.requests = append(upstream.requests, account)
			upstream.mu.Unlock()
			writeWebSocketEvent(t, conn, map[string]any{
				"type":     "response.created",
				"response": map[string]any{"id": "response"},
			})
		}
	}))
	return upstream
}

func (u *affinityWebSocketUpstream) RequestAccounts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.requests...)
}

func (u *affinityWebSocketUpstream) ConnectionAccounts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.connections...)
}

func newAffinityProxyWebSocketServer(
	t *testing.T,
	upstream string,
	accounts []*Account,
) (*httptest.Server, *AffinityStore, func()) {
	t.Helper()
	server, store, closeUpstream := newAffinityHTTPServer(t, accounts, func(http.ResponseWriter, *http.Request) {})
	closeUpstream()
	server.upstream = upstream
	proxy := httptest.NewServer(server.routes())
	return proxy, store, proxy.Close
}

func newRefreshableAffinityProxyWebSocketServer(
	t *testing.T,
	upstream string,
	accounts []*Account,
) *httptest.Server {
	t.Helper()
	server, _, closeUnusedUpstream := newAffinityHTTPServer(t, accounts, func(http.ResponseWriter, *http.Request) {})
	closeUnusedUpstream()
	server.upstream = upstream
	return httptest.NewServer(server.routes())
}

func useOAuthRefreshServer(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"refreshed-token","refresh_token":"refreshed-refresh"}`)
	}))
	previous := oauthEndpoint
	oauthEndpoint = oauth.URL
	t.Cleanup(func() {
		oauthEndpoint = previous
		oauth.Close()
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

func dialAffinityWebSocket(t *testing.T, proxyURL string, headers http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxyURL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		if response != nil {
			t.Fatalf("dial: %v, status = %d", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	return conn
}

func writeWebSocketEvent(t *testing.T, conn *websocket.Conn, value any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, value); err != nil {
		t.Fatal(err)
	}
}

func readWebSocketEvent(t *testing.T, conn *websocket.Conn) websocketEnvelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var event websocketEnvelope
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatal(err)
	}
	return event
}
