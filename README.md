# codex-balancer

<img width="3000" height="1930" alt="screenshot-Codex Balancer" src="https://github.com/user-attachments/assets/199da68d-c73e-4614-8776-349ec60df87b" />

_I wrote this README by hand, no LLM :)_

Balancing usage across several ChatGPT Codex accounts.

- One Responses endpoint, with HTTP and WebSocket transports
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

- `/v1/responses` — HTTP `POST` (SSE or JSON) and WebSocket `GET`
- `/codex/responses` and `/v1/codex/responses` — WebSocket `GET` aliases for pi
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
HTTP Responses is supported at `POST /v1/responses`, not at the Codex aliases.
Pi's Codex SSE fallback uses those aliases, which remain GET-only; keep this
provider on `"auto"` rather than forcing `"sse"`.

This redirects **all** `openai-codex` models through the balancer. When migrating
from a custom `balancer` provider, remove its old block and update any saved
`defaultProvider` or `enabledModels` references in pi's settings. Model metadata
comes from pi, not discovery from the balancer's `/v1/models` endpoint; refresh
pi's catalog with `pi update --models`. A listed model still needs an eligible
account in the pool.

## Point OpenCode at it

Use the standard OpenAI Responses provider with a balancer API key:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "openai/gpt-6-astra",
  "provider": {
    "openai": {
      "options": {
        "baseURL": "https://codex-balancer.exe.xyz/v1",
        "apiKey": "{env:CODEX_BALANCER_API_KEY}"
      }
    }
  }
}
```

Choose a model your pool supports; the balancer never substitutes model names.
For a local server, use `http://127.0.0.1:8317/v1`. Export
`CODEX_BALANCER_API_KEY` before starting OpenCode. No experimental WebSocket
flag, OAuth login, per-model instructions, or disabled title generation is
needed. An existing ChatGPT OAuth login in OpenCode can redirect requests away
from this endpoint; disconnect that login and use the balancer key instead.

OpenCode inherits the OpenAI catalog. A `provider.openai.models` list is not
required. That catalog is **not pool discovery**: model availability and context
limits can differ from your pool. OpenCode may independently select an unavailable
small/background model. If necessary, set `"small_model": "openai/gpt-6-astra"`
(or another pool-supported model) at the top level. Selecting the default model
does not make every catalog model routable or reconcile catalog limits.

### HTTP Responses contract

`POST /v1/responses` accepts a JSON object with a non-empty string `model`.
`stream:true` returns incrementally flushed Responses SSE events; omitted or
false returns a Responses JSON object. Each request has its own upstream Codex
WebSocket, including overlapping conversation and title requests.

| Request field | HTTP adapter policy |
| --- | --- |
| `model` | Required; preserved exactly. Existing pool/model/tier selection applies. |
| `input` | String becomes one user message; arrays preserve remaining item order and roles. Missing input becomes `[]`. |
| `instructions`, leading system/developer messages | Explicit instructions first, then the contiguous leading instruction messages, joined with two newlines. Text parts use the same separator. Absent instructions become `""`; no default prompt is invented. Later instruction messages stay in place. Non-text or extra semantic fields on lifted instructions are rejected rather than discarded. |
| `stream` | Boolean HTTP control, not forwarded upstream. |
| `store` | Omitted/false becomes false; true is rejected. |
| `background` | Omitted/false accepted and removed; true is rejected. |
| `previous_response_id`, `conversation` | Only absent/null accepted. Send full history, including encrypted reasoning and tool results, on every request. Stored item references are rejected. |
| `max_output_tokens`, `temperature`, `top_p` | Valid positive integer token limits and valid numeric sampling ranges are accepted but omitted for Codex compatibility. They are **not enforced**; do not rely on an output token cap or sampling control. |
| `type`, `generate`, `response`, `status`, `status_code`, `headers`, `stream_options` | Rejected HTTP/protocol controls. One `response.create` is generated by the adapter. |
| Tools/calls/results, images, `text` structured output, `reasoning`, `include`, encrypted reasoning, cache keys, metadata | Preserved, including unknown JSON fields and numeric precision. Actual feature support still depends on Codex and the selected model. |
| `service_tier` | Preserved unless the global fast-mode policy overrides it, before selection and accounting. |

The three compatibility omissions are intentionally narrow: the pinned OpenAI
SDK can emit them, while Codex's request types have no matching generation
controls. OpenCode's chat hook removes its output-token limit, but auxiliary
paths can bypass that hook. Unknown fields are not guessed away or retried with
different payloads; upstream rejects unsupported semantics normally.

Requests are limited to 8 MiB. Compressed bodies are rejected with 415. JSON
retention and upstream frames are bounded to 16 MiB; oversized/unfinished output
fails rather than returning truncated success. JSON uses non-empty terminal
output, otherwise complete `response.output_item.done` items ordered by
`output_index`. Empty output with no item events remains empty; unfinished items
are not converted to empty success. It does not buffer event history. No stored-response retrieval,
background execution, or HTTP response-ID continuation is implemented.

See [HTTP execution and errors](ROUTING.md#http-execution-and-errors) for failure,
retry, timeout and shutdown behavior. This is a Codex-backed subset of Responses,
not a complete implementation of the OpenAI API.

### Hermetic client verification

The opt-in test uses OpenCode's pinned `@ai-sdk/openai` 3.0.88, its existing SDK
patch, and `ai` 6.0.168 against a local mock upstream. It exercises streaming
text/reasoning/tool calls, a full-history JSON tool-result turn, and an auxiliary
structured-output request, plus failures before and after SSE starts. It runs
the **actual AI SDK adapter**, not the full
OpenCode application or live ChatGPT inference. Normal Go tests use protocol
fixtures and require no JavaScript dependencies.

Prepare dependencies outside both repositories (no lifecycle scripts):

```sh
sdk_dir="$(mktemp -d)"
npm install --prefix "$sdk_dir" --ignore-scripts --no-audit --no-fund --save-exact \
  @ai-sdk/openai@3.0.88 ai@6.0.168 zod@4.1.8
# Use the existing patch from the read-only OpenCode checkout at 95daf90670b7:
(cd "$sdk_dir/node_modules/@ai-sdk/openai" && \
  git apply /path/to/opencode/patches/@ai-sdk%2Fopenai@3.0.88.patch)
CODEX_BALANCER_TEST_SDK="$sdk_dir" go test ./internal/app -run TestHTTPResponsesPinnedSDK -count=1 -v
```

The test isolates HOME/XDG state and restricts inference fetches to its local
balancer. It does not install plugins, load personal credentials, or discover
models over the network.

## Routing

Routing logic is in ROUTING.md.
