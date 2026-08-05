# codex-balancer

Spread Codex turns across several ChatGPT accounts. One proxy endpoint, no
dashboard, no database.

## Install

```sh
go install github.com/supabitapp/codex-balancer@latest
```

## Add accounts

Log in to Codex on each account, then import the credentials it wrote:

```sh
codex-balancer accounts add                  # reads ~/.codex/auth.json
codex-balancer accounts add ./other.json
codex-balancer accounts list
```

Accounts live in `~/.codex-balancer/accounts.json`, mode 0600.

## Serve

```sh
export CODEX_BALANCER_KEY=$(openssl rand -hex 16)
codex-balancer server
```

## Point Codex at it

In `~/.codex/config.toml`:

```toml
model_provider = "balancer"

[model_providers.balancer]
name = "OpenAI"
base_url = "http://127.0.0.1:8317/v1"
requires_openai_auth = true
```

`name` must be exactly `OpenAI`: Codex compares it verbatim to decide whether a
provider supports remote compaction and whether to keep encrypted tool
arguments intact.

Authenticate the CLI against the balancer once:

```sh
printenv CODEX_BALANCER_KEY | codex login --with-api-key
```

## What it serves

| Method | Path | |
| --- | --- | --- |
| POST | `/v1/responses` | Picks an account, swaps in its credentials, streams the reply back untouched |
| GET | `/v1/models` | `{"models":[]}`, so `codex doctor` passes and Codex keeps its built-in catalog |

Everything else Codex can reach is either unused or belongs to a dashboard we
do not have.

## How it routes

A turn sticks to the account that served its thread, keyed on the session
headers Codex already sends. Bindings expire after 12 idle hours. When the
bound account is rate limited the turn goes elsewhere and the binding stays, so
traffic returns once the window resets.

Otherwise it picks the account with the most headroom, read from the
`x-codex-*-used-percent` headers on every upstream reply, and breaks ties by
whichever account has idled longest.

Failover happens before the first byte reaches the client: a 429, a 5xx or a
dead connection moves the turn to another account, up to three. After the
stream opens the turn is committed, so a mid-stream failure just closes the
connection and Codex retries the turn itself.

## Develop

```sh
go build ./...
go test ./...
```
