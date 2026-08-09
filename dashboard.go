package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	dashboardFrame           = 500 * time.Millisecond
	dashboardMaxConnections  = 32
	dashboardContextBaseline = 12_000
)

//go:embed web/dashboard.html web/dashboard.js web/htmx-2.0.10.min.js web/ws-2.0.4.min.js
var dashboardFiles embed.FS

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"dashboardAssetURL": dashboardAssetURL,
	"dashboardStatus":   dashboardStatus,
}).ParseFS(dashboardFiles, "web/dashboard.html"))

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
}

type dashboardAccountView struct {
	Name           string
	Plan           string
	Status         accountStatus
	Weekly         string
	Banked         string
	BankedInfo     string
	ResetIn        string
	Turns          string
	OpenWebSockets string
	Traffic        string
	RateLimits     string
	Activity       string
}

type dashboardStatusView struct {
	Mark  string
	Label string
}

type dashboardTotal struct {
	Name  string
	Value string
	Info  string
}

type dashboardThreadView struct {
	KeyPrefix     string
	Info          string
	ClientID      string
	Account       string
	Model         string
	Via           string
	Fast          bool
	UncachedInput string
	CacheRate     string
	Output        string
	ContextLeft   string
	ContextInfo   string
	Latency       string
	LatencyInfo   string
	Requests      string
	Last          string
}

type dashboardEventView struct {
	At      string
	Kind    string
	Account string
	Detail  string
}

func dashboardAssetURL(name string) string {
	content, err := dashboardFiles.ReadFile("web/" + name)
	if err != nil {
		panic(err)
	}
	hash := sha256.Sum256(content)
	return fmt.Sprintf("/dashboard/assets/%s?v=%x", name, hash[:8])
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
	var previous []byte
	for {
		payload, err := renderDashboard("dashboard-update", s.currentDashboard(time.Now()))
		if err != nil {
			return
		}
		if !bytes.Equal(payload, previous) {
			if err := conn.Write(r.Context(), websocket.MessageText, payload); err != nil {
				return
			}
			previous = payload
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
		weekly := "--"
		if account.WeeklyRemainingPercent != nil {
			weekly = formatDecimal(*account.WeeklyRemainingPercent)
		}
		banked := "--"
		if account.BankedResets != nil {
			banked = dashboardNumber(*account.BankedResets)
		}
		bankedInfo := dashboardResetInfo(now, account.ResetCredits)
		resetIn := "--"
		if account.ResetAt != nil {
			resetIn = short(account.ResetAt.Sub(now))
		}
		traffic := ""
		if snapshot.Turns > 0 {
			traffic = dashboardNumber(account.Turns * 100 / snapshot.Turns)
		}
		accounts = append(accounts, dashboardAccountView{
			Name:           name,
			Plan:           account.Plan,
			Status:         account.Status,
			Weekly:         weekly,
			Banked:         banked,
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
			summary = append(summary, dashboardCount{Count: count, Label: item.label})
		}
	}

	threadViews := make([]dashboardThreadView, 0, len(snapshot.Threads))
	for _, thread := range snapshot.Threads {
		limits := s.catalog.contextLimits(thread.Account, thread.Model)
		used := thread.LatestUsage.contextTokens()
		threadViews = append(threadViews, dashboardThreadView{
			KeyPrefix:     shortKey(thread.Key),
			Info:          dashboardThreadInfo(thread.Metadata),
			ClientID:      thread.ClientID,
			Account:       names[thread.Account],
			Model:         dashboardModel(thread.Model, thread.Effort),
			Via:           strings.ToUpper(string(thread.Via)),
			Fast:          isFastServiceTier(thread.ServiceTier),
			UncachedInput: formatTokenCount(thread.Usage.nonCachedInput()),
			CacheRate:     dashboardCacheRate(thread.Usage),
			Output:        formatTokenCount(thread.Usage.OutputTokens),
			ContextLeft:   dashboardContext(used, limits, thread.Compactions),
			ContextInfo:   dashboardContextInfo(used, limits, thread.Compactions),
			Latency:       formatLatency(thread.Latency),
			LatencyInfo:   dashboardLatencyInfo(thread.TTFB, thread.Latency),
			Requests:      dashboardNumber(thread.Turns),
			Last:          agoAt(now, thread.Last),
		})
	}

	events := make([]dashboardEventView, 0, len(snapshot.Events))
	for i := len(snapshot.Events) - 1; i >= 0; i-- {
		event := snapshot.Events[i]
		events = append(events, dashboardEventView{
			At:      event.At.UTC().Format("15:04:05") + " UTC",
			Kind:    event.Kind,
			Account: names[event.Account],
			Detail:  event.Detail,
		})
	}

	monthInfo := "Calculated from " + calendarMonthStart(now).Format("2 January 2006, 15:04 MST")
	return dashboardView{
		Summary:  summary,
		Meta:     rate(snapshot) + " · up " + short(snapshot.Uptime),
		Accounts: accounts,
		Totals: []dashboardTotal{
			{Name: "turns", Value: strconv.FormatInt(snapshot.Turns, 10)},
			{Name: "http", Value: strconv.FormatInt(snapshot.Turns-snapshot.WSTurns, 10)},
			{Name: "ws turns", Value: strconv.FormatInt(snapshot.WSTurns, 10)},
			{Name: "ws open", Value: strconv.FormatInt(snapshot.WSOpen, 10)},
			{Name: "input tokens", Value: formatTokenCount(snapshot.MonthlyUsage.InputTokens), Info: monthInfo},
			{Name: "cached input", Value: formatTokenCount(snapshot.MonthlyUsage.InputDetails.CachedTokens), Info: monthInfo},
			{Name: "output tokens", Value: formatTokenCount(snapshot.MonthlyUsage.OutputTokens), Info: monthInfo},
			{
				Name:  "API estimate",
				Value: formatAPIPrice(snapshot.APICostNanoDollars, snapshot.UnpricedResponses),
				Info:  monthInfo,
			},
		},
		Threads: threadViews,
		Events:  events,
	}
}

func dashboardModel(model, effort string) string {
	if effort == "" {
		return model
	}
	return model + " (" + effort + ")"
}

func dashboardThreadInfo(metadata turnMetadata) string {
	lines := make([]string, 0, 8)
	for _, item := range []struct {
		label string
		value string
	}{
		{"Request", metadata.RequestKind},
		{"Codex thread", shortKey(metadata.ThreadID)},
		{"Turn", shortKey(metadata.TurnID)},
		{"Window", shortKey(metadata.WindowID)},
		{"Agent", metadata.SubagentKind},
		{"Parent thread", shortKey(metadata.ParentThreadID)},
		{"Parent turn", shortKey(metadata.ParentTurnID)},
		{"Forked from", shortKey(metadata.ForkedFromThreadID)},
	} {
		if item.value != "" {
			lines = append(lines, item.label+": "+item.value)
		}
	}
	return strings.Join(lines, "\n")
}

func dashboardCacheRate(usage responseUsage) string {
	if usage.InputTokens == 0 {
		return "--"
	}
	return formatDecimal(float64(usage.InputDetails.CachedTokens) * 100 / float64(usage.InputTokens))
}

func dashboardContext(used int64, limits modelContextLimits, compactions int64) string {
	context := "--"
	if limits.Window > 0 {
		context = fmt.Sprintf("%.0f%%", dashboardContextRemaining(used, limits.Window))
	}
	if compactions > 0 {
		context += " (" + strconv.FormatInt(compactions, 10) + ")"
	}
	return context
}

func dashboardContextInfo(used int64, limits modelContextLimits, compactions int64) string {
	lines := make([]string, 0, 5)
	if limits.Window > 0 {
		lines = append(lines, "Context window: "+formatTokenCount(limits.Window))
	}
	if limits.AutoCompact > 0 {
		lines = append(lines, "Auto compact at: "+formatTokenCount(limits.AutoCompact))
	}
	if used > 0 {
		lines = append(lines, "Used: "+formatTokenCount(used))
	}
	if limits.Window > 0 {
		lines = append(lines, "Left: "+formatPercent(dashboardContextRemaining(used, limits.Window)))
	}
	lines = append(lines, "Compactions: "+strconv.FormatInt(compactions, 10))
	return strings.Join(lines, "\n")
}

func dashboardContextRemaining(used, window int64) float64 {
	if window <= dashboardContextBaseline {
		return 0
	}
	available := window - dashboardContextBaseline
	consumed := max(used-dashboardContextBaseline, 0)
	return float64(max(available-consumed, 0)) * 100 / float64(available)
}

func formatLatency(value time.Duration) string {
	if value <= 0 {
		return "--"
	}
	return value.Round(time.Millisecond).String()
}

func dashboardLatencyInfo(ttfb, total time.Duration) string {
	if ttfb <= 0 && total <= 0 {
		return ""
	}
	return "First byte: " + formatLatency(ttfb) + "\nTotal: " + formatLatency(total)
}

func dashboardResetInfo(now time.Time, credits []resetCreditStatsResponse) string {
	lines := make([]string, 0, len(credits))
	for _, credit := range credits {
		if credit.ExpiresAt == nil {
			lines = append(lines, "Does not expire")
		} else if credit.ExpiresAt.After(now) {
			lines = append(lines, "Expires in "+short(credit.ExpiresAt.Sub(now))+" at "+credit.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
		} else {
			lines = append(lines, "Expired at "+credit.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
		}
	}
	return strings.Join(lines, "\n")
}

func dashboardStatus(status accountStatus) dashboardStatusView {
	switch status {
	case accountPaused:
		return dashboardStatusView{Mark: "⏸", Label: "paused"}
	case accountNeedsReauth:
		return dashboardStatusView{Mark: "✕", Label: "reauth"}
	case accountCooling:
		return dashboardStatusView{Mark: "◐", Label: "cooling"}
	case accountChecking:
		return dashboardStatusView{Mark: "◌", Label: "checking"}
	case accountLive:
		return dashboardStatusView{Mark: "●", Label: "live"}
	default:
		return dashboardStatusView{Label: string(status)}
	}
}

func dashboardNumber(value int64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}
