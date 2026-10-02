package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/attribute"
)

const (
	maxHTTPResponseBody = 256 << 20
	maxHTTPOutput       = 256 << 20
	httpResponseIOWait  = 30 * time.Second
	httpIdleWait        = 6 * time.Minute
)

func (s *server) responsesHTTP(w http.ResponseWriter, request *http.Request) {
	controller := http.NewResponseController(w)
	controller.SetReadDeadline(time.Now().Add(httpResponseIOWait))
	apiKey, authorized := s.authorizeAPIKey(request)
	observed := observation(request.Context())
	observed.event(request.Context(), "authorization", attribute.Bool("authorized", authorized), attribute.Bool("auth_required", s.lookupAPIKey != nil))
	if !authorized {
		observed.reject(request.Context(), http.StatusUnauthorized, "invalid_bearer_key")
		// Do not let net/http drain a body the rejected client has not sent.
		w.Header().Set("Connection", "close")
		writeHTTPResponseError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	observed.rememberPool() // never inspect pool credentials for rejected admission/auth
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	if s.ctx != nil {
		stop := context.AfterFunc(s.ctx, cancel)
		defer stop()
	}
	request = request.WithContext(ctx)
	// Deadlines interrupt body reads and writes too, not just upstream work.
	interrupted := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		controller.SetReadDeadline(time.Now())
		controller.SetWriteDeadline(time.Now())
		request.Body.Close()
	})
	defer func() {
		if !stopInterrupt() {
			<-interrupted
		}
		// Leave deadlines active through net/http's final buffered write/body
		// close. The server resets them for subsequent requests.
	}()
	encoding, supported := responseContentEncoding(request.Header)
	if !supported {
		observed.reject(ctx, http.StatusUnsupportedMediaType, "unsupported_encoding")
		w.Header().Set("Connection", "close")
		writeHTTPResponseError(w, http.StatusUnsupportedMediaType, "Content-Encoding must be identity or zstd")
		return
	}
	if contentType := request.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			observed.reject(ctx, http.StatusUnsupportedMediaType, "unsupported_content_type")
			w.Header().Set("Connection", "close")
			writeHTTPResponseError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
	}
	readCtx, readSpan := s.responseTracer().Start(ctx, "http.request.read")
	readStarted := time.Now()
	data, wireBytes, err := readResponseBody(ctx, w, request, encoding)
	spanFailure(readSpan, err)
	observed.event(readCtx, "body_read", attribute.String("content_encoding", encoding), attribute.Int("wire_bytes", wireBytes), attribute.Int("body_bytes", len(data)), attribute.Int64("elapsed_ms", time.Since(readStarted).Milliseconds()), attribute.String("error_type", telemetryErrorClass(err)))
	readSpan.End()
	if err != nil {
		status := responseBodyStatus(err)
		observed.reject(ctx, status, "body_read_failed")
		w.Header().Set("Connection", "close")
		writeHTTPResponseError(w, status, "could not read/decode request body within size/time limits")
		return
	}
	controller.SetReadDeadline(time.Time{})
	_, translationSpan := s.responseTracer().Start(ctx, "codex.request.translate")
	event, stream, err := validateHTTPResponse(data)
	spanFailure(translationSpan, err)
	translationSpan.End()
	if err != nil {
		observed.reject(ctx, http.StatusBadRequest, "request_translation_failed")
		writeHTTPResponseError(w, http.StatusBadRequest, err.Error())
		return
	}
	mode, changed := s.fastMode.snapshot()
	requestedTier := event.ServiceTier
	data, event.ServiceTier, err = mode.override(data, event.ServiceTier)
	if err != nil {
		observed.reject(ctx, http.StatusInternalServerError, "fast_mode_override_failed")
		writeHTTPResponseError(w, http.StatusInternalServerError, "could not apply fast mode")
		return
	}
	observed.normalized(data, event, stream, mode, requestedTier)
	peer := &httpResponsesDownstream{
		writer: w, controller: controller, request: data, stream: stream, ctx: ctx,
		items: map[int]json.RawMessage{}, pending: map[int]bool{},
		responsesRedactor: s.responsesRedactor(),
	}
	s.forwardHTTPResponse(peer, request, data, event, apiKey, changed)

}

func writeHTTPResponseError(w http.ResponseWriter, status int, message string) {
	// This is a terminal write, not a timeout on body read or generation.
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(httpResponseIOWait))
	writeError(w, status, message)
}

func httpResponseRequestHeaders(inbound http.Header) http.Header {
	out := http.Header{}
	for _, name := range []string{
		"User-Agent", "Originator", "OpenAI-Beta", "Session_id", "Session-Id",
		"OpenAI-Service-Tier", "X-Codex-Session-Id", "X-Codex-Conversation-Id", "X-Session-Affinity", "X-Session-Id",
		"Thread-Id", "X-Client-Request-Id", codexTurnStateKey,
	} {
		if value := inbound.Get(name); value != "" {
			out.Set(name, value)
		}
	}
	for name, values := range inbound {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-codex-") || strings.HasPrefix(lower, "x-openai-") {
			out[name] = values
		}
	}
	for _, value := range inbound.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			out.Del(strings.TrimSpace(name))
		}
	}
	return out
}

func copyHTTPResponseHeaders(dst, src http.Header) {
	// Never forward upgrade, authorization, account or turn-state headers.
	for _, name := range []string{"Retry-After", "X-Request-Id"} {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
}

type httpResponsesDownstream struct {
	writer          http.ResponseWriter
	ctx             context.Context
	controller      *http.ResponseController
	request         []byte
	stream          bool
	committed       bool
	finished        bool
	sequence        int64
	accountBound    bool
	routeIdentified bool
	retryAt         time.Time
	responsesRedactor
	outputSeen   bool
	created      responseFields
	createdBytes int
	items        map[int]json.RawMessage
	pending      map[int]bool
	itemBytes    int
}

func (d *httpResponsesDownstream) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	if d.request != nil {
		data := d.request
		d.request = nil
		return websocket.MessageText, data, nil
	}
	<-ctx.Done()
	return 0, nil, ctx.Err()
}

func (d *httpResponsesDownstream) prepare(message websocketMessage) (websocketMessage, error) {
	if message.kind != websocket.MessageText {
		observation(d.ctx).event(d.ctx, "invalid_upstream_frame", attribute.Int("bytes", len(message.data)), attribute.String("reason", "binary"))
		return message, errors.New("binary upstream frame")
	}
	fields, err := responseObject(message.data)
	if err != nil {
		observation(d.ctx).event(d.ctx, "invalid_upstream_frame", attribute.Int("bytes", len(message.data)), attribute.String("reason", "invalid_json"))
		return message, err
	}
	observation(d.ctx).received(d.ctx, fields, len(message.data))
	kind, ok := responseString(fields["type"])
	if !ok || kind == "" || strings.ContainsAny(kind, "\r\n\x00") {
		return message, errors.New("invalid event type")
	}
	if kind == "response.done" {
		// The pinned HTTP SDK recognizes completed, not Codex's legacy done.
		kind = "response.completed"
		fields["type"] = json.RawMessage(`"response.completed"`)
	}
	if kind == "error" && fields["error"] == nil {
		// OpenAI's HTTP error shape is flat; Codex usually nests it. Supply the
		// accounting envelope without removing the original fields.
		details := responseFields{}
		for _, key := range []string{"code", "message", "param"} {
			if value := fields[key]; value != nil {
				details[key] = value
			}
		}
		fields["error"], _ = json.Marshal(details)
	}
	if kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed" {
		response, err := responseObject(fields["response"])
		if err != nil {
			return message, errors.New("terminal event has no response object")
		}
		if raw, exists := response["status"]; exists {
			if status, ok := responseString(raw); !ok || "response."+status != kind {
				return message, errors.New("conflicting terminal status")
			}
		}
	}
	// Add missing sequence numbers for Codex error events. Preserve supplied
	// numbers and all payload fields (including unknown event types).
	if fields["sequence_number"] == nil {
		fields["sequence_number"], _ = json.Marshal(d.sequence)
	}
	d.sequence++
	message.data, err = json.Marshal(fields)
	return message, err
}

func (d *httpResponsesDownstream) Write(ctx context.Context, _ websocket.MessageType, data []byte) (err error) {
	defer func() { observation(ctx).delivery(ctx, err, d.committed) }()
	fields, _ := responseObject(data) // validated by prepare before accounting
	kind, _ := responseString(fields["type"])
	if !d.committed {
		var envelope websocketEnvelope
		json.Unmarshal(data, &envelope)
		copyHTTPResponseHeaders(d.writer.Header(), websocketEventHeaders(envelope.Headers))
	}
	delete(fields, "headers") // observed by the relay, never part of HTTP output
	terminal := kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed" || kind == "error"
	failed := kind == "error" || kind == "response.failed"
	if failed {
		failure := responseFailure(fields, 0)
		if !d.committed && !(d.stream && kind == "response.failed") {
			return d.fail(failure)
		}
		observation(ctx).failure(ctx, failure.Status, failure.Code, true)
	}
	if kind == "error" {
		failure := responseFailure(fields, 0)
		fields["message"], _ = json.Marshal(failure.Message)
		fields["code"], _ = json.Marshal(failure.Code)
		fields["param"] = failure.Param
	}
	if d.stream {
		if err := d.writeEvent(fields); err != nil {
			d.finished = true
			return err
		}
		if observed := observation(ctx); observed != nil {
			observed.forwarded++
			observed.status, observed.sseCommitted = 200, d.committed
			observed.emit(ctx, slog.LevelDebug, "downstream_event", false, attribute.String("event_type", kind))
			if terminal {
				observed.terminal(ctx, kind, fields)
			}
		}
		if terminal {
			d.finished = true
			if err := d.writeDone(); err != nil {
				return err
			}
			return errResponseFinished
		}
		return nil
	}
	if err := d.collect(kind, fields); err != nil {
		return d.fail(httpResponseFailure{Status: 502, Code: "invalid_upstream_output", Message: err.Error()})
	}
	if terminal {
		response, err := d.result(fields)
		if err != nil {
			return d.fail(httpResponseFailure{Status: 502, Code: "invalid_upstream_output", Message: err.Error()})
		}
		d.finished = true
		if observed := observation(ctx); observed != nil {
			observed.status = 200
			observed.terminal(ctx, kind, fields)
			observed.event(ctx, "json_response", attribute.Int("response_bytes", len(response)), attribute.Int("retained_items", len(d.items)), attribute.Int("retained_bytes", d.itemBytes+d.createdBytes))
		}
		response = d.redact(response)
		d.writeDeadline()
		d.writer.Header().Set("Content-Type", "application/json")
		_, err = d.writer.Write(response)
		if err != nil {
			return err
		}
		return errResponseFinished
	}
	return nil
}

func (d *httpResponsesDownstream) reject(ctx context.Context, message websocketMessage, _ websocket.StatusCode, _ string) error {
	return d.Write(ctx, message.kind, message.data)
}

func (d *httpResponsesDownstream) requestFailed(_ context.Context, failure httpResponseFailure, _ websocket.StatusCode) error {
	return d.fail(failure)
}

func (d *httpResponsesDownstream) Close(status websocket.StatusCode, reason string) error {
	failure := httpResponseFailure{Status: 502, Code: "upstream_disconnected", Message: reason}
	switch {
	case strings.Contains(reason, "timed out"):
		failure.Status, failure.Code = 504, "upstream_timeout"
	case strings.Contains(reason, "fast mode"):
		failure.Status, failure.Code = 503, "policy_changed"
	case strings.Contains(reason, "account-bound"):
		failure.Status, failure.Code, failure.Type = 400, "account_bound_request", "invalid_request_error"
	case strings.Contains(reason, "account") || strings.Contains(reason, "route owner"):
		failure.Status, failure.Code = 503, "route_unavailable"
	case strings.Contains(reason, "canceled"):
		failure.Status, failure.Code = 503, "request_canceled"
	case status == websocket.StatusInternalError:
		failure.Code = "invalid_upstream_response"
	}
	return d.fail(failure)
}

func (d *httpResponsesDownstream) writeEvent(fields responseFields) (err error) {
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	kind, _ := responseString(fields["type"])
	redactor := d.redactor()
	kind = redactor.Replace(kind)
	data = redactJSONStrings(data, redactor)
	d.writeDeadline()
	defer func() {
		// HTTP/2 expires the stream even when no write is pending. Only
		// terminal/error writes may leave a deadline armed for finishRequest.
		if err == nil && !d.finished {
			d.controller.SetWriteDeadline(time.Time{})
		}
	}()
	if !d.committed {
		d.writer.Header().Set("Content-Type", "text/event-stream")
		d.writer.Header().Set("Cache-Control", "no-cache, no-transform")
		d.writer.Header().Set("X-Accel-Buffering", "no")
		d.committed = true
	}
	if _, err := io.WriteString(d.writer, "event: "+kind+"\ndata: "); err != nil {
		return err
	}
	if _, err := d.writer.Write(data); err != nil {
		return err
	}
	if _, err := io.WriteString(d.writer, "\n\n"); err != nil {
		return err
	}
	return d.controller.Flush()
}

func (d *httpResponsesDownstream) writeDone() error {
	d.writeDeadline()
	if _, err := io.WriteString(d.writer, "data: [DONE]\n\n"); err != nil {
		return err
	}
	return d.controller.Flush()
}

func (d *httpResponsesDownstream) writeDeadline() {
	d.controller.SetWriteDeadline(time.Now().Add(httpResponseIOWait))
	if d.ctx != nil && d.ctx.Err() != nil {
		d.controller.SetWriteDeadline(time.Now())
	}
}
