# Browser admin

Open `/admin` to manage fast mode, pause or resume accounts, change routing
priority, remove accounts, create or revoke API keys, and view connections and
recent events. This is a hidden route with no link from the public dashboard.
Adding an account uses the existing `/accounts` sign-in flow.

## Set or reset the password

Run this on the server:

```sh
codex-balancer admin password
```

Enter the new password twice at the hidden prompts. Use at least 8 bytes
(a long passphrase works). The same command resets a forgotten password;
it does not require the previous password. Remote entry requires a terminal:

```sh
ssh -t codex-balancer.exe.xyz codex-balancer admin password
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

Admin sessions are kept in server memory and expire after 12 hours. Server
restarts, password resets, and disabling access end existing sessions. Cookies
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
- Pause and removal retire an account's existing sockets immediately. Resume
  and routing priority changes use the same pool operations as the TUI/CLI.
- New API key secrets appear only in the creation response. Key lists show
  names, status, dates, and usage, never existing secrets. Revocation rejects
  new requests using the key; it does not terminate already-authenticated
  WebSockets.
- The status section refreshes every five seconds. Refresh the page to pick
  up account or key changes made in another browser or through the CLI.

The UI uses Go templates and HTMX. Full-page form submissions also work
without JavaScript; HTMX provides section updates and confirmation dialogs.
