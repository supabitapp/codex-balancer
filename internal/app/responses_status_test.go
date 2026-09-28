package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestResponseEventStatusDecoding(t *testing.T) {
	for _, test := range []struct {
		name, data string
		status     int
		rejection  websocketRejectionKind
	}{
		{"absent", `{"type":"response.reasoning_summary_part.done"}`, 0, websocketRejectionNone},
		{"null", `{"type":"response.reasoning_summary_part.done","status":null}`, 0, websocketRejectionNone},
		{"lifecycle", `{"type":"response.reasoning_summary_part.done","status":"completed"}`, 0, websocketRejectionNone},
		{"future lifecycle", `{"type":"response.reasoning_summary_part.done","status":"future_state"}`, 0, websocketRejectionNone},
		{"numeric string is not HTTP status", `{"type":"response.reasoning_summary_part.done","status":"401"}`, 0, websocketRejectionNone},
		{"unauthorized", `{"type":"error","status":401}`, 401, websocketRejectionUnauthorized},
		{"rate limited", `{"type":"error","status":429}`, 429, websocketRejectionRateLimited},
		{"status code", `{"type":"error","status_code":401}`, 401, websocketRejectionUnauthorized},
		{"status code with lifecycle", `{"type":"error","status":"failed","status_code":429}`, 429, websocketRejectionRateLimited},
		{"status precedence", `{"type":"error","status":401,"status_code":429}`, 401, websocketRejectionUnauthorized},
		{"error code with lifecycle", `{"type":"error","status":"failed","error":{"code":"usage_limit_reached"}}`, 0, websocketRejectionUsageLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			var event websocketEnvelope
			if err := json.Unmarshal([]byte(test.data), &event); err != nil {
				t.Fatal(err)
			}
			if got := websocketStatus(event); got != test.status {
				t.Fatalf("status=%d, want %d", got, test.status)
			}
			if got := websocketRejection(event); got != test.rejection {
				t.Fatalf("rejection=%q, want %q", got, test.rejection)
			}
		})
	}
	for _, value := range []string{`true`, `{}`, `[]`, `1.5`, `987654321098765432109876543210`} {
		t.Run("invalid/"+value, func(t *testing.T) {
			var event websocketEnvelope
			err := json.Unmarshal([]byte(`{"status":`+value+`}`), &event)
			var mismatch *json.UnmarshalTypeError
			if !errors.As(err, &mismatch) || mismatch.Field != "status" {
				t.Fatalf("expected status type mismatch, got %v", err)
			}
		})
	}
	var reused websocketEnvelope
	for _, data := range []string{`{"status":401}`, `{"status":"completed"}`} {
		if err := json.Unmarshal([]byte(data), &reused); err != nil {
			t.Fatal(err)
		}
	}
	if websocketStatus(reused) != 0 {
		t.Fatal("lifecycle string retained a stale HTTP status")
	}
}

const summaryPartWithStatus = `{"type":"response.reasoning_summary_part.done","item_id":"rs_test","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"Summary"},"status":"completed","sequence_number":1,"unknown":9007199254740993}`

func TestHTTPResponsesForwardsSummaryLifecycleStatus(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := newHTTPUpstream(t, func(_ *http.Request, conn *testResponseStream, _ []byte) {
				sendHTTPEvents(t, conn, httpCreatedEvent, summaryPartWithStatus,
					`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_test","type":"reasoning","summary":[{"type":"summary_text","text":"Summary"}]}}`,
					httpCompletedEvent)
			})
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
			srv.admission = newAdmissionGate(1)
			logs, _, _ := observeTestServer(t, srv)
			response := postResponse(t, proxy.URL, fmt.Sprintf(`{"model":"m","stream":%t}`, stream), nil)
			body := readHTTPBody(t, response)
			assertHTTPClean(t, srv)
			if response.StatusCode != 200 || strings.Contains(body, "invalid_upstream_response") || strings.Contains(logs.String(), "invalid_upstream_event") {
				t.Fatalf("status=%d body=%s logs=%s", response.StatusCode, body, logs.String())
			}
			if stream {
				found := false
				for _, line := range strings.Split(body, "\n") {
					if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, "response.reasoning_summary_part.done") {
						continue
					}
					found = true
					fields, err := responseObject([]byte(strings.TrimPrefix(line, "data: ")))
					if err != nil || string(fields["status"]) != `"completed"` || string(fields["unknown"]) != "9007199254740993" {
						t.Fatalf("summary event changed: %s (%v)", line, err)
					}
				}
				if !found || !strings.Contains(body, "response.completed") {
					t.Fatalf("stream did not deliver summary and completion: %s", body)
				}
			} else if !strings.Contains(body, `"status":"completed"`) {
				t.Fatalf("JSON response did not complete: %s", body)
			}
		})
	}
}

func TestWebSocketForwardsSummaryLifecycleStatus(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		sendHTTPEvents(t, conn, httpCreatedEvent, summaryPartWithStatus, httpCompletedEvent)
	})
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	conn, _ := dialWebSocket(t, proxy.URL, nil)
	defer conn.CloseNow()
	writeWebSocketEvent(t, conn, map[string]any{"type": "response.create", "model": "m"})
	if event := readWebSocketEvent(t, conn); event.Type != "response.created" {
		t.Fatalf("unexpected event %q", event.Type)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil || string(data) != summaryPartWithStatus {
		t.Fatalf("summary event changed: %s (%v)", data, err)
	}
	if event := readWebSocketEvent(t, conn); event.Type != "response.completed" {
		t.Fatalf("unexpected event %q", event.Type)
	}
}
