package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

const systemdListenFD = 3

func serverListener(address string) (net.Listener, error) {
	listener, inherited, err := activatedListener(
		os.Getpid(),
		os.Getenv("LISTEN_PID"),
		os.Getenv("LISTEN_FDS"),
		systemdListenFD,
	)
	if err != nil {
		return nil, fmt.Errorf("systemd socket activation: %w", err)
	}
	if inherited {
		os.Unsetenv("LISTEN_PID")
		os.Unsetenv("LISTEN_FDS")
		os.Unsetenv("LISTEN_FDNAMES")
		return listener, nil
	}
	return net.Listen("tcp", address)
}

func activatedListener(processID int, listenPID, listenFDs string, fd uintptr) (net.Listener, bool, error) {
	if listenPID == "" && listenFDs == "" {
		return nil, false, nil
	}
	owner, err := strconv.Atoi(listenPID)
	if err != nil {
		return nil, false, fmt.Errorf("invalid LISTEN_PID %q", listenPID)
	}
	if owner != processID {
		return nil, false, nil
	}
	count, err := strconv.Atoi(listenFDs)
	if err != nil {
		return nil, false, fmt.Errorf("invalid LISTEN_FDS %q", listenFDs)
	}
	if count != 1 {
		return nil, false, fmt.Errorf("received %d sockets, want 1", count)
	}
	file := os.NewFile(fd, "codex-balancer.socket")
	if file == nil {
		return nil, false, fmt.Errorf("socket descriptor %d is invalid", fd)
	}
	listener, err := net.FileListener(file)
	closeErr := file.Close()
	if err != nil {
		return nil, false, err
	}
	if closeErr != nil {
		listener.Close()
		return nil, false, closeErr
	}
	return listener, true, nil
}
