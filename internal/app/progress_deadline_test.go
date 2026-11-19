package app

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
}

func (l pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l pipeListener) Close() error {
	close(l.closed)
	return nil
}

func (l pipeListener) Addr() net.Addr { return &net.TCPAddr{} }

func uploadInChunks(t *testing.T, chunks int, gap time.Duration) (int, time.Duration) {
	t.Helper()
	listener := pipeListener{conns: make(chan net.Conn, 1), closed: make(chan struct{})}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = withProgressReadDeadline(http.NewResponseController(w), r.Body)
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})}
	go server.Serve(listener)
	client, peer := net.Pipe()
	listener.conns <- peer
	written := make(chan struct{})
	defer func() {
		client.Close()
		<-written
		server.Close()
		time.Sleep(time.Second)
	}()

	started := time.Now()
	go func() {
		defer close(written)
		fmt.Fprintf(client, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", chunks)
		for range chunks {
			time.Sleep(gap)
			if _, err := client.Write([]byte("x")); err != nil {
				return
			}
		}
	}()
	response, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode, time.Since(started)
}

func TestBodyReadDeadlineExtendsWhileUploadProgresses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		status, elapsed := uploadInChunks(t, 6, 20*time.Second)
		if status != http.StatusOK || elapsed < 2*time.Minute {
			t.Fatalf("status %d after %s, want 200 after a 2m steady upload", status, elapsed)
		}
	})
}

func TestBodyReadDeadlineFailsStalledUpload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		status, _ := uploadInChunks(t, 2, httpResponseIOWait+time.Second)
		if status != http.StatusBadRequest {
			t.Fatalf("status %d, want 400 for an upload idle past %s", status, httpResponseIOWait)
		}
	})
}

func TestBodyReadDeadlineCapsTotalUploadTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gap := 20 * time.Second
		chunks := int(httpBodyReadLimit/gap) + 2
		status, elapsed := uploadInChunks(t, chunks, gap)
		if status != http.StatusBadRequest || elapsed > httpBodyReadLimit+gap {
			t.Fatalf("status %d after %s, want 400 by %s", status, elapsed, httpBodyReadLimit)
		}
	})
}
