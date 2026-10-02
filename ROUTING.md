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

WebSocket GET requests use upstream WebSockets, and HTTP POST requests use
upstream HTTP. `/v1/responses`, `/codex/responses`, and `/v1/codex/responses`
share admission and account-routing policy. There is no account-specific route.

## Client usage reporting

Response headers and default `codex.rate_limits` events report the pool's average
used percentage for each quota window. Paused, signed-out, and non-routable
accounts are excluded. Exhausted and cooling accounts remain in the average.
Each window includes only accounts with known, finite usage. A window with
conflicting durations is omitted.

Each account has equal weight. This is an estimate when account quotas differ.
Pooled reports omit reset times, credit balances, and plan metadata because those
belong to individual accounts. Separate named quota categories pass through
unchanged. Routing and the dashboard continue to use each account's own usage.

## Tool endpoints

Codex calls standalone web search (`/v1/alpha/search`, used with responses-lite
models) and image generation (`/v1/images/generations`, `/v1/images/edits`) on
the provider base URL. The balancer proxies each as one unary POST with a pool
account's bearer and account ID, forwarding the same vetted request headers as
HTTP inference. These calls carry no conversation state, so they use fresh
placement without claims or affinity, skipping accounts that do not carry the
requested model. A `401` refreshes the account once; a usage limit marks it
spent and a transient `429` cools it, and the next eligible account is tried.
Dispatch updates last-used so equal-quota tool traffic is spread across accounts.
A credential refresh retries the same account. Transport failures return `502`
(`504` for timeouts) without sending another POST, since the upstream may already
have received it. Upstream redirects are not followed. When every account rejects
the call, the client receives the last upstream
rejection with its status, `Retry-After` and redacted body. Request bodies are
capped at 256 MiB and responses stream through unchanged up to 256 MiB.

## Fresh placement

For a session tree with neither an accepted route nor a provisional claim, the
server considers accounts in this order:

1. Exclude paused, spent, cooling, signed-out, unknown-quota, non-routable, and
   weekly-exhausted accounts from included-quota placement.
2. If all available accounts publish model catalogs and at least one of them
   carries the requested model and service tier, exclude the accounts that lack
   it. When no available account carries it, keep every candidate and let
   upstream answer for the model.
3. Prefer manual priority.
4. Prefer an account with a reset credit that expires within 24 hours, ordered
   by expiration time.
5. Choose the account with the lowest peak usage across its rate-limit windows.
6. Within the winning priority group, consider accounts at most one percentage
   point above its lowest peak usage. Choose the oldest last-used timestamp,
   then account ID. The result is independent of account-list order.

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
withdraws the models a newer client uses. Codex does not read
`minimal_client_version`, so the model endpoint filters the union by the
requesting `client_version` itself. The newest version persists in SQLite and
seeds the catalog at startup, so the first refresh runs before any client
asks. A request waits at most four seconds for a refresh in progress and
otherwise serves the cached catalog, staying inside Codex's five-second
fetch timeout.

When no eligible account with included quota is available, routing can use an
account's credit balance. The account must have a known quota window lasting at
least a week with 100% usage, plus a positive finite balance with `has_credits`
enabled or unlimited credits. Paused, signed-out, and non-routable accounts,
cooldowns, spend caps, and credit overage limits still exclude an account.

Credit fallback retains an eligible conversation owner when possible, then
uses the usual placement ranking. Included quota elsewhere in the pool takes
precedence over the credit owner's affinity. The upstream handles credit
billing. A usage-limit or `credit_balance_exhausted` rejection blocks further
credit fallback on that account until the next successful usage poll. Default
rate-limit events update the observed balance without exposing it to clients.
Banked quota-reset credits are redeemed manually through the admin Reset action.

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
For a cooling owner, `Retry-After` reports that owner's remaining cooldown,
rounded up to seconds. Fresh placement blocked by cooldowns reports the earliest
eligible recovery, including credit accounts. Unknown quota and provisional
claim conflicts retain the default retry hint.

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
  allows the next portable turn only when that account remains eligible for
  credit fallback and no account with included quota is available. Otherwise
  it closes before forwarding; reconnect can choose another account.
- A spent, paused, removed, signed-out, non-routable, or model-incompatible
  owner permits replacement on reconnect.
- A replacement request must omit `previous_response_id` and
  `x-codex-turn-state`. Encrypted reasoning does not bind a full replay to its
  prior account. The relay forwards it unchanged. A bound request on a moved
  socket receives a typed `400` `account_bound_request` error before the
  `1013` close, so Codex fails that turn at once instead of replaying the
  same token through its retry budget and HTTP fallback.
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
event `response account switch accepted` for cache-boundary changes. The
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
accepted switches. HTTP also emits request-correlated `http responses` records:
count `stage=response_accepted` with `accepted_switch=true`, not
`stage=route_selected` or a handshake. Optional OTel spans expose transmission,
acceptance and cache usage without changing this policy. See
[OBSERVABILITY.md](OBSERVABILITY.md) for the trace and safe-switch checklist.

## Persistence and migrations

The `routes` table and account `last_used_at` values hold routing state across
restarts. Schema migrations preserve both. Any route reset needs a migration
policy because the reset discards cache affinity.

Schema 8 persists the newest model-catalog client version. Schema 7 removes
the legacy `draining` routing mode. Databases on schema 5 or 6
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
- The downstream upgrade is answered within ten seconds. Refresh waits,
  upstream handshakes and account failover that run past that budget fail the
  attempt with `503` `upgrade_timeout` and no account penalty, so Codex sees a
  clean retry inside its own fifteen-second connect timeout instead of a
  client-side timeout followed by HTTP fallback.
- For handshake `401`, refresh the same account once before routing elsewhere.
  For an event-level `401`, refresh the same account once, send a retryable
  error containing the upstream details, then retire the socket. A permanent
  failure marks the account signed out and preserves its owner boundary for a
  portable reconnect.
- For a structured `server_is_overloaded` or `slow_down` event, send a retryable
  error containing the upstream details, preserve the accepted or provisional
  owner, and close the downstream socket with `1012`. Codex reconnects and
  replays the request to the same account. The balancer neither replays the request nor marks the
  account spent or cooling.
- For transient `429`, cool down the account and retire the socket. A wrapped
  `type:error` with `rate_limit_exceeded` becomes a retryable `502`, preserving
  upstream details, followed by a `1012` close. Codex otherwise treats this error
  as terminal. Other error codes retain their existing behavior. The balancer
  does not replay the request.
  Token-bound provisional routes with thread or session identity retain an owner
  barrier, including for native `response.failed` rate-limit events. An anonymous
  token-bound error is not rewritten into an automatic retry.
  The cooldown uses a fixed five-second base independent of retry-loop position,
  extended by upstream `Retry-After`, and capped at one hour. Later failures can
  only extend the deadline. Usage polling can clear exhausted quota but cannot
  clear a transient cooldown. Usage-window reset times never set this deadline.
  A retained owner can remain blocked for the full advised period. Codex ignores
  `Retry-After` on WebSocket retries and can exhaust that budget or fall back to
  HTTP; HTTP clients receive the actual remaining cooldown.
- For a usage limit, mark the account spent. If the socket has a thread or
  session identity, exactly one request is pending,
  it has not received `response.created`, no turn-state token was sent in its
  metadata or either side of the handshake, and another eligible account can
  serve its model and tier, send a retryable error containing the upstream
  details and close with `1012`.
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
- Upstream WebSocket closes produce a typed error with the original close code
  and reason. Code `1009` becomes retryable `request_too_large` with `status: 502`.
  Codex can exhaust its WebSocket stream retries and then send full history over
  HTTP. The balancer does not switch transports or replay the request. Codex
  remote compaction can exceed the same upstream byte limit even when the token
  context window has room. Protocol, payload, and policy rejections are permanent
  request errors. Transport failures remain retryable.
- WebSocket recovery errors use `type: error`, `status: 502`, and
  `retryable: true`. The nested `error` retains the upstream code, type, message,
  parameter, and extra fields. `upstream_type` and, when supplied,
  `upstream_status` retain the original event classification. This lets Codex
  report the cause while retaining its existing reconnect and replay behavior.
- Permanent request failures use `status: 400` on every transport because
  Codex treats other HTTP error statuses, including `413`, as retryable and
  would resend the same HTTP payload or account-bound request. An upstream HTTP
  `413` remains a permanent `request_too_large` error. The error code and
  `upstream_close_status` preserve the original failure. Setup errors
  retain the upstream HTTP status in `upstream_status` and preserve `Retry-After`.
- Newly surfaced error messages redact known account credentials. Raw upstream
  close reasons remain excluded from logs.

## HTTP execution and errors

Each HTTP POST sends one upstream HTTP POST to the configured Codex base URL
plus `/responses`. WebSocket GET requests continue to use upstream WebSockets.
The balancer does not choose a transport by payload size, follow redirects, or
replay an HTTP inference request. Clients own retries and transport fallback.

Account selection, provisional claims, durable ownership, credential refresh
before dispatch, model eligibility, fast-mode policy, and usage accounting are
shared with WebSocket requests. HTTP requests register cancellation callbacks
for account pause, removal, sign-out, and policy changes. They do not increment
open-WebSocket statistics. Account or catalog changes after dispatch cannot
cause another POST. Unaccepted owners are retained for policy changes, upstream
timeouts, usage limits, and model capacity failures. Other failures release
provisional claims so a client retry can select an eligible account, except a
rate-limited request with nonportable input or turn-state retains its owner.

Before streaming starts, a known `rate_limit_exceeded` error becomes `503` with
the upstream details and a retry hint at least as long as the account's remaining
cooldown. This lets Codex retry a transient throttle. Anonymous nonportable
requests keep their terminal `429`, since no route identity can retain their
account boundary. Committed SSE events keep their original framing.

Thread affinity prefers `thread-id`, then `x-client-request-id`. Session affinity
prefers `session_id`, `session-id`, `x-codex-session-id`,
`x-codex-conversation-id`, `x-session-affinity`, then `x-session-id`. Overlapping
identified requests share provisional ownership and use separate HTTP requests.
Requests without affinity are placed independently. Account-bound response IDs
and turn-state tokens cannot move to a replacement account.

HTTP authenticates the client before inspecting pool credentials. One admission
slot covers body reading, upstream execution, and downstream delivery. Request
JSON is checked for a model and valid routing fields. Other request fields pass
through unchanged, except for an explicitly configured fast-mode override.
Incoming identity and zstd bodies have separate 256 MiB wire and decoded limits.
Decoded requests are sent upstream without a compression header.

Only vetted identity and affinity headers are forwarded, including `x-codex-*`
and `x-openai-*`. Connection-nominated headers are removed. The selected pool
account supplies authorization and account ID. HTTP sends JSON content type and
SSE accept headers without adding a WebSocket beta token. Only `Retry-After` and
`X-Request-Id` are returned from upstream response headers, with credential
redaction. Quota headers update the account internally.

SSE events are read incrementally, including multiline data and comment
keepalives. Unknown events retain their fields. Terminal events close the
request promptly, even if upstream keeps the connection open. Streaming clients
receive SSE; non-streaming clients receive upstream JSON or collected SSE
output. Route acceptance is persisted at `response.created`, and terminal usage
is recorded once before downstream delivery. JSON terminal responses record the
same acceptance and usage. Successful JSON bodies are forwarded with credential
redaction.

### HTTP failures

Upstream errors retain their details with credential redaction. A recognized
usage limit becomes 429, and pool credential rejection becomes 503. An HTTP 401
refreshes credentials for the client's next attempt without resending the POST.
An upstream 413 becomes 400 `request_too_large` so Codex does not retry an
unchanged oversized body. Redirects, malformed responses, premature EOF, and
network failures produce 502. Upload/header or idle timeouts produce 504.
Account invalidation and fast-mode changes produce 503.

Before SSE commitment, failures return JSON with an error status. After an SSE
event has been flushed, failures remain in the stream and cannot change its HTTP
status. An upstream `response.failed` remains an SSE event for streaming clients.
No incomplete transport is reported as a successful completion.

Limits are 30 seconds for reading the client body, 90 seconds for the upstream
upload and response headers, six minutes without upstream body activity, and
30 seconds per downstream write. Keepalives reset the upstream idle timer.
Generation has no total duration limit. Non-terminal downstream writes clear
their write deadline while waiting for generation, including on HTTP/2.
Individual SSE events, collected output, JSON responses, and upstream error
bodies are bounded at 256 MiB.

Client cancellation and server shutdown interrupt upstream work. Every exit
closes the response body, stops policy observation, removes active registration,
and releases claims, live statistics, and admission. Shutdown drains admitted
requests before cancelling server work and joining credential refresh completion.

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
