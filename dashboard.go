package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	dashboardFrame          = 500 * time.Millisecond
	dashboardMaxConnections = 32
)

type dashboardResponse struct {
	UptimeSeconds           float64            `json:"uptime_seconds"`
	Turns                   int64              `json:"turns"`
	WebSocketTurns          int64              `json:"websocket_turns"`
	OpenWebSockets          int64              `json:"open_websockets"`
	Failovers               int64              `json:"failovers"`
	RateLimits              int64              `json:"rate_limits"`
	AverageTTFBMilliseconds float64            `json:"average_ttfb_ms"`
	Accounts                []dashboardAccount `json:"accounts"`
	Threads                 []dashboardThread  `json:"threads"`
	Events                  []dashboardEvent   `json:"events"`
}

type dashboardAccount struct {
	Name                   string        `json:"name"`
	Plan                   string        `json:"plan"`
	Status                 accountStatus `json:"status"`
	WeeklyRemainingPercent *float64      `json:"weekly_remaining_percent"`
	BankedResets           *int64        `json:"banked_resets"`
	ResetAt                *time.Time    `json:"reset_at"`
	Turns                  int64         `json:"turns"`
	OpenWebSockets         int64         `json:"open_websockets"`
	RateLimits             int64         `json:"rate_limits"`
	Activity               []int64       `json:"activity"`
}

type dashboardThread struct {
	KeyPrefix   string    `json:"key_prefix"`
	Account     string    `json:"account"`
	ServiceTier string    `json:"service_tier"`
	Turns       int64     `json:"turns"`
	Last        time.Time `json:"last"`
	Via         transport `json:"via"`
}

type dashboardEvent struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Account string    `json:"account"`
	Detail  string    `json:"detail"`
}

func (s *server) dashboardPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self' ws: wss:; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write([]byte(dashboardHTML))
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
		if err := wsjson.Write(r.Context(), conn, s.currentDashboard(time.Now())); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *server) currentDashboard(now time.Time) dashboardResponse {
	snapshot := s.stats.snapshot()
	stats := s.statsResponseAt(now, snapshot)
	publicAccounts := make([]dashboardAccount, 0, len(stats.Accounts))
	names := make(map[string]string, len(stats.Accounts))
	for i, account := range stats.Accounts {
		name := account.Email
		if name == "" {
			name = "account " + strconv.Itoa(i+1)
		}
		names[account.ID] = name
		publicAccounts = append(publicAccounts, dashboardAccount{
			Name:                   name,
			Plan:                   account.Plan,
			Status:                 account.Status,
			WeeklyRemainingPercent: account.WeeklyRemainingPercent,
			BankedResets:           account.BankedResets,
			ResetAt:                account.ResetAt,
			Turns:                  account.Turns,
			OpenWebSockets:         account.OpenWebSockets,
			RateLimits:             account.RateLimits,
			Activity:               account.Activity,
		})
	}
	threads := make([]dashboardThread, 0, len(snapshot.Threads))
	for _, thread := range snapshot.Threads {
		threads = append(threads, dashboardThread{
			KeyPrefix:   shortKey(thread.Key),
			Account:     names[thread.Account],
			ServiceTier: thread.ServiceTier,
			Turns:       thread.Turns,
			Last:        thread.Last,
			Via:         thread.Via,
		})
	}
	events := make([]dashboardEvent, 0, len(snapshot.Events))
	for _, event := range snapshot.Events {
		events = append(events, dashboardEvent{
			At:      event.At,
			Kind:    event.Kind,
			Account: names[event.Account],
			Detail:  event.Detail,
		})
	}
	return dashboardResponse{
		UptimeSeconds:           stats.UptimeSeconds,
		Turns:                   stats.Turns,
		WebSocketTurns:          stats.WebSocketTurns,
		OpenWebSockets:          stats.OpenWebSockets,
		Failovers:               stats.Failovers,
		RateLimits:              stats.RateLimits,
		AverageTTFBMilliseconds: stats.AverageTTFBMilliseconds,
		Accounts:                publicAccounts,
		Threads:                 threads,
		Events:                  events,
	}
}

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Codex Balancer</title>
<style>
:root { color-scheme: dark; font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; background: #11111b; color: #cdd6f4 }
* { box-sizing: border-box }
body { min-height: 100vh; margin: 0; background: #11111b }
.dashboard { width: min(112rem, 100%); margin: 0 auto; padding: 1rem }
.panel { border: 1px solid #45475a; border-radius: .75rem; background: #181825 }
.top { display: flex; align-items: center; justify-content: space-between; gap: 1rem; padding: .9rem 1.1rem }
.brand { color: #89b4fa; font-weight: 800; letter-spacing: .04em }
.summary, .meta { color: #6c7086 }
.summary { margin-left: .8rem }
.dot { margin: 0 .35rem; color: #45475a }
.live { color: #a6e3a1 }
.checking, .paused { color: #6c7086 }
.cooling, .fast { color: #fab387 }
.reauth, .limits { color: #f38ba8 }
.section { margin-top: 1rem; overflow: hidden }
.section h2 { margin: 0; padding: .8rem 1rem; border-bottom: 1px solid #313244; color: #89b4fa; font-size: .85rem; letter-spacing: .08em }
.scroll { overflow-x: auto }
table { width: 100%; border-collapse: collapse; white-space: nowrap }
th, td { padding: .65rem .8rem; border-bottom: 1px solid #252538; text-align: left }
th { color: #89b4fa; font-size: .75rem; font-weight: 700 }
td { color: #a6adc8 }
tbody tr:last-child td { border-bottom: 0 }
.number { color: #cdd6f4; font-weight: 700 }
.good { color: #a6e3a1 }
.warn { color: #f9e2af }
.bad { color: #f38ba8 }
.accent { color: #89b4fa }
.dim { color: #6c7086 }
.spark { color: #89b4fa; letter-spacing: .08em }
.empty { padding: 1rem; color: #6c7086 }
.totals { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); background: #181825 }
.total { min-width: 0; padding: .8rem 1rem; border-right: 1px solid #313244; border-bottom: 1px solid #313244; background: #181825 }
.total span { display: block; color: #6c7086; font-size: .72rem; text-transform: uppercase }
.total strong { display: block; margin-top: .3rem; overflow: hidden; color: #cdd6f4; font-size: 1.05rem; text-overflow: ellipsis }
@media (max-width: 760px) {
  .dashboard { padding: .6rem }
  .top { align-items: flex-start; flex-direction: column }
  .summary { display: block; margin: .35rem 0 0 }
  .meta { font-size: .78rem }
  .totals { grid-template-columns: repeat(2, minmax(0, 1fr)) }
  th, td { padding: .6rem }
}
</style>
</head>
<body>
<main class="dashboard">
<header class="top panel">
<div><span class="brand">CODEX BALANCER</span><span id="summary" class="summary"></span></div>
<div id="meta" class="meta">connecting…</div>
</header>
<section class="section panel">
<h2>ACCOUNTS</h2>
<div id="accounts" class="scroll"></div>
</section>
<section class="section panel">
<h2>TOTALS</h2>
<div id="totals" class="totals"></div>
</section>
<section class="section panel">
<h2 id="threads-title">THREADS</h2>
<div id="threads" class="scroll"></div>
</section>
<section class="section panel">
<h2>EVENTS</h2>
<div id="events" class="scroll"></div>
</section>
</main>
<script>
const blocks = ['▁', '▂', '▃', '▄', '▅', '▆', '▇', '█']
let reconnectTimer

function connect() {
  clearTimeout(reconnectTimer)
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const socket = new WebSocket(scheme + '//' + location.host + '/dashboard/ws')
  socket.addEventListener('message', event => render(JSON.parse(event.data)))
  socket.addEventListener('close', () => {
    document.getElementById('meta').textContent = 'reconnecting…'
    reconnectTimer = setTimeout(connect, 1000)
  })
  socket.addEventListener('error', () => {
    document.getElementById('meta').textContent = 'connection failed'
  })
}

function render(stats) {
  const counts = stats.accounts.reduce((all, account) => {
    all[account.status] = (all[account.status] || 0) + 1
    return all
  }, {})
  const activity = stats.accounts.reduce((total, account) => {
    account.activity.forEach((turns, index) => total[index] = (total[index] || 0) + turns)
    return total
  }, [])
  const summary = []
  addCount(summary, counts.live, 'live', 'live')
  addCount(summary, counts.checking, 'checking', 'checking')
  addCount(summary, counts.cooling, 'cooling', 'cooling')
  addCount(summary, counts.paused, 'paused', 'paused')
  addCount(summary, counts.needs_reauth, 'need reauth', 'reauth')
  document.getElementById('summary').innerHTML = summary.join('<span class="dot">·</span>')
  document.getElementById('meta').textContent = rate(activity) + ' · up ' + short(stats.uptime_seconds * 1000)
  renderAccounts(stats.accounts, stats.turns)
  renderTotals(stats)
  renderThreads(stats.threads)
  renderEvents(stats.events)
}

function addCount(parts, count, label, kind) {
  if (count) parts.push('<span class="' + kind + '">' + count + ' ' + h(label) + '</span>')
}

function renderAccounts(accounts, turns) {
  if (!accounts.length) {
    document.getElementById('accounts').innerHTML = '<div class="empty">(none)</div>'
    return
  }
  const rows = accounts.map(account => {
    const remaining = account.weekly_remaining_percent
    const remainingClass = remaining === null ? 'dim' : remaining <= 10 ? 'bad' : remaining <= 30 ? 'warn' : 'good'
    const traffic = turns ? Math.floor(account.turns * 100 / turns) + '%' : ''
    return '<tr>' +
      cell(account.name, 'number') +
      cell(account.plan, 'dim') +
      cell(status(account.status), statusClass(account.status)) +
      cell(remaining === null ? '--' : percent(remaining), remainingClass) +
      cell(account.banked_resets === null ? '--' : account.banked_resets, account.banked_resets === null ? 'dim' : 'number') +
      cell(account.reset_at ? short(new Date(account.reset_at).getTime() - Date.now()) : '--', 'dim') +
      cell(account.turns || '', 'number') +
      cell(account.open_websockets || '', 'good') +
      cell(traffic, 'dim') +
      cell(account.rate_limits || '', 'limits') +
      cell(spark(account.activity), 'spark') +
      '</tr>'
  }).join('')
  document.getElementById('accounts').innerHTML = '<table><thead><tr>' +
    headers(['Account', 'Plan', 'Status', 'Weekly', 'Banked', 'Reset in', 'Turns', 'WS', 'Traffic', 'Limits', 'Activity']) +
    '</tr></thead><tbody>' + rows + '</tbody></table>'
}

function renderTotals(stats) {
  const values = [
    ['turns', stats.turns],
    ['http', stats.turns - stats.websocket_turns],
    ['ws turns', stats.websocket_turns],
    ['ws open', stats.open_websockets],
    ['threads', stats.threads.length],
    ['accounts', stats.accounts.length],
    ['failovers', stats.failovers],
    ['rate limits', stats.rate_limits],
    ['ttfb', short(stats.average_ttfb_ms)],
    ['uptime', short(stats.uptime_seconds * 1000)]
  ]
  document.getElementById('totals').innerHTML = values.map(value =>
    '<div class="total"><span>' + h(value[0]) + '</span><strong>' + h(value[1]) + '</strong></div>'
  ).join('')
}

function renderThreads(threads) {
  const sorted = [...threads].sort((left, right) => new Date(right.last) - new Date(left.last))
  document.getElementById('threads-title').textContent = 'THREADS  ' + sorted.length
  if (!sorted.length) {
    document.getElementById('threads').innerHTML = '<div class="empty">nothing routed yet</div>'
    return
  }
  const rows = sorted.map(thread => '<tr>' +
    cell(thread.key_prefix, 'number') +
    cell('→', 'dim') +
    cell(thread.account, 'accent') +
    cell(String(thread.via).toUpperCase(), 'good') +
    cell(thread.service_tier === 'priority' ? 'FAST' : '', 'fast') +
    cell(plural(thread.turns, 'turn'), 'dim') +
    cell(ago(thread.last), 'dim') +
    '</tr>').join('')
  document.getElementById('threads').innerHTML = '<table><tbody>' + rows + '</tbody></table>'
}

function renderEvents(events) {
  const sorted = [...events].reverse()
  if (!sorted.length) {
    document.getElementById('events').innerHTML = '<div class="empty">quiet</div>'
    return
  }
  const rows = sorted.map(event => '<tr>' +
    cell(clock(event.at), 'dim') +
    cell(event.kind, event.kind === 'failover' ? 'bad' : event.kind === 'rate limited' ? 'fast' : 'dim') +
    cell(event.account, 'number') +
    cell(event.detail, 'dim') +
    '</tr>').join('')
  document.getElementById('events').innerHTML = '<table><tbody>' + rows + '</tbody></table>'
}

function headers(values) {
  return values.map(value => '<th>' + h(value) + '</th>').join('')
}

function cell(value, className) {
  return '<td class="' + className + '">' + h(value) + '</td>'
}

function status(value) {
  const labels = {
    paused: '⏸ paused',
    needs_reauth: '✕ reauth',
    cooling: '◐ cooling',
    checking: '◌ checking',
    live: '● live'
  }
  return labels[value] || value
}

function statusClass(value) {
  return value === 'live' ? 'good' : value === 'cooling' ? 'fast' : value === 'needs_reauth' ? 'bad' : 'dim'
}

function spark(activity) {
  const peak = Math.max(1, ...activity)
  return [...activity].reverse().map(value => blocks[Math.floor(value * (blocks.length - 1) / peak)]).join('')
}

function rate(activity) {
  const turns = activity.reduce((total, value) => total + value, 0)
  return (turns / 12).toFixed(1) + ' turns/min'
}

function short(milliseconds) {
  const seconds = Math.max(0, milliseconds) / 1000
  if (seconds < 1) return 'now'
  if (seconds < 10) return seconds.toFixed(1) + 's'
  if (seconds < 60) return Math.floor(seconds) + 's'
  if (seconds < 3600) return Math.floor(seconds / 60) + 'm'
  if (seconds < 86400) return Math.floor(seconds / 3600) + 'h' + String(Math.floor(seconds / 60) % 60).padStart(2, '0') + 'm'
  return Math.floor(seconds / 86400) + 'd' + Math.floor(seconds / 3600 % 24) + 'h'
}

function ago(value) {
  const elapsed = Date.now() - new Date(value).getTime()
  return elapsed >= 1000 ? short(elapsed) + ' ago' : 'now'
}

function clock(value) {
  return new Date(value).toLocaleTimeString([], {hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit'})
}

function plural(count, noun) {
  return count + ' ' + noun + (count === 1 ? '' : 's')
}

function percent(value) {
  return Number(value.toFixed(2)) + '%'
}

function h(value) {
  return String(value == null ? '' : value).replace(/[&<>"']/g, character => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[character]))
}

connect()
</script>
</body>
</html>`
