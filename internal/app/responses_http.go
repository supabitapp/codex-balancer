package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	maxHTTPResponseBody = 8 << 20
	maxHTTPOutput       = 16 << 20
	httpResponseIOWait  = 30 * time.Second
)

func (s *server) responsesHTTP(w http.ResponseWriter, request *http.Request) {
	controller := http.NewResponseController(w)
	controller.SetReadDeadline(time.Now().Add(httpResponseIOWait))
	apiKey, authorized := s.authorizeAPIKey(request)
	if !authorized {
		// Do not let net/http drain a body the rejected client has not sent.
		w.Header().Set("Connection", "close")
		writeHTTPResponseError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
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
	if encoding := request.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		w.Header().Set("Connection", "close")
		writeHTTPResponseError(w, http.StatusUnsupportedMediaType, "compressed request bodies are unsupported")
		return
	}
	if contentType := request.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			w.Header().Set("Connection", "close")
			writeHTTPResponseError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
	}
	body := http.MaxBytesReader(w, request.Body, maxHTTPResponseBody)
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeHTTPResponseError(w, status, "could not read request body within size/time limit")
		return
	}
	controller.SetReadDeadline(time.Time{})
	data, stream, err := translateHTTPResponse(data)
	if err != nil {
		writeHTTPResponseError(w, http.StatusBadRequest, err.Error())
		return
	}
	mode, changed := s.fastMode.snapshot()
	var event websocketEnvelope
	json.Unmarshal(data, &event)
	data, event.ServiceTier, err = mode.override(data, event.ServiceTier)
	if err != nil {
		writeHTTPResponseError(w, http.StatusInternalServerError, "could not apply fast mode")
		return
	}
	peer := &httpResponsesDownstream{
		writer: w, controller: controller, request: data, stream: stream, ctx: ctx,
		items: map[int]json.RawMessage{}, pending: map[int]bool{},
		accounts: s.pool.all(),
	}
	for _, account := range peer.accounts {
		state := account.persisted()
		peer.secrets = append(peer.secrets, state.AccessToken, state.RefreshToken, state.IDToken)
	}
	route := websocketRouteFrom(request.Header)
	upstreamRequest := request.Clone(ctx)
	upstreamRequest.Header = httpResponseRequestHeaders(request.Header)
	// Retain the existing stats IP calculation without forwarding its headers.
	upstreamRequest.RemoteAddr = requestIP(request)
	dial, failed, err := s.dialResponsesWebSocket(upstreamRequest, route, event.Model, event.ServiceTier)
	if err != nil || failed != nil {
		defer closeWebSocketResponse(failed)
		peer.setupFailed(ctx, failed, err)
		return
	}
	state := dial.account.persisted()
	peer.secrets = append(peer.secrets, state.AccessToken, state.RefreshToken, state.IDToken)
	dial.conn.SetReadLimit(maxHTTPOutput)
	// The request context is linked to server shutdown above. The same relay
	// handles claims, invalidation, acceptance and usage for both transports.
	relay := newResponsesWebSocketRelay(s, peer, upstreamRequest, dial, route, apiKey, mode, changed)
	relay.idleTimeout = upstreamWait
	relay.messageLimit = maxHTTPOutput
	relay.messages = make(chan websocketMessage, 1)
	relay.run()
	if !peer.finished {
		peer.fail(httpResponseFailure{Status: 502, Code: "upstream_disconnected", Message: "upstream ended without a terminal response; retry with full history"})
	}
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
		"X-Codex-Session-Id", "X-Codex-Conversation-Id", "X-Session-Affinity", "X-Session-Id",
		"Thread-Id", "X-Client-Request-Id", codexTurnStateKey,
	} {
		if value := inbound.Get(name); value != "" {
			out.Set(name, value)
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
	writer       http.ResponseWriter
	ctx          context.Context
	controller   *http.ResponseController
	request      []byte // read only by the downstream reader goroutine
	stream       bool
	committed    bool
	finished     bool
	sequence     int64
	accounts     []*Account
	secrets      []string
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
		return message, errors.New("binary upstream frame")
	}
	fields, err := responseObject(message.data)
	if err != nil {
		return message, err
	}
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

func (d *httpResponsesDownstream) Write(_ context.Context, _ websocket.MessageType, data []byte) error {
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
	if failed && !d.committed {
		return d.fail(responseFailure(fields, 0))
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
	// The WebSocket adapter suppresses retryable events in favor of 1012.
	// HTTP clients need the actual error, not an empty successful stream.
	return d.Write(ctx, message.kind, message.data)
}

func (d *httpResponsesDownstream) Close(status websocket.StatusCode, reason string) error {
	failure := httpResponseFailure{Status: 502, Code: "upstream_disconnected", Message: reason}
	switch {
	case strings.Contains(reason, "timed out"):
		failure.Status, failure.Code = 504, "upstream_timeout"
	case strings.Contains(reason, "fast mode"):
		failure.Status, failure.Code = 503, "policy_changed"
	case strings.Contains(reason, "account-bound"):
		failure.Status, failure.Code = 409, "account_bound_request"
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
