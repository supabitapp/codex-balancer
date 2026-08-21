# Routing

## Fresh placement

A Codex session tree without an accepted route uses the normal account order:

- Exclude paused, spent, cooling, unknown, and signed-out accounts.
- If every available account has a model catalog, exclude accounts that lack the requested model or service tier.
- Manual priority wins.
- An account with a reset credit expiring within 24 hours wins next. Earlier expiry wins.
- Otherwise, choose the account with the lowest peak usage across its rate-limit windows.
- When peak usage differs by at most one percentage point, choose the least recently used account, then break a tie by account ID.
- The model endpoint lists the union of known account catalogs.

## Route retention

- Persist the account from each `response.created` event. A `generate:false` warmup establishes the route but does not count as a turn.
- On each WebSocket connection, try the latest accepted `thread-id` account, then the latest accepted `session-id` account, then fresh placement.
- A healthy retained account wins over manual priority, reset credits, quota pressure, and least-recently-used order.
- Child threads inherit the session account. A thread's own accepted account wins over a newer sibling route.
- Keep one upstream account for the life of a WebSocket. Check the first turn against the model catalog and change account before sending only when the request can move safely.
- If a later turn needs another account, close the socket before sending it. The client reconnects and routes the turn again.
- Keep the retained route while its account cools down or its quota state is unknown. Return `503` so the client retries instead of moving the session.
- A spent, paused, removed, signed-out, or model-incompatible owner cannot continue. A reconnect may move only a full request without `previous_response_id` or `x-codex-turn-state`.
- Reject an account-bound request before forwarding it to a different account. Return WebSocket status `1013` so the client can retry with full input.
- Replace the retained route only after the new account emits `response.created`. Once replaced, recovered quota on the old account does not move the thread back.
- Derive retained routes from accepted attempt facts in SQLite. No separate binding or response-ID store exists.

`previous_response_id` belongs to the current upstream WebSocket. Codex drops it after reconnect and sends full input. `x-codex-turn-state` belongs to one turn and may cross a retry, but not a completed turn.

## Failure policy

- Retry network errors and `5xx` on the same account for a short time. Do not cool down the account, switch accounts, or replay work.
- On `401`, refresh once. Retry an accepted owner after a temporary refresh failure. If the account needs sign-in, a portable reconnect may use another account.
- On a transient `429` or connection limit, cool down the account and keep any accepted route there until retry.
- On a usage limit, mark the account spent. The client's next full replay may use another eligible account.
- Before a connection opens, an account-specific failure may try another eligible account only when the retained owner cannot continue.
- Never replay an in-flight request.
