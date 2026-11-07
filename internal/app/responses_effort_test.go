package app

import (
	"testing"

	"github.com/coder/websocket"
)

func TestWebSocketThreadShowsEffortFromConfigurationUpdates(t *testing.T) {
	upstream := newWebSocketUpstream(t, func(_ string, conn *websocket.Conn, _ websocketEnvelope) {
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("owner", 0)})
	conn, _ := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "thread"))
	defer conn.CloseNow()
	update := map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": "xhigh"}}
	for _, step := range []struct {
		name    string
		request map[string]any
		want    string
	}{
		{"update overrides the pinned request effort", map[string]any{"reasoning": map[string]any{"effort": "low"}, "input": []any{update}}, "xhigh"},
		{"incremental turn keeps the update", map[string]any{"reasoning": map[string]any{"effort": "low"}, "previous_response_id": "r1", "input": []any{}}, "xhigh"},
		{"changed request effort replaces the update", map[string]any{"reasoning": map[string]any{"effort": "medium"}, "previous_response_id": "r2", "input": []any{}}, "medium"},
		{"full input without an update uses the request effort", map[string]any{"reasoning": map[string]any{"effort": "low"}, "input": "hello"}, "low"},
	} {
		step.request["type"] = "response.create"
		completeWebSocketTurn(t, conn, step.request)
		threads := srv.stats.snapshot().Threads
		if len(threads) != 1 || threads[0].Effort != step.want {
			t.Fatalf("%s: threads = %+v, want effort %q", step.name, threads, step.want)
		}
	}
}
