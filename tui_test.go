package main

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestDashboardSectionsDoNotShareRows(t *testing.T) {
	d := dashboard{
		pool:   &Pool{accounts: []*Account{{IDToken: jwtFor("acct-a")}}},
		stats:  newStats(),
		width:  120,
		height: 40,
	}
	d.snap = d.stats.snapshot()

	titles := []string{"ACCOUNTS", "TOTALS", "THREADS", "EVENTS"}
	positions := map[string]int{}
	for i, line := range strings.Split(d.render(), "\n") {
		sections := 0
		for _, title := range titles {
			if strings.Contains(line, title) {
				sections++
				positions[title] = i
			}
		}
		if sections > 1 {
			t.Fatalf("sections share a row: %q", line)
		}
	}
	for i, title := range titles {
		position, ok := positions[title]
		if !ok {
			t.Fatalf("dashboard does not contain %s", title)
		}
		if i > 0 && position <= positions[titles[i-1]] {
			t.Fatalf("%s is out of order", title)
		}
	}
}

func TestDashboardResizeKeepsSelectedAccountAndSectionsVisible(t *testing.T) {
	accounts := make([]*Account, 10)
	for i := range accounts {
		accounts[i] = &Account{IDToken: jwtFor(fmt.Sprintf("acct-%02d", i))}
	}
	d := dashboard{
		pool:   &Pool{accounts: accounts},
		stats:  newStats(),
		cursor: len(accounts) - 1,
	}
	d.snap = d.stats.snapshot()

	model, _ := d.Update(tea.WindowSizeMsg{Width: 90, Height: 24})
	d = model.(dashboard)
	rendered := d.render()

	if !strings.Contains(rendered, "ACCOUNTS  5-10/10") {
		t.Fatalf("dashboard does not show the visible account range:\n%s", rendered)
	}
	if !strings.Contains(rendered, "acct-09@example.com") {
		t.Fatalf("dashboard does not show the selected account:\n%s", rendered)
	}
	for _, title := range []string{"TOTALS", "THREADS", "EVENTS"} {
		if !strings.Contains(rendered, title) {
			t.Fatalf("dashboard does not contain %s after resize:\n%s", title, rendered)
		}
	}
	assertDashboardFits(t, rendered, d.width, d.height)

	model, _ = d.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	d = model.(dashboard)
	rendered = d.render()
	if !strings.Contains(rendered, "acct-00@example.com") {
		t.Fatalf("dashboard does not restore account rows after growing:\n%s", rendered)
	}
	assertDashboardFits(t, rendered, d.width, d.height)
}

func assertDashboardFits(t *testing.T, rendered string, width, height int) {
	t.Helper()
	if got := lipgloss.Height(rendered); got > height {
		t.Fatalf("dashboard is %d rows high, want at most %d:\n%s", got, height, rendered)
	}
	for _, line := range strings.Split(rendered, "\n") {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("dashboard row is %d columns wide, want at most %d:\n%s", got, width, rendered)
		}
	}
}
