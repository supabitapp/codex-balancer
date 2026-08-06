package main

import (
	"testing"
	"time"
)

func TestDashboardTitleShowsTotalWeeklyUsageLeft(t *testing.T) {
	now := time.Now()
	accounts := []*Account{
		{primary: window{usedPercent: 20, minutes: 300, seenAt: now}, secondary: window{usedPercent: 20, minutes: 10080, seenAt: now}},
		{secondary: window{usedPercent: 30, minutes: 10080, seenAt: now}},
		{secondary: window{usedPercent: 40, minutes: 10080, seenAt: now}},
		{Paused: true, secondary: window{usedPercent: 10, minutes: 10080, seenAt: now}},
		{dead: "needs reauth", secondary: window{usedPercent: 10, minutes: 10080, seenAt: now}},
	}
	d := dashboard{pool: &Pool{accounts: accounts}, stats: newStats()}

	if got := d.View().WindowTitle; got != "week 210%" {
		t.Fatalf("window title = %q, want %q", got, "week 210%")
	}
}

func TestDashboardTitleWaitsForWeeklyUsage(t *testing.T) {
	d := dashboard{pool: &Pool{accounts: []*Account{{}}}, stats: newStats()}

	if got := d.View().WindowTitle; got != "week --" {
		t.Fatalf("window title = %q, want %q", got, "week --")
	}
}
