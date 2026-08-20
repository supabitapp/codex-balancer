# Routing decisions

This file records why the routing rules are what they are. 

## Design

- One account per WebSocket. Selection happens once, at dial time, before any request arrives.
- Score by worst window: `max(used)` across primary and secondary equals minimum remaining headroom. Tie within 1% breaks by least recently used.
- A socket stays pinned until it closes or the account rejects a request.

## Failure policy

- Network errors and `5xx` are shared backend failures. No account cooldown, no account switch, no replay. Retry the same account briefly, then hand the failure to the client; the client reconnects.
- `401` refreshes the same account once. A second `401` cools the account down and moves on.
- `429` and usage limits are account-specific: cool down or mark spent, then try the next eligible account.
- A connection-limit rejection cools the account down briefly so reconnects use another account.
- Failover tries every eligible account, but only before the connection opens and only after account-specific failures. Never replay an in-flight request.
