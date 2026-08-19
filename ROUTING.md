# Routing decisions

This file records why the routing rules are what they are. The code states the
rules; this states the evidence behind them, so we stop re-deriving them.

## Design

- One account per WebSocket. Selection happens once, at dial time, before any
  request arrives.
- Score by worst window: `max(used)` across primary and secondary equals
  minimum remaining headroom. Tie within 1% breaks by least recently used.
- Accounts above 95% use drain: new portable work goes to them until empty.
  Manual drain and expiring reset credits also drain.
- Drain beats reset-credit priority. Reset-credit priority beats headroom.
- A socket stays pinned until it closes or the account rejects a request.
  Only portable reconnects move; account-owned state (turn state,
  `previous_response_id`, conversation, file IDs) keeps its account.

## Failure policy

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

- Under-5%-remaining draining is deliberate. 3,215 live turns showed frequent safe handoff points, so emptying near-spent accounts is cheap.
- An idle thread moved A → B → A without losing context,
  which proved portable handoffs work and killed persistent session affinity.
