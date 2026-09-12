# Routing

Accepted conversation state stays with the account that created it. An account
change crosses upstream cache and response-chain boundaries. The balancer
requires a full replay without a `previous_response_id` or
`x-codex-turn-state` before it sends work to a replacement account. Encrypted
reasoning remains part of that replay and moves unchanged. The replacement
account owns the route after it emits `response.created`.

Codex CLI sets the lifecycle that the balancer matches:

- A running Codex process keeps the account used during authentication. Token
  refresh for the same account preserves the conversation boundary.
- Logout ends the authenticated client lifecycle.
- Login followed by resume creates a new client session from the saved history.
- On WebSocket reconnect, Codex discards socket-scoped
  `previous_response_id` state and sends full input. Codex replays the request.
  The balancer closes the socket and waits for the client.

Inference uses upstream WebSockets. Clients can use `GET /v1/responses`
(and the GET aliases `/codex/responses`, `/v1/codex/responses`) or stateless
HTTP `POST /v1/responses`. Both transports use the same relay and account
routing policy; there is no account-specific route.

## Fresh placement

For a session tree with neither an accepted route nor a provisional claim, the
server considers accounts in this order:

1. Exclude paused, spent, cooling, signed-out, unknown-quota, and non-routable
   accounts.
2. If all available accounts publish model catalogs and at least one of them
   carries the requested model and service tier, exclude the accounts that lack
   it. When no available account carries it, keep every candidate and let
   upstream answer for the model.
3. Prefer manual priority.
4. Prefer an account with a reset credit that expires within 24 hours, ordered
   by expiration time.
5. Choose the account with the lowest peak usage across its rate-limit windows.
6. For a peak-usage difference of one percentage point or less, choose the
   oldest last-used timestamp, then account ID.

Self-serve Business Pro Lite (`self_serve_business_prolite`) participates in
normal per-account routing once quota is known. Other Business and Enterprise
workspace plans remain excluded from routing.

A reported `spend_control.reached` makes an account unavailable for fresh and
retained routing even if its rate-limit windows have capacity. A pinned socket
retires before forwarding the next portable turn; reconnect follows the usual
replacement and full-replay rules. Rate-limit reset credits cannot clear a
spending limit. Usage polling returns the account to routing after the spend
limit clears and rate-limit capacity is available.

The model endpoint returns the union of known account catalogs.

Upstream gates each model on a minimum client version, so the catalog follows
the newest `client_version` the model endpoint has seen. A newer client
refreshes the catalog at once. An older client neither refreshes it nor
withdraws the models a newer client uses; every entry carries its own
`minimal_client_version` for the client to filter on.

When no account is available, quota polling and new connection attempts recover
an exhausted account using the usable reset credit that expires soonest across
the pool. This fallback also considers credits expiring more than 24 hours away;
credits without an expiration come last. Paused, signed-out, and non-routable
accounts are excluded, and temporary cooldowns alone do not spend a reset.
Recovery runs one account at a time and refreshes its quota before routing
resumes. If a reset fails to restore capacity, the next eligible account is tried.

For an identified route, a handshake leaves the last-used timestamp unchanged.
`response.created` updates it. An anonymous socket has no thread or session
key, so its handshake updates the timestamp and spreads connection bursts
across accounts.

## Provisional claims

An upstream handshake can finish before `response.created`. During that gap,
overlapping connections could choose different accounts for the same
conversation.

The server holds the claim-registry lock while it chooses an account and records
the claim, then dials upstream:

- The registry indexes each claim by thread and session. Connections for one
  thread join the same claim. Sibling threads join through their session key.
- An accepted thread owner outranks a sibling session claim. An in-flight claim
  for the same thread outranks a stale owner that recovered after replacement
  work started.
- A live claim controls its keys until the joined connections release it or one
  request receives `response.created`. If the claimed account cannot serve a
  new connection, the router blocks a competing account.
- Each joined connection adds a reference. Handshake failure, downstream
  upgrade failure, selection retry, model preflight switch, and socket closure
  release that reference.
- On `response.created`, the server writes the route to SQLite. The server
  removes the claim after SQLite accepts the write and keeps it after an error
  until its connections close.
- Account invalidation converts unaccepted claims into owner barriers before it
  closes their sockets. The barriers are also written to SQLite, so reconnects
  cannot lose the account boundary during invalidation or restart. Acceptance
  by a replacement clears the corresponding barriers.
- Server restart drops claims from memory. SQLite retains accepted routes and
  invalidation tombstones.
- Anonymous sockets create no claims.

## Accepted route retention

The router uses this affinity precedence:

1. In-flight claim for the same thread
2. Invalidated provisional owner for the same thread
3. Accepted thread owner
4. In-flight claim for the same session
5. Invalidated provisional owner for the same session
6. Accepted session owner
7. Fresh placement

A retained owner in cooldown, or one with unknown quota, blocks weaker entries
and returns `503`. Codex then retries the route.

The server records the account from each `response.created` event. A
`generate:false` warmup records the route without adding a turn.

### Relay and ownership

- A healthy retained account outranks manual priority, reset credits, quota
  pressure, and last-used order.
- Child threads inherit the session account. A thread's accepted route outranks
  a newer sibling route.
- The relay checks model support before it sends the first turn. It may change
  accounts at that point if the request carries portable input. Once the first
  turn starts, the relay pins the socket to its account.
- A later turn that needs another account receives a `1012` close before the
  relay sends it. Codex reconnects and routes the turn again.
- If quota polling marks the pinned account spent between turns, the relay
  closes before forwarding the next portable turn. The reconnect can then
  choose another account without sending a doomed request first.
- A spent, paused, removed, signed-out, non-routable, or model-incompatible
  owner permits replacement on reconnect.
- A replacement request must omit `previous_response_id` and
  `x-codex-turn-state`. Encrypted reasoning does not bind a full replay to its
  prior account. The relay forwards it unchanged.
- A replacement account takes ownership after `response.created`. Recovery of
  the old owner's quota leaves the new route in place.
- SQLite accepted-attempt records and provisional invalidation tombstones
  determine retained routes. The server stores no response-ID map.

`previous_response_id` belongs to the current upstream WebSocket. Codex drops
it on reconnect and sends full input. `x-codex-turn-state` belongs to one turn
and can survive a retry within that turn. Completion ends its scope. The relay
refuses both values during an account move.

## Account login, logout, and removal

Token refresh for the same account updates credentials while preserving
accepted routes, provisional claims, and live sockets. Concurrent callers share
one exchange, but each can cancel its own wait. The exchange has a server-owned
30-second deadline: canceling one waiter cannot abort another's refresh. The last
waiter leaving, or server shutdown, cancels the network exchange, not persistence
of a result already decoded. Persistence, publication and owner notification have
a separate 30-second completion budget. A later caller waits for an abandoned
operation to finish before starting another exchange; existing credential-
persistence ordering is preserved.

The server registers each refresh operation before launching it and owns it
through completion, even after every request waiter has left. Permanent refresh
failure invalidation belongs to that completion callback, not to a waiter or a
later watcher reload. Connection callbacks run after releasing the account, pool
and routing-ownership locks. Failed completion is logged once without credentials.

The following transitions invalidate an account:

- Pause
- Removal
- Permanent sign-out
- Change to a non-routable managed workspace

The server converts the account's provisional claims into owner barriers and
closes its downstream sockets with `1012` (`Service Restart`). The close reason
carries the routing reason.

The live-socket registry follows a socket that changes accounts during
first-turn model selection. Invalidation of the old account then leaves that
socket under its replacement account.

The server keeps accepted SQLite routes during invalidation and account
deletion. Route rows are owner tombstones rather than account children. On
reconnect, the stored owner supplies the switch reason, and the router requires
portable input before choosing a replacement. Login under the same account ID
can reuse those routes. A different account ID receives no ownership transfer.

The account watcher polls file changes. The TUI calls invalidation in the server
process as part of a pause.

## Switch logs

Debug logs describe routing attempts and successful handshakes. Count the info
event `websocket account switch accepted` for cache-boundary changes. The
server writes it after `response.created` with these fields:

- `thread`
- `from_account`
- `to_account`
- `routing_reason`
- `route_persisted`

Joined sockets share an accepted-switch marker. The first joined socket that
receives `response.created` writes the event. Other sockets on that claim do
not write a duplicate.

`route_persisted=false` means upstream accepted the request and SQLite failed
to record the replacement. The claim remains until its sockets close, which
keeps concurrent work on one account.

Routing logs use these reasons:

- `fresh`, `retained`
- `provisional_claim`, `provisional_claim_unavailable`
- `owner_removed`, `owner_paused`, `owner_signed_out`,
  `owner_not_routable`
- `owner_spent`, `owner_unavailable`
- `owner_model_incompatible`, `owner_attempt_failed`

Ignore `account_move=true` on routing-attempt and handshake logs when counting
accepted switches.

## Persistence and migrations

The `routes` table and account `last_used_at` values hold routing state across
restarts. Schema migrations preserve both. Any route reset needs a migration
policy because the reset discards cache affinity.

Schema 7 removes the legacy `draining` routing mode. Databases on schema 5 or 6
upgrade automatically: saved draining accounts return to `normal`, and saved
priority accounts stay `priority`. The migration preserves account credentials,
usage attribution, routes, API keys, and settings.

The claim and live-socket registries live in memory. Server restart discards
them. Codex reconnects, and the server rebuilds affinity from SQLite routes.

## Global fast mode

The global `fast-mode` setting has three values:

- `default`: forward the client's service tier unchanged (the initial setting).
- `on`: force `service_tier: "priority"` on every `response.create`.
- `off`: force `service_tier: "default"` on every `response.create`.

```sh
codex-balancer settings set fast-mode on
codex-balancer settings set fast-mode off
codex-balancer settings set fast-mode default
codex-balancer settings get fast-mode
codex-balancer settings list -json
codex-balancer settings set -state /path/to/state.db fast-mode on
```

Settings persist in SQLite across restarts. Running servers poll them every
500 ms. The CLI reports the saved setting; it does not acknowledge that a
server has applied it. The dashboard shows the server's applied policy as
default, force fast, or force standard.

A policy change activates the new value and asks all existing WebSockets to
close with `1012`. Connections still completing their handshake also retire
if they began under the old policy. Repeating the same value causes no restart.
The relay preserves provisional owners before closing, and accepted routes
remain in SQLite. Reconnection retains the account under the normal affinity
rules, including model/tier eligibility and portable-input checks. Anonymous
connections have no retained owner.

The override runs before model selection and request usage tracking, preserving
all other request fields. It is independent of account routing priority.
Turning it off forces standard service; choosing `default` restores the
client's preference. Disconnecting can interrupt in-flight work; the client
owns reconnect and replay, as with other service restarts.

## WebSocket rollover

[OpenAI caps each Responses WebSocket connection at 60 minutes](https://developers.openai.com/api/docs/guides/websocket-mode#connection-behavior-and-limits).
At the limit, upstream sends `websocket_connection_limit_reached` and requires a
new connection.

The error applies to one WebSocket. The server leaves account capacity, rate
limits, quota, and credentials unchanged.

The balancer forwards the original typed error event unchanged. Codex owns the
socket reset, reconnects to the retained account, and replays the request. The
balancer does not add a second reconnect path.

## Failure policy

- For a handshake network error or `5xx`, return that attempt without an
  internal retry loop. Codex owns its reconnect policy.
- For handshake `401`, refresh the same account once before routing elsewhere.
  For an event-level `401`, forward the original event, refresh the same account
  once, then retire the socket. A permanent failure marks the account signed
  out and preserves its owner boundary for a portable reconnect.
- For a structured `server_is_overloaded` or `slow_down` event, suppress the
  terminal event, preserve the accepted or provisional owner, and close the
  downstream socket with `1012`. Codex reconnects and replays the request to
  the same account. The balancer neither replays the request nor marks the
  account spent or cooling.
- For transient `429`, forward the original event, cool down the account, and
  retire the socket. The balancer does not replay the request.
- For a usage limit, mark the account spent. If the socket has a thread or
  session identity, exactly one request is pending,
  it has not received `response.created`, no turn-state token was sent in its
  metadata or either side of the handshake, and another eligible account can
  serve its model and tier, suppress the terminal event and close with `1012`.
  Preserve the prior owner boundary, including unaccepted provisional claims.
  Codex app-server (also used by Paseo) can then reconnect and replay full
  history, including encrypted reasoning, without an old response ID.
- Otherwise, forward the original usage-limit event and retire the socket.
  A fresh user turn or cold resume can still choose another eligible account.
  Reconnect is not identical to logout/login/resume: it clears response IDs
  but retains turn-state tokens within the current turn. Never strip those
  tokens to force a switch. Reject an account-moving handshake carrying a
  turn-state header before opening the replacement upstream connection.
- Replacement availability is a snapshot, not a reservation. Reconnect still
  applies ownership, quota, model, and replay checks. If capacity disappears,
  the client may exhaust its normal retry budget.
- For an account-specific setup failure, try another eligible account if the
  retained owner cannot continue and no provisional claim conflicts.
- The balancer does not replay in-flight work.

## HTTP execution and errors

HTTP adapts one request to one upstream WebSocket and one `response.create`.
It does not relay through a public endpoint, share a busy conversation socket,
or replay an in-flight generation. SSE and JSON are downstream serializers;
account setup, provisional claims, first-turn model checks, acceptance at
`response.created`, quota observation, retry-owner preservation, switch logs,
active-connection invalidation and API-key usage accounting are the same relay
operations used by WebSocket clients. HTTP-backed upstream sockets appear in
connection statistics and retire on pause, removal, sign-out and fast-mode
changes too.

Affinity keeps the existing thread precedence (`thread-id`, then
`x-client-request-id`). Session precedence is `session_id`, `session-id`,
`x-codex-session-id`, `x-codex-conversation-id`, then the OpenCode fallbacks
`x-session-affinity` and `x-session-id`. API keys and client account-ID headers
never choose an account or supply affinity. Requests without these identifiers
remain anonymous; they do not share a global route. Overlapping identified
requests join provisional ownership but use independent sockets and turn state.

HTTP uses exact stored bearer-key authentication and one admission slot for the
entire request, including body read, generation and response writes. Both new
JWT-shaped keys and legacy keys work; configured no-auth mode is unchanged.
Only a vetted request-header list is forwarded. Response transport headers are
removed from SSE events; only `Retry-After` and `X-Request-Id` may become HTTP
response headers. Pool credentials are redacted from decoded JSON strings in
upstream error/output data, including alternate JSON escapes. Unrelated strings
and raw numeric values retain their representation.

The [request-field policy](README.md#http-responses-contract) applies only to
HTTP. Existing WebSocket messages are not normalized. HTTP has no response-ID
storage: send complete replayable history on each request. A turn-state token
still cannot move accounts, in either handshake headers or client metadata.

### Terminal and failure mapping

| Outcome | Before SSE commitment / JSON | After SSE commitment |
| --- | --- | --- |
| Missing/invalid/revoked client key | 401 JSON error | Authentication is checked once on entry. |
| Admission full or draining, no eligible route/retained owner | 503 JSON error, retry hint | Not newly admitted. |
| Invalid JSON/types/controls or persistence/continuation request | 400 JSON error | Rejected before inference. |
| Body too large / unsupported encoding or content type | 413 / 415 JSON error | Rejected before inference. |
| `response.completed` | 200 Responses object or SSE | Forward terminal once, then `[DONE]`, close. |
| Legacy `response.done` | Normalize to `response.completed` for HTTP only | Same terminal handling. |
| Valid `response.incomplete` | 200, preserve status, partial output, details and any usage | Forward incomplete once, then `[DONE]`, close; not a transport failure. |
| `error` / `response.failed` | Non-2xx JSON error with useful code/message/param | Typed error or failed event, then `[DONE]`, close. |
| Context overflow / invalid request | 400 unless upstream supplies another error status | Preserve error code; no successful completion. |
| Model unavailable | 404 for `model_not_found`/`model_not_available`, or upstream error status | Preserve error code. No model substitution. |
| Rate/usage limit | 429 | Preserve typed error; shared quota/cooldown rules apply. |
| Capacity, connection rollover, upstream credential rejection | 503 | Preserve typed error; client owns retries. |
| Account invalidation / fast-mode change | 503 | Typed `route_unavailable` / `policy_changed` error. |
| Account-bound move | 409 JSON error | Typed error, never transmit bound input to replacement. |
| Malformed/binary frame, missing/oversized output or premature EOF | 502 | Typed error; never synthesize completion. |
| Upstream handshake or event idle timeout | 504 | Typed `upstream_timeout` error. |

Handshake server errors retain their status, without inference replay. A failed
upgrade carrying a non-error HTTP status (including 200 or 204) becomes 502, never
success. Initial setup and first-turn model-preflight failures use the same
semantic error mapping, preserving error codes and `Retry-After`; a recognized
403 usage-limit rejection becomes 429. Existing safe account-setup retries and
same-account credential refresh still apply.
When WebSocket clients would receive only a reconnect close (capacity or safe
usage-limit recovery), HTTP clients instead receive the original typed failure.
Once any SSE event is flushed, the adapter never attempts another HTTP status or
appends a plain JSON error body. Unknown valid events are forwarded as events;
normal terminal handling closes promptly even if upstream leaves the socket open.

Limits are 30 seconds to read the body, 90 seconds per upstream handshake/write
or idle event wait, and 30 seconds per downstream write/flush. Write deadlines
are not armed while waiting for generation, and successful non-terminal SSE
flushes clear them between events. Terminal writes keep their deadline through
net/http's final buffered write. This also supports HTTP/2 hosting without its
write timer cutting off an otherwise valid idle stream; the current server
command itself still uses plain HTTP/1, not TLS or h2c. The event queue
holds one queued frame for HTTP, applying backpressure; JSON retains completed
items, not deltas or event history. Cancellation interrupts body reads, handshakes,
refreshes, event waits and writes. The relay closes sockets, joins its readers,
releases claims and live statistics, then releases admission on every exit.

Shutdown stops admission and allows active requests to drain for up to 10
seconds, then closes refresh-operation registration and cancels the server
context (including network exchanges). It joins refresh completion before closing
SQLite, with up to 30 additional seconds of grace; each operation's own
completion deadline still applies. The HTTP adapter is tied to the server
context and the client request. Cancellation/disconnection can
prevent delivery of a final error; it never turns unfinished inference into a
successful response. Even an uncommitted JSON request is not replayed by the
balancer after transmission. Clients decide whether and how to retry full history.

Refresh completion uses cancelable pool/routing-lock acquisition and SQL calls.
If completion grace expires, shutdown cancels those waits, joins the workers and
returns an error before closing storage. SQLite retains its five-second busy
limit; canceled transactions also have a five-second rollback cleanup budget.
Pending persistence/notification failures during shutdown are reported even if
they occur before grace expires. A blocked or failing store can therefore cause
an explicit failed shutdown, not an empty successful drain or a write against a
closed database. No already-decoded result is discarded merely because its
request canceled. The same stop/join barrier runs on server startup/error exits.

### Verifying client recovery

The opt-in app-server integration test runs a real Codex binary against a local
mock upstream with synthetic credentials and an isolated `CODEX_HOME`:

```sh
CODEX_BALANCER_TEST_CODEX="$(command -v codex)" go test ./internal/app -run TestCodexAppServerUsageLimitReplaysFullHistory -count=1
```

It checks automatic replay after an incremental request hits a usage limit,
preservation of history and encrypted reasoning, refusal to move a turn-state
token, and cold resume in a new app-server process. Cold resume may warm up the
replacement first; later requests may reference that replacement's response ID.
This tests the client/proxy protocol, not live upstream decryption or login.
