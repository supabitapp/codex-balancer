package app

import (
	"net"
	"os"
	"strconv"
	"testing"
)

func TestActivatedListener(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	file, err := base.(*net.TCPListener).File()
	if err != nil {
		base.Close()
		t.Fatal(err)
	}
	base.Close()

	processID := os.Getpid()
	listener, inherited, err := activatedListener(processID, strconv.Itoa(processID), "1", file.Fd())
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !inherited {
		t.Fatal("listener was not inherited")
	}
	defer listener.Close()

	accepted := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			err = connection.Close()
		}
		accepted <- err
	}()
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
}

func TestActivatedListenerIgnoresAnotherProcess(t *testing.T) {
	listener, inherited, err := activatedListener(os.Getpid(), strconv.Itoa(os.Getpid()+1), "1", systemdListenFD)
	if err != nil {
		t.Fatal(err)
	}
	if listener != nil || inherited {
		t.Fatalf("listener = %v, inherited = %t", listener, inherited)
	}
}

func TestActivatedListenerRejectsInvalidEnvironment(t *testing.T) {
	processID := os.Getpid()
	for _, test := range []struct {
		name      string
		listenPID string
		listenFDs string
	}{
		{name: "PID", listenPID: "invalid", listenFDs: "1"},
		{name: "descriptor count", listenPID: strconv.Itoa(processID), listenFDs: "invalid"},
		{name: "no descriptors", listenPID: strconv.Itoa(processID), listenFDs: "0"},
		{name: "many descriptors", listenPID: strconv.Itoa(processID), listenFDs: "2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, inherited, err := activatedListener(processID, test.listenPID, test.listenFDs, systemdListenFD)
			if err == nil {
				t.Fatalf("listener = %v, inherited = %t", listener, inherited)
			}
		})
	}
}
