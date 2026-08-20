# codex-balancer

<img width="3024" height="1898" alt="CleanShot 2026-08-14 at 12 20 51@2x" src="https://github.com/user-attachments/assets/9df4be94-7c98-406c-9af5-6ad5d97e4ca0" />

Spread Codex WebSocket connections across several ChatGPT accounts.

Dead simple, one WebSocket-only proxied endpoint, 1 SQLite database.

## Install

```sh
go install github.com/supabitapp/codex-balancer@latest
```

## Serve

```sh
export CODEX_BALANCER_KEY=$(openssl rand -hex 16)
codex-balancer server           # serve the proxy with a TUI
```

The server runs at http://127.0.0.1:8317

- `/dashboard` — HTML dashboard
- `/stats` — JSON stats of the server
- `/accounts` — add an account. On a real server, send this to your friends so they join the pool without exposing credentials.

## CLI

There is a CLI to manage the accounts

```sh
codex-balancer accounts add                 # sign in through a local browser
codex-balancer accounts list
```

State lives in `~/.codex-balancer/state.db`.

SQLite stores credentials, routed turns, and events. The server fetches current limits, reset credits, current-cycle credit burn, auth state, and model lists. It derives status, totals, activity, response time, and current-month cost from those facts.

## Point Codex at it

On every machine that runs Codex, save the server key at `~/.codex/balancer-api-key` with mode `600`:

```sh
chmod 600 ~/.codex/balancer-api-key
```

In `~/.codex/config.toml`:

```toml
model_provider = "balancer"

[model_providers.balancer]
name = "OpenAI" # must be exactly this for server-side compaction to work
base_url = "http://127.0.0.1:8317/v1"
supports_websockets = true

[model_providers.balancer.auth]
command = "/bin/sh"
args = ["-c", "exec /bin/cat \"$HOME/.codex/balancer-api-key\""]
```

## Routing
Routing logic is in ROUTING.md, keep that up to date and simple, human readable
