package main

import (
	"bytes"
	"crypto/sha256"
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
	dashboardFrame           = 500 * time.Millisecond
	dashboardMaxConnections  = 32
	dashboardContextBaseline = 12_000
)

//go:embed web/dashboard.html web/dashboard.js web/favicon.svg web/htmx-2.0.10.min.js web/ws-2.0.4.min.js
var dashboardFiles embed.FS

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"dashboardAssetURL": dashboardAssetURL,
	"dashboardStatus":   dashboardStatus,
}).ParseFS(dashboardFiles, "web/dashboard.html"))

type dashboardView struct {
	Summary  []dashboardCount
	Accounts []dashboardAccountView
	Overview []dashboardMetric
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
	StatusInfo     string
	Weekly         string
	Banked         string
	BankedInfo     string
	ResetIn        string
	CreditBurn     string
	CreditBurnInfo string
	OpenWebSockets string
	Traffic        string
	Activity       string
}

type dashboardStatusView struct {
	Mark  string
	Label string
}

type dashboardMetric struct {
	Name       string
	Value      string
	ValueClass string
	Info       string
	InfoStrong string
}

type dashboardThreadView struct {
	KeyPrefix     string
	Info          string
	ClientID      string
	Account       string
	Model         string
	ModelInfo     string
	Fast          bool
	UncachedInput string
	CacheRate     string
	Output        string
	ContextUsed   string
	ContextInfo   string
	Latency       string
	LatencyInfo   string
	Requests      string
	Cost          string
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

func webAsset(path, contentType, cacheControl string) http.HandlerFunc {
	content, err := dashboardFiles.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", cacheControl)
		w.Header().Set("Content-Type", contentType)
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
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; connect-src 'self' ws: wss:; img-src 'self'; base-uri 'none'; frame-ancestors 'none'")
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
	s.countries.refresh(snapshot.Threads)
	stats := s.statsResponseAt(now, snapshot)
	monthInfo := calendarMonthStart(now).Format("From Jan 2")
	traffic := trafficPercentages(stats.Accounts)
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
		creditBurn := "--"
		creditBurnInfo := ""
		if account.CreditBurn != nil {
			creditBurn = formatDecimal(*account.CreditBurn)
			creditBurnInfo = monthInfo
		}
		accounts = append(accounts, dashboardAccountView{
			Name:           name,
			Plan:           account.Plan,
			Status:         account.Status,
			StatusInfo:     dashboardAccountStatusInfo(now, account),
			Weekly:         weekly,
			Banked:         banked,
			BankedInfo:     bankedInfo,
			ResetIn:        resetIn,
			CreditBurn:     creditBurn,
			CreditBurnInfo: creditBurnInfo,
			OpenWebSockets: dashboardNumber(account.OpenWebSockets),
			Traffic:        dashboardNumber(traffic[i]),
			Activity:       sparkline(account.Activity),
		})
	}

	summary := make([]dashboardCount, 0, 6)
	for _, item := range []struct {
		status accountStatus
		label  string
	}{
		{accountLive, "live"},
		{accountPriority, "priority"},
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
	clientNames := dashboardClientNames(snapshot.Threads, s.clientIDKey, &s.countries)
	for _, thread := range snapshot.Threads {
		clientName := clientNames[clientIDForIP(thread.ClientIP, s.clientIDKey)]
		threadViews = append(threadViews, newDashboardThreadView(thread, names[thread.Account], clientName, s.catalog.contextLimits(thread.Account, thread.Model), now))
	}

	events := make([]dashboardEventView, 0, len(snapshot.Events))
	for i := len(snapshot.Events) - 1; i >= 0; i-- {
		event := snapshot.Events[i]
		events = append(events, dashboardEventView{
			At:      event.At.UTC().Format("15:04:05") + " UTC",
			Kind:    displayEventKind(event.Kind),
			Account: eventAccountName(names, event.Account),
			Detail:  event.Detail,
		})
	}

	priceInfo := monthInfo
	if !snapshot.PriceFetchedAt.IsZero() {
		priceInfo += ". Prices from models.dev, updated " + snapshot.PriceFetchedAt.In(now.Location()).Format("2 January 2006, 15:04 MST")
	}
	if snapshot.UnpricedResponses == 0 {
		priceInfo = funCostEquivalents(snapshot.APICostNanoDollars) + "\n" + priceInfo
	}
	overview := []dashboardMetric{dashboardCapacityMetric(now, stats.weeklyPace)}
	overview = append(overview, dashboardResourceMetrics(s.resources.usage(now))...)
	overview = append(overview,
		dashboardMetric{Name: "uptime", Value: short(snapshot.Uptime)},
		dashboardMetric{Name: "input tokens", Value: formatTokenCount(snapshot.MonthlyUsage.InputTokens), Info: monthInfo},
		dashboardMetric{Name: "cached input", Value: formatTokenCount(snapshot.MonthlyUsage.InputDetails.CachedTokens), Info: monthInfo},
		dashboardMetric{Name: "output tokens", Value: formatTokenCount(snapshot.MonthlyUsage.OutputTokens), Info: monthInfo},
		dashboardMetric{
			Name:  "API estimate",
			Value: formatAPIPrice(snapshot.APICostNanoDollars, snapshot.UnpricedResponses),
			Info:  priceInfo,
		},
	)
	return dashboardView{
		Summary:  summary,
		Accounts: accounts,
		Overview: overview,
		Threads:  threadViews,
		Events:   events,
	}
}

func dashboardCapacityMetric(now time.Time, estimate usagePaceEstimate) dashboardMetric {
	if !estimate.known {
		return dashboardMetric{
			Name:  "Capacity",
			Value: "❔ Unknown",
			Info:  "Not enough limit data to estimate whether the pool will last.",
		}
	}
	pace := estimate.pace()
	switch pace {
	case usagePaceOnTrack:
		return dashboardMetric{
			Name:       "Capacity",
			Value:      "✅ Lasts to reset",
			ValueClass: "capacity-good",
			Info:       "At the average burn since reset. Expected capacity at reset: ",
			InfoStrong: formatPercent(-estimate.shortfallPercent),
		}
	case usagePaceClose:
		return dashboardCapacityShortfall(now, estimate, "⚠️", "capacity-warning")
	default:
		return dashboardCapacityShortfall(now, estimate, "❌", "capacity-danger")
	}
}

func dashboardCapacityShortfall(now time.Time, estimate usagePaceEstimate, mark, valueClass string) dashboardMetric {
	if estimate.runway < time.Second {
		return dashboardMetric{
			Name:       "Capacity",
			Value:      mark + " Empty",
			ValueClass: valueClass,
			Info:       "Nothing left until reset.",
		}
	}
	return dashboardMetric{
		Name:       "Capacity",
		Value:      mark + " Runs out in " + short(estimate.runway),
		ValueClass: valueClass,
		Info:       "At the average burn since reset. Expected to run out: ",
		InfoStrong: now.Add(estimate.runway).Format("2 January 2006, 15:04 MST"),
	}
}

func trafficPercentages(accounts []accountStatsResponse) []int64 {
	percentages := make([]int64, len(accounts))
	remainders := make([]int64, len(accounts))
	var total int64
	for i, account := range accounts {
		percentages[i] = activityTotal(account.Activity)
		total += percentages[i]
	}
	if total == 0 {
		return percentages
	}
	var assigned int64
	for i, turns := range percentages {
		numerator := turns * 100
		percentages[i] = numerator / total
		remainders[i] = numerator % total
		assigned += percentages[i]
	}
	for assigned < 100 {
		largest := 0
		for i := 1; i < len(remainders); i++ {
			if remainders[i] > remainders[largest] {
				largest = i
			}
		}
		percentages[largest]++
		remainders[largest] = -1
		assigned++
	}
	return percentages
}

func dashboardClientNames(threads []ThreadSnapshot, clientIDKey []byte, countries *countryResolver) map[string]string {
	countriesByID := make(map[string]string)
	ids := make([]string, 0)
	for _, thread := range threads {
		id := clientIDForIP(thread.ClientIP, clientIDKey)
		if id == "" {
			continue
		}
		if _, exists := countriesByID[id]; exists {
			continue
		}
		country := ""
		if countries != nil {
			country = countries.label(thread.ClientIP)
		}
		countriesByID[id] = country
		ids = append(ids, id)
	}
	slices.Sort(ids)
	names := make(map[string]string, len(ids))
	for i, id := range ids {
		country := countriesByID[id]
		if country == "" {
			country = "Unknown"
		}
		names[id] = fmt.Sprintf("%s-%d", country, i+1)
	}
	return names
}

func newDashboardThreadView(thread ThreadSnapshot, account, clientName string, limits modelContextLimits, now time.Time) dashboardThreadView {
	used := thread.LatestUsage.contextTokens()
	cost := "--"
	if !thread.Usage.empty() {
		cost = formatAPIPrice(thread.apiCostNanoDollars, thread.unpricedResponses)
	}
	model, modelInfo := dashboardThreadModel(thread)
	return dashboardThreadView{
		KeyPrefix:     shortKey(thread.Key),
		Info:          dashboardThreadInfo(thread.Metadata),
		ClientID:      clientName,
		Account:       account,
		Model:         model,
		ModelInfo:     modelInfo,
		Fast:          isFastServiceTier(thread.ServiceTier),
		UncachedInput: formatTokenCount(thread.Usage.nonCachedInput()),
		CacheRate:     dashboardCacheRate(thread.Usage),
		Output:        formatTokenCount(thread.Usage.OutputTokens),
		ContextUsed:   dashboardContextUsed(used, limits, thread.Compactions),
		ContextInfo:   dashboardContextInfo(used, limits, thread.Compactions),
		Latency:       formatLatency(thread.Latency),
		LatencyInfo:   dashboardLatencyInfo(thread.TTFB, thread.Latency),
		Requests:      dashboardNumber(thread.Turns),
		Cost:          cost,
		Last:          agoAt(now, thread.Last),
	}
}

func dashboardThreadModel(thread ThreadSnapshot) (string, string) {
	if len(thread.models) < 2 {
		return dashboardModel(thread.Model, thread.Effort), ""
	}
	models := make([]string, len(thread.models))
	for index, model := range thread.models {
		models[index] = model.name
		if len(model.efforts) > 0 {
			models[index] += " " + strings.Join(model.efforts, ", ")
		}
	}
	return "🔀 mixed", strings.Join(models, "\n")
}

func dashboardModel(model, effort string) string {
	model = dashboardModelName(model)
	if effort == "" {
		return model
	}
	return model + " " + effort
}

func dashboardModelName(model string) string {
	switch {
	case model == "gpt-5.6-sol" || strings.HasPrefix(model, "gpt-5.6-sol-"):
		return "☀️"
	case model == "gpt-5.6-terra" || strings.HasPrefix(model, "gpt-5.6-terra-"):
		return "🌍"
	case model == "gpt-5.6-luna" || strings.HasPrefix(model, "gpt-5.6-luna-"):
		return "🌙"
	}
	return model
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

func dashboardContextUsed(used int64, limits modelContextLimits, compactions int64) string {
	context := "--"
	if limits.Window > 0 {
		context = fmt.Sprintf("%.0f%%", dashboardContextUsedPercent(used, limits.Window))
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
		lines = append(lines, "Tokens used: "+formatTokenCount(used))
	}
	if limits.Window > 0 {
		lines = append(lines, "Context used: "+formatPercent(dashboardContextUsedPercent(used, limits.Window)))
	}
	lines = append(lines, "Compactions: "+strconv.FormatInt(compactions, 10))
	return strings.Join(lines, "\n")
}

func dashboardContextUsedPercent(used, window int64) float64 {
	if window <= dashboardContextBaseline {
		return 0
	}
	available := window - dashboardContextBaseline
	consumed := max(used-dashboardContextBaseline, 0)
	return float64(min(consumed, available)) * 100 / float64(available)
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
	case accountPriority:
		return dashboardStatusView{Mark: "◆", Label: "priority"}
	default:
		return dashboardStatusView{Label: string(status)}
	}
}

func dashboardAccountStatusInfo(now time.Time, account accountStatsResponse) string {
	switch account.Status {
	case accountPriority:
		if account.RoutingMode == routingModePriority {
			return "Manual priority for new connections."
		}
		return dashboardRoutingPriorityInfo(now, account.RoutingPriority)
	default:
		return ""
	}
}

func dashboardRoutingPriorityInfo(now time.Time, priority *routingPriorityStatsResponse) string {
	if priority == nil {
		return ""
	}
	return fmt.Sprintf(
		"Prioritized for new connections: a banked reset expires in %s; %s weekly capacity remains.",
		short(priority.ExpiresAt.Sub(now)),
		formatPercent(priority.RemainingPercent),
	)
}

func displayEventKind(kind string) string {
	if kind == eventFailover {
		return "connection retry"
	}
	return kind
}

func dashboardNumber(value int64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func funCostEquivalents(nanoDollars int64) string {
	equivalents := [...]struct {
		emoji        string
		singular     string
		plural       string
		priceDollars int64
	}{
		{"☕", "iced latte", "iced lattes", 6},
		{"🌮", "taco", "tacos", 12},
	}
	lines := make([]string, 0, len(equivalents))
	for _, equivalent := range equivalents {
		priceNanoDollars := equivalent.priceDollars * 1_000_000_000
		count := nanoDollars / priceNanoDollars
		if nanoDollars%priceNanoDollars >= priceNanoDollars/2 {
			count++
		}
		name := equivalent.plural
		if count == 1 {
			name = equivalent.singular
		}
		lines = append(lines, fmt.Sprintf("%s %d %s ($%d each)", equivalent.emoji, count, name, equivalent.priceDollars))
	}
	return strings.Join(lines, "\n")
}
