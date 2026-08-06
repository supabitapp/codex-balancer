package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
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
	jsonLogs := fs.Bool("json", false, "emit logs as JSON")
	plain := fs.Bool("no-tui", false, "log to stderr instead of showing the dashboard")
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
	if len(pool.accounts) == 0 {
		return fmt.Errorf("no accounts in %s; add one with: codex-balancer accounts add", *path)
	}

	stats := newStats()
	log := newLogger(*jsonLogs, !*plain)
	sticky := newSticky()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &server{
		ctx:      ctx,
		pool:     pool,
		sticky:   sticky,
		stats:    stats,
		upstream: *upstream,
		key:      *key,
		client:   newProxyClient(),
		log:      log,
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 30 * time.Second,
	}

	go sweepSticky(ctx, sticky)
	go srv.watchUsage(ctx, *poll)

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdown)
	}()

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

	if !*plain {
		board := dashboard{pool: pool, stats: stats, addr: listener.Addr().String()}
		if _, err := tea.NewProgram(board, tea.WithContext(ctx)).Run(); err != nil &&
			!errors.Is(err, context.Canceled) {
			return err
		}
		httpServer.Shutdown(context.Background())
		return <-serving
	}

	log.Info("listening", "addr", listener.Addr().String(), "accounts", len(pool.accounts), "upstream", *upstream)
	return <-serving
}

func sweepSticky(ctx context.Context, s *Sticky) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep()
		}
	}
}

func newLogger(asJSON, quiet bool) *slog.Logger {
	switch {
	case quiet:
		return slog.New(slog.DiscardHandler)
	case asJSON:
		return slog.New(slog.NewJSONHandler(os.Stderr, nil))
	default:
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
}
