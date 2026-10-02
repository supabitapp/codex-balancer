package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type httpResponseFailure struct {
	UpstreamStatus int
	CloseStatus    int
	Status         int
	Code           string
	Type           string
	Message        string
	Param          json.RawMessage
	Details        responseFields
}

func (f httpResponseFailure) errorObject() responseFields {
	object := responseFields{}
	for key, value := range f.Details {
		object[key] = value
	}
	for key, value := range map[string]string{"code": f.Code, "type": f.Type, "message": f.Message} {
		object[key], _ = json.Marshal(value)
	}
	object["param"] = f.Param
	return object
}

func (f httpResponseFailure) transientRateLimit() bool {
	var event websocketEnvelope
	event.Error.Code, event.Error.Type = f.Code, f.Type
	return websocketRejection(event) == websocketRejectionRateLimited && websocketErrorIs(event, "rate_limit_exceeded")
}

func responseFailure(fields responseFields, fallback int) httpResponseFailure {
	failure := httpResponseFailure{Status: fallback, Code: "upstream_error", Type: "upstream_error", Message: "upstream rejected response"}
	var envelope websocketEnvelope
	data, _ := json.Marshal(fields)
	json.Unmarshal(data, &envelope)
	if status := websocketStatus(envelope); status >= 400 && status <= 599 {
		failure.Status = status
	}
	failure.UpstreamStatus = failure.Status
	details := fields
	if nested, err := responseObject(fields["error"]); err == nil {
		details = nested
		failure.Details = nested
	} else if response, err := responseObject(fields["response"]); err == nil {
		if nested, err := responseObject(response["error"]); err == nil {
			details = nested
			failure.Details = nested
		}
	}
	for key, target := range map[string]*string{"code": &failure.Code, "type": &failure.Type, "message": &failure.Message} {
		if text, ok := responseString(details[key]); ok && text != "" {
			*target = text
		}
	}
	if param, exists := details["param"]; exists {
		failure.Param = param
	}
	if failure.Code == "upstream_error" && failure.Type == "usage_limit_reached" {
		failure.Code = failure.Type
	}
	switch {
	case websocketRejection(envelope) == websocketRejectionUnauthorized || failure.Status == 401:
		failure.Status = 503
	case websocketRejection(envelope) == websocketRejectionModelCapacity:
		failure.Status = 503
	case websocketRejection(envelope) == websocketRejectionConnectionLimit:
		failure.Status = 503
	case websocketRejection(envelope) == websocketRejectionRateLimited || websocketRejection(envelope) == websocketRejectionUsageLimit:
		failure.Status = 429
	case failure.Code == "model_not_found" || failure.Code == "model_not_available":
		if failure.Status == 0 {
			failure.Status = 404
		}
	case failure.Code == "context_length_exceeded" || failure.Code == "context_window_exceeded" || failure.Type == "invalid_request_error":
		if failure.Status == 0 {
			failure.Status = 400
		}
	}
	if failure.Status < 400 || failure.Status > 599 {
		failure.Status = 502
	}
	return failure
}

func (d *httpResponsesDownstream) fail(failure httpResponseFailure) (err error) {
	defer func() { observation(d.ctx).delivery(d.ctx, err, d.committed) }()
	if d.finished {
		return errResponseFinished
	}
	if !d.committed && failure.Status == 429 && failure.transientRateLimit() && (!d.accountBound || d.routeIdentified) {
		// Codex treats a raw HTTP 429 as terminal. A known transient throttle
		// can be retried by the client if the account boundary is preserved.
		failure.Status = 503
		if !d.retryAt.IsZero() && retryAfterHeader(d.writer.Header()).Before(d.retryAt) {
			d.writer.Header().Set("Retry-After", retryAfterSeconds(d.retryAt))
		}
	}
	d.finished = true
	// A failed WebSocket handshake is never an HTTP success, even if its
	// non-JSON response carried 200/204 (or any other non-error status).
	if failure.Status < 400 || failure.Status > 599 {
		failure.Status = 502
	}
	observation(d.ctx).failure(d.ctx, failure.Status, failure.Code, d.committed)
	if failure.Type == "" {
		failure.Type = "balancer_error"
	}
	if failure.Param == nil {
		failure.Param = json.RawMessage("null")
	}
	if d.committed {
		response := responseFields{}
		for key, value := range d.created {
			response[key] = value
		}
		response["status"] = json.RawMessage(`"failed"`)
		response["error"], _ = json.Marshal(failure.errorObject())
		fields := responseFields{"type": json.RawMessage(`"response.failed"`)}
		fields["response"], _ = json.Marshal(response)
		fields["sequence_number"], _ = json.Marshal(d.sequence)
		if err := d.writeEvent(fields); err != nil {
			return err
		}
		// A failed event is the terminal outcome; never invent a completed event.
		if err := d.writeDone(); err != nil {
			return err
		}
	} else {
		d.writer.Header().Set("Content-Type", "application/json")
		if failure.Status == 429 || failure.Status == 503 {
			if d.writer.Header().Get("Retry-After") == "" {
				d.writer.Header().Set("Retry-After", "1")
			}
		}
		envelope := map[string]any{"error": failure.errorObject()}
		if failure.UpstreamStatus != 0 && failure.UpstreamStatus != failure.Status {
			envelope["upstream_status"] = failure.UpstreamStatus
		}
		data, _ := json.Marshal(envelope)
		data = d.redact(data)
		d.writeDeadline()
		d.writer.WriteHeader(failure.Status)
		_, writeErr := d.writer.Write(data)
		observation(d.ctx).delivery(d.ctx, writeErr, d.committed)
	}
	return errResponseFinished
}

func (d *httpResponsesDownstream) collect(kind string, fields responseFields) error {
	if strings.HasPrefix(kind, "response.output_") || strings.HasPrefix(kind, "response.function_call_") || strings.HasPrefix(kind, "response.reasoning_") || strings.HasPrefix(kind, "response.custom_tool_") {
		d.outputSeen = true
	}
	switch kind {
	case "response.created":
		response, err := responseObject(fields["response"])
		if err != nil {
			return errors.New("response.created has no response object")
		}
		// Only identity metadata is a fallback. Terminal fields are authoritative.
		d.created = responseFields{}
		d.createdBytes = 0
		for _, key := range []string{"id", "object", "created_at", "model"} {
			if value := response[key]; value != nil {
				d.created[key] = value
				d.createdBytes += len(value)
			}
		}
	case "response.output_item.added", "response.output_item.done":
		var index int
		if fields["output_index"] == nil || json.Unmarshal(fields["output_index"], &index) != nil || index < 0 || index >= 65536 {
			return errors.New("output item has invalid output_index")
		}
		if _, err := responseObject(fields["item"]); err != nil {
			return errors.New("output item is not an object")
		}
		if kind == "response.output_item.added" {
			d.pending[index] = true
			return nil
		}
		item := fields["item"]
		d.itemBytes += len(item) - len(d.items[index])
		if d.itemBytes+d.createdBytes > maxHTTPOutput {
			return fmt.Errorf("retained output exceeds %d MiB", maxHTTPOutput>>20)
		}
		d.items[index] = item
		delete(d.pending, index)
	}
	return nil
}

func (d *httpResponsesDownstream) result(fields responseFields) ([]byte, error) {
	response, err := responseObject(fields["response"])
	if err != nil {
		return nil, errors.New("terminal response is not an object")
	}
	for key, value := range d.created {
		if response[key] == nil {
			response[key] = value
		}
	}
	if id, ok := responseString(response["id"]); !ok || id == "" {
		return nil, errors.New("terminal response has no response id")
	}
	if response["object"] == nil {
		response["object"] = json.RawMessage(`"response"`)
	}
	if response["status"] == nil {
		kind, _ := responseString(fields["type"])
		response["status"], _ = json.Marshal(kind[len("response."):])
	}
	var terminalOutput []json.RawMessage
	if raw, exists := response["output"]; exists {
		if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &terminalOutput) != nil {
			return nil, errors.New("terminal output is not an array")
		}
	}
	// An empty terminal output is not complete when item events supplied the
	// output. Prefer a non-empty terminal array; otherwise use completed items.
	if len(terminalOutput) == 0 && (response["output"] == nil || d.outputSeen) {
		if len(d.pending) != 0 || d.outputSeen && len(d.items) == 0 {
			return nil, errors.New("terminal response omitted unfinished output items")
		}
		output := make([]json.RawMessage, len(d.items))
		for index, item := range d.items {
			if index >= len(output) {
				return nil, errors.New("terminal response omitted output indexes")
			}
			output[index] = item
		}
		response["output"], _ = json.Marshal(output)
	}
	data, err := json.Marshal(response)
	if len(data) > maxHTTPOutput {
		return nil, fmt.Errorf("response exceeds %d MiB", maxHTTPOutput>>20)
	}
	return data, err
}
