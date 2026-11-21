package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestWebSocketAdvertisesMessageLimitWithinProxyLimits(t *testing.T) {
	for _, test := range []struct {
		name     string
		upstream string
		mode     fastMode
		want     int
	}{
		{"upstream", "134217728", fastModeDefault, 128 << 20},
		{"missing", "", fastModeDefault, maxWebSocketMessage},
		{"invalid", "lots", fastModeDefault, maxWebSocketMessage},
		{"above proxy", strconv.Itoa(1 << 30), fastModeDefault, maxWebSocketMessage},
		{"zero", "0", fastModeDefault, 0},
		{"negative", "-1", fastModeDefault, maxWebSocketMessage},
		{"below headroom", "1", fastModeOn, 0},
		{"fast rewrite", "134217728", fastModeOn, 128<<20 - fastModeRewriteHeadroom},
		{"standard rewrite", "134217728", fastModeOff, 128<<20 - fastModeRewriteHeadroom},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.upstream != "" {
					w.Header().Set(codexWebSocketMessageLimitHeader, test.upstream)
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				conn.Read(r.Context())
			}))
			defer upstream.Close()
			srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
			srv.fastMode.set(test.mode)
			conn, response := dialWebSocket(t, proxy.URL, codexWebSocketHeaders("session", "thread"))
			defer conn.CloseNow()
			if got := response.Header.Values(codexWebSocketMessageLimitHeader); fmt.Sprint(got) != fmt.Sprint([]string{strconv.Itoa(test.want)}) {
				t.Fatalf("advertised limit = %v, want %d", got, test.want)
			}
		})
	}
}

func TestFastModeRewriteStaysWithinHeadroom(t *testing.T) {
	original := []byte(`{"type":"response.create","input":[{"type":"message","content":"` + strings.Repeat(`if a < b && c > d { return \"<tag>\" }`, 4096) + `"}]}`)
	for _, mode := range []fastMode{fastModeOn, fastModeOff} {
		data, _, err := mode.override(original, "")
		if err != nil {
			t.Fatal(err)
		}
		if growth := len(data) - len(original); growth > fastModeRewriteHeadroom {
			t.Fatalf("%s grew request by %d bytes", mode, growth)
		}
	}
}
