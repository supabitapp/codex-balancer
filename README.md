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
export CODEX_BALANCER_API_KEY=$(codex-balancer keys add my-laptop)
codex-balancer server           # serve the proxy with a TUI
```

`keys add` prints the generated key. Save it on the client that will use it.
Run the command with another name to provision another client.

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

Use the CLI to manage client API keys:

```sh
codex-balancer keys add my-laptop
codex-balancer keys list
codex-balancer keys rm my-laptop
```

`keys list` includes the input, cached, output, and total tokens attributed to
each key.

State lives in `~/.codex-balancer/state.db`.

SQLite stores account credentials and settings, client API keys with cumulative
token counters, the latest route owner for active conversations, a stable client
identity salt, and only the current month's response facts needed to reprice API
cost. Limits, reset credits, credit burn, model lists, prices, dashboard events,
and other computed state stay in memory and are fetched or rebuilt after restart.

## Point Codex at it

On each machine that runs Codex, export a key from the server before starting
Codex:

```sh
export CODEX_BALANCER_API_KEY="<server-key>"
```

For Fish, use `set -x CODEX_BALANCER_API_KEY "<server-key>"`. To persist it, add the appropriate command to a private startup file that your shell sources.

In `~/.codex/config.toml`:

```toml
model_provider = "balancer"

[model_providers.balancer]
name = "OpenAI" # must be exactly this for server-side compaction to work
base_url = "http://127.0.0.1:8317/v1"
env_key = "CODEX_BALANCER_API_KEY"
requires_openai_auth = true
supports_websockets = true
```

Codex reads the bearer token from the environment that launches it. Do not also configure `[model_providers.balancer.auth]`; command-backed auth and `env_key` are mutually exclusive.

## Routing
Routing logic is in ROUTING.md, keep that up to date and simple, human readable
