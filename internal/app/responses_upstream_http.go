package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/attribute"
)

var (
	errHTTPPolicyChanged      = errors.New("fast mode changed")
	errHTTPAccountUnavailable = errors.New("account became unavailable")
	errHTTPUpstreamTimeout    = errors.New("upstream response timed out")
	errHTTPRetryOwner         = errors.New("retain upstream request owner")
	errHTTPInvalidResponse    = errors.New("invalid upstream response")
)

func (s *server) forwardHTTPResponse(peer *httpResponsesDownstream, request *http.Request, data []byte, event websocketEnvelope, apiKey apiKeyIdentity, changed <-chan struct{}) {
	ctx, cancel := context.WithCancelCause(request.Context())
	defer cancel(nil)
	request = request.WithContext(ctx)
	route := websocketRouteFrom(request.Header)
	router, err := newResponseAccountRouter(s, request, route, event.Model, event.ServiceTier)
	if err != nil {
		peer.setupFailed(ctx, nil, err)
		return
	}
	account, err := router.selectHTTPAccount(event)
	if err != nil {
		peer.setupFailed(ctx, nil, err)
		return
	}
	defer account.releaseClaim()
	defer func() {
		if errors.Is(context.Cause(ctx), errHTTPPolicyChanged) || errors.Is(context.Cause(ctx), errHTTPUpstreamTimeout) || errors.Is(err, errHTTPRetryOwner) {
			s.preserveResponseOwner(account)
		}
	}()
	accountID := account.account.id()
	ctx, span := observation(ctx).start(ctx, "codex.response", attribute.String("account", accountID), attribute.String("model", event.Model))
	defer span.End()
	request = request.WithContext(ctx)
	active := s.activeWebSockets.add(accountID, func(_, _ string) { cancel(errHTTPAccountUnavailable) })
	defer s.activeWebSockets.remove(active, accountID)
	policyDone := make(chan struct{})
	go func() {
		defer close(policyDone)
		select {
		case <-changed:
			cancel(errHTTPPolicyChanged)
		case <-ctx.Done():
		}
	}()
	defer func() { cancel(nil); <-policyDone }()
	if !s.accountRoutable(accountID) || !account.claim.active() {
		cancel(errHTTPAccountUnavailable)
	}
	select {
	case <-changed:
		cancel(errHTTPPolicyChanged)
	default:
	}
	accounting := &responseAccounting{server: s, request: request, apiKey: apiKey, via: transportHTTP, route: route, thread: route.key(), ctx: ctx, liveThreads: map[string]struct{}{}, account: account}
	defer func() {
		for thread := range accounting.liveThreads {
			s.stats.deactivateThread(thread)
		}
		if observed := observation(ctx); observed != nil {
			observed.cleaned = true
			observed.event(ctx, "cleanup", attribute.Bool("claim_released", true), attribute.Bool("active_registry_removed", true))
		}
	}()
	if ctx.Err() != nil {
		peer.fail(httpTransportFailure(ctx, ctx.Err()))
		return
	}
	upstream, err := url.Parse(s.upstream)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") {
		peer.fail(httpResponseFailure{Status: 502, Code: "invalid_upstream", Message: "invalid HTTP upstream URL"})
		return
	}
	upstream.Path = strings.TrimRight(upstream.Path, "/") + "/responses"
	upstreamRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.String(), bytes.NewReader(data))
	if err != nil {
		peer.setupFailed(ctx, nil, err)
		return
	}
	upstreamRequest.GetBody = nil
	upstreamRequest.Header = httpResponseRequestHeaders(request.Header)
	state := account.account.persisted()
	account.accessToken = accessTokenDigest(state.AccessToken)
	peer.secrets = append(peer.secrets, state.AccessToken, state.RefreshToken, state.IDToken)
	observation(ctx).remember(state)
	upstreamRequest.Header.Set("Authorization", "Bearer "+state.AccessToken)
	upstreamRequest.Header.Set("Chatgpt-Account-Id", claimsFromToken(state.IDToken).Auth.AccountID)
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Set("Accept", "text/event-stream")
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	timer := time.AfterFunc(upstreamWait, func() { cancel(errHTTPUpstreamTimeout) })
	defer timer.Stop()
	if observed := observation(ctx); observed != nil {
		observed.account, observed.writeAttempted = accountID, true
	}
	accounting.startTurn(event)
	observation(ctx).event(ctx, "upstream_write_started", attribute.Int("frame_bytes", len(data)), attribute.String("transport", "http"))
	response, err := client.Do(upstreamRequest)
	observation(ctx).event(ctx, "upstream_write_finished", attribute.Bool("success", err == nil))
	if err != nil {
		peer.fail(httpTransportFailure(ctx, err))
		return
	}
	defer response.Body.Close()
	timer.Reset(httpIdleWait)
	response.Body = &httpActivityReader{ReadCloser: response.Body, timer: timer}
	account.account.observe(response.Header)
	observation(ctx).event(ctx, "http_response_headers", attribute.Int("upstream_status", response.StatusCode))
	if observed := observation(ctx); observed != nil {
		observed.writeSucceeded = true
	}
	copyHTTPResponseHeaders(peer.writer.Header(), response.Header)
	s.pool.clientUsage().writeHeaders(peer.writer.Header())
	redactor := peer.redactor()
	for _, name := range []string{"Retry-After", "X-Request-Id"} {
		if value := peer.writer.Header().Get(name); value != "" {
			peer.writer.Header().Set(name, redactor.Replace(value))
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failure := readHTTPRejection(ctx, response)
		var rejected websocketEnvelope
		rejected.Status = responseEventStatus(response.StatusCode)
		rejected.Error.Code, rejected.Error.Type = failure.Code, failure.Type
		rejection := websocketRejection(rejected)
		if rejection != websocketRejectionNone {
			s.handleWebSocketRejection(account.account, rejection, response.Header, route.key())
		}
		if rejection == websocketRejectionUsageLimit || rejection == websocketRejectionModelCapacity {
			err = errHTTPRetryOwner
		}
		observation(ctx).event(ctx, "upstream_rejected", attribute.Int("upstream_status", response.StatusCode), attribute.Bool("inference_sent", true), attribute.String("retry_owner", "client"))
		if response.StatusCode == http.StatusUnauthorized {
			failure.Status = http.StatusServiceUnavailable
			s.refreshRejectedHTTPAccount(ctx, account)
		}
		if response.StatusCode == http.StatusRequestEntityTooLarge {
			failure.Status, failure.Code, failure.Type = 400, "request_too_large", "invalid_request_error"
		}
		peer.fail(failure)
		return
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType == "application/json" {
		err = s.deliverHTTPJSON(peer, accounting, response.Body)
	} else if mediaType == "text/event-stream" || response.Header.Get("Content-Type") == "" {
		err = readResponseSSE(response.Body, func(data []byte) error { return s.deliverHTTPEvent(peer, accounting, data) })
	} else {
		err = errHTTPInvalidResponse
	}
	if errors.Is(err, errResponseFinished) {
		return
	}
	if !peer.finished {
		failure := httpTransportFailure(ctx, err)
		observation(ctx).event(ctx, "upstream_failed", attribute.String("error_class", failure.Code))
		peer.fail(failure)
	}
}

func (s *server) deliverHTTPEvent(peer *httpResponsesDownstream, accounting *responseAccounting, data []byte) error {
	message, err := peer.prepare(websocketMessage{kind: websocket.MessageText, data: data})
	if err != nil {
		observation(accounting.ctx).invalidHTTPEvent(accounting.ctx, "prepare", data, err)
		return errors.Join(errHTTPInvalidResponse, err)
	}
	var event websocketEnvelope
	if err := json.Unmarshal(message.data, &event); err != nil {
		observation(accounting.ctx).invalidHTTPEvent(accounting.ctx, "envelope_decode", message.data, err)
		return errors.Join(errHTTPInvalidResponse, err)
	}
	headers := websocketEventHeaders(event.Headers)
	message.data = s.pool.clientUsageEvent(accounting.account.account, message.data, event)
	if !peer.committed && (event.Type == "codex.rate_limits" || len(event.Headers) > 0) {
		s.pool.clientUsage().writeHeaders(peer.writer.Header())
	}
	rejection := websocketRejection(event)
	if rejection != websocketRejectionNone {
		s.handleWebSocketRejection(accounting.account.account, rejection, headers, accounting.thread)
		observation(accounting.ctx).event(accounting.ctx, "upstream_rejected", attribute.Bool("accepted_before_rejection", len(accounting.turns) > 0 && accounting.turns[0].created), attribute.String("retry_owner", "client"), attribute.Bool("inference_replayed", false))
	}
	if rejection == websocketRejectionUnauthorized {
		s.refreshRejectedHTTPAccount(accounting.ctx, accounting.account)
	}
	switch event.Type {
	case "response.created":
		if !accounting.responseCreated() {
			return errHTTPAccountUnavailable
		}
	case "response.completed", "response.failed", "response.incomplete", "error":
		accounting.responseFinished(event)
	}
	err = peer.Write(accounting.ctx, websocket.MessageText, message.data)
	if rejection == websocketRejectionUsageLimit || rejection == websocketRejectionModelCapacity {
		return errors.Join(err, errHTTPRetryOwner)
	}
	return err
}

func (s *server) deliverHTTPJSON(peer *httpResponsesDownstream, accounting *responseAccounting, body io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(body, maxHTTPOutput+1))
	if err != nil {
		return err
	}
	if len(data) > maxHTTPOutput {
		return errHTTPInvalidResponse
	}
	fields, err := responseObject(data)
	if err != nil {
		return errors.Join(errHTTPInvalidResponse, err)
	}
	status, _ := responseString(fields["status"])
	if status != "completed" && status != "incomplete" && status != "failed" {
		return errHTTPInvalidResponse
	}
	if peer.stream {
		return errHTTPInvalidResponse
	}
	accepted := accounting.responseCreated()
	encoded, _ := json.Marshal(responseFields{"type": json.RawMessage(`"response.` + status + `"`), "response": data})
	var event websocketEnvelope
	json.Unmarshal(encoded, &event)
	accounting.responseFinished(event)
	if !accepted {
		return errHTTPAccountUnavailable
	}
	peer.finished = true
	if observed := observation(accounting.ctx); observed != nil {
		observed.status = 200
		eventFields, _ := responseObject(encoded)
		observed.terminal(accounting.ctx, event.Type, eventFields)
	}
	peer.writeDeadline()
	peer.writer.Header().Set("Content-Type", "application/json")
	_, err = peer.writer.Write(peer.redact(data))
	observation(accounting.ctx).delivery(accounting.ctx, err, true)
	if err != nil {
		return err
	}
	return errResponseFinished
}

type httpActivityReader struct {
	io.ReadCloser
	timer *time.Timer
}

func (r *httpActivityReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.timer.Reset(httpIdleWait)
	}
	return n, err
}

func httpTransportFailure(ctx context.Context, err error) httpResponseFailure {
	failure := httpResponseFailure{Status: 502, Code: "upstream_disconnected", Message: "upstream ended without a terminal response"}
	if cause := context.Cause(ctx); cause != nil {
		err = cause
	}
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, errHTTPInvalidResponse):
		failure.Code, failure.Message = "invalid_upstream_response", "invalid upstream response"
	case errors.Is(err, errHTTPUpstreamTimeout), errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		failure.Status, failure.Code, failure.Message = 504, "upstream_timeout", "upstream response timed out"
	case errors.Is(err, errHTTPPolicyChanged):
		failure.Status, failure.Code, failure.Message = 503, "policy_changed", err.Error()
	case errors.Is(err, errHTTPAccountUnavailable):
		failure.Status, failure.Code, failure.Message = 503, "route_unavailable", err.Error()
	case errors.Is(err, context.Canceled):
		failure.Status, failure.Code, failure.Message = 503, "request_canceled", "request canceled or server shutting down"
	}
	return failure
}

func (s *server) refreshRejectedHTTPAccount(ctx context.Context, account *responseAccount) {
	if !account.account.markRejectedAccessToken(account.accessToken) {
		return
	}
	if !s.refreshedContext(ctx, account.account, account.account.id()) && !account.account.needsReauth() {
		account.account.clearRejectedAccessToken(account.accessToken)
	}
}

func readHTTPRejection(ctx context.Context, response *http.Response) httpResponseFailure {
	failure := httpResponseFailure{Status: response.StatusCode, UpstreamStatus: response.StatusCode, Code: "upstream_rejected", Message: "upstream rejected request"}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamErrorBody+1))
	if err != nil {
		return httpTransportFailure(ctx, err)
	}
	if len(data) <= maxUpstreamErrorBody {
		if fields, err := responseObject(data); err == nil {
			failure = responseFailure(fields, response.StatusCode)
		}
	}
	if failure.Status < 400 || failure.Status > 599 {
		failure.Status = 502
	}
	return failure
}
