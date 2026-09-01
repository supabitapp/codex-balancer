# codex-balancer

<img width="3000" height="1930" alt="screenshot-Codex Balancer" src="https://github.com/user-attachments/assets/199da68d-c73e-4614-8776-349ec60df87b" />

_I wrote this README by hand, no LLM :)_

Balancing usage across several ChatGPT Codex accounts.

- Dead simple, 1 single websocket endpoint
- 1 single SQLite database

## Install

```sh
go install github.com/supabitapp/codex-balancer@latest
```

## Running the proxy

```
codex-balancer server           # serve the proxy with a TUI at 
```

The server runs at http://127.0.0.1:8317

- `/v1/responses` - the websocket only proxy route
- `/dashboard` — HTML dashboard
- `/stats` — JSON stats of the server
- `/accounts` — add an account. On a real server, send this to your friends so they join the pool without exposing credentials.

The TUI also allows you to put a `pause` or `priority` on some accounts.

## CLI

There is a CLI to manage the accounts

```sh
codex-balancer accounts add                 # sign in through a local browser
codex-balancer accounts list
codex-balancer accounts mode you@example.com priority
codex-balancer accounts mode you@example.com normal
```

Adding an account turns off ChatGPT model training for that account before it
enters the pool.

Use the CLI to manage client API keys:

```sh
codex-balancer keys add my-laptop
codex-balancer keys list
codex-balancer keys rm my-laptop
```

`keys list` includes the input, cached, output, and total tokens attributed to
each key.

State lives in `~/.codex-balancer/state.db`.

## Point Codex at it

On each machine that runs Codex, export a key from the server before starting
Codex:

```sh
export CODEX_BALANCER_API_KEY="<server-key>"
```

add that to your `~/.zshrc` or whatever env loading mechanism or shell you use.

Then in `~/.codex/config.toml`:

```toml
model_provider = "balancer"

[model_providers.balancer]
name = "OpenAI" # must be exactly this for server-side compaction to work
base_url = "http://127.0.0.1:8317/v1"
env_key = "CODEX_BALANCER_API_KEY"
requires_openai_auth = true
supports_websockets = true
```

## Routing
Routing logic is in ROUTING.md.
