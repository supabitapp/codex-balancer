package app

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPResponsesRejectsBeforeReadingSlowBody(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		limit   int
		auth    bool
		headers string
		status  int
	}{
		{"unauthorized", 1, false, "", 401},
		{"busy", 0, true, "", 503},
		{"encoding", 1, true, "Content-Encoding: gzip\r\n", 415},
		{"content type", 1, true, "Content-Type: text/plain\r\n", 415},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			srv, proxy := newWebSocketProxy(t, "http://127.0.0.1:1", nil)
			srv.admission = newAdmissionGate(scenario.limit)
			srv.lookupAPIKey = func(string) (string, bool, error) { return "test", scenario.auth, nil }
			conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxy.URL, "http://"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			// Send headers but no chunks. Rejection must not wait for body bytes
			// or net/http's normal request-body drain before writing its status.
			if _, err := fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: localhost\r\nTransfer-Encoding: chunked\r\n%s\r\n", scenario.headers); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
			if err != nil {
				t.Fatal(err)
			}
			body := readHTTPBody(t, response)
			if response.StatusCode != scenario.status {
				t.Fatalf("status=%d body=%s", response.StatusCode, body)
			}
			assertHTTPClean(t, srv)
		})
	}
}
