package app

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type clientUsage struct {
	Primary   *clientUsageWindow `json:"primary"`
	Secondary *clientUsageWindow `json:"secondary"`
}

type clientUsageWindow struct {
	UsedPercent float64 `json:"used_percent"`
	Minutes     int     `json:"window_minutes,omitempty"`
}

type usageAverage struct {
	total      float64
	count      int
	minutes    int
	compatible bool
}

func (a *usageAverage) add(w window) {
	if !w.known() || math.IsNaN(w.usedPercent) || math.IsInf(w.usedPercent, 0) {
		return
	}
	if a.count == 0 {
		a.minutes = w.minutes
		a.compatible = true
	} else if a.minutes != w.minutes {
		a.compatible = false
	}
	a.total += min(max(w.usedPercent, 0), 100)
	a.count++
}

func (a usageAverage) window() *clientUsageWindow {
	if a.count == 0 || !a.compatible {
		return nil
	}
	return &clientUsageWindow{UsedPercent: a.total / float64(a.count), Minutes: a.minutes}
}

func (p *Pool) clientUsage() clientUsage {
	var primary, secondary usageAverage
	for _, account := range p.all() {
		candidate := account.routingCandidate()
		if !candidate.routingEnabled() || candidate.paused || candidate.reauth != "" {
			continue
		}
		primary.add(candidate.primary)
		secondary.add(candidate.secondary)
	}
	return clientUsage{Primary: primary.window(), Secondary: secondary.window()}
}

func clientUsageHeader(name string) bool {
	name = strings.ToLower(name)
	for _, prefix := range []string{"x-codex-primary-", "x-codex-secondary-"} {
		if suffix, found := strings.CutPrefix(name, prefix); found {
			switch suffix {
			case "used-percent", "window-minutes", "reset-at", "reset-after-seconds":
				return true
			}
		}
	}
	return name == "x-codex-limit-name" || strings.HasPrefix(name, "x-codex-credits-")
}

func (u clientUsage) writeHeaders(headers http.Header) {
	for name := range headers {
		if clientUsageHeader(name) {
			delete(headers, name)
		}
	}
	for _, entry := range []struct {
		prefix string
		window *clientUsageWindow
	}{{"x-codex-primary", u.Primary}, {"x-codex-secondary", u.Secondary}} {
		if entry.window == nil {
			continue
		}
		headers.Set(entry.prefix+"-used-percent", strconv.FormatFloat(entry.window.UsedPercent, 'f', -1, 64))
		if entry.window.Minutes > 0 {
			headers.Set(entry.prefix+"-window-minutes", strconv.Itoa(entry.window.Minutes))
		}
	}
	if u.Primary != nil || u.Secondary != nil {
		headers.Set("x-codex-limit-name", "Account pool (average)")
	}
}

func (p *Pool) clientUsageEvent(account *Account, data []byte, event websocketEnvelope) []byte {
	if len(event.Headers) > 0 {
		account.observe(websocketEventHeaders(event.Headers))
	}
	quota := event.Type == "codex.rate_limits"
	hasHeaders := false
	for name := range event.Headers {
		if clientUsageHeader(name) {
			hasHeaders = true
			break
		}
	}
	if !quota && !hasHeaders {
		return data
	}
	fields, err := responseObject(data)
	if err != nil {
		return data
	}
	if quota {
		name, _ := responseString(fields["metered_limit_name"])
		if name == "" {
			name, _ = responseString(fields["limit_name"])
		}
		quota = name == "" || strings.EqualFold(strings.TrimSpace(name), "codex")
	}
	if quota {
		account.observeRateLimitEvent(fields["rate_limits"])
		account.observeCreditEvent(fields["credits"])
	}
	if !quota && !hasHeaders {
		return data
	}
	usage := p.clientUsage()
	if quota {
		fields["rate_limits"], _ = json.Marshal(usage)
		for _, name := range []string{"plan_type", "credits"} {
			delete(fields, name)
		}
	}
	if hasHeaders {
		headers := make(map[string]json.RawMessage, len(event.Headers))
		for name, value := range event.Headers {
			if !clientUsageHeader(name) {
				headers[name] = value
			}
		}
		pooled := http.Header{}
		usage.writeHeaders(pooled)
		for name := range pooled {
			headers[name], _ = json.Marshal(pooled.Get(name))
		}
		fields["headers"], _ = json.Marshal(headers)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return data
	}
	return encoded
}

func (a *Account) observeCreditEvent(data json.RawMessage) {
	if len(data) == 0 {
		return
	}
	var credits *creditsPayload
	if json.Unmarshal(data, &credits) != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.credits = credits
}

func (a *Account) observeRateLimitEvent(data []byte) {
	type eventWindow struct {
		UsedPercent *float64 `json:"used_percent"`
		Minutes     *int     `json:"window_minutes"`
		ResetAt     *int64   `json:"reset_at"`
	}
	var limits struct {
		Primary   *eventWindow `json:"primary"`
		Secondary *eventWindow `json:"secondary"`
	}
	if json.Unmarshal(data, &limits) != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for _, entry := range []struct {
		source *eventWindow
		target *window
	}{{limits.Primary, &a.primary}, {limits.Secondary, &a.secondary}} {
		if entry.source == nil || entry.source.UsedPercent == nil {
			continue
		}
		entry.target.usedPercent = *entry.source.UsedPercent
		entry.target.seenAt = now
		if entry.source.Minutes != nil {
			entry.target.minutes = *entry.source.Minutes
		}
		if entry.source.ResetAt != nil {
			entry.target.resetsAt = time.Time{}
			if *entry.source.ResetAt > 0 {
				entry.target.resetsAt = time.Unix(*entry.source.ResetAt, 0)
			}
		}
	}
}
