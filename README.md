# codex-balancer

<img width="2984" height="1796" alt="CleanShot 2026-08-09 at 17 55 30@2x" src="https://github.com/user-attachments/assets/e39dcef1-1de3-4e4e-8cc3-af94769c2054" />


Spread Codex turns across several ChatGPT accounts. 

Dead simple, one proxied endpoint, 1 SQLite database.

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

State lives in `~/.codex-balancer/state.db`.

SQLite stores credentials, affinity owners, routed turns, and events. The
server fetches current limits, reset credits, auth state, and model lists. It
derives status, totals, activity, response time, and current-month cost from
those facts.

## Serve

```sh
export CODEX_BALANCER_KEY=$(openssl rand -hex 16)
codex-balancer server           # serve the proxy
```

The usage poll redeems the earliest available rate-limit reset credit once it
is within one hour of expiry.

Fresh routes keep hard and soft affinity, then prefer live accounts with a
quota window that resets within one hour. The earliest reset wins. Remaining
routes use the lowest limit use, then the account used least recently.

`GET /stats` returns the same live account status as public JSON. Account emails
hide their domain and all but the first and last local-part characters, such as
`k***i@***.com`. The dashboard stores and shows only a short ID keyed by a
server-only secret; source IPs never enter saved state.

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
