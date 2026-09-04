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

Inference uses only `GET /v1/responses` as a WebSocket endpoint. The balancer
does not expose an HTTP Responses fallback or a second account-specific route.

## Fresh placement

For a session tree with neither an accepted route nor a provisional claim, the
server considers accounts in this order:

1. Exclude paused, spent, cooling, signed-out, unknown-quota, and non-routable
   accounts.
2. If all available accounts publish model catalogs, exclude accounts that lack
   the requested model or service tier.
3. Prefer manual priority.
4. Prefer an account with a reset credit that expires within 24 hours, ordered
   by expiration time.
5. Choose the account with the lowest peak usage across its rate-limit windows.
6. For a peak-usage difference of one percentage point or less, choose the
   oldest last-used timestamp, then account ID.

The model endpoint returns the union of known account catalogs.

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
accepted routes, provisional claims, and live sockets.

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

The claim and live-socket registries live in memory. Server restart discards
them. Codex reconnects, and the server rebuilds affinity from SQLite routes.

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
