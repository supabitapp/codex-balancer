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

- `/v1/responses` - the websocket only proxy route (also `/codex/responses` and `/v1/codex/responses` for pi)
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

Adding an account preserves its existing model training setting.
Self-serve Business Pro Lite
(`self_serve_business_prolite`) accounts route using their per-account quota.
Other Business and Enterprise workspaces are displayed but excluded from routing.

Use the CLI to manage client API keys:

```sh
codex-balancer keys add my-laptop
codex-balancer keys list
codex-balancer keys rm my-laptop
```

`keys list` includes the input, cached, output, and total tokens attributed to
each key.

New keys use a JWT envelope for compatibility with clients such as pi. They are
still opaque bearer credentials: the server requires an exact match of the
entire stored key, not trusted JWT claims or signature validation. The embedded
account ID is synthetic, not a pool account. Existing `cb_` and legacy keys
remain valid; no database migration or key rotation is required for Codex.

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

## Point pi at it

Use pi's built-in `openai-codex` provider; no fork or patch is needed.

Use a JWT-shaped key created by this version of the server, either in the admin
page or with `codex-balancer keys add pi-laptop` on the server. Older `cb_` and
UUID-style keys still work with the balancer, but pi rejects them locally before
sending a request. Create a new key for pi rather than editing an existing key.

Export a server key before starting pi, just as for Codex:

```sh
export CODEX_BALANCER_API_KEY="<server-key>"
```

Merge this into `~/.pi/agent/models.json`, keeping any unrelated providers:

```json
{
  "providers": {
    "openai-codex": {
      "baseUrl": "http://127.0.0.1:8317/v1",
      "apiKey": "$CODEX_BALANCER_API_KEY"
    }
  }
}
```

Keep the `$` in the `apiKey` value: it tells
pi to read the environment variable rather than send its name as a literal key.

If you previously used `/login` for `openai-codex`, run `/logout` in pi and select
that provider. Saved credentials in `~/.pi/agent/auth.json` take precedence over
the configured key. You do not need a ChatGPT login on the pi client; the server
manages the pool's accounts.

Then start pi and select a model your pool supports:

```sh
pi --provider openai-codex --model gpt-5.6-sol
```

Or use `/model` and choose an `openai-codex` model. Leave `transport` at `"auto"`
(the default), which tries WebSockets first. If you previously set it to `"sse"`
in `~/.pi/agent/settings.json` or `.pi/settings.json`, change it to `"auto"`.
The balancer does not support HTTP/SSE responses: a failed WebSocket connection
followed by an SSE fallback can surface as `405 Method Not Allowed`.

This redirects **all** `openai-codex` models through the balancer. When migrating
from a custom `balancer` provider, remove its old block and update any saved
`defaultProvider` or `enabledModels` references in pi's settings. Model metadata
comes from pi, not discovery from the balancer's `/v1/models` endpoint; refresh
pi's catalog with `pi update --models`. A listed model still needs an eligible
account in the pool.

## Routing

Routing logic is in ROUTING.md.
