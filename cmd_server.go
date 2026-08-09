package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
)

const serverHelp = `Serve the balancing proxy.

Usage:
  codex-balancer server [flags]

Point Codex at it by adding to ~/.codex/config.toml:

  model_provider = "balancer"
  [model_providers.balancer]
  name = "OpenAI"
  base_url = "http://127.0.0.1:8317/v1"
  requires_openai_auth = true
  supports_websockets = true

then authenticate once with: codex login --with-api-key

Flags:
`

func serverCmd(args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, serverHelp)
		fs.PrintDefaults()
	}
	addr := fs.String("addr", "127.0.0.1:8317", "address to listen on")
	path := fs.String("accounts", defaultAccountsPath(), "account pool file")
	upstream := fs.String("upstream", "https://chatgpt.com/backend-api/codex", "upstream Codex base URL")
	key := fs.String("key", os.Getenv("CODEX_BALANCER_KEY"), "bearer key clients must present (env CODEX_BALANCER_KEY)")
	insecure := fs.Bool("no-auth", false, "serve without a bearer key; any local process can spend your quota")
	jsonLogs := fs.Bool("json", false, "format logs as JSON")
	plain := fs.Bool("no-tui", false, "show logs on stderr instead of the dashboard")
	logPath := fs.String("log-file", defaultLogPath(), "log file; empty disables file logging")
	poll := fs.Duration("poll", 2*time.Minute, "how often to read each account's limits upstream; 0 turns it off")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *key == "" && !*insecure {
		return errors.New("set -key or CODEX_BALANCER_KEY, or pass -no-auth to accept unauthenticated clients")
	}
	if *insecure {
		*key = ""
	}

	pool, err := loadPool(*path)
	if err != nil {
		return err
	}

	stats := newStats()
	log, logFile, err := newLogger(*jsonLogs, *plain, *logPath)
	if err != nil {
		return err
	}
	if logFile != nil {
		defer logFile.Close()
	}
	affinity, err := newAffinityStore(defaultAffinityPath())
	if err != nil {
		return fmt.Errorf("load affinity bindings: %w", err)
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &server{
		ctx:       ctx,
		pool:      pool,
		affinity:  affinity,
		stats:     stats,
		upstream:  *upstream,
		key:       *key,
		client:    newProxyClient(),
		log:       log,
		admission: newAdmissionGate(maxActiveProxyRequests),
	}
	if err := pool.watch(ctx, func(change poolChange) {
		log.Info("accounts updated", "added", change.added, "removed", change.removed, "updated", change.updated)
		stats.note("accounts updated", "", fmt.Sprintf("%d added, %d removed, %d updated", change.added, change.removed, change.updated))
	}, func(err error) {
		log.Warn("account watch failed", "error", err)
		stats.note("account watch failed", "", err.Error())
	}); err != nil {
		return fmt.Errorf("watch accounts: %w", err)
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	shutdownDone := make(chan struct{})
	var shutdownOnce sync.Once
	startShutdown := func() {
		shutdownOnce.Do(func() {
			go func() {
				drainServer(httpServer, srv.admission, cancel)
				close(shutdownDone)
			}()
		})
	}
	go func() {
		<-signalCtx.Done()
		startShutdown()
	}()

	srv.pollAllUsage(ctx)
	if ctx.Err() != nil {
		return nil
	}

	go sweepAffinity(ctx, affinity, log)
	go srv.watchUsage(ctx, *poll)

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}

	serving := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serving <- err
	}()
	log.Info("listening", "addr", listener.Addr().String(), "accounts", pool.count(), "upstream", *upstream, "log_file", *logPath)

	if !*plain {
		board := dashboard{pool: pool, stats: stats, addr: listener.Addr().String()}
		if _, err := tea.NewProgram(board, tea.WithContext(signalCtx)).Run(); err != nil &&
			!errors.Is(err, context.Canceled) {
			return err
		}
		startShutdown()
		err := <-serving
		<-shutdownDone
		return err
	}

	err = <-serving
	if signalCtx.Err() != nil {
		<-shutdownDone
	}
	return err
}

func drainServer(httpServer *http.Server, admission *admissionGate, cancel context.CancelFunc) {
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	admission.beginDrain()
	httpServer.Shutdown(shutdown)
	admission.wait(shutdown)
	cancel()
}

func sweepAffinity(ctx context.Context, s *AffinityStore, log *slog.Logger) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.sweep(); err != nil {
				log.Warn("affinity binding sweep failed", "error", err)
			}
		}
	}
}

func defaultLogPath() string {
	return filepath.Join(homeDir(), ".codex-balancer", "server.log")
}

func newLogger(asJSON, stderr bool, path string) (*slog.Logger, *os.File, error) {
	var writers []io.Writer
	var file *os.File
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, nil, fmt.Errorf("create log directory: %w", err)
		}
		var err error
		file, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file: %w", err)
		}
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			return nil, nil, fmt.Errorf("secure log file: %w", err)
		}
		writers = append(writers, file)
	}
	if stderr {
		writers = append(writers, os.Stderr)
	}
	if len(writers) == 0 {
		return slog.New(slog.DiscardHandler), nil, nil
	}
	options := &slog.HandlerOptions{Level: slog.LevelDebug}
	output := io.MultiWriter(writers...)
	if asJSON {
		return slog.New(slog.NewJSONHandler(output, options)), file, nil
	}
	return slog.New(slog.NewTextHandler(output, options)), file, nil
}
