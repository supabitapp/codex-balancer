const stateSchema = `
CREATE TABLE IF NOT EXISTS encryption_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  verifier TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS credential_generations (
  account_id TEXT PRIMARY KEY,
  last_revision INTEGER NOT NULL CHECK (last_revision > 0)
) STRICT;

CREATE TABLE IF NOT EXISTS accounts (
  account_id TEXT PRIMARY KEY REFERENCES credential_generations(account_id),
  id_token TEXT NOT NULL,
  access_token TEXT NOT NULL,
  refresh_token TEXT NOT NULL,
  paused INTEGER NOT NULL CHECK (paused IN (0, 1)),
  last_refresh_at_ms INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS account_runtime (
  account_id TEXT PRIMARY KEY REFERENCES accounts(account_id) ON DELETE CASCADE,
  reauth_reason TEXT,
  cooldown_until_ms INTEGER NOT NULL DEFAULT 0,
  usage_checked_at_ms INTEGER,
  limit_reached INTEGER CHECK (limit_reached IS NULL OR limit_reached IN (0, 1)),
  banked_resets INTEGER CHECK (banked_resets IS NULL OR banked_resets >= 0),
  open_websockets INTEGER NOT NULL DEFAULT 0 CHECK (open_websockets >= 0)
) STRICT;

CREATE TABLE IF NOT EXISTS account_windows (
  account_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK (kind IN ('primary', 'secondary')),
  used_percent REAL NOT NULL,
  window_minutes INTEGER NOT NULL,
  resets_at_ms INTEGER,
  observed_at_ms INTEGER NOT NULL,
  PRIMARY KEY (account_id, kind)
) STRICT;

CREATE TABLE IF NOT EXISTS bindings (
  kind TEXT NOT NULL CHECK (kind IN ('session', 'prompt_cache', 'turn_state', 'response', 'conversation', 'file')),
  value TEXT NOT NULL,
  account_id TEXT NOT NULL,
  created_at_ms INTEGER NOT NULL,
  last_used_at_ms INTEGER NOT NULL,
  abandoned_at_ms INTEGER,
  PRIMARY KEY (kind, value)
) STRICT;

CREATE INDEX IF NOT EXISTS bindings_account ON bindings (account_id);

CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY,
  at_ms INTEGER NOT NULL,
  kind TEXT NOT NULL,
  account_id TEXT NOT NULL,
  detail TEXT NOT NULL,
  duration_ms REAL NOT NULL,
  input_tokens INTEGER NOT NULL,
  cached_tokens INTEGER NOT NULL,
  cache_write_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS events_kind_at ON events (kind, at_ms);
CREATE INDEX IF NOT EXISTS events_account_at ON events (account_id, at_ms);

CREATE TABLE IF NOT EXISTS model_catalog_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  client_version TEXT NOT NULL,
  refreshed_at_ms INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS account_model_catalogs (
  account_id TEXT PRIMARY KEY REFERENCES accounts(account_id) ON DELETE CASCADE,
  models_json TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS account_invites (
  invite_id TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  created_at_ms INTEGER NOT NULL,
  expires_at_ms INTEGER NOT NULL,
  used_at_ms INTEGER,
  session_hash TEXT,
  CHECK ((used_at_ms IS NULL) = (session_hash IS NULL)),
  CHECK (expires_at_ms > created_at_ms)
) STRICT;

CREATE INDEX IF NOT EXISTS account_invites_expires ON account_invites (expires_at_ms);

CREATE TABLE IF NOT EXISTS device_logins (
  login_id TEXT PRIMARY KEY,
  invite_id TEXT NOT NULL UNIQUE REFERENCES account_invites(invite_id) ON DELETE CASCADE,
  device_auth_id TEXT NOT NULL UNIQUE,
  user_code TEXT NOT NULL,
  poll_interval_ms INTEGER NOT NULL CHECK (poll_interval_ms > 0),
  expires_at_ms INTEGER NOT NULL,
  next_poll_at_ms INTEGER NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending', 'complete', 'failed')),
  verification_url TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS device_logins_due ON device_logins (status, next_poll_at_ms);
`;

export const initializeStateSchema = (sql: SqlStorage): void => {
  sql.exec("PRAGMA foreign_keys = ON");
  sql.exec(stateSchema);
};
