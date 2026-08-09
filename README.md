# codex-balancer

<img width="3024" height="1898" alt="image" src="https://github.com/user-attachments/assets/52f14b54-d3d3-4a00-bd5b-50afc5109337" />


Spread Codex turns across several ChatGPT accounts. 

Dead simple, one proxied endpoint, no database.

## Install

```sh
go install github.com/supabitapp/codex-balancer@latest
```

## Add accounts

```sh
codex-balancer accounts add                 # sign in through a local browser
codex-balancer accounts list
```

Or open `/accounts` in a browser to do it on the web.

Accounts live in `~/.codex-balancer/accounts.json`

## Serve

```sh
export CODEX_BALANCER_KEY=$(openssl rand -hex 16)
codex-balancer server           # serve the proxy
```

`GET /stats` returns the same live account status as public JSON. Account emails
hide their domain and all but the first and last local-part characters, such as
`k***i@***.com`.

Open `/dashboard` and enter the server key for the live web dashboard. It
streams the TUI account, total, thread, and event data over WebSocket.

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
