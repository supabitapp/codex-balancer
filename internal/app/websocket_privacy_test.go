package app

import (
	"encoding/json"
	"testing"

	"github.com/coder/websocket"
)

func TestForceResponseStorageOff(t *testing.T) {
	for _, request := range []string{
		`{"type":"response.create","model":"gpt-5.6-sol"}`,
		`{"type":"response.create","model":"gpt-5.6-sol","store":true}`,
	} {
		message := websocketMessage{kind: websocket.MessageText, data: []byte(request)}
		var event websocketEnvelope
		if err := json.Unmarshal(message.data, &event); err != nil {
			t.Fatal(err)
		}
		private, err := forceResponseStorageOff(message, event.Store)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(private.data, &payload); err != nil {
			t.Fatal(err)
		}
		if string(payload["store"]) != "false" {
			t.Fatalf("private request = %s", private.data)
		}
	}
}

func TestForceResponseStorageOffLeavesPrivateRequestUntouched(t *testing.T) {
	request := []byte(`{"type": "response.create", "store": false}`)
	store := false
	private, err := forceResponseStorageOff(websocketMessage{kind: websocket.MessageText, data: request}, &store)
	if err != nil {
		t.Fatal(err)
	}
	if string(private.data) != string(request) {
		t.Fatalf("private request changed to %s", private.data)
	}
}

func TestWebSocketForcesResponseStorageOff(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, request websocketEnvelope) {
		if request.Store == nil || *request.Store {
			t.Errorf("upstream store = %v, want false", request.Store)
		}
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("account", 0)})
	conn, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "thread"))
	defer conn.CloseNow()
	completeWebSocketTurn(t, conn, map[string]any{
		"type":  "response.create",
		"input": []any{},
		"store": true,
	})
}
