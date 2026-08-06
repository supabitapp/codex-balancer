# codex-balancer

Spread Codex turns across several ChatGPT accounts. One proxy endpoint, no
dashboard, no database.

New conversations go to the account with the most quota left, and then they stay
there. A conversation replays its whole history every turn, and that history
carries reasoning only the account that wrote it can read, so moving one mid-way
would break it. When a pinned account runs out, its conversations wait for the
window to reset; start a new conversation to use another account.

## Install

```sh
go install github.com/supabitapp/codex-balancer@latest
```

## Add accounts

```sh
codex-balancer accounts add     # opens a browser, sign in, repeat per account
codex-balancer accounts list
```

`add` runs the ChatGPT sign-in itself: it starts the OAuth flow, waits on
`127.0.0.1:1455` for the redirect, and trades the code for tokens. Nothing reads
or writes `~/.codex`, so the balancer and Codex hold separate sessions.

Accounts live in `~/.codex-balancer/accounts.json`, mode 0600. Each sign-in
mints a refresh token the balancer alone rotates, so copying that file to a
second machine and using it there retires the first copy.

## Serve

```sh
export CODEX_BALANCER_KEY=$(openssl rand -hex 16)
codex-balancer server           # live dashboard
codex-balancer server -no-tui   # log to stderr instead
```

The dashboard shows each account's two rate limit windows as gauges with their
reset countdowns, a sparkline of the last twelve minutes, which conversation is
pinned where, and a rolling event feed.

Limits come from two places. Every upstream reply carries them, and every couple
of minutes the balancer reads `backend-api/wham/usage` for each account, which
costs no quota. So the gauges keep moving while you are idle, and an account
parked by a 429 comes back the moment upstream says its window has room rather
than when the reset header guessed. `-poll 0` turns the reads off.

## Point Codex at it

In `~/.codex/config.toml`:

```toml
model_provider = "balancer"

[model_providers.balancer]
name = "OpenAI"
base_url = "http://127.0.0.1:8317/v1"
requires_openai_auth = true
env_key = "CODEX_BALANCER_KEY"
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
