package main

import (
	"bytes"
	"cmp"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	dashboardFrame          = 500 * time.Millisecond
	dashboardMaxConnections = 32
)

//go:embed web/dashboard.html web/htmx-2.0.10.min.js web/ws-2.0.4.min.js
var dashboardFiles embed.FS

var dashboardTemplate = template.Must(template.ParseFS(dashboardFiles, "web/dashboard.html"))

type dashboardView struct {
	Summary  []dashboardCount
	Meta     string
	Accounts []dashboardAccountView
	Totals   []dashboardTotal
	Threads  []dashboardThreadView
	Events   []dashboardEventView
}

type dashboardCount struct {
	Count int
	Label string
	Class string
}

type dashboardAccountView struct {
	Name           string
	Plan           string
	Status         string
	StatusClass    string
	Weekly         string
	WeeklyClass    string
	Banked         string
	BankedClass    string
	BankedInfo     string
	ResetIn        string
	Turns          string
	OpenWebSockets string
	Traffic        string
	RateLimits     string
	Activity       string
}

type dashboardTotal struct {
	Name  string
	Value string
}

type dashboardThreadView struct {
	KeyPrefix string
	Account   string
	Via       string
	Fast      bool
	Turns     string
	Last      string
}

type dashboardEventView struct {
	At      string
	Kind    string
	Class   string
	Account string
	Detail  string
}

func dashboardScript(path string) http.HandlerFunc {
	content, err := dashboardFiles.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(content)
	}
}

func (s *server) dashboardPage(w http.ResponseWriter, _ *http.Request) {
	payload, err := renderDashboard("page", s.currentDashboard(time.Now()))
	if err != nil {
		http.Error(w, "render dashboard", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; connect-src 'self' ws: wss:; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(payload)
}

func (s *server) dashboardWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.dashboardConnections.Add(1) > dashboardMaxConnections {
		s.dashboardConnections.Add(-1)
		http.Error(w, "dashboard is busy", http.StatusServiceUnavailable)
		return
	}
	defer s.dashboardConnections.Add(-1)

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ticker := time.NewTicker(dashboardFrame)
	defer ticker.Stop()
	for {
		payload, err := renderDashboard("dashboard", s.currentDashboard(time.Now()))
		if err != nil {
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageText, payload); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func renderDashboard(name string, view dashboardView) ([]byte, error) {
	var output bytes.Buffer
	if err := dashboardTemplate.ExecuteTemplate(&output, name, view); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (s *server) currentDashboard(now time.Time) dashboardView {
	snapshot := s.stats.snapshot()
	stats := s.statsResponseAt(now, snapshot)
	counts := map[accountStatus]int{}
	names := make(map[string]string, len(stats.Accounts))
	accounts := make([]dashboardAccountView, 0, len(stats.Accounts))
	for i, account := range stats.Accounts {
		counts[account.Status]++
		name := account.Email
		if name == "" {
			name = "account " + strconv.Itoa(i+1)
		}
		names[account.ID] = name
		status, statusClass := dashboardStatus(account.Status)
		weekly, weeklyClass := "--", "dim"
		if account.WeeklyRemainingPercent != nil {
			weekly = formatPercent(*account.WeeklyRemainingPercent)
			weeklyClass = "good"
			switch {
			case *account.WeeklyRemainingPercent <= 10:
				weeklyClass = "bad"
			case *account.WeeklyRemainingPercent <= 30:
				weeklyClass = "warn"
			}
		}
		banked, bankedClass := "--", "dim"
		if account.BankedResets != nil {
			banked = strconv.FormatInt(*account.BankedResets, 10)
			bankedClass = "strong"
		}
		bankedInfo := dashboardResetInfo(now, account.BankedResets, account.ResetCredits)
		resetIn := "--"
		if account.ResetAt != nil {
			resetIn = short(account.ResetAt.Sub(now))
		}
		traffic := ""
		if snapshot.Turns > 0 {
			traffic = fmt.Sprintf("%d%%", account.Turns*100/snapshot.Turns)
		}
		accounts = append(accounts, dashboardAccountView{
			Name:           name,
			Plan:           account.Plan,
			Status:         status,
			StatusClass:    statusClass,
			Weekly:         weekly,
			WeeklyClass:    weeklyClass,
			Banked:         banked,
			BankedClass:    bankedClass,
			BankedInfo:     bankedInfo,
			ResetIn:        resetIn,
			Turns:          dashboardNumber(account.Turns),
			OpenWebSockets: dashboardNumber(account.OpenWebSockets),
			Traffic:        traffic,
			RateLimits:     dashboardNumber(account.RateLimits),
			Activity:       sparkline(account.Activity),
		})
	}

	summary := make([]dashboardCount, 0, 5)
	for _, item := range []struct {
		status accountStatus
		label  string
	}{
		{accountLive, "live"},
		{accountChecking, "checking"},
		{accountCooling, "cooling"},
		{accountPaused, "paused"},
		{accountNeedsReauth, "need reauth"},
	} {
		if count := counts[item.status]; count > 0 {
			_, class := dashboardStatus(item.status)
			summary = append(summary, dashboardCount{Count: count, Label: item.label, Class: class})
		}
	}

	threads := slices.Clone(snapshot.Threads)
	slices.SortFunc(threads, func(left, right ThreadSnapshot) int {
		return cmp.Compare(right.Last.UnixNano(), left.Last.UnixNano())
	})
	threadViews := make([]dashboardThreadView, 0, len(threads))
	for _, thread := range threads {
		threadViews = append(threadViews, dashboardThreadView{
			KeyPrefix: shortKey(thread.Key),
			Account:   names[thread.Account],
			Via:       strings.ToUpper(string(thread.Via)),
			Fast:      isFastServiceTier(thread.ServiceTier),
			Turns:     plural(thread.Turns, "turn"),
			Last:      agoAt(now, thread.Last),
		})
	}

	events := make([]dashboardEventView, 0, len(snapshot.Events))
	for i := len(snapshot.Events) - 1; i >= 0; i-- {
		event := snapshot.Events[i]
		class := "dim"
		switch event.Kind {
		case "failover":
			class = "bad"
		case "rate limited":
			class = "hot"
		}
		events = append(events, dashboardEventView{
			At:      event.At.UTC().Format("15:04:05") + " UTC",
			Kind:    event.Kind,
			Class:   class,
			Account: names[event.Account],
			Detail:  event.Detail,
		})
	}

	return dashboardView{
		Summary:  summary,
		Meta:     rate(snapshot) + " · up " + short(snapshot.Uptime),
		Accounts: accounts,
		Totals: []dashboardTotal{
			{Name: "turns", Value: strconv.FormatInt(snapshot.Turns, 10)},
			{Name: "http", Value: strconv.FormatInt(snapshot.Turns-snapshot.WSTurns, 10)},
			{Name: "ws turns", Value: strconv.FormatInt(snapshot.WSTurns, 10)},
			{Name: "ws open", Value: strconv.FormatInt(snapshot.WSOpen, 10)},
			{Name: "threads", Value: strconv.Itoa(len(snapshot.Threads))},
			{Name: "accounts", Value: strconv.Itoa(len(accounts))},
			{Name: "failovers", Value: strconv.FormatInt(snapshot.Failures, 10)},
			{Name: "rate limits", Value: strconv.FormatInt(snapshot.Limited, 10)},
			{Name: "ttfb", Value: short(snapshot.TTFB)},
			{Name: "uptime", Value: short(snapshot.Uptime)},
			{Name: "API estimate", Value: formatAPIPrice(snapshot.APICostNanoDollars, snapshot.UnpricedResponses)},
		},
		Threads: threadViews,
		Events:  events,
	}
}

func dashboardResetInfo(now time.Time, count *int64, credits []resetCreditStatsResponse) string {
	if count == nil {
		return ""
	}
	sections := []string{plural(*count, "reset credit") + " available"}
	for _, credit := range credits {
		lines := []string{cmp.Or(strings.TrimSpace(credit.Title), "Rate-limit reset")}
		if description := strings.TrimSpace(credit.Description); description != "" {
			lines = append(lines, description)
		}
		if credit.ExpiresAt == nil {
			lines = append(lines, "Does not expire")
		} else if credit.ExpiresAt.After(now) {
			lines = append(lines, "Expires in "+short(credit.ExpiresAt.Sub(now))+" at "+credit.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
		} else {
			lines = append(lines, "Expired at "+credit.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

func dashboardStatus(status accountStatus) (string, string) {
	switch status {
	case accountPaused:
		return "⏸ paused", "dim"
	case accountNeedsReauth:
		return "✕ reauth", "bad"
	case accountCooling:
		return "◐ cooling", "hot"
	case accountChecking:
		return "◌ checking", "dim"
	case accountLive:
		return "● live", "good"
	default:
		return string(status), ""
	}
}

func dashboardNumber(value int64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}
