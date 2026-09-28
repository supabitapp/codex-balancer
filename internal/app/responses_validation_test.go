package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPResponsesValidationDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name, event, stage, class, reason, field, valueKind string
	}{
		{"syntax", `{"type":"response.reasoning_summary_part.done","part": SENSITIVE_VALUE}`, "prepare", "json_syntax", "", "", ""},
		{"duplicate key", `{"type":"response.reasoning_summary_part.done","SENSITIVE_KEY":1,"SENSITIVE_KEY":2}`, "prepare", "validation_error", "duplicate_field", "", ""},
		{"invalid type", `{"type":"response.created\n"}`, "prepare", "validation_error", "invalid_event_type", "", ""},
		{"terminal object", `{"type":"response.completed","response":"SENSITIVE_RESPONSE"}`, "prepare", "validation_error", "missing_terminal_response", "", ""},
		{"terminal status", `{"type":"response.completed","response":{"status":"SENSITIVE_STATUS"}}`, "prepare", "validation_error", "conflicting_terminal_status", "", ""},
		{"envelope type", `{"type":"response.reasoning_summary_part.done","status":"SENSITIVE_STATUS","part":{"text":"SENSITIVE_TEXT token-a"}}`, "envelope_decode", "json_type_mismatch", "", "status", "string"},
		{"envelope number", `{"type":"response.reasoning_summary_part.done","status":987654321098765432109876543210}`, "envelope_decode", "json_type_mismatch", "", "status", "number"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", httpCreatedEvent, test.event)
			}))
			defer upstream.Close()
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
			srv.admission = newAdmissionGate(1)
			logs, exporter, _ := observeTestServer(t, srv)
			response := postResponse(t, proxy.URL, `{"model":"m","stream":true}`, nil)
			body := readHTTPBody(t, response)
			assertHTTPClean(t, srv)
			if response.StatusCode != 200 || !strings.Contains(body, "invalid_upstream_response") || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, calls.Load(), body)
			}
			found := 0
			for _, record := range observationRecords(t, logs) {
				if record["stage"] != "invalid_upstream_event" {
					continue
				}
				found++
				if record["validation_stage"] != test.stage || record["error_class"] != test.class || record["request_id"] != response.Header.Get(responseRequestIDHeader) || record["level"] != "WARN" {
					t.Fatalf("incorrect classification/correlation: %v", record)
				}
				for key, want := range map[string]string{"validation_reason": test.reason, "json_field_root": test.field, "json_value_kind": test.valueKind} {
					if want != "" && record[key] != want {
						t.Errorf("%s=%v, want %q", key, record[key], want)
					}
				}
				if record["event_bytes"].(float64) <= 0 || record["json_valid"] != json.Valid([]byte(test.event)) || record["utf8_valid"] != true {
					t.Fatalf("incorrect framing metadata: %v", record)
				}
				if test.class == "json_type_mismatch" && record["expected_kind"] != "int" {
					t.Errorf("expected kind: %v", record)
				}
			}
			if found != 1 {
				t.Fatalf("diagnostic records=%d", found)
			}
			payload := observationPayload(t, logs, exporter.GetSpans())
			for _, secret := range []string{"SENSITIVE_KEY", "SENSITIVE_VALUE", "SENSITIVE_TYPE", "SENSITIVE_RESPONSE", "SENSITIVE_STATUS", "SENSITIVE_TEXT", "token-a", "987654321098765432109876543210"} {
				if strings.Contains(payload, secret) {
					t.Errorf("telemetry exposed %s", secret)
				}
			}
		})
	}
}

func TestHTTPResponsesValidSummaryPartDoesNotLogDiagnostic(t *testing.T) {
	const event = `{"type":"response.reasoning_summary_part.done","item_id":"rs_test","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"SENSITIVE_TEXT"},"sequence_number":1}`
	upstream := newHTTPUpstream(t, func(_ *http.Request, stream *testResponseStream, _ []byte) {
		sendHTTPEvents(t, stream, httpCreatedEvent, event, httpCompletedEvent)
	})
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	srv.admission = newAdmissionGate(1)
	logs, _, _ := observeTestServer(t, srv)
	response := postResponse(t, proxy.URL, `{"model":"m","stream":true}`, nil)
	body := readHTTPBody(t, response)
	assertHTTPClean(t, srv)
	if response.StatusCode != 200 || !strings.Contains(body, "response.reasoning_summary_part.done") || !strings.Contains(body, "response.completed") || strings.Contains(logs.String(), "invalid_upstream_event") || strings.Contains(logs.String(), "SENSITIVE_TEXT") {
		t.Fatalf("status=%d body=%s logs=%s", response.StatusCode, body, logs.String())
	}
}

func TestHTTPResponsesValidationDiagnosticsUnknownErrorsStayPrivate(t *testing.T) {
	srv := newTestServer(t, nil)
	logs, exporter, _ := observeTestServer(t, srv)
	handler := srv.observedResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed := observation(r.Context())
		observed.invalidHTTPEvent(r.Context(), "envelope_decode", []byte(`{}`), &json.UnmarshalTypeError{
			Field: "SENSITIVE_FIELD.SENSITIVE_CHILD", Value: "SENSITIVE_VALUE", Type: reflect.TypeFor[int](),
		})
		observed.invalidHTTPEvent(r.Context(), "prepare", []byte(`{}`), errors.New("SENSITIVE_ERROR"))
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/responses", nil))
	payload := observationPayload(t, logs, exporter.GetSpans())
	if strings.Contains(payload, "SENSITIVE_") || !strings.Contains(logs.String(), `"json_field_root":"other"`) || !strings.Contains(logs.String(), `"json_value_kind":"other"`) || !strings.Contains(logs.String(), `"validation_reason":"other"`) {
		t.Fatal("unknown errors/fields were not safely classified")
	}
	// Instrumentation must remain optional.
	var observed *responseObservation
	observed.invalidHTTPEvent(context.Background(), "prepare", nil, nil)
}
