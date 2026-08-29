package app

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dashboardInterval        = time.Second
	dashboardMaxStreams      = 32
	dashboardEventName       = "dashboard"
	dashboardSubscriberQueue = 2
	dashboardContextBaseline = 12_000
	waterCSSURL              = "https://cdn.jsdelivr.net/npm/water.css@2/out/water.css"
)

//go:embed web/accounts.css web/accounts.html web/accounts.js web/dashboard.html web/dashboard.js web/favicon.svg web/htmx-2.0.10.min.js web/idiomorph-0.7.4.min.js web/sse-2.2.4.min.js
var dashboardFiles embed.FS

func webTemplate(name string) *template.Template {
	return template.New(name).Funcs(template.FuncMap{
		"dashboardAssetURL": dashboardAssetURL,
		"waterCSSURL":       func() string { return waterCSSURL },
	})
}

var dashboardTemplate = template.Must(webTemplate("dashboard").Funcs(template.FuncMap{
	"dashboardAssetURL": dashboardAssetURL,
	"dashboardStatus":   dashboardStatus,
}).ParseFS(dashboardFiles, "web/dashboard.html"))

var dashboardUpdateTemplates = []string{
	"overview-update",
	"summary-update",
	"accounts-update",
	"workspace-summary-update",
	"workspaces-update",
	"routing-count-update",
	"threads-update",
	"events-update",
}

type dashboardView struct {
	Summary    []dashboardCount
	Accounts   []dashboardAccountView
	Workspaces []dashboardWorkspaceView
	Overview   []dashboardMetric
	Threads    []dashboardThreadView
	Events     []dashboardEventView
}

type dashboardCount struct {
	Count int
	Label string
}

type dashboardAccountView struct {
	DOMID             string
	Name              string
	Plan              string
	Status            accountStatus
	StatusInfo        string
	Weekly            string
	Banked            string
	BankedInfo        string
	ResetIn           string
	RoutedCredits     string
	RoutedCreditsInfo string
	OpenWebSockets    string
	Traffic           string
	Activity          string
}

type dashboardWorkspaceView struct {
	DOMID       string
	Name        string
	Plan        string
	Status      accountStatus
	StatusInfo  string
	Limit       string
	Used        string
	Remaining   string
	UsedPercent string
	ResetIn     string
}

type dashboardStatusView struct {
	Mark  string
	Label string
}

type dashboardMetric struct {
	DOMID      string
	Name       string
	Value      string
	Info       string
	InfoStrong string
}

type dashboardThreadView struct {
	DOMID         string
	KeyPrefix     string
	Info          string
	Client        string
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
	DOMID   string
	At      string
	Kind    string
	Account string
	Detail  string
}

type dashboardBroadcaster struct {
	mu          sync.Mutex
	subscribers map[chan []byte]struct{}
	latest      []byte
	running     bool
}

func (b *dashboardBroadcaster) subscribe() (chan []byte, bool) {
	updates := make(chan []byte, dashboardSubscriberQueue)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subscribers == nil {
		b.subscribers = make(map[chan []byte]struct{})
	}
	b.subscribers[updates] = struct{}{}
	if len(b.latest) > 0 {
		updates <- b.latest
	}
	start := !b.running
	if start {
		b.running = true
	}
	return updates, start
}

func (b *dashboardBroadcaster) unsubscribe(updates chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subscribers[updates]; !ok {
		return
	}
	delete(b.subscribers, updates)
	close(updates)
}

func (b *dashboardBroadcaster) publish(update, full []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.latest = full
	for updates := range b.subscribers {
		select {
		case updates <- update:
		default:
			delete(b.subscribers, updates)
			close(updates)
		}
	}
}

func (b *dashboardBroadcaster) stopIfIdle() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subscribers) > 0 {
		return false
	}
	b.latest = nil
	b.running = false
	return true
}

func (b *dashboardBroadcaster) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for updates := range b.subscribers {
		delete(b.subscribers, updates)
		close(updates)
	}
	b.latest = nil
	b.running = false
}

func dashboardAssetURL(name string) string {
	content, err := dashboardFiles.ReadFile("web/" + name)
	if err != nil {
		panic(err)
	}
	hash := sha256.Sum256(content)
	return fmt.Sprintf("/dashboard/assets/%s?v=%x", name, hash[:8])
}

func dashboardDOMID(prefix string, values ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return fmt.Sprintf("%s-%x", prefix, hash[:8])
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
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline' "+waterCSSURL+"; script-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(payload)
}

func (s *server) dashboardEvents(w http.ResponseWriter, r *http.Request) {
	if s.dashboardStreams.Add(1) > dashboardMaxStreams {
		s.dashboardStreams.Add(-1)
		http.Error(w, "dashboard is busy", http.StatusServiceUnavailable)
		return
	}
	defer s.dashboardStreams.Add(-1)

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)

	updates, start := s.dashboardUpdates.subscribe()
	defer s.dashboardUpdates.unsubscribe(updates)
	if start {
		go s.broadcastDashboard()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case payload, ok := <-updates:
			if !ok {
				return
			}
			if err := writeSSEEvent(w, dashboardEventName, payload); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
	}
}

func (s *server) broadcastDashboard() {
	ticker := time.NewTicker(dashboardInterval)
	defer ticker.Stop()
	previous := make(map[string][]byte, len(dashboardUpdateTemplates))
	var done <-chan struct{}
	if s.ctx != nil {
		done = s.ctx.Done()
	}
	for {
		if s.dashboardUpdates.stopIfIdle() {
			return
		}
		payload, err := renderDashboardChanges(s.currentDashboard(time.Now()), previous)
		if err != nil {
			s.dashboardUpdates.stop()
			return
		}
		if len(payload) > 0 {
			s.dashboardUpdates.publish(payload, fullDashboardUpdate(previous))
		}
		select {
		case <-done:
			s.dashboardUpdates.stop()
			return
		case <-ticker.C:
		}
	}
}

func writeSSEEvent(w io.Writer, event string, data []byte) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if _, err := io.WriteString(w, "data: "); err != nil {
			return err
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func renderDashboard(name string, view dashboardView) ([]byte, error) {
	var output bytes.Buffer
	if err := dashboardTemplate.ExecuteTemplate(&output, name, view); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func renderDashboardChanges(view dashboardView, previous map[string][]byte) ([]byte, error) {
	var output bytes.Buffer
	for _, name := range dashboardUpdateTemplates {
		fragment, err := renderDashboard(name, view)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(fragment, previous[name]) {
			continue
		}
		output.Write(fragment)
		previous[name] = fragment
	}
	return output.Bytes(), nil
}

func fullDashboardUpdate(fragments map[string][]byte) []byte {
	var output bytes.Buffer
	for _, name := range dashboardUpdateTemplates {
		output.Write(fragments[name])
	}
	return output.Bytes()
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
	workspaces := make([]dashboardWorkspaceView, 0)
	for i, account := range stats.Accounts {
		name := account.Email
		if name == "" {
			name = "account " + strconv.Itoa(i+1)
		}
		names[account.ID] = name
		if managedWorkspacePlan(account.Plan) {
			workspaces = append(workspaces, newDashboardWorkspaceView(now, name, account))
			continue
		}
		counts[account.Status]++
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
		routedCredits := "--"
		routedCreditsInfo := "No routed credit data is available for this reset window yet."
		if account.RoutedCredits != nil && account.RoutedCreditsSince != nil {
			routedCredits = formatDecimal(*account.RoutedCredits)
			routedCreditsInfo = "Calculated from traffic routed through this balancer since " + account.RoutedCreditsSince.In(now.Location()).Format("2 January 2006, 15:04 MST") + ". Usage elsewhere is not included. OpenAI credit rates checked " + creditRatesCheckedAt + "."
		}
		accounts = append(accounts, dashboardAccountView{
			DOMID:             dashboardDOMID("account", account.ID),
			Name:              name,
			Plan:              account.Plan,
			Status:            account.Status,
			StatusInfo:        dashboardAccountStatusInfo(now, account),
			Weekly:            weekly,
			Banked:            banked,
			BankedInfo:        bankedInfo,
			ResetIn:           resetIn,
			RoutedCredits:     routedCredits,
			RoutedCreditsInfo: routedCreditsInfo,
			OpenWebSockets:    dashboardNumber(account.OpenWebSockets),
			Traffic:           dashboardNumber(traffic[i]),
			Activity:          sparkline(account.Activity),
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
	for _, thread := range snapshot.Threads {
		clientName := dashboardClientName(thread, &s.countries)
		threadViews = append(threadViews, newDashboardThreadView(thread, names[thread.Account], clientName, s.catalog.contextLimits(thread.Account, thread.Model), now))
	}

	events := make([]dashboardEventView, 0, len(snapshot.Events))
	for i := len(snapshot.Events) - 1; i >= 0; i-- {
		event := snapshot.Events[i]
		events = append(events, dashboardEventView{
			DOMID:   dashboardDOMID("event", event.At.Format(time.RFC3339Nano), event.Kind, event.Account, event.Detail),
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
	if modelCosts := formatModelCosts(snapshot.ModelCosts); modelCosts != "" {
		priceInfo = modelCosts + "\n" + priceInfo
	}
	overview := []dashboardMetric{{Name: "active WS", Value: strconv.FormatInt(snapshot.WSOpen, 10)}}
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
	for i := range overview {
		overview[i].DOMID = dashboardDOMID("metric", overview[i].Name)
	}
	return dashboardView{
		Summary:    summary,
		Accounts:   accounts,
		Workspaces: workspaces,
		Overview:   overview,
		Threads:    threadViews,
		Events:     events,
	}
}

func newDashboardWorkspaceView(now time.Time, name string, account accountStatsResponse) dashboardWorkspaceView {
	view := dashboardWorkspaceView{
		DOMID:       dashboardDOMID("workspace", account.ID),
		Name:        name,
		Plan:        account.Plan,
		Status:      account.Status,
		StatusInfo:  dashboardAccountStatusInfo(now, account),
		Limit:       "--",
		Used:        "--",
		Remaining:   "--",
		UsedPercent: "--",
		ResetIn:     "--",
	}
	if spend := account.SpendControl; spend != nil {
		view.Limit = dashboardSpendAmount(spend.Limit)
		view.Used = dashboardSpendAmount(spend.Used)
		view.Remaining = dashboardSpendAmount(spend.Remaining)
		if spend.UsedPercent != nil {
			view.UsedPercent = formatDecimal(*spend.UsedPercent)
		}
		if spend.ResetAt != nil {
			view.ResetIn = short(spend.ResetAt.Sub(now))
		}
	}
	return view
}

func dashboardSpendAmount(value string) string {
	if value == "" {
		return "--"
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return value
	}
	return formatDecimal(parsed)
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

func dashboardClientName(thread ThreadSnapshot, countries *countryResolver) string {
	country := ""
	if countries != nil {
		country = countries.label(thread.ClientIP)
	}
	if country == "" {
		country = "Unknown"
	}
	if thread.APIKeySuffix == "" {
		return country
	}
	return country + " " + thread.APIKeySuffix
}

func newDashboardThreadView(thread ThreadSnapshot, account, clientName string, limits modelContextLimits, now time.Time) dashboardThreadView {
	used := thread.LatestUsage.contextTokens()
	cost := "--"
	if !thread.Usage.empty() {
		cost = formatAPIPrice(thread.apiCostNanoDollars, thread.unpricedResponses)
	}
	model, modelInfo := dashboardThreadModel(thread)
	return dashboardThreadView{
		DOMID:         dashboardDOMID("thread", thread.Key),
		KeyPrefix:     shortKey(thread.Key),
		Info:          dashboardThreadInfo(thread.Metadata),
		Client:        clientName,
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
	case accountNotRouted:
		return dashboardStatusView{Mark: "○", Label: "not routed"}
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
	case accountNotRouted:
		return "Business and Enterprise workspaces are displayed here but excluded from routing."
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

func formatModelCosts(costs []ModelCostSnapshot) string {
	lines := make([]string, 0, len(costs))
	for _, cost := range costs {
		model := cost.Model
		if model == "" {
			model = "unknown model"
		}
		price := formatAPIPrice(cost.APICostNanoDollars, cost.UnpricedResponses)
		if cost.UnpricedResponses > 0 {
			response := "responses"
			if cost.UnpricedResponses == 1 {
				response = "response"
			}
			price += fmt.Sprintf(" (%d unpriced %s)", cost.UnpricedResponses, response)
		}
		lines = append(lines, model+": "+price)
	}
	return strings.Join(lines, "\n")
}
