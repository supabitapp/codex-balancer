package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
)

// Invalid events may contain prompts, credentials, or arbitrary object keys.
// Report only fixed classifications and scalar framing metadata, never the
// event itself or err.Error() (JSON errors can include keys and number values).
func (t *responseObservation) invalidHTTPEvent(ctx context.Context, stage string, data []byte, err error) {
	if t == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("validation_stage", stage),
		attribute.Int("event_bytes", len(data)),
		attribute.Bool("json_valid", json.Valid(data)),
		attribute.Bool("utf8_valid", utf8.Valid(data)),
	}
	class := "validation_error"
	var syntax *json.SyntaxError
	var mismatch *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		class = "json_syntax"
		attrs = append(attrs, attribute.Int64("json_offset", syntax.Offset))
	case errors.As(err, &mismatch):
		class = "json_type_mismatch"
		// Keep even field names bounded: an upstream error must not turn a
		// dynamic key into a log field or a high-cardinality trace attribute.
		field, _, _ := strings.Cut(mismatch.Field, ".")
		switch field {
		case "type", "generate", "model", "reasoning", "service_tier", "previous_response_id", "client_metadata", "status", "status_code", "headers", "error", "response":
		default:
			field = "other"
		}
		kind := "other"
		switch mismatch.Value {
		case "array", "object", "bool", "string", "null", "number":
			kind = mismatch.Value
		default:
			if strings.HasPrefix(mismatch.Value, "number ") {
				kind = "number"
			}
		}
		attrs = append(attrs, attribute.String("json_field_root", field), attribute.String("json_value_kind", kind), attribute.Int64("json_offset", mismatch.Offset))
		if mismatch.Type != nil {
			attrs = append(attrs, attribute.String("expected_kind", mismatch.Type.Kind().String()))
		}
	default:
		reason := "other"
		if err != nil {
			switch err.Error() {
			case "JSON must be UTF-8":
				reason = "invalid_utf8"
			case "expected a JSON object":
				reason = "expected_object"
			case "invalid event type":
				reason = "invalid_event_type"
			case "terminal event has no response object":
				reason = "missing_terminal_response"
			case "conflicting terminal status":
				reason = "conflicting_terminal_status"
			case "unexpected trailing JSON":
				reason = "trailing_json"
			default:
				if strings.HasPrefix(err.Error(), "duplicate field ") {
					reason = "duplicate_field"
				}
			}
		}
		attrs = append(attrs, attribute.String("validation_reason", reason))
	}
	attrs = append(attrs, attribute.String("error_class", class))
	t.emit(ctx, slog.LevelWarn, "invalid_upstream_event", true, attrs...)
}
