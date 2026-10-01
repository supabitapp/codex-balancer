package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const responseRequestIDHeader = "X-Codex-Balancer-Request-Id"

type responseObservationKey struct{}
type responseLogKeys struct {
	once sync.Once
	key  [32]byte
}

// State is owned by the HTTP handler/relay goroutine, never by the upstream
// reader or the independent shared-refresh worker. Nothing here selects routes.
type responseObservation struct {
	server         *server
	ctx            context.Context
	root           trace.Span
	id             string
	started        time.Time
	account        string
	status         int
	outcome        string
	writeAttempted bool
	writeSucceeded bool
	writeAt        time.Time
	firstDelta     bool
	accepted       bool
	sseCommitted   bool
	admitted       bool
	cleaned        bool
	events         int
	forwarded      int
	upstreamBytes  int64
	secrets        []string
	redactor       *strings.Replacer
}

func observation(ctx context.Context) *responseObservation {
	if ctx == nil {
		return nil
	}
	value, _ := ctx.Value(responseObservationKey{}).(*responseObservation)
	return value
}

func (s *server) observedResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public client trace context is a link, not authority over our trace's
		// identity/sampling, and never routing affinity. Ignore baggage/tracestate.
		opts := []trace.SpanStartOption{trace.WithNewRoot(), trace.WithSpanKind(trace.SpanKindServer)}
		incoming := trace.SpanContextFromContext((propagation.TraceContext{}).Extract(context.Background(), propagation.HeaderCarrier(r.Header)))
		if incoming.IsValid() {
			incoming = trace.NewSpanContext(trace.SpanContextConfig{TraceID: incoming.TraceID(), SpanID: incoming.SpanID(), TraceFlags: incoming.TraceFlags(), Remote: true})
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: incoming}))
		}
		method, path := responseLogRoute(r)
		ctx, span := s.responseTracer().Start(r.Context(), method+" "+path, opts...)
		t := &responseObservation{server: s, ctx: ctx, root: span, id: rand.Text(), started: time.Now(), outcome: "unfinished"}
		t.secrets = append(t.secrets, r.Header.Get("Authorization"), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		t.rebuildRedactor()
		ctx = context.WithValue(ctx, responseObservationKey{}, t)
		t.ctx = ctx
		w.Header().Set(responseRequestIDHeader, t.id)
		route := websocketRouteFrom(r.Header)
		kind := "anonymous"
		if route.session != "" {
			kind = "session"
		}
		if route.thread != "" {
			kind = "thread"
		}
		attrs := []attribute.KeyValue{
			attribute.String("request_id", t.id), attribute.String("http.request.method", method), attribute.String("http.route", path),
			attribute.String("affinity", kind), attribute.String("session_hash", t.fingerprint("session", []byte(route.session))),
			attribute.String("thread_hash", t.fingerprint("thread", []byte(route.thread))), attribute.Int64("content_length", r.ContentLength),
			attribute.Bool("turn_state_header_present", strings.TrimSpace(r.Header.Get(codexTurnStateKey)) != ""),
		}
		t.root.SetAttributes(t.safe(attrs)...)
		t.emit(ctx, slog.LevelInfo, "started", false, attrs...)
		defer func() {
			if t.outcome == "unfinished" {
				t.outcome = "aborted"
				t.root.SetStatus(codes.Error, "request_unfinished")
			}
			t.root.SetAttributes(attribute.Int("http.response.status_code", t.status), attribute.String("outcome", t.outcome), attribute.Bool("write_attempted", t.writeAttempted), attribute.Bool("write_succeeded", t.writeSucceeded), attribute.Bool("response_created", t.accepted), attribute.Bool("inference_replayed", false), attribute.Int("events_received", t.events))
			t.emit(ctx, slog.LevelInfo, "finished", false,
				attribute.Int("http_status", t.status), attribute.String("outcome", t.outcome), attribute.String("account", t.account),
				attribute.Int64("elapsed_ms", time.Since(t.started).Milliseconds()), attribute.Bool("write_attempted", t.writeAttempted),
				attribute.Bool("write_succeeded", t.writeSucceeded), attribute.Bool("accepted", t.accepted), attribute.Bool("sse_committed", t.sseCommitted),
				attribute.Bool("relay_cleaned", t.cleaned), attribute.Bool("admission_released", t.admitted),
				attribute.Int("events_received", t.events), attribute.Int("events_forwarded", t.forwarded), attribute.Int64("upstream_bytes", t.upstreamBytes),
				attribute.Bool("client_canceled", r.Context().Err() != nil), attribute.Bool("server_canceled", s.ctx != nil && s.ctx.Err() != nil),
				attribute.Bool("inference_replayed", false),
			)
			span.End()
		}()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (t *responseObservation) rememberPool() {
	if t == nil || t.server.pool == nil {
		return
	}
	for _, account := range t.server.pool.all() {
		state := account.persisted()
		t.secrets = append(t.secrets, state.AccessToken, state.RefreshToken, state.IDToken)
	}
	t.rebuildRedactor()
}

func (t *responseObservation) remember(state accountState) {
	if t == nil {
		return
	}
	t.secrets = append(t.secrets, state.AccessToken, state.RefreshToken, state.IDToken)
	t.rebuildRedactor()
}
func (t *responseObservation) rebuildRedactor() {
	pairs := []string{}
	seen := map[string]bool{}
	for _, secret := range t.secrets {
		if secret != "" && !seen[secret] {
			pairs = append(pairs, secret, "[redacted]")
			seen[secret] = true
		}
	}
	t.redactor = strings.NewReplacer(pairs...)
}

func (t *responseObservation) fingerprint(domain string, data []byte) string {
	return t.server.logFingerprint(domain, data)
}

func (s *server) logFingerprint(domain string, data []byte) string {
	if len(data) == 0 {
		return "absent"
	}
	keys := &s.responseLogKeys
	keys.once.Do(func() { _, _ = rand.Read(keys.key[:]) })
	hash := hmac.New(sha256.New, keys.key[:])
	hash.Write([]byte(domain))
	hash.Write([]byte{0})
	hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil)[:16])
}

// Instrumentation accepts only explicit scalar metadata, not payloads, error
// messages, arbitrary headers or JSON maps. Decode strings before passing them.
func (t *responseObservation) safe(attrs []attribute.KeyValue) []attribute.KeyValue {
	out := append([]attribute.KeyValue(nil), attrs...)
	for i, attr := range out {
		if attr.Value.Type() == attribute.STRING {
			value := t.redactor.Replace(attr.Value.AsString())
			if len(value) > 256 {
				value = strings.ToValidUTF8(value[:256], "")
			}
			out[i] = attribute.String(string(attr.Key), value)
		}
	}
	return out
}

func (t *responseObservation) emit(ctx context.Context, level slog.Level, stage string, spanEvent bool, attrs ...attribute.KeyValue) {
	if t == nil {
		return
	}
	attrs = t.safe(attrs)
	if spanEvent {
		trace.SpanFromContext(ctx).AddEvent(stage, trace.WithAttributes(attrs...))
	}
	if t.server.log == nil || !t.server.log.Enabled(ctx, level) {
		return
	}
	logs := []slog.Attr{slog.String("stage", stage), slog.String("request_id", t.id)}
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		logs = append(logs, slog.String("trace_id", span.TraceID().String()), slog.String("span_id", span.SpanID().String()), slog.Bool("trace_sampled", span.IsSampled()))
	}
	for _, attr := range attrs {
		if attr.Key == "request_id" {
			continue
		} // the correlation field is already attached
		// AsInterface is the SDK's scalar-to-logger bridge, not a payload decode.
		logs = append(logs, slog.Any(string(attr.Key), attr.Value.AsInterface()))
	}
	t.server.log.LogAttrs(ctx, level, "http responses", logs...)
}

func (t *responseObservation) start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if t == nil {
		return ctx, trace.SpanFromContext(context.Background())
	}
	return t.server.responseTracer().Start(ctx, name, trace.WithAttributes(t.safe(attrs)...))
}

func (t *responseObservation) event(ctx context.Context, stage string, attrs ...attribute.KeyValue) {
	t.emit(ctx, slog.LevelDebug, stage, true, attrs...)
}

func (t *responseObservation) reject(ctx context.Context, status int, code string) {
	if t == nil {
		return
	}
	t.status, t.outcome = status, "rejected"
	t.root.SetStatus(codes.Error, "request_rejected")
	t.emit(ctx, slog.LevelWarn, "rejected", true, attribute.Int("http_status", status), attribute.String("error_code", code), attribute.Bool("write_attempted", t.writeAttempted))
}

func (t *responseObservation) failure(ctx context.Context, status int, code string, committed bool) {
	if t == nil {
		return
	}
	t.status, t.outcome, t.sseCommitted = status, "failed", committed
	if committed {
		t.status = 200
	}
	attrs := t.safe([]attribute.KeyValue{attribute.String("error_code", code), attribute.Int("failure_status", status)})
	t.root.SetAttributes(attrs...)
	t.root.SetStatus(codes.Error, "response_failed")
	t.emit(ctx, slog.LevelWarn, "failure", true, append(attrs,
		attribute.Bool("sse_committed", committed), attribute.Bool("write_attempted", t.writeAttempted), attribute.Bool("accepted", t.accepted),
		attribute.String("retry_owner", "client"), attribute.Bool("inference_replayed", false))...)
}

func (t *responseObservation) terminal(ctx context.Context, kind string, fields responseFields) {
	if t == nil {
		return
	}
	t.outcome = strings.TrimPrefix(kind, "response.")
	if kind == "error" || kind == "response.failed" {
		t.root.SetStatus(codes.Error, "upstream_failed")
	}
	reason := ""
	if response, err := responseObject(fields["response"]); err == nil {
		if details, err := responseObject(response["incomplete_details"]); err == nil {
			reason, _ = responseString(details["reason"])
		}
	}
	t.emit(ctx, slog.LevelInfo, "terminal", true, attribute.String("event_type", kind), attribute.String("incomplete_reason", reason), attribute.Bool("accepted", t.accepted), attribute.Bool("sse_committed", t.sseCommitted))
}

func (t *responseObservation) delivery(ctx context.Context, err error, committed bool) {
	if t == nil || err == nil || errors.Is(err, errResponseFinished) {
		return
	}
	t.outcome = "downstream_write_failed"
	t.sseCommitted = committed
	t.root.SetStatus(codes.Error, "downstream_write_failed")
	t.emit(ctx, slog.LevelWarn, "delivery_failed", true, attribute.String("error_type", telemetryErrorClass(err)), attribute.Bool("sse_committed", committed))
}

func (t *responseObservation) normalized(data []byte, event websocketEnvelope, stream bool, mode fastMode, requestedTier string) {
	if t == nil {
		return
	}
	fields, err := responseObject(data)
	if err != nil {
		return
	} // logging cannot reject or transform the request
	var input, tools []json.RawMessage
	json.Unmarshal(fields["input"], &input)
	json.Unmarshal(fields["tools"], &tools)
	instructions, _ := responseString(fields["instructions"])
	reasoning, encrypted, functionCalls, functionResults, customCalls, customResults := 0, 0, 0, 0, 0, 0
	for _, raw := range input {
		item, err := responseObject(raw)
		if err != nil {
			continue
		}
		kind, _ := responseString(item["type"])
		switch kind {
		case "reasoning":
			reasoning++
			if value, ok := responseString(item["encrypted_content"]); ok && value != "" {
				encrypted++
			}
		case "function_call":
			functionCalls++
		case "function_call_output":
			functionResults++
		case "custom_tool_call":
			customCalls++
		case "custom_tool_call_output":
			customResults++
		}
	}
	attrs := []attribute.KeyValue{
		attribute.String("model", event.Model), attribute.String("requested_tier", requestedTier), attribute.String("effective_tier", event.ServiceTier),
		attribute.String("fast_mode", string(mode)), attribute.String("reasoning_effort", event.Reasoning.Effort), attribute.Bool("stream", stream),
		attribute.Int("upstream_request_bytes", len(data)), attribute.Int("input_items", len(input)), attribute.Int("tools", len(tools)), attribute.Int("instructions_bytes", len(instructions)),
		attribute.String("instructions_hash", t.fingerprint("instructions", []byte(instructions))), attribute.String("tools_hash", t.fingerprint("tools", fields["tools"])),
		attribute.String("prompt_cache_key_hash", t.fingerprint("cache_key", fields["prompt_cache_key"])),
		attribute.Bool("portable_frame", websocketRequestPortable(event)), attribute.Bool("turn_state_metadata_present", strings.TrimSpace(event.ClientMetadata[codexTurnStateKey]) != ""),
		attribute.Int("reasoning_items", reasoning), attribute.Int("encrypted_reasoning_items", encrypted), attribute.Int("function_calls", functionCalls), attribute.Int("function_results", functionResults), attribute.Int("custom_tool_calls", customCalls), attribute.Int("custom_tool_results", customResults),
	}
	t.root.SetAttributes(t.safe(attrs)...)
	t.event(t.ctx, "normalized", attrs...)
}

func (t *responseObservation) selection(ctx context.Context, selection claimedRoutingDecision, attempt int) {
	if t == nil {
		return
	}
	decision := selection.routingDecision
	selected := ""
	if decision.account != nil {
		selected = decision.account.id()
	}
	t.event(ctx, "route_selected", attribute.Int("attempt", attempt+1), attribute.String("account", selected),
		attribute.String("prior_owner", decision.priorOwner), attribute.String("blocked_owner", decision.blocked), attribute.String("routing_reason", string(decision.reason)),
		attribute.Bool("account_move", decision.moved()), attribute.Bool("credit_fallback", decision.creditFallback), attribute.Bool("claim_present", selection.claim != nil), attribute.Bool("claim_joined", selection.joined),
		attribute.Bool("write_attempted", t.writeAttempted), attribute.Bool("response_created", false))
	for _, c := range decision.candidates {
		t.emit(ctx, slog.LevelDebug, "routing_candidate", false, attribute.Int("attempt", attempt+1), attribute.String("account", c.id),
			attribute.Bool("selected", c.id == selected), attribute.String("status", string(c.status(decision.now))), attribute.String("routing_mode", string(c.mode)),
			attribute.Bool("spent", c.spent), attribute.Bool("quota_known", c.quotaKnown()), attribute.Float64("usage_pressure", c.pressure))
	}
}

func (t *responseObservation) received(ctx context.Context, fields responseFields, size int) {
	if t == nil {
		return
	}
	t.events++
	t.upstreamBytes += int64(size)
	kind, _ := responseString(fields["type"])
	attrs := []attribute.KeyValue{attribute.String("event_type", kind), attribute.Int("bytes", size), attribute.Int("event_number", t.events)}
	if t.events == 1 {
		latency := attribute.Int64("first_event_ms", time.Since(t.started).Milliseconds())
		t.root.SetAttributes(latency)
		t.event(ctx, "first_upstream_event", latency, attribute.String("event_type", kind))
	}
	if !t.firstDelta && strings.HasSuffix(kind, ".delta") {
		t.firstDelta = true
		latency := attribute.Int64("first_delta_ms", time.Since(t.started).Milliseconds())
		t.root.SetAttributes(latency)
		deltaAttrs := []attribute.KeyValue{latency, attribute.String("event_type", kind)}
		if !t.writeAt.IsZero() {
			deltaAttrs = append(deltaAttrs, attribute.Int64("since_write_ms", time.Since(t.writeAt).Milliseconds()))
		}
		t.event(ctx, "first_delta", deltaAttrs...)
	}
	for _, key := range []string{"sequence_number", "output_index", "content_index", "summary_index"} {
		var value int64
		if fields[key] != nil && json.Unmarshal(fields[key], &value) == nil {
			attrs = append(attrs, attribute.Int64(key, value))
		}
	}
	for _, key := range []string{"item_id", "response_id"} {
		if id, ok := responseString(fields[key]); ok {
			attrs = append(attrs, attribute.String(key+"_hash", t.fingerprint(key, []byte(id))))
		}
	}
	// No per-token spans or span events: preserve lifecycle events within the
	// bounded OTel event budget. Debug logs contain framing metadata only.
	t.emit(ctx, slog.LevelDebug, "upstream_event", false, attrs...)
}

func (t *responseObservation) usage(ctx context.Context, model, tier, terminal string, usage responseUsage) {
	if t == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("model", model), attribute.String("effective_tier", tier), attribute.String("terminal", terminal), attribute.String("account", t.account),
		attribute.Int64("input_tokens", usage.InputTokens), attribute.Int64("cached_tokens", usage.InputDetails.CachedTokens),
		attribute.Int64("non_cached_tokens", usage.nonCachedInput()), attribute.Int64("cache_write_tokens", usage.InputDetails.CacheWriteTokens),
		attribute.Int64("output_tokens", usage.OutputTokens), attribute.Int64("reasoning_tokens", usage.OutputDetails.ReasoningTokens), attribute.Bool("usage_present", !usage.empty()),
	}
	if usage.InputTokens > 0 {
		attrs = append(attrs, attribute.Float64("cached_input_percent", 100*float64(usage.InputDetails.CachedTokens)/float64(usage.InputTokens)))
	}
	trace.SpanFromContext(ctx).SetAttributes(t.safe(attrs)...)
	t.root.SetAttributes(t.safe(attrs)...)
	t.emit(ctx, slog.LevelInfo, "usage", true, attrs...)
}
