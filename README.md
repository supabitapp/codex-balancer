# codex-balancer

Spread Codex turns across several ChatGPT accounts. One proxy endpoint, no
dashboard, no database.

A turn sticks to the account that served its thread; otherwise it goes to the
account with the most headroom. Rate limits and dead connections fail over to
another account before the first byte reaches the client.

## Install

```sh
go install github.com/supabitapp/codex-balancer@latest
```

## Add accounts

Log in to Codex on each account, then import the credentials it wrote:

```sh
codex-balancer accounts add                        # reads ~/.codex/auth.json
codex-balancer accounts add ~/.codex/accounts/*.auth.json
codex-balancer accounts list
```

Accounts live in `~/.codex-balancer/accounts.json`, mode 0600. Importing copies
the refresh token, and refresh tokens rotate on use, so once the balancer
refreshes an account, pointing Codex back at it directly means logging in again.
Treat the balancer as the owner of every account you give it.

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
