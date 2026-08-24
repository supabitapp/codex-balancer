package main

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	logDateLayout    = "2006-01-02"
	logMiB           = 1 << 20
	logRetentionDays = 14
	logMaxFileSize   = 25 * logMiB
	logMaxTotalSize  = 500 * logMiB
)

type logPolicy struct {
	retentionDays int
	maxFileSize   int64
	maxTotalSize  int64
}

type rotatingLog struct {
	mu     sync.Mutex
	path   string
	policy logPolicy
	now    func() time.Time
	day    string
	file   *os.File
	closed bool
}

type archivedLog struct {
	path    string
	size    int64
	modTime time.Time
}

func openRotatingLog(path string, policy logPolicy, now func() time.Time) (*rotatingLog, error) {
	if policy.retentionDays < 1 {
		return nil, errors.New("log retention must be at least one day")
	}
	if policy.maxFileSize < 1 {
		return nil, errors.New("maximum log file size must be positive")
	}
	if policy.maxTotalSize < policy.maxFileSize {
		return nil, errors.New("maximum total log size must cover one log file")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	log := &rotatingLog{
		path:   path,
		policy: policy,
		now:    now,
		day:    now().UTC().Format(logDateLayout),
	}
	if err := log.rollOverExistingFile(); err != nil {
		return nil, err
	}
	if err := log.maintain(); err != nil {
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
	rotate, err := l.shouldRotate(day, len(data))
	if err != nil {
		return 0, err
	}
	if rotate {
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

func (l *rotatingLog) shouldRotate(day string, writeSize int) (bool, error) {
	if day != l.day {
		return true, nil
	}
	if l.file == nil {
		return false, nil
	}
	info, err := l.file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat log file: %w", err)
	}
	return info.Size() > 0 && info.Size()+int64(writeSize) > l.policy.maxFileSize, nil
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
	if err := l.maintain(); err != nil {
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
		exists, err := anyPathExists(path, path+".gz")
		if err != nil {
			return "", fmt.Errorf("stat rotated log file: %w", err)
		}
		if !exists {
			return path, nil
		}
	}
}

func anyPathExists(paths ...string) (bool, error) {
	for _, path := range paths {
		_, err := os.Stat(path)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func (l *rotatingLog) maintain() error {
	if err := l.compressArchives(); err != nil {
		return err
	}
	if err := l.removeExpired(); err != nil {
		return err
	}
	return l.removeExcess()
}

func (l *rotatingLog) compressArchives() error {
	directory := filepath.Dir(l.path)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read log directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".gz") {
			continue
		}
		if _, ok := l.archiveDay(entry.Name()); !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat rotated log file: %w", err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		compressedPath := path + ".gz"
		exists, err := anyPathExists(compressedPath)
		if err != nil {
			return fmt.Errorf("stat compressed log file: %w", err)
		}
		if exists {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove duplicate log file: %w", err)
			}
			continue
		}
		if err := compressLog(path); err != nil {
			return err
		}
	}
	return nil
}

func compressLog(path string) error {
	input, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open rotated log file: %w", err)
	}
	directory := filepath.Dir(path)
	output, err := os.CreateTemp(directory, ".log-*.gz")
	if err != nil {
		input.Close()
		return fmt.Errorf("create compressed log file: %w", err)
	}
	temporaryPath := output.Name()
	defer os.Remove(temporaryPath)
	if err := output.Chmod(0o600); err != nil {
		input.Close()
		output.Close()
		return fmt.Errorf("secure compressed log file: %w", err)
	}
	compressed := gzip.NewWriter(output)
	if _, err := io.Copy(compressed, input); err != nil {
		input.Close()
		compressed.Close()
		output.Close()
		return fmt.Errorf("compress rotated log file: %w", err)
	}
	if err := input.Close(); err != nil {
		compressed.Close()
		output.Close()
		return fmt.Errorf("close rotated log file: %w", err)
	}
	if err := compressed.Close(); err != nil {
		output.Close()
		return fmt.Errorf("finish compressed log file: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close compressed log file: %w", err)
	}
	if err := os.Rename(temporaryPath, path+".gz"); err != nil {
		return fmt.Errorf("store compressed log file: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove uncompressed log file: %w", err)
	}
	return nil
}

func (l *rotatingLog) removeExpired() error {
	directory := filepath.Dir(l.path)
	cutoff := l.now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1-l.policy.retentionDays)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read log directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		day, ok := l.archiveDay(entry.Name())
		if !ok || !day.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
			return fmt.Errorf("remove expired log file: %w", err)
		}
	}
	return nil
}

func (l *rotatingLog) archiveDay(name string) (time.Time, bool) {
	if strings.HasSuffix(name, ".gz") {
		name = strings.TrimSuffix(name, ".gz")
	}
	extension := filepath.Ext(l.path)
	stem := strings.TrimSuffix(filepath.Base(l.path), extension)
	if !strings.HasPrefix(name, stem+"-") || !strings.HasSuffix(name, extension) {
		return time.Time{}, false
	}
	date := strings.TrimSuffix(strings.TrimPrefix(name, stem+"-"), extension)
	if len(date) < len(logDateLayout) {
		return time.Time{}, false
	}
	sequence := date[len(logDateLayout):]
	if sequence != "" {
		if !strings.HasPrefix(sequence, "-") {
			return time.Time{}, false
		}
		number, err := strconv.Atoi(strings.TrimPrefix(sequence, "-"))
		if err != nil || number < 2 {
			return time.Time{}, false
		}
	}
	day, err := time.Parse(logDateLayout, date[:len(logDateLayout)])
	return day, err == nil
}

func (l *rotatingLog) removeExcess() error {
	directory := filepath.Dir(l.path)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read log directory: %w", err)
	}
	var total int64
	var archives []archivedLog
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		isCurrent := entry.Name() == filepath.Base(l.path)
		if _, ok := l.archiveDay(entry.Name()); !isCurrent && !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat log file: %w", err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		total += info.Size()
		if !isCurrent {
			archives = append(archives, archivedLog{
				path:    filepath.Join(directory, entry.Name()),
				size:    info.Size(),
				modTime: info.ModTime(),
			})
		}
	}
	sort.Slice(archives, func(i, j int) bool {
		if archives[i].modTime.Equal(archives[j].modTime) {
			return archives[i].path < archives[j].path
		}
		return archives[i].modTime.Before(archives[j].modTime)
	})
	for _, archive := range archives {
		if total <= l.policy.maxTotalSize {
			break
		}
		if err := os.Remove(archive.path); err != nil {
			return fmt.Errorf("remove excess log file: %w", err)
		}
		total -= archive.size
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
