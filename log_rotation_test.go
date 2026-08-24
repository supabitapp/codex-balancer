package main

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogPolicy() logPolicy {
	return logPolicy{retentionDays: 7, maxFileSize: logMiB, maxTotalSize: 10 * logMiB}
}

func readCompressedLog(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	data, err := io.ReadAll(compressed)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRotatingLogRollsOverAtUTCMidnight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")
	now := time.Date(2026, time.August, 10, 23, 59, 0, 0, time.UTC)
	log, err := openRotatingLog(path, testLogPolicy(), func() time.Time { return now })
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

	if got := readCompressedLog(t, filepath.Join(dir, "server-2026-08-10.log.gz")); got != "before\n" {
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

	log, err := openRotatingLog(path, testLogPolicy(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	if got := readCompressedLog(t, firstArchive+".gz"); got != "first\n" {
		t.Fatalf("archived log = %q", got)
	}
	if got := readCompressedLog(t, filepath.Join(dir, "server-2026-08-10-2.log.gz")); got != "previous\n" {
		t.Fatalf("second archived log = %q", got)
	}
}

func TestRotatingLogRollsOverAtSizeLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	policy := logPolicy{retentionDays: 7, maxFileSize: 10, maxTotalSize: 100}
	log, err := openRotatingLog(path, policy, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	if _, err := log.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}

	if got := readCompressedLog(t, filepath.Join(dir, "server-2026-08-10.log.gz")); got != "12345678" {
		t.Fatalf("archived log = %q", got)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(current); got != "abc" {
		t.Fatalf("current log = %q", got)
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

	log, err := openRotatingLog(path, testLogPolicy(), func() time.Time { return now })
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
	if _, err := os.Stat(retained + ".gz"); err != nil {
		t.Fatalf("retained log: %v", err)
	}
	if _, err := os.Stat(unowned); err != nil {
		t.Fatalf("unowned log: %v", err)
	}
}

func TestRotatingLogRemovesOldestFilesOverTotalLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	archives := []string{
		filepath.Join(dir, "server-2026-08-09.log.gz"),
		filepath.Join(dir, "server-2026-08-10.log.gz"),
		filepath.Join(dir, "server-2026-08-11.log.gz"),
	}
	for index, archive := range archives {
		if err := os.WriteFile(archive, make([]byte, 20), 0o600); err != nil {
			t.Fatal(err)
		}
		modified := now.Add(time.Duration(index-3) * time.Hour)
		if err := os.Chtimes(archive, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	policy := logPolicy{retentionDays: 7, maxFileSize: 10, maxTotalSize: 45}

	log, err := openRotatingLog(path, policy, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	if _, err := os.Stat(archives[0]); !os.IsNotExist(err) {
		t.Fatalf("oldest log still exists: %v", err)
	}
	for _, archive := range archives[1:] {
		if _, err := os.Stat(archive); err != nil {
			t.Fatalf("retained log: %v", err)
		}
	}
}
