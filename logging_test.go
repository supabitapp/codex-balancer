package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestTUILoggerWritesDebugLogFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	path := filepath.Join(dir, "server.log")
	logger, file, err := newLogger(false, false, path)
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("routing candidate", "account", "acct-a")
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("level=DEBUG")) || !bytes.Contains(data, []byte("account=acct-a")) {
		t.Fatalf("log = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %o, want 600", info.Mode().Perm())
	}
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("log directory mode = %o, want 700", info.Mode().Perm())
	}
}

func TestRoutingLogExplainsPinnedCoolingAccount(t *testing.T) {
	reset := time.Now().Add(4 * 24 * time.Hour).Truncate(time.Second)
	headers := http.Header{}
	headers.Set("x-codex-secondary-primary-used-percent", "100")
	headers.Set("x-codex-secondary-primary-window-minutes", "10080")
	headers.Set("x-codex-secondary-primary-reset-at", strconv.FormatInt(reset.Unix(), 10))
	account := accountFor("acct-a")
	account.observe(headers)
	account.rateLimited(headers, 0)

	var output bytes.Buffer
	s := &server{
		pool: &Pool{accounts: []*Account{account}},
		log:  slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if got := s.pickAccount("thread-1", "acct-a", nil, 0, transportHTTP); got != account {
		t.Fatalf("selected account = %v, want acct-a", got)
	}

	decoder := json.NewDecoder(&output)
	for {
		var entry struct {
			Message   string        `json:"msg"`
			Transport transport     `json:"transport"`
			Thread    string        `json:"thread"`
			Account   string        `json:"account"`
			Status    accountStatus `json:"status"`
			Selected  bool          `json:"selected"`
			Pinned    bool          `json:"pinned"`
			Skipped   bool          `json:"skipped"`
			Cooldown  time.Time     `json:"cooldown_until"`
			Secondary struct {
				Known         bool      `json:"known"`
				UsedPercent   float64   `json:"used_percent"`
				WindowMinutes int       `json:"window_minutes"`
				ResetAt       time.Time `json:"reset_at"`
			} `json:"secondary"`
		}
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if entry.Message != "routing candidate" {
			continue
		}
		if entry.Transport != transportHTTP || entry.Thread != "thread-1" {
			t.Fatalf("candidate route context = %+v", entry)
		}
		if entry.Account != "acct-a" || entry.Status != accountCooling || !entry.Selected || !entry.Pinned || entry.Skipped {
			t.Fatalf("candidate selection = %+v", entry)
		}
		if entry.Cooldown.IsZero() || !entry.Secondary.Known || entry.Secondary.UsedPercent != 100 ||
			entry.Secondary.WindowMinutes != 10080 || !entry.Secondary.ResetAt.Equal(reset) {
			t.Fatalf("candidate limits = %+v", entry)
		}
		return
	}
	t.Fatal("routing candidate log not found")
}
