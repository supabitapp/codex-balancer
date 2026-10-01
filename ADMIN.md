# Browser admin

Open `/admin` for the live dashboard with nothing redacted: full account
emails and IDs, client IP addresses, API key names on active threads, full
thread and turn identifiers, and unmasked event details. Admin controls sit
inline: fast mode in the overview; a routing dropdown, banked reset, and a
⋯ menu with refresh and remove on each account row; and an API keys section
with a + new key button and per-key revoke. This
is a hidden route with no link from the public dashboard. Adding an account
uses the existing `/accounts` sign-in flow.

## Set or reset the password

Run this on the server:

```sh
codex-balancer admin password
```

Enter the new password twice at the hidden prompts. Use at least 8 bytes
(a long passphrase works). The same command resets a forgotten password;
it does not require the previous password. Remote entry requires a terminal:

```sh
ssh -t balancer codex-balancer admin password
```

To choose another database or turn admin access off:

```sh
codex-balancer admin password -state /path/to/state.db
codex-balancer admin disable
```

Passwords cannot be passed as command arguments or through a pipe. Setting
or resetting the password and disabling access take effect without restarting
the server. Existing admin sessions become invalid on their next request.

## Storage and access

The existing `~/.codex-balancer/state.db` stores the password verifier in
`settings.admin_password_hash`. It contains a random 16-byte salt and a
PBKDF2-HMAC-SHA256 hash using 600,000 iterations. The plaintext password is
not stored. Database backups also contain this verifier and the existing
account credentials and API keys.

Admin access starts disabled until a password is set. Client inference API
keys do not grant admin access. Serve the admin interface over HTTPS; plain
HTTP cookies are allowed only for direct loopback development.

Admin sessions are stored in `admin_sessions` in the same database, so they
survive restarts and deploys and are shared by servers using that database.
Only a SHA-256 digest of each session token is stored. A session lasts 30 days
and extends to 30 days again on use once a day has passed, so an admin who
visits at least monthly stays signed in. Signing out, password resets, and
disabling access end sessions; at most 128 are kept, dropping the oldest. Cookies
are host-only, HttpOnly, Secure, and SameSite=Strict in production. Forms use
CSRF tokens and reject cross-site submissions. Login attempts are limited to
10 per minute across the server to bound password-hashing work, including
behind a reverse proxy.

## Controls

- Fast mode saves to SQLite and applies immediately in the server handling the
  request. Other servers sharing the database pick it up on their settings
  poll. A changed mode restarts existing WebSockets and preserves account
  ownership under the normal routing rules. Repeating a mode does not restart
  connections.
- Account routing uses the normal, priority, and paused dropdown on each row
  and saves on change. Paused retires the account's existing sockets immediately;
  normal or priority resumes routing. Removal also retires existing sockets.
- New API key secrets appear only in the creation response and use the
  [pi-compatible JWT format](README.md#point-pi-at-it). Existing keys remain valid.
  Key lists show names, status, dates, and usage, never existing secrets.
  Revocation rejects new requests using the key; it does not terminate
  already-authenticated WebSockets.
- The admin dashboard streams updates every second over `/admin/events`, a
  per-session stream that ends when the session expires or signs out. API key
  usage totals refresh every 30 seconds and after key changes.

The UI reuses the dashboard's Go templates with HTMX. Full-page form
submissions also work without JavaScript; HTMX provides in-place updates,
notices, and confirmation dialogs.
