package main

import (
	"testing"
	"time"
)

func TestDashboardTitleShowsTotalWeeklyUsageLeft(t *testing.T) {
	now := time.Now()
	accounts := []*Account{
		{primary: window{usedPercent: 20, minutes: 300, seenAt: now}, secondary: window{usedPercent: 20, minutes: 10080, seenAt: now}},
		{secondary: window{usedPercent: 30.125, minutes: 10080, seenAt: now}},
		{secondary: window{usedPercent: 40, minutes: 10080, seenAt: now}},
		{accountState: accountState{Paused: true}, secondary: window{usedPercent: 10, minutes: 10080, seenAt: now}},
		{dead: "needs reauth", secondary: window{usedPercent: 10, minutes: 10080, seenAt: now}},
	}
	d := dashboard{pool: &Pool{accounts: accounts}, stats: newStats()}

	if got := d.View().WindowTitle; got != "week 209.88%" {
		t.Fatalf("window title = %q, want %q", got, "week 209.88%")
	}
}

func TestPercentFormattingKeepsUpToTwoDecimalPlaces(t *testing.T) {
	for value, want := range map[float64]string{
		0.875:  "0.88%",
		94:     "94%",
		94.5:   "94.5%",
		99.999: "100%",
	} {
		if got := formatPercent(value); got != want {
			t.Errorf("formatPercent(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestDashboardTitleWaitsForWeeklyUsage(t *testing.T) {
	d := dashboard{pool: &Pool{accounts: []*Account{{}}}, stats: newStats()}

	if got := d.View().WindowTitle; got != "week --" {
		t.Fatalf("window title = %q, want %q", got, "week --")
	}
}
