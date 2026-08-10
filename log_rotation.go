package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	logRetentionDays = 7
	logDateLayout    = "2006-01-02"
)

type rotatingLog struct {
	mu            sync.Mutex
	path          string
	retentionDays int
	now           func() time.Time
	day           string
	file          *os.File
	closed        bool
}

func openRotatingLog(path string, retentionDays int, now func() time.Time) (*rotatingLog, error) {
	if retentionDays < 1 {
		return nil, errors.New("log retention must be at least one day")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	log := &rotatingLog{
		path:          path,
		retentionDays: retentionDays,
		now:           now,
		day:           now().UTC().Format(logDateLayout),
	}
	if err := log.rollOverExistingFile(); err != nil {
		return nil, err
	}
	if err := log.removeExpired(); err != nil {
		return nil, err
	}
	if err := log.open(); err != nil {
		return nil, err
	}
	return log, nil
}

func (l *rotatingLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, os.ErrClosed
	}
	day := l.now().UTC().Format(logDateLayout)
	if day != l.day {
		if err := l.rollOver(day); err != nil {
			return 0, err
		}
	}
	if l.file == nil {
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	return l.file.Write(data)
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.file == nil {
		return nil
	}
	return l.file.Close()
}

func (l *rotatingLog) rollOver(day string) error {
	if err := l.file.Close(); err != nil {
		l.file = nil
		return l.reopen(fmt.Errorf("close log file: %w", err))
	}
	l.file = nil
	if err := l.archive(l.day); err != nil {
		return l.reopen(err)
	}
	l.day = day
	if err := l.open(); err != nil {
		return err
	}
	if err := l.removeExpired(); err != nil {
		return err
	}
	return nil
}

func (l *rotatingLog) rollOverExistingFile() error {
	info, err := os.Stat(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat log file: %w", err)
	}
	if info.Size() == 0 {
		return nil
	}
	return l.archive(info.ModTime().UTC().Format(logDateLayout))
}

func (l *rotatingLog) archive(day string) error {
	path, err := l.nextArchivePath(day)
	if err != nil {
		return err
	}
	if err := os.Rename(l.path, path); err != nil {
		return fmt.Errorf("rotate log file: %w", err)
	}
	return nil
}

func (l *rotatingLog) nextArchivePath(day string) (string, error) {
	extension := filepath.Ext(l.path)
	stem := strings.TrimSuffix(l.path, extension)
	for sequence := 1; ; sequence++ {
		suffix := ""
		if sequence > 1 {
			suffix = "-" + strconv.Itoa(sequence)
		}
		path := stem + "-" + day + suffix + extension
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", fmt.Errorf("stat rotated log file: %w", err)
		}
	}
}

func (l *rotatingLog) removeExpired() error {
	directory := filepath.Dir(l.path)
	extension := filepath.Ext(l.path)
	stem := strings.TrimSuffix(filepath.Base(l.path), extension)
	prefix := stem + "-"
	cutoff := l.now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1-l.retentionDays)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read log directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, extension) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat rotated log file: %w", err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		date := strings.TrimSuffix(strings.TrimPrefix(name, prefix), extension)
		if len(date) < len(logDateLayout) {
			continue
		}
		sequence := date[len(logDateLayout):]
		if sequence != "" {
			if !strings.HasPrefix(sequence, "-") {
				continue
			}
			number, err := strconv.Atoi(strings.TrimPrefix(sequence, "-"))
			if err != nil || number < 2 {
				continue
			}
		}
		day, err := time.Parse(logDateLayout, date[:len(logDateLayout)])
		if err != nil || !day.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(directory, name)); err != nil {
			return fmt.Errorf("remove expired log file: %w", err)
		}
	}
	return nil
}

func (l *rotatingLog) open() error {
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("secure log file: %w", err)
	}
	l.file = file
	return nil
}

func (l *rotatingLog) reopen(rotationErr error) error {
	if err := l.open(); err != nil {
		return errors.Join(rotationErr, err)
	}
	return rotationErr
}
