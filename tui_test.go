package main

import (
	"strings"
	"testing"
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
