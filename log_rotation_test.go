package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotatingLogRollsOverAtUTCMidnight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")
	now := time.Date(2026, time.August, 10, 23, 59, 0, 0, time.UTC)
	log, err := openRotatingLog(path, 7, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	if _, err := log.Write([]byte("before\n")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := log.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}

	archived, err := os.ReadFile(filepath.Join(dir, "server-2026-08-10.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(archived); got != "before\n" {
		t.Fatalf("archived log = %q", got)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(current); got != "after\n" {
		t.Fatalf("current log = %q", got)
	}
}

func TestRotatingLogRollsOverCurrentLogOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")
	firstArchive := filepath.Join(dir, "server-2026-08-10.log")
	previous := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(firstArchive, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("previous\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, previous, previous); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 10, 18, 0, 0, 0, time.UTC)

	log, err := openRotatingLog(path, 7, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	archived, err := os.ReadFile(firstArchive)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(archived); got != "first\n" {
		t.Fatalf("archived log = %q", got)
	}
	archived, err = os.ReadFile(filepath.Join(dir, "server-2026-08-10-2.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(archived); got != "previous\n" {
		t.Fatalf("second archived log = %q", got)
	}
}

func TestRotatingLogKeepsSevenUTCDays(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")
	expired := filepath.Join(dir, "server-2026-08-04.log")
	expiredSequence := filepath.Join(dir, "server-2026-08-04-2.log")
	retained := filepath.Join(dir, "server-2026-08-05.log")
	unowned := filepath.Join(dir, "server-2026-08-04-backup.log")
	for _, name := range []string{expired, expiredSequence, retained, unowned} {
		if err := os.WriteFile(name, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)

	log, err := openRotatingLog(path, 7, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("expired log still exists: %v", err)
	}
	if _, err := os.Stat(expiredSequence); !os.IsNotExist(err) {
		t.Fatalf("expired sequence still exists: %v", err)
	}
	if _, err := os.Stat(retained); err != nil {
		t.Fatalf("retained log: %v", err)
	}
	if _, err := os.Stat(unowned); err != nil {
		t.Fatalf("unowned log: %v", err)
	}
}
