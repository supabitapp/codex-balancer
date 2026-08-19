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
  supports_websockets = true

  [model_providers.balancer.auth]
  command = "/bin/sh"
  args = ["-c", "exec /bin/cat \"$HOME/.codex/balancer-api-key\""]

Store the server key in ~/.codex/balancer-api-key with mode 600.

Flags:
`

func serverCmd(args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, serverHelp)
		fs.PrintDefaults()
	}
	addr := fs.String("addr", "127.0.0.1:8317", "address to listen on")
	path := fs.String("state", defaultStatePath(), "state database")
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

	store, err := openStateStore(*path)
	if err != nil {
		return err
	}
	defer store.Close()
	clientIDKey, err := store.clientIDKey()
	if err != nil {
		return fmt.Errorf("load client ID key: %w", err)
	}
	pool, err := loadPool(store)
	if err != nil {
		return err
	}

	log, logFile, err := newLogger(*jsonLogs, *plain, *logPath)
	if err != nil {
		return err
	}
	if logFile != nil {
		defer logFile.Close()
	}
	prices, err := newPriceCatalog(store)
	if err != nil {
		return fmt.Errorf("load price catalog: %w", err)
	}
	if prices.refreshIn(time.Now()) == 0 {
		refreshCtx, cancelRefresh := context.WithTimeout(context.Background(), priceRefreshTimeout)
		snapshot, refreshErr := prices.refresh(refreshCtx)
		cancelRefresh()
		if refreshErr != nil {
			log.Warn("price refresh failed", "error", refreshErr)
		} else {
			prices.install(snapshot)
		}
	}
	stats, err := newPersistentStats(store, prices.current(), func(err error) {
		log.Error("state write failed", "error", err)
	})
	if err != nil {
		return fmt.Errorf("load dashboard state: %w", err)
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &server{
		ctx:         ctx,
		pool:        pool,
		catalog:     newModelCatalog(),
		prices:      prices,
		stats:       stats,
		upstream:    *upstream,
		key:         *key,
		clientIDKey: clientIDKey,
		client:      newProxyClient(),
		log:         log,
		admission:   newAdmissionGate(maxActiveProxyRequests),
		resources:   newResourceMonitor(),
	}
	pool.watch(ctx, func(change poolChange) {
		log.Info("accounts updated", "added", change.added, "removed", change.removed, "updated", change.updated)
		stats.note("accounts updated", "", fmt.Sprintf("%d added, %d removed, %d updated", change.added, change.removed, change.updated))
		srv.catalog.invalidate()
		go func() {
			if err := srv.refreshModels(ctx, srv.catalog.version()); err != nil && ctx.Err() == nil {
				log.Warn("model refresh failed", "error", err)
			}
		}()
	}, func(err error) {
		log.Warn("account watch failed", "error", err)
		stats.note("account watch failed", "", err.Error())
	})

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

	go srv.watchUsage(ctx, *poll)
	go srv.watchModels(ctx)
	go srv.watchPriceCatalog(ctx)

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
		board := dashboard{pool: pool, catalog: srv.catalog, clientIDKey: clientIDKey, stats: stats, countries: &srv.countries, addr: listener.Addr().String()}
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

func defaultLogPath() string {
	return filepath.Join(homeDir(), ".codex-balancer", "server.log")
}

func newLogger(asJSON, stderr bool, path string) (*slog.Logger, io.Closer, error) {
	var writers []io.Writer
	var file io.WriteCloser
	if path != "" {
		var err error
		file, err = openRotatingLog(path, logRetentionDays, time.Now)
		if err != nil {
			return nil, nil, err
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
