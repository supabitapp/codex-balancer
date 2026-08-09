# codex-balancer

Spread Codex turns across several ChatGPT accounts from one Cloudflare Worker.

## Architecture

- A regular Worker handles routes, auth, HTTP and WebSocket proxying.
- One `BalancerState` Durable Object, named `global`, owns all state in its SQLite store.
- Cloudflare serves the dashboard, admin, and account setup files from `public/` through the `ASSETS` binding.

| Route                                 | Methods                       | Auth                                                               |
| ------------------------------------- | ----------------------------- | ------------------------------------------------------------------ |
| `/healthz`                            | `GET`                         | None                                                               |
| `/v1/models`                          | `GET`                         | `Bearer BALANCER_KEY`                                              |
| `/v1/responses`                       | `POST`, WebSocket `GET`       | `Bearer BALANCER_KEY`                                              |
| `/dashboard`                          | `GET`, `HEAD`                 | None                                                               |
| `/stats`                              | `GET`                         | None                                                               |
| `/dashboard/ws`                       | WebSocket `GET`               | None                                                               |
| `/accounts?invite=TOKEN`              | `GET`                         | Valid invite; sets an HttpOnly cookie and redirects to `/accounts` |
| `/accounts`                           | `GET`, `HEAD`                 | None                                                               |
| `/accounts/status`                    | `GET`                         | Invite cookie                                                      |
| `/accounts/device`                    | `POST`                        | Invite cookie                                                      |
| `/admin`, `/admin.js`, `/admin/state` | `GET`, plus `HEAD` for assets | Cloudflare Access                                                  |
| `/admin/invites`                      | `POST`                        | Cloudflare Access                                                  |
| `/admin/accounts/:id`                 | `PATCH`, `DELETE`             | Cloudflare Access                                                  |

`/` redirects to `/dashboard`. Unknown routes return `404`.

The dashboard routes are public. They expose account aliases, plans, quota status, and aggregate traffic only. They never expose emails, account IDs, tokens, thread IDs, session IDs, or raw errors.

## Local development

Use Node 26.

```sh
npm ci
cp .dev.vars.example .dev.vars
$EDITOR .dev.vars
npm test
npm run dev
```

`.dev.vars` must set:

- `BALANCER_KEY`: bearer key used by Codex.
- `TOKEN_ENCRYPTION_KEY`: base64 or base64url encoding of exactly 32 random bytes.

Generate new values with:

```sh
openssl rand -hex 32
openssl rand -base64 32 | tr -d '\n'
```

Run the full local gate with `npm run check`. `/admin*` fails closed unless the request carries a valid Cloudflare Access assertion.

## Deploy

Use a Workers Paid plan. The Worker reserves up to five minutes of CPU for bounded decompression and relay work.

Choose a custom hostname. `codex-balancer.supabit.app` is an example only; it is not the chosen domain in this guide. Refer to your choice as `HOST` below.

Set these GitHub repository secrets:

| Secret                  | Value                                             |
| ----------------------- | ------------------------------------------------- |
| `CLOUDFLARE_API_TOKEN`  | Cloudflare API token allowed to deploy the Worker |
| `CLOUDFLARE_ACCOUNT_ID` | Cloudflare account ID                             |
| `BALANCER_KEY`          | New random bearer key                             |
| `TOKEN_ENCRYPTION_KEY`  | Base64-encoded 32-byte key                        |

Generate and store the Worker secrets without printing them:

```sh
openssl rand -hex 32 | gh secret set BALANCER_KEY
openssl rand -base64 32 | tr -d '\n' | gh secret set TOKEN_ENCRYPTION_KEY
```

Set `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` with `gh secret set NAME`. Keep `TOKEN_ENCRYPTION_KEY` stable or stored account tokens become unreadable.

In Cloudflare:

1. Add `HOST` as the Worker's custom domain.
2. Create a self-hosted Access application for `https://HOST/admin*`.
3. Add the required allow policy.
4. Copy its audience tag and your team domain.

Set the values as GitHub repository variables:

```sh
gh variable set ACCESS_AUD --body '<application-audience-tag>'
gh variable set ACCESS_TEAM_DOMAIN --body '<team>.cloudflareaccess.com'
```

Do not include a scheme or path in `ACCESS_TEAM_DOMAIN`. The Worker verifies the Access assertion as well as relying on the edge policy.

Pull requests run `npm run check`. Each push to `main` runs the same gate, deploys, then calls the deployment URL at `/healthz`. The smoke test passes only when the body matches:

```json
{
  "status": "ok",
  "sha": "<exact GitHub commit SHA>",
  "storage": "ok"
}
```

The workflow checks `.status == "ok"`, `.sha == github.sha`, and `.storage == "ok"`.

This starts a new Cloudflare state store. Add accounts through `/admin`; no local SQLite data is imported.

## Add accounts

Open `https://HOST/admin` through Cloudflare Access. Create an invite and send its single-use URL to your friend. They open it, press **Start sign-in**, and finish the device sign-in. A link preview does not consume the invite or start sign-in.

## Configure Codex

Set the same `BALANCER_KEY` in `CODEX_BALANCER_KEY`, then add this to `~/.codex/config.toml`:

```toml
model_provider = "balancer"

[model_providers.balancer]
name = "OpenAI"
base_url = "https://HOST/v1"
requires_openai_auth = true
env_key = "CODEX_BALANCER_KEY"
supports_websockets = true
```

`name` must be exactly `OpenAI`. Replace `HOST` with the custom hostname.
