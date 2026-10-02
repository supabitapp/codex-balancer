package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func observeTestServer(t *testing.T, srv *server) (*testLogBuffer, *tracetest.InMemoryExporter, *sdktrace.TracerProvider) {
	t.Helper()
	logs := &testLogBuffer{}
	srv.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	srv.tracer = provider.Tracer(telemetryScope)
	lifetime, cancel := context.WithCancel(srv.ctx)
	srv.ctx = lifetime
	t.Cleanup(func() {
		if err := srv.stopRefreshes(cancel); err != nil {
			t.Error(err)
		}
		ctx, stop := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
		defer stop()
		if err := provider.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return logs, exporter, provider
}

func observationRecords(t *testing.T, logs *testLogBuffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "http responses" {
			records = append(records, record)
		}
	}
	return records
}

func spanAttribute(span tracetest.SpanStub, key string) attribute.Value {
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			return attr.Value
		}
	}
	return attribute.Value{}
}

func observationPayload(t *testing.T, logs *testLogBuffer, spans tracetest.SpanStubs) string {
	t.Helper()
	var values []string
	for _, span := range spans {
		values = append(values, span.Name, span.Status.Description, span.SpanContext.TraceState().String())
		for _, attr := range span.Attributes {
			values = append(values, fmt.Sprint(attr.Value.AsInterface()))
		}
		for _, event := range span.Events {
			values = append(values, event.Name)
			for _, attr := range event.Attributes {
				values = append(values, fmt.Sprint(attr.Value.AsInterface()))
			}
		}
		for _, link := range span.Links {
			values = append(values, link.SpanContext.TraceState().String())
		}
	}
	records, _ := json.Marshal(observationRecords(t, logs))
	return string(records) + strings.Join(values, "\n")
}

func TestResponsesObservabilityRetainsOwnersAndShowsCacheUsage(t *testing.T) {
	var requests atomic.Int64
	accounts := make(chan string, 3)
	upstream := newHTTPUpstream(t, func(r *http.Request, conn *testResponseStream, _ []byte) {
		if r.Header.Get("Traceparent") != "" || r.Header.Get("Tracestate") != "" || r.Header.Get("Baggage") != "" {
			t.Error("client tracing headers reached inference upstream")
		}
		accounts <- r.Header.Get("Chatgpt-Account-Id")
		cached := 0
		if requests.Add(1) == 2 {
			cached = 75
		}
		sendHTTPEvents(t, conn, httpCreatedEvent, fmt.Sprintf(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":%d},"output_tokens":10,"total_tokens":110}}}`, cached))
	})
	a, b := testAccount("a", 0), testAccount("b", 20)
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
	srv.admission = newAdmissionGate(1)
	logs, exporter, _ := observeTestServer(t, srv)
	headers := http.Header{"Session-Id": {"SESSION_PRIVATE"}, "Traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"}, "Tracestate": {"private=TRACESTATE_PRIVATE"}, "Baggage": {"private=BAGGAGE_PRIVATE"}, "Authorization": {"Bearer CLIENT_KEY_PRIVATE"}}
	if err := srv.pool.store.addAPIKey(storedAPIKey{Name: "client", Secret: "CLIENT_KEY_PRIVATE", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	srv.lookupAPIKey = srv.pool.store.apiKeyName
	const body = `{"model":"m","instructions":"INSTRUCTIONS_PRIVATE","input":"USER_PROMPT_PRIVATE","tools":[{"type":"function","name":"run","description":"TOOL_DESCRIPTION_PRIVATE","parameters":{"type":"object"}}],"prompt_cache_key":"CACHE_KEY_PRIVATE"}`
	ids := []string{}
	for index := 0; index < 3; index++ {
		if index == 1 {
			setTestAccountUsage(a, 90)
			setTestAccountUsage(b, 0)
		}
		if index == 2 {
			a.markSpent()
		}
		resp := postResponse(t, proxy.URL, body, headers)
		if resp.StatusCode != 200 {
			t.Fatal(readHTTPBody(t, resp))
		}
		ids = append(ids, resp.Header.Get(responseRequestIDHeader))
		readHTTPBody(t, resp)
		assertHTTPClean(t, srv)
	}
	if got := []string{<-accounts, <-accounts, <-accounts}; fmt.Sprint(got) != "[a a b]" {
		t.Fatal("logging changed retention/switching:", got)
	}
	if ids[0] == "" || ids[0] == ids[1] || ids[1] == ids[2] {
		t.Fatal("requests lack independent correlation IDs")
	}
	records := observationRecords(t, logs)
	for index, id := range ids {
		stages := map[string]int{}
		for position, record := range records {
			if record["request_id"] != id {
				continue
			}
			stage, _ := record["stage"].(string)
			stages[stage] = position
			if record["trace_id"] == nil || record["span_id"] == nil {
				t.Fatalf("uncorrelated record: %v", record)
			}
			if stage == "response_accepted" && record["accepted_switch"] != (index == 2) {
				t.Fatalf("switch acceptance=%v", record)
			}
			if stage == "usage" && index == 1 && record["cached_input_percent"] != float64(75) {
				t.Fatalf("cache usage=%v", record)
			}
			if stage == "finished" && (record["relay_cleaned"] != true || record["admission_released"] != true || record["inference_replayed"] != false) {
				t.Fatalf("cleanup/replay=%v", record)
			}
		}
		for _, stage := range []string{"started", "admission", "body_read", "normalized", "route_selected", "http_response_headers", "upstream_write_started", "upstream_write_finished", "response_accepted", "usage", "terminal", "cleanup", "finished"} {
			if _, ok := stages[stage]; !ok {
				t.Errorf("request %s missing %s", id, stage)
			}
		}
		if stages["response_accepted"] <= stages["upstream_write_finished"] || stages["finished"] <= stages["cleanup"] {
			t.Fatal("incorrect lifecycle ordering")
		}
	}
	spans := exporter.GetSpans()
	roots := []tracetest.SpanStub{}
	for _, span := range spans {
		if span.Name != "POST /v1/responses" {
			continue
		}
		roots = append(roots, span)
		if span.Parent.IsValid() || span.SpanContext.TraceID().String() == "4bf92f3577b34da6a3ce929d0e0e4736" || len(span.Links) != 1 || !span.SpanContext.IsSampled() {
			t.Fatalf("public trace parent was trusted: %+v", span)
		}
	}
	if len(roots) != 3 {
		t.Fatalf("root spans=%d", len(roots))
	}
	if spanAttribute(roots[0], "instructions_hash").AsString() == "" || spanAttribute(roots[0], "instructions_hash") != spanAttribute(roots[1], "instructions_hash") || spanAttribute(roots[0], "tools_hash") != spanAttribute(roots[2], "tools_hash") {
		t.Fatal("cache prefix fingerprints are not stable within the process")
	}
	payload := observationPayload(t, logs, spans)
	for _, secret := range []string{"SESSION_PRIVATE", "TRACESTATE_PRIVATE", "BAGGAGE_PRIVATE", "CLIENT_KEY_PRIVATE", "INSTRUCTIONS_PRIVATE", "USER_PROMPT_PRIVATE", "TOOL_DESCRIPTION_PRIVATE", "CACHE_KEY_PRIVATE", "token-a", "token-b"} {
		if strings.Contains(payload, secret) {
			t.Errorf("telemetry exposed %s", secret)
		}
	}
}

func TestResponsesObservabilityErrorsAndGuards(t *testing.T) {
	for _, scenario := range []string{"unauthorized", "busy", "invalid JSON", "handshake", "in-band"} {
		t.Run(scenario, func(t *testing.T) {
			var upstreamRequests atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamRequests.Add(1)
				if scenario == "handshake" {
					w.Header().Set("Retry-After", "17")
					w.WriteHeader(429)
					io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"ERROR_BODY_PRIVATE token-a"}}`)
					return
				}
				conn, err := acceptResponseTestStream(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				if _, _, err := conn.Read(r.Context()); err != nil {
					return
				}
				sendHTTPEvents(t, conn, httpCreatedEvent, `{"type":"response.output_text.delta","item_id":"ITEM_PRIVATE","delta":"OUTPUT_PRIVATE"}`, `{"type":"response.failed","response":{"status":"failed","error":{"code":"token-\u0061","message":"ERROR_BODY_PRIVATE"}}}`)
				conn.Read(r.Context())
			}))
			defer upstream.Close()
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
			srv.admission = newAdmissionGate(1)
			logs, exporter, _ := observeTestServer(t, srv)
			body := `{"model":"m","stream":true}`
			want := 200
			switch scenario {
			case "unauthorized":
				srv.lookupAPIKey = func(string) (string, bool, error) { return "", false, nil }
				want = 401
			case "busy":
				srv.admission = newAdmissionGate(0)
				want = 503
			case "invalid JSON":
				body = `{`
				want = 400
			case "handshake":
				want = 503
			}
			resp := postResponse(t, proxy.URL, body, nil)
			readHTTPBody(t, resp)
			assertHTTPClean(t, srv)
			if resp.StatusCode != want {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			if (scenario == "unauthorized" || scenario == "busy" || scenario == "invalid JSON") && upstreamRequests.Load() != 0 {
				t.Fatal("guard opened upstream")
			}
			records := observationRecords(t, logs)
			if len(records) < 2 || records[len(records)-1]["stage"] != "finished" {
				t.Fatal("missing terminal log")
			}
			if records[len(records)-1]["outcome"] == "completed" {
				t.Fatal("failure logged as success")
			}
			payload := observationPayload(t, logs, exporter.GetSpans())
			for _, secret := range []string{"token-a", "ERROR_BODY_PRIVATE", "OUTPUT_PRIVATE", "ITEM_PRIVATE"} {
				if strings.Contains(payload, secret) {
					t.Errorf("telemetry exposed %s", secret)
				}
			}
		})
	}
}

func TestResponsesObservabilityExplainsUsageReplayBoundary(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			var requests atomic.Int64
			upstream := newHTTPUpstream(t, func(_ *http.Request, conn *testResponseStream, _ []byte) {
				requests.Add(1)
				if accepted {
					sendHTTPEvents(t, conn, httpCreatedEvent)
				}
				sendHTTPEvents(t, conn, `{"type":"error","error":{"code":"usage_limit_reached","message":"quota"}}`)
			})
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 20)})
			srv.admission = newAdmissionGate(1)
			logs, _, _ := observeTestServer(t, srv)
			resp := postResponse(t, proxy.URL, `{"model":"m","stream":true}`, http.Header{"Session-Id": {"session"}})
			readHTTPBody(t, resp)
			assertHTTPClean(t, srv)
			if requests.Load() != 1 {
				t.Fatal("telemetry introduced inference replay")
			}
			found := false
			for _, record := range observationRecords(t, logs) {
				if record["stage"] != "upstream_rejected" {
					continue
				}
				found = true
				if record["accepted_before_rejection"] != accepted || record["retry_owner"] != "client" || record["inference_replayed"] != false {
					t.Fatalf("incorrect replay boundary diagnostics: %v", record)
				}
			}
			if !found {
				t.Fatal("missing upstream rejection")
			}
		})
	}
}

func TestSharedRefreshTelemetryUsesIndependentLinkedSpan(t *testing.T) {
	srv, account, exchanges, _ := blockedRefreshServer(t)
	_, exporter, _ := observeTestServer(t, srv)
	firstCtx, firstSpan := srv.tracer.Start(context.Background(), "caller-a", trace.WithNewRoot())
	firstCtx, cancelFirst := context.WithCancel(firstCtx)
	defer cancelFirst()
	first := make(chan bool, 1)
	go func() { first <- srv.refreshedContext(firstCtx, account, account.id()) }()
	exchange := <-exchanges
	secondCtx, secondSpan := srv.tracer.Start(context.Background(), "caller-b-wait", trace.WithNewRoot())
	waiter := &refreshWaitContext{Context: secondCtx, joined: make(chan struct{})}
	second := make(chan error, 1)
	go func() { second <- account.refresh(waiter, srv.client, srv.pool.persistAccountState) }()
	waitHTTPSignal(t, waiter.joined)
	account.mu.Lock()
	operation := account.inflight
	account.mu.Unlock()
	cancelFirst()
	if <-first {
		t.Fatal("canceled waiter succeeded")
	}
	firstSpan.End()
	if !operation.observed.span.IsRecording() {
		t.Fatal("request cancellation ended shared operation span")
	}
	close(exchange.release)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	secondSpan.End()
	if err := srv.refreshes.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	var refresh *tracetest.SpanStub
	spans := exporter.GetSpans()
	for i := range spans {
		if spans[i].Name == "codex.credentials.refresh" {
			if refresh != nil {
				t.Fatal("duplicate operation span")
			}
			refresh = &spans[i]
		}
	}
	if refresh == nil || refresh.Parent.IsValid() || len(refresh.Links) != 2 {
		t.Fatalf("shared refresh span=%+v", refresh)
	}
	if refresh.SpanContext.TraceID() == firstSpan.SpanContext().TraceID() || refresh.SpanContext.TraceID() == secondSpan.SpanContext().TraceID() {
		t.Fatal("shared refresh inherited a request trace")
	}
	foundComplete := false
	for _, span := range spans {
		if span.Name == "codex.credentials.complete" {
			foundComplete = true
			if span.Parent.SpanID() != refresh.SpanContext.SpanID() || span.EndTime.After(refresh.EndTime) {
				t.Fatal("completion span not owned by shared operation")
			}
		}
	}
	if !foundComplete {
		t.Fatal("missing publication span")
	}
}
