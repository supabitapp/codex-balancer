package app

import (
	"strings"
	"testing"
	"time"
)

func TestDashboardAndStatsShowManualAndAutomaticDraining(t *testing.T) {
	manual := testAccount("manual", 20)
	manual.RoutingMode = routingModeDraining
	automatic := testAccount("automatic", 97)
	priority := testAccount("priority", 99)
	priority.RoutingMode = routingModePriority
	s := &server{
		pool:  &Pool{accounts: []*Account{manual, automatic, priority}},
		stats: newStatsWithPrices(priceSnapshot{}),
	}
	now := time.Now()
	stats := s.currentStats(now)
	for _, account := range stats.Accounts {
		wantMode, wantStatus := routingModeNormal, accountDraining
		if account.ID == manual.id() {
			wantMode = routingModeDraining
		} else if account.ID == priority.id() {
			wantMode, wantStatus = routingModePriority, accountPriority
		}
		if account.RoutingMode != wantMode || account.Status != wantStatus {
			t.Fatalf("account mode/status = %s/%s, want %s/%s", account.RoutingMode, account.Status, wantMode, wantStatus)
		}
	}
	view := s.currentDashboard(now)
	foundManual, foundAuto := false, false
	for _, account := range view.Accounts {
		if account.Status != accountDraining {
			continue
		}
		foundManual = foundManual || strings.Contains(account.StatusInfo, "Manual draining.")
		foundAuto = foundAuto || strings.Contains(account.StatusInfo, "less than 5% remaining")
		for _, want := range []string{"new placements only", "Existing conversations keep their owners", "fast-mode policy is unchanged"} {
			if !strings.Contains(account.StatusInfo, want) {
				t.Fatalf("drain tooltip missing %q: %s", want, account.StatusInfo)
			}
		}
	}
	if !foundManual || !foundAuto {
		t.Fatal("dashboard must distinguish manual and automatic draining")
	}
	foundSummary := false
	for _, count := range view.Summary {
		foundSummary = foundSummary || count == (dashboardCount{Count: 2, Label: "draining"})
	}
	if !foundSummary {
		t.Fatalf("summary = %+v, want two draining accounts", view.Summary)
	}
	page, err := renderDashboard("page", view)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := renderDashboardChanges(view, make(map[string][]byte))
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range [][]byte{page, stream} {
		if !strings.Contains(string(payload), `status-draining">▼</span> draining`) {
			t.Fatal("dashboard must render the drain marker in page and live updates")
		}
	}
	tui := dashboard{pool: s.pool, stats: s.stats, width: 160}
	if !strings.Contains(tui.header(), "2 draining") || !strings.Contains(tui.accounts(3), "▼ draining") {
		t.Fatal("TUI must show drain status and counts")
	}
	if routing(manual) != "draining" || routing(automatic) != "normal" {
		t.Fatal("CLI routing labels must show the saved mode, not automatic state")
	}
}
