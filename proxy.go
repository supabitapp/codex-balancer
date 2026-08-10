package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	maxRequestBody       = 64 << 20
	maxResponseEventLine = 1 << 20
	maxUpstreamErrorBody = 64 << 10
	maxAttempts          = 3
	maxUpstreamRetries   = 3
	upstreamRetryBudget  = 5 * time.Second
	refreshTimeout       = 30 * time.Second
	upstreamWait         = 90 * time.Second
)

var requestBodyDecoder = newRequestBodyDecoder()

func newRequestBodyDecoder() *zstd.Decoder {
	decoder, err := zstd.NewReader(
		nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(maxRequestBody),
	)
	if err != nil {
		panic(err)
	}
	return decoder
}

func newProxyClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = upstreamWait
	return &http.Client{Transport: transport}
}

type server struct {
	ctx                  context.Context
	pool                 *Pool
	catalog              *modelCatalog
	affinity             *AffinityStore
	compactionRotation   *compactionRotation
	stats                *Stats
	logins               accountLoginStore
	upstream             string
	authIssuer           string
	key                  string
	clientIDKey          []byte
	client               *http.Client
	log                  *slog.Logger
	admission            *admissionGate
	retryBackoff         func(int) time.Duration
	dashboardConnections atomic.Int64
}

func upstreamRetryBackoff(retry int) time.Duration {
	return upstreamRetryBudget * time.Duration(1<<(retry-1)) / time.Duration((1<<maxUpstreamRetries)-1)
}

func upstreamRetryDelay(retry int) time.Duration {
	return time.Duration(float64(upstreamRetryBackoff(retry)) * (0.9 + rand.Float64()*0.2))
}

func (s *server) waitForUpstreamRetry(ctx context.Context, retry int) bool {
	delay := upstreamRetryDelay(retry)
	if s.retryBackoff != nil {
		delay = s.retryBackoff(retry)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

var hopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"content-length":      true,
	"host":                true,
	"authorization":       true,
	"chatgpt-account-id":  true,
}

var websocketHopByHop = map[string]bool{
	"accept-encoding":          true,
	"cookie":                   true,
	"sec-websocket-accept":     true,
	"sec-websocket-extensions": true,
	"sec-websocket-key":        true,
	"sec-websocket-protocol":   true,
	"sec-websocket-version":    true,
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusPermanentRedirect)
	})
	mux.HandleFunc("GET /accounts", s.accountsPage)
	mux.HandleFunc("GET /dashboard", s.dashboardPage)
	mux.HandleFunc("GET /dashboard/assets/dashboard.js", dashboardScript("web/dashboard.js"))
	mux.HandleFunc("GET /dashboard/assets/htmx-2.0.10.min.js", dashboardScript("web/htmx-2.0.10.min.js"))
	mux.HandleFunc("GET /dashboard/assets/ws-2.0.4.min.js", dashboardScript("web/ws-2.0.4.min.js"))
	mux.HandleFunc("GET /dashboard/ws", s.dashboardWebSocket)
	mux.HandleFunc("GET /stats", s.statsJSON)
	mux.Handle("POST /v1/responses", s.admitted(s.responses))
	mux.Handle("GET /v1/responses", s.admitted(s.responsesWebSocket))
	mux.HandleFunc("GET /v1/models", s.models)
	return mux
}

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	clientVersion := strings.TrimSpace(r.URL.Query().Get("client_version"))
	if clientVersion != "" {
		if err := s.refreshModels(r.Context(), clientVersion); err != nil && s.log != nil {
			s.log.Warn("model refresh failed", "error", err)
		}
	}
	models := []modelEntry{}
	if s.catalog != nil {
		models = s.catalog.entries()
	}
	w.Header().Set("Content-Type", "application/json")
	if clientVersion != "" {
		json.NewEncoder(w).Encode(map[string]any{"models": models})
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		data = append(data, map[string]any{
			"id":       modelSlug(model),
			"object":   "model",
			"owned_by": "openai",
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (s *server) responses(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "could not read request body")
		}
		return
	}
	inspectionBody, err := decodeRequestBody(r.Header, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	request := responseRequest(inspectionBody)
	metadata := requestTurnMetadata(r.Header.Get(codexTurnMetadataKey), request.ClientMetadata)
	affinity, err := affinityFromRequest(r.Header, inspectionBody)
	if err != nil {
		status, message := affinityErrorStatus(err)
		writeError(w, status, message)
		return
	}
	resolution, err := s.affinity.resolve(affinity, s.pool)
	if err != nil {
		status, message := affinityErrorStatus(err)
		writeError(w, status, message)
		return
	}
	key := affinity.statsKey(r.Header)
	skip := map[string]bool{}
	reauthed := map[string]bool{}
	modelRetried := false
	s.log.Debug("http turn received",
		"thread", key,
		"required_account", resolution.required,
		"preferred_account", resolution.preferred,
		"hard_affinity", resolution.hard,
		"hard_affinity_kinds", affinity.hardKinds(),
		"compaction_replay", affinity.compactionReplay,
		"service_tier", request.ServiceTier,
	)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		account := s.pickAccount(key, resolution.required, resolution.preferred, request.Model, request.ServiceTier, skip, attempt, transportHTTP)
		if account == nil {
			writeError(w, http.StatusServiceUnavailable, noAccountAvailableMessage)
			return
		}
		id := account.id()

		if account.stale(time.Now()) && !reauthed[id] {
			reauthed[id] = true
			if !s.refreshed(account, id) {
				skip[id] = true
				continue
			}
		}
		if resolution.hard {
			if err := s.affinity.claimAll(hardAffinityRefs(resolution.bindings), id); err != nil {
				status, message := affinityErrorStatus(err)
				writeError(w, status, message)
				return
			}
			resolution.required = id
		}

		var sent time.Time
		var resp *http.Response
		var err error
		upstreamRetries := 0
		for {
			sent = time.Now()
			resp, err = s.forward(r, body, account)
			if err != nil {
				break
			}
			s.log.Debug("upstream responded",
				"transport", transportHTTP,
				"thread", key,
				"account", id,
				"attempt", attempt+1,
				"retry", upstreamRetries,
				"status", resp.StatusCode,
				"header_latency", time.Since(sent),
			)
			if resp.StatusCode < 500 || upstreamRetries == maxUpstreamRetries {
				break
			}
			resp.Body.Close()
			upstreamRetries++
			s.log.Info("retrying upstream server failure", "transport", transportHTTP, "thread", key, "account", id, "attempt", attempt+1, "retry", upstreamRetries, "status", resp.StatusCode)
			if !s.waitForUpstreamRetry(r.Context(), upstreamRetries) {
				return
			}
		}
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			s.log.Warn("upstream unreachable", "transport", transportHTTP, "thread", key, "account", id, "attempt", attempt+1, "error", err)
			s.stats.failedOver(id, "unreachable")
			account.failed(attempt)
			skip[id] = true
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized && !reauthed[id] {
			resp.Body.Close()
			reauthed[id] = true
			if !s.refreshed(account, id) {
				skip[id] = true
			}
			attempt--
			continue
		}

		unsupportedModel := resp.StatusCode == http.StatusBadRequest && responseAccountModelUnsupported(resp, request.Model)
		if status := resp.StatusCode; status == http.StatusTooManyRequests ||
			status == http.StatusUnauthorized || status >= 500 || unsupportedModel {
			if status == http.StatusTooManyRequests {
				account.observe(resp.Header)
				account.rateLimited(resp.Header, attempt)
				s.stats.rateLimited(id)
				attrs := []any{"transport", transportHTTP, "thread", key, "attempt", attempt + 1}
				attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
				s.log.Info("account rate limited", attrs...)
			} else if !unsupportedModel {
				s.log.Warn("upstream refused the turn", "transport", transportHTTP, "thread", key, "account", id, "attempt", attempt+1, "status", status)
				account.failed(attempt)
			}
			noReplacement := false
			if unsupportedModel && !resolution.hard && !modelRetried {
				replacementSkip := maps.Clone(skip)
				replacementSkip[id] = true
				noReplacement = s.pool.pick("", "", replacementSkip, s.allowedAccounts(request.Model, request.ServiceTier)) == nil
			}
			if resolution.hard || noReplacement || unsupportedModel && modelRetried {
				if resolution.hard {
					s.log.Info("hard affinity stays on its account", "transport", transportHTTP, "thread", key, "account", id, "status", status)
				}
				copyHeaders(w.Header(), resp.Header)
				w.WriteHeader(status)
				io.Copy(w, resp.Body)
				resp.Body.Close()
				return
			}
			resp.Body.Close()
			s.stats.failedOver(id, resp.Status)
			skip[id] = true
			if unsupportedModel {
				modelRetried = true
			}
			continue
		}
		if resp.StatusCode/100 == 2 {
			primed, err := primeResponseBody(resp.Body)
			if err != nil {
				resp.Body.Close()
				account.failed(attempt)
				s.log.Warn("upstream response ended before its body", "transport", transportHTTP, "thread", key, "account", id, "attempt", attempt+1, "error", err)
				if resolution.hard {
					writeError(w, http.StatusBadGateway, "upstream response ended before its body")
					return
				}
				s.stats.failedOver(id, "empty response")
				skip[id] = true
				continue
			}
			resp.Body = primed
		}

		account.observe(resp.Header)
		bindings := resolution.bindings
		if turnState := turnStateAffinity(resp.Header); turnState.valid() {
			bindings = append(bindings, turnState)
		}
		if err := s.affinity.bindAll(bindings, id); err != nil {
			s.log.Warn("affinity save failed", "thread", key, "account", id, "error", err)
		}
		s.stats.routed(key, requestClientID(r, s.clientIDKey), id, request.Model, request.Reasoning.Effort, request.ServiceTier, transportHTTP, metadata)
		attrs := []any{"transport", transportHTTP, "thread", key, "attempt", attempt + 1, "status", resp.StatusCode}
		attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
		s.log.Debug("http turn routed", attrs...)
		s.relay(w, resp, sent, key, id, request, metadata)
		return
	}
	s.log.Warn("every account failed", "transport", transportHTTP, "thread", key, "attempts", maxAttempts)
	writeError(w, http.StatusServiceUnavailable, "every account failed this turn")
}

func decodeRequestBody(headers http.Header, body []byte) ([]byte, error) {
	encoding := strings.TrimSpace(headers.Get("Content-Encoding"))
	if encoding == "" {
		return body, nil
	}
	if !strings.EqualFold(encoding, "zstd") {
		return nil, fmt.Errorf("unsupported content encoding %q", encoding)
	}
	decoded, err := requestBodyDecoder.DecodeAll(body, nil)
	if err != nil {
		return nil, fmt.Errorf("decode zstd: %w", err)
	}
	return decoded, nil
}

type responseRequestData struct {
	Model          string            `json:"model"`
	Reasoning      responseReasoning `json:"reasoning"`
	ServiceTier    string            `json:"service_tier"`
	ClientMetadata map[string]string `json:"client_metadata"`
}

type responseReasoning struct {
	Effort string `json:"effort"`
}

func responseRequest(body []byte) responseRequestData {
	var request responseRequestData
	json.Unmarshal(body, &request)
	return request
}

func responseAccountModelUnsupported(resp *http.Response, model string) bool {
	original := resp.Body
	prefix, _ := io.ReadAll(io.LimitReader(original, maxUpstreamErrorBody))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{
		Reader: io.MultiReader(bytes.NewReader(prefix), original),
		Closer: original,
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(prefix, &envelope) != nil {
		return false
	}
	return accountModelUnsupported(envelope.Error.Code, envelope.Error.Message, model)
}

func accountModelUnsupported(code, message, model string) bool {
	if code != "invalid_request_error" || model == "" {
		return false
	}
	want := fmt.Sprintf("The '%s' model is not supported when using Codex with a ChatGPT account.", model)
	return strings.TrimSpace(message) == want
}

func (s *server) refreshed(account *Account, id string) bool {
	s.log.Debug("refreshing account", "account", id)
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	if err := account.refresh(ctx, s.client, s.pool.persistTokens); err != nil {
		s.log.Warn("refresh failed", "account", id, "error", err)
		return false
	}
	s.log.Debug("account refreshed", "account", id)
	return true
}

func copyHeaders(dst, src http.Header) {
	copyHeadersExcept(dst, src, nil)
}

func copyWebSocketHeaders(dst, src http.Header) {
	copyHeadersExcept(dst, src, websocketHopByHop)
}

func copyHeadersExcept(dst, src http.Header, extra map[string]bool) {
	connection := map[string]bool{}
	for _, value := range src.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			connection[strings.ToLower(strings.TrimSpace(token))] = true
		}
	}
	for name, values := range src {
		lower := strings.ToLower(name)
		if hopByHop[lower] || extra[lower] || connection[lower] {
			continue
		}
		dst[name] = values
	}
}

func (s *server) forward(r *http.Request, body []byte, account *Account) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.upstream+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Del("Accept-Encoding")
	account.mu.Lock()
	token := account.AccessToken
	account.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("chatgpt-account-id", account.id())
	return s.client.Do(req)
}

type prefixedResponseBody struct {
	prefix  *bytes.Reader
	body    io.ReadCloser
	pending error
}

func primeResponseBody(body io.ReadCloser) (io.ReadCloser, error) {
	buffer := make([]byte, 32<<10)
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			return &prefixedResponseBody{
				prefix:  bytes.NewReader(buffer[:n]),
				body:    body,
				pending: err,
			}, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (b *prefixedResponseBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	if b.pending != nil {
		err := b.pending
		b.pending = nil
		return 0, err
	}
	return b.body.Read(p)
}

func (b *prefixedResponseBody) Close() error {
	return b.body.Close()
}

func (s *server) relay(w http.ResponseWriter, resp *http.Response, sent time.Time, thread, account string, request responseRequestData, metadata turnMetadata) {
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	control := http.NewResponseController(w)
	inspector := responseOwnerInspector{
		store:       s.affinity,
		stats:       s.stats,
		thread:      thread,
		account:     account,
		model:       request.Model,
		serviceTier: request.ServiceTier,
		metadata:    metadata,
		started:     sent,
		log:         s.log,
	}
	first := true
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			inspector.write(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			control.Flush()
			if first {
				s.stats.answered(thread, time.Since(sent))
				first = false
			}
		}
		if err != nil {
			inspector.finish()
			if err != io.EOF {
				s.log.Warn("stream cut short", "error", err)
			}
			return
		}
	}
}

type responseOwnerInspector struct {
	store         *AffinityStore
	stats         *Stats
	thread        string
	account       string
	model         string
	serviceTier   string
	metadata      turnMetadata
	started       time.Time
	buffer        []byte
	event         []byte
	afterCR       bool
	discardLine   bool
	discardEvent  bool
	usageRecorded bool
	completed     bool
	log           *slog.Logger
}

func (i *responseOwnerInspector) write(data []byte) {
	if i.afterCR {
		if len(data) > 0 && data[0] == '\n' {
			data = data[1:]
		}
		i.afterCR = false
	}
	if i.discardLine {
		index := bytes.IndexAny(data, "\r\n")
		if index < 0 {
			return
		}
		advance := index + 1
		if data[index] == '\r' {
			if advance < len(data) && data[advance] == '\n' {
				advance++
			} else if advance == len(data) {
				i.afterCR = true
			}
		}
		i.discardLine = false
		data = data[advance:]
	}
	i.buffer = append(i.buffer, data...)
	for {
		index := bytes.IndexAny(i.buffer, "\r\n")
		if index < 0 {
			if len(i.buffer) > maxResponseEventLine {
				i.buffer = nil
				i.discardLine = true
			}
			return
		}
		line := i.buffer[:index]
		ending := i.buffer[index]
		advance := index + 1
		if ending == '\r' {
			if advance < len(i.buffer) && i.buffer[advance] == '\n' {
				advance++
			} else if advance == len(i.buffer) {
				i.afterCR = true
			}
		}
		i.buffer = i.buffer[advance:]
		if len(line) > maxResponseEventLine {
			i.event = nil
			i.discardEvent = true
		} else {
			i.line(line)
		}
	}
}

func (i *responseOwnerInspector) finish() {
	if len(i.buffer) > 0 {
		i.line(i.buffer)
	}
	i.dispatch()
	i.buffer = nil
	i.discardLine = false
	i.discardEvent = false
}

func (i *responseOwnerInspector) line(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		if i.discardEvent {
			i.event = nil
			i.discardEvent = false
		} else {
			i.dispatch()
		}
		return
	}
	if i.discardEvent {
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		data := bytes.TrimPrefix(line, []byte("data:"))
		if len(data) > 0 && data[0] == ' ' {
			data = data[1:]
		}
		separator := 0
		if len(i.event) > 0 {
			separator = 1
		}
		if len(i.event)+separator+len(data) > maxResponseEventLine {
			i.event = nil
			i.discardEvent = true
			return
		}
		if separator > 0 {
			i.event = append(i.event, '\n')
		}
		i.event = append(i.event, data...)
		return
	}
	i.inspect(line)
}

func (i *responseOwnerInspector) dispatch() {
	i.inspect(i.event)
	i.event = nil
}

func (i *responseOwnerInspector) inspect(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
		return
	}
	var event struct {
		responsePayload
		Object   string          `json:"object"`
		Type     string          `json:"type"`
		Response responsePayload `json:"response"`
	}
	if json.Unmarshal(line, &event) != nil {
		return
	}
	payload := event.Response
	if event.Object == "response" {
		payload = event.responsePayload
	}
	if !i.usageRecorded && !payload.Usage.empty() && (event.Object == "response" || event.Type == "response.completed" || event.Type == "response.failed" || event.Type == "response.incomplete") {
		model := payload.Model
		if model == "" {
			model = i.model
		}
		serviceTier := payload.ServiceTier
		if serviceTier == "" {
			serviceTier = i.serviceTier
		}
		i.stats.recordUsage(i.thread, model, serviceTier, payload.Usage)
		i.usageRecorded = true
	}
	if !i.completed && (event.Type == "response.completed" || event.Object == "response" && payload.Status == "completed") {
		i.stats.completed(i.thread, i.metadata, time.Since(i.started))
		i.completed = true
	}
	if payload.ID != "" && (event.Type == "" || event.Type == "response.created") {
		if err := i.store.bind(affinityRef{kind: affinityResponse, value: payload.ID}, i.account); err != nil {
			i.log.Warn("response affinity save failed", "response", payload.ID, "account", i.account, "error", err)
		}
	}
}

func (s *server) authorized(r *http.Request) bool {
	if s.key == "" {
		return true
	}
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return s.validKey(presented)
}

func (s *server) validKey(presented string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.key)) == 1
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": "balancer_error"},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
