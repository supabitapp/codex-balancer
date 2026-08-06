# codex-balancer

Spread Codex turns across several ChatGPT accounts. One proxy endpoint, no
database.

New conversations go to the account with the most quota left, and then they stay
there. Conversation history carries reasoning only the account that wrote it can
read, and WebSocket follow-ups can refer to responses owned by that account, so
moving one mid-way would break it. When a pinned account runs out, its
conversations wait for the window to reset; start a new conversation to use
another account.

## Install

```sh
go install github.com/supabitapp/codex-balancer@latest
```

## Add accounts

```sh
codex-balancer accounts add                 # sign in through a local browser
codex-balancer accounts add --device-auth   # sign in with a code on another device
codex-balancer accounts list
```

`add` runs the ChatGPT sign-in itself: it starts the OAuth flow, accepts the
redirect on `127.0.0.1:1455` or as a pasted callback URL, and trades the code for
tokens. `--device-auth` instead prints a link and one-time code, so the command
also works on a remote machine with no browser. Nothing reads or writes
`~/.codex`, so the balancer and Codex hold separate sessions.

Accounts live in `~/.codex-balancer/accounts.json`, mode 0600. Each sign-in
mints a refresh token the balancer alone rotates, so copying that file to a
second machine and using it there retires the first copy.

The running server watches this file. Accounts added, replaced, paused, or
removed by another command reach routing and the dashboard without a restart.
The server can start with an empty pool, so it can add its first account over
HTTP.

Open `/accounts` in a browser to add an account. The public page shows a sign-in
link and one-time code. The server adds the account in the background, and
`/stats` shows it when done. Only one sign-in can run at a time.

<http://127.0.0.1:8317/accounts>

## Serve

```sh
export CODEX_BALANCER_KEY=$(openssl rand -hex 16)
codex-balancer server           # live dashboard
codex-balancer server -no-tui   # show the same logs on stderr
```

The server appends verbose logs to `~/.codex-balancer/server.log`, mode 0600,
with or without the dashboard. Each route records its thread pin, attempt, chosen
account, every account's status and quota windows, upstream result, usage polls,
and WebSocket turns. Tokens and request bodies stay out of the log. `-log-file`
changes the path; an empty path disables the file.

The dashboard lists every account with its status, weekly quota, banked usage
resets, next quota reset, turns served, open WebSockets, and recent activity,
plus a rolling event feed. Each conversation row shows its account, transport,
and Fast mode.

Move the cursor with ↑↓ or j/k and press space to pause the account under it. A
paused account takes no turns at all, including from threads already pinned to
it, so those threads stop until you press space again. The pause is written to
the pool file, so it outlives a restart, and `accounts list` shows it.

Limits come from two places. Every upstream reply carries them, and every couple
of minutes the balancer reads `backend-api/wham/usage` for each account, which
costs no quota. So the gauges keep moving while you are idle, and an account
parked by a 429 comes back the moment upstream says its window has room rather
than when the reset header guessed. `-poll 0` turns the reads off.

`GET /stats` returns the same live account status as public JSON. Account emails
keep their first and last local-part characters and domain, such as
`k***i@example.com`.

```sh
curl http://127.0.0.1:8317/stats
```

## Point Codex at it

In `~/.codex/config.toml`:

```toml
model_provider = "balancer"

[model_providers.balancer]
name = "OpenAI"
base_url = "http://127.0.0.1:8317/v1"
requires_openai_auth = true
env_key = "CODEX_BALANCER_KEY"
supports_websockets = true
```

`name` must be exactly `OpenAI`: Codex compares it verbatim to decide whether a
provider supports remote compaction and whether to keep encrypted tool
arguments intact.

`env_key` reads the key from the environment, so every shell running `codex`
must export it. To bake the key into `~/.codex/auth.json` instead, drop the line
and log in once:

```sh
printenv CODEX_BALANCER_KEY | codex login --with-api-key
```

That replaces whatever ChatGPT session the file held.

Codex opens `GET /v1/responses` as a WebSocket and keeps each connection on one
account. The balancer can fail over while opening the upstream connection, but
never replays a `response.create` frame after upstream may have received it.
Pausing an account stops the next WebSocket turn before it reaches upstream.
