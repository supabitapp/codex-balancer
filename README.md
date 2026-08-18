# codex-balancer

<img width="3024" height="1898" alt="CleanShot 2026-08-14 at 12 20 51@2x" src="https://github.com/user-attachments/assets/9df4be94-7c98-406c-9af5-6ad5d97e4ca0" />


Spread Codex WebSocket connections across several ChatGPT accounts.

Dead simple, one WebSocket-only proxied endpoint, 1 SQLite database.

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

SQLite stores credentials, routed turns, and events. The
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

Balancing applies to new client WebSocket connections. The proxy selects one
account for each connection and keeps it pinned until the connection closes or
the proxy confirms that the account rejected a request. A rejection causes the
client to reconnect and the proxy to reroute the new connection. When an
account enters draining, the proxy restarts sockets pinned to other accounts;
idle sockets restart at once, while active sockets finish their current turns
first. Their new connections prefer the draining account. Sockets already
pinned to it stay connected. Their turns use the fast service tier when that
account's model catalog supports it, so the remaining quota empties sooner.
Priority affects new connections. Other new connections first prefer an
account with a banked reset that expires within 24 hours, then use the lowest
limit use and the account used least recently.

In the TUI, select an account and press `r` to cycle its routing mode through
normal, priority, and draining. Press Space to pause it.

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

On every machine that runs Codex, save the server key at
`~/.codex/balancer-api-key` with mode `600`:

```sh
chmod 600 ~/.codex/balancer-api-key
```

In `~/.codex/config.toml`:

```toml
model_provider = "balancer"

[model_providers.balancer]
name = "OpenAI"
base_url = "http://127.0.0.1:8317/v1"
supports_websockets = true

[model_providers.balancer.auth]
command = "/bin/sh"
args = ["-c", "exec /bin/cat \"$HOME/.codex/balancer-api-key\""]
```

`name` must be exactly `OpenAI`: Codex compares it verbatim to decide whether a
provider supports remote compaction and whether to keep encrypted tool
arguments intact.

Codex runs the auth command and uses its output as the bearer token. The key
stays in one file, and shell startup files need no secret exports.
