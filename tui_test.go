package main

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestShortOmitsSecondsAfterOneMinute(t *testing.T) {
	for duration, want := range map[time.Duration]string{
		time.Minute:                     "1m",
		time.Minute + time.Second:       "1m",
		13*time.Minute + 33*time.Second: "13m",
		59*time.Minute + 59*time.Second: "59m",
	} {
		if got := short(duration); got != want {
			t.Errorf("short(%s) = %q, want %q", duration, got, want)
		}
	}
}

func TestDashboardTogglesCompactionRotation(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rotation, err := newCompactionRotation(store, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard{
		pool:               &Pool{accounts: []*Account{testAccount("account", 0)}},
		stats:              newStats(),
		compactionRotation: rotation,
		width:              120,
	}
	model, _ := d.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	d = model.(dashboard)
	if !rotation.isEnabled() {
		t.Fatal("r did not enable rotation")
	}
	if view := d.accounts(1); !strings.Contains(view, "r rotation ON") {
		t.Fatalf("accounts view = %q", view)
	}
}
