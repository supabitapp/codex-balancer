package main

import (
	"net/http"
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
	Threads                 int                `json:"threads"`
	Failovers               int64              `json:"failovers"`
	RateLimits              int64              `json:"rate_limits"`
	AverageTTFBMilliseconds float64            `json:"average_ttfb_ms"`
	Accounts                []dashboardAccount `json:"accounts"`
	Activity                []int64            `json:"activity"`
}

type dashboardAccount struct {
	Email  string        `json:"email"`
	Status accountStatus `json:"status"`
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
	accounts := s.pool.sorted()
	publicAccounts := make([]dashboardAccount, 0, len(accounts))
	for _, account := range accounts {
		publicAccounts = append(publicAccounts, dashboardAccount{
			Email:  maskEmail(account.email()),
			Status: account.status(now),
		})
	}
	activity := make([]int64, activityLen)
	for _, account := range snapshot.Accounts {
		for i, turns := range account.Activity {
			activity[i] += turns
		}
	}
	return dashboardResponse{
		UptimeSeconds:           snapshot.Uptime.Seconds(),
		Turns:                   snapshot.Turns,
		WebSocketTurns:          snapshot.WSTurns,
		OpenWebSockets:          snapshot.WSOpen,
		Threads:                 len(snapshot.Threads),
		Failovers:               snapshot.Failures,
		RateLimits:              snapshot.Limited,
		AverageTTFBMilliseconds: float64(snapshot.TTFB) / float64(time.Millisecond),
		Accounts:                publicAccounts,
		Activity:                activity,
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
.cooling { color: #fab387 }
.reauth { color: #f38ba8 }
.section { margin-top: 1rem; overflow: hidden }
.section h2 { margin: 0; padding: .8rem 1rem; border-bottom: 1px solid #313244; color: #89b4fa; font-size: .85rem; letter-spacing: .08em }
.accounts { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)) }
.account { display: flex; align-items: center; justify-content: space-between; gap: 1rem; min-width: 0; padding: .8rem 1rem; border-right: 1px solid #313244; border-bottom: 1px solid #313244 }
.account strong { overflow: hidden; color: #cdd6f4; text-overflow: ellipsis; white-space: nowrap }
.account span { white-space: nowrap }
.empty { padding: 1rem; color: #6c7086 }
.activity { display: flex; align-items: center; justify-content: space-between; gap: 1rem; padding: 1rem }
.spark { overflow: hidden; color: #89b4fa; font-size: 1.25rem; letter-spacing: .08em; white-space: nowrap }
.rate { color: #6c7086; white-space: nowrap }
.totals { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); background: #181825 }
.total { min-width: 0; padding: .8rem 1rem; border-right: 1px solid #313244; border-bottom: 1px solid #313244; background: #181825 }
.total span { display: block; color: #6c7086; font-size: .72rem; text-transform: uppercase }
.total strong { display: block; margin-top: .3rem; overflow: hidden; color: #cdd6f4; font-size: 1.05rem; text-overflow: ellipsis }
@media (max-width: 760px) {
  .dashboard { padding: .6rem }
  .top { align-items: flex-start; flex-direction: column }
  .summary { display: block; margin: .35rem 0 0 }
  .meta { font-size: .78rem }
  .accounts { grid-template-columns: 1fr }
  .activity { align-items: flex-start; flex-direction: column }
  .totals { grid-template-columns: repeat(2, minmax(0, 1fr)) }
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
<div id="accounts" class="accounts"></div>
</section>
<section class="section panel">
<h2>ACTIVITY</h2>
<div class="activity"><strong id="activity" class="spark"></strong><span id="rate" class="rate"></span></div>
</section>
<section class="section panel">
<h2>TOTALS</h2>
<div id="totals" class="totals"></div>
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
  const turnsPerMinute = rate(stats.activity)
  const summary = []
  addCount(summary, counts.live, 'live', 'live')
  addCount(summary, counts.checking, 'checking', 'checking')
  addCount(summary, counts.cooling, 'cooling', 'cooling')
  addCount(summary, counts.paused, 'paused', 'paused')
  addCount(summary, counts.needs_reauth, 'need reauth', 'reauth')
  document.getElementById('summary').innerHTML = summary.join('<span class="dot">·</span>')
  document.getElementById('meta').textContent = turnsPerMinute + ' · up ' + short(stats.uptime_seconds * 1000)
  renderAccounts(stats.accounts)
  document.getElementById('activity').textContent = spark(stats.activity)
  document.getElementById('rate').textContent = turnsPerMinute
  renderTotals(stats)
}

function addCount(parts, count, label, kind) {
  if (count) parts.push('<span class="' + kind + '">' + count + ' ' + h(label) + '</span>')
}

function renderAccounts(accounts) {
  document.getElementById('accounts').innerHTML = accounts.length ? accounts.map((account, index) =>
    '<div class="account"><strong>' + h(account.email || 'account ' + (index + 1)) + '</strong><span class="' + statusClass(account.status) + '">' + h(status(account.status)) + '</span></div>'
  ).join('') : '<div class="empty">(none)</div>'
}

function renderTotals(stats) {
  const values = [
    ['turns', stats.turns],
    ['http', stats.turns - stats.websocket_turns],
    ['ws turns', stats.websocket_turns],
    ['ws open', stats.open_websockets],
    ['threads', stats.threads],
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
  return value === 'needs_reauth' ? 'reauth' : value
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

function h(value) {
  return String(value == null ? '' : value).replace(/[&<>"']/g, character => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[character]))
}

connect()
</script>
</body>
</html>`
