# Routing decisions

This file records why the routing rules are what they are. 

## Design

- One account per WebSocket. Selection happens once, at dial time, before any request arrives.
- Score by worst window: `max(used)` across primary and secondary equals minimum remaining headroom. Tie within 1% breaks by least recently used.
- Accounts above 95% use drain: new portable work goes to them until empty. Manual drain does the same on demand; an expiring reset credit makes an account priority, not draining.
- A socket stays pinned until it closes or the account rejects a request. Only portable reconnects move; account-owned state (turn state, `previous_response_id`, conversation, file IDs) keeps its account.

## Failure policy

- Network errors and `5xx` are shared backend failures. No account cooldown, no account switch, no replay. Retry the same account briefly, then hand the failure to the client; the client reconnects.
- `401` refreshes the same account once. A second `401` cools the account down and moves on.
- `429` and usage limits are account-specific: cool down or mark spent, then try the next eligible account.
- Failover tries every eligible account, but only before the connection opens and only after account-specific failures. Never replay an in-flight request.

## Drain policy

- OpenAI checks the limit when a turn starts, not while it runs. A turn that starts with quota left finishes even after the account reaches zero: "We want you to be able to complete work already in progress. If you reach your usage limits during an active turn, the agent will be able to continue working on that turn, subject to fair use limits." — [Codex pricing](https://developers.openai.com/codex/pricing), repeated in the [help center](https://help.openai.com/en/articles/11369540-using-codex-with-your-chatgpt-plan).
- So the last few percent of an account buy whole turns, not a few percent of one. Split across accounts they buy nothing. Pooled on one socket they start turns that run to the end.
- That is why an account under 5% remaining takes all new portable work until it empties, and why the balancer restarts other sockets toward it instead of waiting for them to close.
- Draining turns use the fast service tier when the account's model catalog allows it. Fast spends the same quota, so the drained turns lose nothing; the account reaches zero sooner and the pool returns to normal balancing.
- The grace covers the turn already running, not the next one. Sockets move to a draining account only between turns, and a turn started after zero is refused like any other over-limit request.
