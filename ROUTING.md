# Routing

## Selection

- Exclude paused, spent, cooling, unknown, and signed-out accounts.
- Manual priority wins.
- An account with a reset credit expiring within 24 hours wins next. Earlier expiry wins.
- Otherwise, choose the account with the lowest peak usage across its rate-limit windows.
- When peak usage differs by at most one percentage point, choose the least recently used account, then break a tie by account ID.
- A WebSocket chooses one account when it connects and stays there until it closes or the account rejects a request.

## Failure policy

- Retry network errors and `5xx` on the same account for a short time. Do not cool down the account, switch accounts, or replay work.
- On `401`, refresh once. If it fails again, cool down the account.
- On `429` or a usage limit, cool down the account or mark it spent.
- On a connection limit, cool down the account so the next connection uses another one.
- Before a connection opens, an account-specific failure may try each eligible account.
- Never replay an in-flight request.
