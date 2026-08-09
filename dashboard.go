package main

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const dashboardFrame = 500 * time.Millisecond
const dashboardAuthTimeout = 10 * time.Second

type dashboardAuth struct {
	Password string `json:"password"`
}

type dashboardResponse struct {
	Stats   statsResponse    `json:"stats"`
	Threads []ThreadSnapshot `json:"threads"`
	Events  []Event          `json:"events"`
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
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1024)

	authContext, cancel := context.WithTimeout(r.Context(), dashboardAuthTimeout)
	defer cancel()
	var auth dashboardAuth
	if err := wsjson.Read(authContext, conn, &auth); err != nil || s.key == "" || !s.validKey(auth.Password) {
		conn.Close(websocket.StatusPolicyViolation, "invalid admin password")
		return
	}

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
	return dashboardResponse{
		Stats:   s.statsResponseAt(now, snapshot),
		Threads: snapshot.Threads,
		Events:  snapshot.Events,
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
[hidden] { display: none !important }
button, input { font: inherit }
.login-shell { min-height: 100vh; display: grid; place-items: center; padding: 1.5rem }
.login { width: min(28rem, 100%); padding: 2rem; border: 1px solid #45475a; border-radius: 1rem; background: #181825 }
.login h1 { margin: 0 0 .65rem; color: #89b4fa; font-size: 1.45rem }
.login p { margin: 0 0 1.4rem; color: #6c7086; line-height: 1.5 }
.login label { display: block; margin-bottom: .5rem; color: #a6adc8 }
.login input { width: 100%; border: 1px solid #45475a; border-radius: .5rem; padding: .75rem; outline: none; background: #11111b; color: #cdd6f4 }
.login input:focus { border-color: #89b4fa }
.login button { width: 100%; margin-top: .85rem; border: 0; border-radius: .5rem; padding: .75rem; background: #89b4fa; color: #11111b; font-weight: 700; cursor: pointer }
.login button:disabled { opacity: .55; cursor: wait }
.login-status { min-height: 1.25rem; margin-top: .8rem; color: #f38ba8 }
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
.totals { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); background: #181825 }
.total { min-width: 0; padding: .8rem 1rem; border-right: 1px solid #313244; border-bottom: 1px solid #313244; background: #181825 }
.total span { display: block; color: #6c7086; font-size: .72rem; text-transform: uppercase }
.total strong { display: block; margin-top: .3rem; overflow: hidden; color: #cdd6f4; font-size: 1.05rem; text-overflow: ellipsis }
.empty { padding: 1rem; color: #6c7086 }
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
<section id="login-shell" class="login-shell">
<form id="login" class="login">
<h1>CODEX BALANCER</h1>
<p>Enter the server key to open the live dashboard.</p>
<label for="password">Admin password</label>
<input id="password" name="password" type="password" autocomplete="current-password" autofocus required>
<button id="connect" type="submit">Connect</button>
<div id="login-status" class="login-status" role="status"></div>
</form>
</section>
<main id="dashboard" class="dashboard" hidden>
<header class="top panel">
<div><span class="brand">CODEX BALANCER</span><span id="summary" class="summary"></span></div>
<div id="meta" class="meta"></div>
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
const loginShell = document.getElementById('login-shell')
const login = document.getElementById('login')
const passwordInput = document.getElementById('password')
const connectButton = document.getElementById('connect')
const loginStatus = document.getElementById('login-status')
const dashboard = document.getElementById('dashboard')
const blocks = ['▁', '▂', '▃', '▄', '▅', '▆', '▇', '█']
let password = ''
let reconnectTimer

login.addEventListener('submit', event => {
  event.preventDefault()
  password = passwordInput.value
  connect()
})

function connect() {
  clearTimeout(reconnectTimer)
  connectButton.disabled = true
  loginStatus.textContent = 'Connecting…'
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const socket = new WebSocket(scheme + '//' + location.host + '/dashboard/ws')
  socket.addEventListener('open', () => socket.send(JSON.stringify({password})))
  socket.addEventListener('message', event => {
    render(JSON.parse(event.data))
    loginShell.hidden = true
    dashboard.hidden = false
    connectButton.disabled = false
    loginStatus.textContent = ''
  })
  socket.addEventListener('close', event => {
    if (event.code === 1008) {
      password = ''
      passwordInput.value = ''
      loginShell.hidden = false
      dashboard.hidden = true
      connectButton.disabled = false
      loginStatus.textContent = 'Invalid admin password'
      passwordInput.focus()
      return
    }
    if (password) reconnectTimer = setTimeout(connect, 1000)
  })
  socket.addEventListener('error', () => {
    loginStatus.textContent = 'Connection failed'
  })
}

function render(payload) {
  const stats = payload.stats
  const accountNames = Object.fromEntries(stats.accounts.map(account => [account.id, shortName(account)]))
  const counts = stats.accounts.reduce((all, account) => {
    all[account.status] = (all[account.status] || 0) + 1
    return all
  }, {})
  const summary = []
  addCount(summary, counts.live, 'live', 'live')
  addCount(summary, counts.checking, 'checking', 'checking')
  addCount(summary, counts.cooling, 'cooling', 'cooling')
  addCount(summary, counts.paused, 'paused', 'paused')
  addCount(summary, counts.needs_reauth, 'need reauth', 'reauth')
  document.getElementById('summary').innerHTML = summary.join('<span class="dot">·</span>')
  document.getElementById('meta').textContent = rate(stats.accounts) + ' · up ' + short(stats.uptime_seconds * 1000)
  renderAccounts(stats)
  renderTotals(stats)
  renderThreads(payload.threads, accountNames)
  renderEvents(payload.events, accountNames)
}

function addCount(parts, count, label, kind) {
  if (count) parts.push('<span class="' + kind + '">' + count + ' ' + h(label) + '</span>')
}

function renderAccounts(stats) {
  if (!stats.accounts.length) {
    document.getElementById('accounts').innerHTML = '<div class="empty">(none)</div>'
    return
  }
  const rows = stats.accounts.map(account => {
    const remaining = account.weekly_remaining_percent
    const remainingClass = remaining === null ? 'dim' : remaining <= 10 ? 'bad' : remaining <= 30 ? 'warn' : 'good'
    const traffic = stats.turns ? Math.floor(account.turns * 100 / stats.turns) + '%' : ''
    return '<tr>' +
      cell(account.email || account.id, 'number') +
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

function renderThreads(threads, names) {
  const sorted = [...threads].sort((left, right) => new Date(right.last) - new Date(left.last))
  document.getElementById('threads-title').textContent = 'THREADS  ' + sorted.length
  if (!sorted.length) {
    document.getElementById('threads').innerHTML = '<div class="empty">nothing routed yet</div>'
    return
  }
  const rows = sorted.map(thread => '<tr>' +
    cell(thread.key.slice(0, 8), 'number') +
    cell('→', 'dim') +
    cell(names[thread.account] || '', 'accent') +
    cell(String(thread.via).toUpperCase(), 'good') +
    cell(thread.service_tier === 'priority' ? 'FAST' : '', 'fast') +
    cell(plural(thread.turns, 'turn'), 'dim') +
    cell(ago(thread.last), 'dim') +
    '</tr>').join('')
  document.getElementById('threads').innerHTML = '<table><tbody>' + rows + '</tbody></table>'
}

function renderEvents(events, names) {
  const sorted = [...events].reverse()
  if (!sorted.length) {
    document.getElementById('events').innerHTML = '<div class="empty">quiet</div>'
    return
  }
  const rows = sorted.map(event => '<tr>' +
    cell(clock(event.at), 'dim') +
    cell(event.kind, event.kind === 'failover' ? 'bad' : event.kind === 'rate limited' ? 'fast' : 'dim') +
    cell(names[event.account] || '', 'number') +
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

function shortName(account) {
  return (account.email || account.id).split('@')[0]
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

function rate(accounts) {
  const turns = accounts.flatMap(account => account.activity).reduce((total, value) => total + value, 0)
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
</script>
</body>
</html>`
