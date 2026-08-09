import { DatabaseSync } from "node:sqlite";

import { describe, expect, it } from "vitest";

import { initializeStateSchema } from "../src/state-schema.js";

const createDatabase = (): DatabaseSync => {
  const database = new DatabaseSync(":memory:");
  const sql = {
    exec(query: string): void {
      database.exec(query);
    },
  } as unknown as SqlStorage;
  initializeStateSchema(sql);
  return database;
};

const addAccount = (database: DatabaseSync, accountId = "account-a"): void => {
  database
    .prepare(
      `INSERT INTO credential_generations (account_id, last_revision)
       VALUES (?, 1)`,
    )
    .run(accountId);
  database
    .prepare(
      `INSERT INTO accounts (
        account_id, id_token, access_token, refresh_token, paused, last_refresh_at_ms
      ) VALUES (?, ?, ?, ?, 0, ?)`,
    )
    .run(accountId, "id-token", "access-token", "refresh-token", 100);
};

const count = (database: DatabaseSync, table: string): number => {
  const row = database.prepare(`SELECT COUNT(*) AS total FROM ${table}`).get();
  if (row === undefined || typeof row.total !== "number") {
    throw new Error(`could not count ${table}`);
  }
  return row.total;
};

describe("state schema", () => {
  it("keeps owned account state linked while retaining history", () => {
    const database = createDatabase();
    addAccount(database);
    database.exec(`
      INSERT INTO account_runtime (account_id) VALUES ('account-a');
      INSERT INTO account_windows (
        account_id, kind, used_percent, window_minutes, observed_at_ms
      ) VALUES ('account-a', 'primary', 12.5, 300, 100);
      INSERT INTO account_model_catalogs (account_id, models_json)
      VALUES ('account-a', '[{"slug":"model-a"}]');
      INSERT INTO bindings (
        kind, value, account_id, created_at_ms, last_used_at_ms
      ) VALUES ('session', 'session-a', 'account-a', 100, 100);
      INSERT INTO events (
        at_ms, kind, account_id, detail, duration_ms, input_tokens,
        cached_tokens, cache_write_tokens, output_tokens
      ) VALUES (
        100, 'route', 'account-a', 'http', 1.5, 10, 2, 1, 3
      );
    `);
    expect(
      database
        .prepare(
          "SELECT open_websockets FROM account_runtime WHERE account_id = 'account-a'",
        )
        .get(),
    ).toEqual({ open_websockets: 0 });

    database.exec("DELETE FROM accounts WHERE account_id = 'account-a'");

    expect(count(database, "account_runtime")).toBe(0);
    expect(count(database, "account_windows")).toBe(0);
    expect(count(database, "account_model_catalogs")).toBe(0);
    expect(count(database, "credential_generations")).toBe(1);
    expect(count(database, "bindings")).toBe(1);
    expect(count(database, "events")).toBe(1);
    database.close();
  });

  it("supports revision-checked credential rotation", () => {
    const database = createDatabase();
    addAccount(database);
    const update = database.prepare(`
      UPDATE accounts
      SET access_token = ?, refresh_token = ?, last_refresh_at_ms = ?
      WHERE account_id = ?
    `);
    const advance = database.prepare(`
      UPDATE credential_generations SET last_revision = last_revision + 1
      WHERE account_id = ? AND last_revision = ?
    `);
    const rotate = (
      expectedRevision: number,
      accessToken: string,
      refreshToken: string,
      refreshedAtMs: number,
    ): number => {
      database.exec("BEGIN IMMEDIATE");
      try {
        if (advance.run("account-a", expectedRevision).changes !== 1) {
          database.exec("ROLLBACK");
          return 0;
        }
        const changed = update.run(
          accessToken,
          refreshToken,
          refreshedAtMs,
          "account-a",
        ).changes;
        if (changed !== 1) {
          throw new Error("account disappeared during rotation");
        }
        database.exec("COMMIT");
        return 1;
      } catch (error) {
        database.exec("ROLLBACK");
        throw error;
      }
    };

    expect(rotate(1, "access-new", "refresh-new", 200)).toBe(1);
    expect(rotate(1, "access-stale", "refresh-stale", 150)).toBe(0);
    expect(
      database
        .prepare(
          `SELECT a.access_token, a.refresh_token, a.last_refresh_at_ms,
                  g.last_revision
           FROM accounts a
           JOIN credential_generations g ON g.account_id = a.account_id
           WHERE a.account_id = 'account-a'`,
        )
        .get(),
    ).toEqual({
      access_token: "access-new",
      last_refresh_at_ms: 200,
      last_revision: 2,
      refresh_token: "refresh-new",
    });
    expect(
      database
        .prepare(
          "SELECT last_revision FROM credential_generations WHERE account_id = 'account-a'",
        )
        .get(),
    ).toEqual({ last_revision: 2 });
    expect(
      database
        .prepare("PRAGMA table_info(accounts)")
        .all()
        .map((column) => column.name),
    ).not.toContain("credential_revision");
    database.close();
  });

  it("enforces single-use onboarding and schedules pending jobs", () => {
    const database = createDatabase();
    database.exec(`
      INSERT INTO account_invites (
        invite_id, token_hash, created_at_ms, expires_at_ms
      ) VALUES ('invite-a', 'hash-a', 100, 1000);
      INSERT INTO account_invites (
        invite_id, token_hash, created_at_ms, expires_at_ms
      ) VALUES ('invite-b', 'hash-b', 100, 1000);
      INSERT INTO account_invites (
        invite_id, token_hash, created_at_ms, expires_at_ms
      ) VALUES ('invite-invalid-interval', 'hash-interval', 100, 1000);
      INSERT INTO account_invites (
        invite_id, token_hash, created_at_ms, expires_at_ms
      ) VALUES ('invite-invalid-status', 'hash-status', 100, 1000);
      INSERT INTO device_logins (
        login_id, invite_id, device_auth_id, user_code, poll_interval_ms,
        expires_at_ms, next_poll_at_ms, status, verification_url
      ) VALUES (
        'login-later', 'invite-a', 'device-a', 'CODE-A', 5000,
        1000, 500, 'pending', 'https://example.com/device'
      );
      INSERT INTO device_logins (
        login_id, invite_id, device_auth_id, user_code, poll_interval_ms,
        expires_at_ms, next_poll_at_ms, status, verification_url
      ) VALUES (
        'login-due', 'invite-b', 'device-b', 'CODE-B', 5000,
        1000, 200, 'pending', 'https://example.com/device'
      );
    `);

    expect(
      database
        .prepare(
          `SELECT login_id FROM device_logins
           WHERE status = 'pending' AND next_poll_at_ms <= ?
           ORDER BY next_poll_at_ms`,
        )
        .all(300),
    ).toEqual([{ login_id: "login-due" }]);
    expect(() => {
      database.exec(`
        INSERT INTO account_invites (
          invite_id, token_hash, created_at_ms, expires_at_ms
        ) VALUES ('invite-c', 'hash-a', 100, 1000)
      `);
    }).toThrow();
    expect(() => {
      database.exec(
        "UPDATE account_invites SET used_at_ms = 200 WHERE invite_id = 'invite-a'",
      );
    }).toThrow();
    expect(() => {
      database.exec(
        "UPDATE account_invites SET session_hash = 'session-a' WHERE invite_id = 'invite-a'",
      );
    }).toThrow();
    expect(
      database
        .prepare(
          `UPDATE account_invites SET used_at_ms = 200, session_hash = 'session-a'
           WHERE invite_id = 'invite-a'`,
        )
        .run().changes,
    ).toBe(1);
    expect(() => {
      database.exec(`
        INSERT INTO account_invites (
          invite_id, token_hash, created_at_ms, expires_at_ms
        ) VALUES ('invite-expired', 'hash-expired', 100, 100)
      `);
    }).toThrow();
    expect(() => {
      database.exec(`
        INSERT INTO device_logins (
          login_id, invite_id, device_auth_id, user_code, poll_interval_ms,
          expires_at_ms, next_poll_at_ms, status, verification_url
        ) VALUES (
          'login-invalid-interval', 'invite-invalid-interval',
          'device-invalid-interval', 'CODE', 0, 1000, 200, 'pending',
          'https://example.com/device'
        )
      `);
    }).toThrow();
    expect(() => {
      database.exec(`
        INSERT INTO device_logins (
          login_id, invite_id, device_auth_id, user_code, poll_interval_ms,
          expires_at_ms, next_poll_at_ms, status, verification_url
        ) VALUES (
          'login-invalid-status', 'invite-invalid-status',
          'device-invalid-status', 'CODE', 5000, 1000, 200, 'expired',
          'https://example.com/device'
        )
      `);
    }).toThrow();
    database.exec("DELETE FROM account_invites WHERE invite_id = 'invite-b'");
    expect(
      database
        .prepare(
          "SELECT login_id FROM device_logins WHERE login_id = 'login-due'",
        )
        .get(),
    ).toBeUndefined();
    database.close();
  });

  it("rejects invalid routing and accounting states", () => {
    const database = createDatabase();
    addAccount(database);

    for (const statement of [
      `INSERT INTO encryption_state (id, verifier) VALUES (2, 'invalid')`,
      `UPDATE accounts SET paused = 2 WHERE account_id = 'account-a'`,
      `UPDATE credential_generations SET last_revision = 0
       WHERE account_id = 'account-a'`,
      `INSERT INTO account_runtime (account_id, limit_reached)
       VALUES ('account-a', 2)`,
      `INSERT INTO account_runtime (account_id, open_websockets)
       VALUES ('account-a', -1)`,
      `INSERT INTO account_windows (
         account_id, kind, used_percent, window_minutes, observed_at_ms
       ) VALUES ('account-a', 'daily', 0, 60, 100)`,
      `INSERT INTO bindings (
         kind, value, account_id, created_at_ms, last_used_at_ms
       ) VALUES ('unknown', 'value', 'account-a', 100, 100)`,
      `INSERT INTO model_catalog_state (id, client_version, refreshed_at_ms)
       VALUES (2, '1.0.0', 100)`,
    ]) {
      expect(() => {
        database.exec(statement);
      }).toThrow();
    }
    database.close();
  });

  it("indexes routing, history, and scheduled work queries", () => {
    const database = createDatabase();
    const queries = [
      [
        "SELECT value FROM bindings WHERE account_id = 'account-a'",
        "bindings_account",
      ],
      [
        "SELECT id FROM events WHERE kind = 'route' ORDER BY at_ms DESC",
        "events_kind_at",
      ],
      [
        "SELECT id FROM events WHERE account_id = 'account-a' ORDER BY at_ms DESC",
        "events_account_at",
      ],
      [
        "SELECT invite_id FROM account_invites WHERE expires_at_ms <= 100 ORDER BY expires_at_ms",
        "account_invites_expires",
      ],
      [
        "SELECT login_id FROM device_logins WHERE status = 'pending' AND next_poll_at_ms <= 100 ORDER BY next_poll_at_ms",
        "device_logins_due",
      ],
    ] as const;

    for (const [query, index] of queries) {
      const plan = database
        .prepare(`EXPLAIN QUERY PLAN ${query}`)
        .all()
        .map((row) => String(row.detail))
        .join("\n");
      expect(plan).toContain(index);
    }
    database.close();
  });

  it("can initialize again after an object restart", () => {
    const database = createDatabase();
    const sql = {
      exec(query: string): void {
        database.exec(query);
      },
    } as unknown as SqlStorage;

    initializeStateSchema(sql);
    addAccount(database);

    expect(count(database, "accounts")).toBe(1);
    database.close();
  });
});
