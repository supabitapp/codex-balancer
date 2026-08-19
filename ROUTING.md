# Routing decisions

This file records why the routing rules are what they are. The code states the
rules; this states the evidence behind them, so we stop re-deriving them.

## Design

- One account per WebSocket. Selection happens once, at dial time, before any
  request arrives. Commits `82b01cb`, `bf4378d`.
- Score by worst window: `max(used)` across primary and secondary equals
  minimum remaining headroom. Tie within 1% breaks by least recently used.
- Accounts above 95% use drain: new portable work goes to them until empty.
  Manual drain and expiring reset credits also drain.
- Drain beats reset-credit priority. Reset-credit priority beats headroom.
- A socket stays pinned until it closes or the account rejects a request.
  Only portable reconnects move; account-owned state (turn state,
  `previous_response_id`, conversation, file IDs) keeps its account.

## Failure policy

Decided in Codex session `9c261037-bac8-7849-b3e0-3bdc62cb96ee` (events
`755fba0a`, `fe32f526`, `27636c71`), approved by khoi.

- Network errors and `5xx` are shared backend failures. No account cooldown,
  no account switch, no replay. Retry the same account briefly, then hand the
  failure to the client; the client reconnects.
- `401` refreshes the same account once. A second `401` cools the account
  down and moves on.
- `429` and usage limits are account-specific: cool down or mark spent,
  then try the next eligible account.
- Failover tries every eligible account, but only before the connection
  opens and only after account-specific failures. Never replay an in-flight
  request.

## Drain policy

Decided in Codex session `79c6afd6-9d4f-71b0-856d-85b0dc00c8f3` (events
`92051560`, `702d02ee`, `bbffdbe5`), approved by khoi.

- Under-5%-remaining draining is deliberate. 3,215 live turns showed frequent
  safe handoff points, so emptying near-spent accounts is cheap.
- An idle thread moved A → B → A without losing context (event `9550391f`),
  which proved portable handoffs work and killed persistent session affinity.

## Comparison, 2026-08-18

Reviewed [subrouter](https://github.com/manaflow-ai/subrouter) at `3a979b2`
and [codex-lb](https://github.com/Soju06/codex-lb) at `8e7589e` against our
`bf4378d`.

They do better: persistent reconnect ownership, separate placement and
retention capacity, live load signals, model-scoped quota. We do better:
one policy instead of eight strategies, reset-credit priority, fast-tier
draining, refusal to replay mid-stream.

Verdicts after checking each idea against the decisions above:

| Idea | Verdict | Why |
|---|---|---|
| Persistent session assignment | Rejected | Portable handoffs proved safe; socket pinning suffices. |
| Restrict automatic draining | Rejected | Under-5% draining was an explicit, tested choice. |
| Hard vs soft affinity store | Covered | Pinning plus portable-only reconnects already splits hard from soft. |
| Restore failure policy | Adopted | Handshake code had regressed to cooling and switching on network/`5xx`. |
| Try every account | Adopted | Fixed three-account cap starved healthy accounts; pre-connection only. |
| Connection caps, live load signals | Deferred | Adopt only if production logs show connection-limit loops or saturation. |
| More reset urgency scoring | Rejected | Reset-credit priority exists; drain outranks it by choice. |
| Model-scoped quota routing | Deferred | Conflicts with selecting at socket open; no evidence it pays. |
| Weighted random, strategy settings | Rejected | Single server, single policy. |

## Deferred triggers

Watch production logs for:

- Reconnect loops after `websocket_connection_limit_reached`.
- Account-owned state failing after unexpected reconnects.

The first justifies temporary connection-cap handling. The second justifies
persistent hard-owner routing across reconnects. Neither has appeared yet.
