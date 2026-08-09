import { DatabaseSync } from "node:sqlite";

import { describe, expect, it } from "vitest";

import type {
  DeviceAuthorization,
  TokenSet,
  UsageObservation,
} from "../src/account-upstream.js";
import { randomToken } from "../src/crypto.js";
import type { AffinityRef, SelectAccountInput } from "../src/domain.js";
import { StateRepository, type StateStorage } from "../src/state-repository.js";
import { initializeStateSchema } from "../src/state-schema.js";

type SqliteBinding = bigint | Uint8Array | string | number | null;

const createEncryptionKey = (): string => randomToken();

const cursor = <T extends Record<string, SqlStorageValue>>(
  rows: readonly T[],
  rowsWritten: number,
): SqlStorageCursor<T> => {
  let index = 0;
  return {
    columnNames: Object.keys(rows[0] ?? {}),
    next() {
      const value = rows[index];
      if (value === undefined) {
        return { done: true };
      }
      index += 1;
      return { done: false, value };
    },
    one() {
      if (rows.length !== 1 || rows[0] === undefined) {
        throw new Error(`expected one row, got ${String(rows.length)}`);
      }
      return rows[0];
    },
    raw<U extends SqlStorageValue[]>() {
      return rows.map((row) => Object.values(row) as U)[Symbol.iterator]();
    },
    rowsRead: rows.length,
    rowsWritten,
    toArray() {
      return [...rows];
    },
    [Symbol.iterator]() {
      return rows[Symbol.iterator]();
    },
  };
};

const createStorage = (): {
  readonly database: DatabaseSync;
  readonly storage: StateStorage;
} => {
  const database = new DatabaseSync(":memory:");
  const sql = {
    exec<T extends Record<string, SqlStorageValue>>(
      query: string,
      ...bindings: SqliteBinding[]
    ): SqlStorageCursor<T> {
      const statementKind = query.trimStart().split(/\s/u, 1)[0]?.toUpperCase();
      if (
        bindings.length === 0 &&
        statementKind !== "SELECT" &&
        statementKind !== "WITH" &&
        query.includes(";")
      ) {
        database.exec(query);
        return cursor([], 0);
      }
      const statement = database.prepare(query);
      if (statementKind === "SELECT" || statementKind === "WITH") {
        return cursor(statement.all(...bindings) as unknown as readonly T[], 0);
      }
      const result = statement.run(...bindings);
      return cursor([], Number(result.changes));
    },
  } as unknown as SqlStorage;
  initializeStateSchema(sql);
  const storage: StateStorage = {
    sql,
    transactionSync<T>(closure: () => T): T {
      database.exec("BEGIN IMMEDIATE");
      try {
        const value = closure();
        database.exec("COMMIT");
        return value;
      } catch (error) {
        database.exec("ROLLBACK");
        throw error;
      }
    },
  };
  return { database, storage };
};

const idToken = (
  accountId: string,
  email = `${accountId}@example.com`,
): string => {
  const payload = Buffer.from(
    JSON.stringify({
      email,
      "https://api.openai.com/auth": {
        chatgpt_account_id: accountId,
        chatgpt_plan_type: "team",
      },
    }),
  ).toString("base64url");
  return `header.${payload}.signature`;
};

const tokens = (accountId: string, revision: string): TokenSet => ({
  accessToken: `access-${revision}`,
  idToken: idToken(accountId),
  refreshToken: `refresh-${revision}`,
});

const selectInput = (
  ref: AffinityRef,
  requiredAccountId: string | null = null,
): SelectAccountInput => ({
  affinity: {
    hard: [ref],
    preferred: null,
    requireUnambiguous: false,
  },
  attempt: 0,
  model: "",
  requiredAccountId,
  serviceTier: "",
  skipAccountIds: [],
  transport: "http",
});

const authorization = (
  deviceAuthId: string,
  expiresAtMs: number,
): DeviceAuthorization => ({
  deviceAuthId,
  expiresAtMs,
  pollIntervalMs: 100,
  userCode: `code-${deviceAuthId}`,
  verificationUrl: "https://example.com/device",
});

let accountSeed = 0;

const addAccount = async (
  repository: StateRepository,
  accountId: string,
  revision: string,
  nowMs: number,
): Promise<void> => {
  accountSeed += 1;
  const seed = String(accountSeed);
  const tokenHash = `seed-hash-${seed}`;
  repository.createInvite(
    `seed-invite-${seed}`,
    tokenHash,
    nowMs - 1,
    nowMs + 10_000,
  );
  const started = repository.startDeviceJob(
    tokenHash,
    `seed-session-${seed}`,
    `seed-login-${seed}`,
    authorization(`seed-device-${seed}`, nowMs + 9_000),
    nowMs,
  );
  if (!started.ok || started.state.login === null) {
    throw new Error("could not seed account");
  }
  const completed = await repository.completeDeviceJob(
    started.state.login,
    tokens(accountId, revision),
    nowMs + 1,
  );
  if (completed !== accountId) {
    throw new Error("could not complete seeded account");
  }
};

describe("state repository credentials", () => {
  it("encrypts tokens and rejects stale credential writes", async () => {
    const { database, storage } = createStorage();
    const repository = new StateRepository(storage, createEncryptionKey());

    await addAccount(repository, "account-a", "one", 100);
    const first = await repository.account("account-a");
    expect(first).toMatchObject({
      accessToken: "access-one",
      credentialRevision: 1,
      refreshToken: "refresh-one",
    });

    const raw = database
      .prepare(
        `SELECT a.id_token, a.access_token, a.refresh_token, g.last_revision
         FROM accounts a
         JOIN credential_generations g ON g.account_id = a.account_id
         WHERE a.account_id = ?`,
      )
      .get("account-a");
    expect(raw).toMatchObject({ last_revision: 1 });
    expect(String(raw?.id_token)).not.toContain("account-a@example.com");
    expect(String(raw?.access_token)).not.toContain("access-one");
    expect(String(raw?.refresh_token)).not.toContain("refresh-one");

    if (first === null) {
      throw new Error("account disappeared");
    }
    await expect(
      repository.updateRefreshedTokens(first, tokens("account-a", "two"), 200),
    ).resolves.toBe(true);
    await expect(
      repository.updateRefreshedTokens(
        first,
        tokens("account-a", "stale"),
        300,
      ),
    ).resolves.toBe(false);
    await expect(repository.account("account-a")).resolves.toMatchObject({
      accessToken: "access-two",
      credentialRevision: 2,
      lastRefreshAtMs: 200,
      refreshToken: "refresh-two",
    });

    database
      .prepare(
        "UPDATE credential_generations SET last_revision = 3 WHERE account_id = ?",
      )
      .run("account-a");
    await expect(repository.account("account-a")).rejects.toThrow();
    database.close();
  });

  it("rejects stale writes after an account is deleted and re-added", async () => {
    const { database, storage } = createStorage();
    const repository = new StateRepository(storage, createEncryptionKey());

    await addAccount(repository, "account-a", "first", 100);
    const stale = await repository.account("account-a");
    if (stale === null) {
      throw new Error("account disappeared");
    }
    expect(repository.deleteAccount("account-a")).toBe(true);
    await expect(
      new StateRepository(storage, createEncryptionKey()).validateEncryption(),
    ).rejects.toThrow();
    await addAccount(repository, "account-a", "replacement", 200);

    await expect(repository.account("account-a")).resolves.toMatchObject({
      accessToken: "access-replacement",
      credentialRevision: 2,
    });
    await expect(
      repository.updateRefreshedTokens(
        stale,
        tokens("account-a", "stale-refresh"),
        300,
      ),
    ).resolves.toBe(false);
    expect(
      repository.setReauthReason(
        "account-a",
        stale.credentialRevision,
        "stale rejection",
      ),
    ).toBe(false);
    await expect(repository.account("account-a")).resolves.toMatchObject({
      accessToken: "access-replacement",
      credentialRevision: 2,
    });
    expect(repository.reauthReason("account-a")).toBeNull();
    expect(
      database
        .prepare(
          "SELECT last_revision FROM credential_generations WHERE account_id = ?",
        )
        .get("account-a"),
    ).toEqual({ last_revision: 2 });
    database.close();
  });

  it("validates the configured encryption key", async () => {
    const empty = createStorage();
    const invalid = new StateRepository(empty.storage, "invalid");
    await expect(invalid.validateEncryption()).rejects.toThrow(
      "TOKEN_ENCRYPTION_KEY",
    );
    const emptyKey = createEncryptionKey();
    await expect(
      new StateRepository(empty.storage, emptyKey).validateEncryption(),
    ).resolves.toBeUndefined();
    await expect(
      new StateRepository(
        empty.storage,
        createEncryptionKey(),
      ).validateEncryption(),
    ).rejects.toThrow();
    empty.database.close();

    const { database, storage } = createStorage();
    const repository = new StateRepository(storage, createEncryptionKey());
    await addAccount(repository, "account-a", "one", 100);
    await expect(repository.validateEncryption()).resolves.toBeUndefined();
    await expect(
      new StateRepository(storage, createEncryptionKey()).validateEncryption(),
    ).rejects.toThrow();
    database.close();
  });
});

describe("state repository account views", () => {
  it("returns one coherent account view snapshot", async () => {
    const { database, storage } = createStorage();
    const repository = new StateRepository(storage, createEncryptionKey());
    await addAccount(repository, "account-a", "one", 100);

    const pending = repository.accountViews();
    repository.setCooldown("account-a", 10_000);

    expect((await pending)[0]?.cooldownUntilMs).toBeNull();
    expect(repository.routingAccounts()[0]?.cooldownUntilMs).toBe(10_000);
    database.close();
  });
});

describe("state repository affinity", () => {
  it("rechecks and preserves an atomic hard-affinity claim", async () => {
    const { database, storage } = createStorage();
    const repository = new StateRepository(storage, createEncryptionKey());
    const ref = { kind: "turn_state", value: "turn-a" } as const;
    const input = selectInput(ref);

    await addAccount(repository, "account-a", "a", 100);
    repository.observeHeaders(
      "account-a",
      new Headers({ "x-codex-primary-used-percent": "0" }),
      150,
    );
    expect(repository.previewSelection(input, 200)).toMatchObject({
      accountId: "account-a",
      ok: true,
    });

    await addAccount(repository, "account-b", "b", 201);
    repository.observeHeaders(
      "account-b",
      new Headers({ "x-codex-primary-used-percent": "0" }),
      201,
    );
    expect(repository.bind([ref], "account-b", 202)).toBe(true);
    expect(repository.commitSelection(input, "account-a", 203)).toMatchObject({
      ok: false,
      retry: true,
    });
    expect(repository.bind([ref], "account-a", 204)).toBe(false);
    expect(
      database
        .prepare("SELECT account_id FROM bindings WHERE kind = ? AND value = ?")
        .get(ref.kind, ref.value),
    ).toEqual({ account_id: "account-b" });
    database.close();
  });

  it("applies a transport account pin before claiming hard affinity", async () => {
    const { database, storage } = createStorage();
    const repository = new StateRepository(storage, createEncryptionKey());
    await addAccount(repository, "account-a", "a", 100);
    await addAccount(repository, "account-b", "b", 100);
    for (const accountId of ["account-a", "account-b"]) {
      repository.observeHeaders(
        accountId,
        new Headers({ "x-codex-primary-used-percent": "0" }),
        150,
      );
    }

    const ref = { kind: "turn_state", value: "turn-new" } as const;
    const input = selectInput(ref, "account-b");
    expect(repository.previewSelection(input, 200)).toMatchObject({
      accountId: "account-b",
      ok: true,
    });
    expect(repository.commitSelection(input, "account-b", 201)).toMatchObject({
      ok: true,
    });
    expect(
      database
        .prepare("SELECT account_id FROM bindings WHERE kind = ? AND value = ?")
        .get(ref.kind, ref.value),
    ).toEqual({ account_id: "account-b" });

    expect(
      repository.previewSelection(selectInput(ref, "account-a"), 202),
    ).toEqual({ failure: "conflict", ok: false });
    database.close();
  });
});

describe("state repository onboarding", () => {
  it("uses invites once and retries only failed unexpired jobs", async () => {
    const { database, storage } = createStorage();
    const repository = new StateRepository(storage, createEncryptionKey());

    repository.createInvite("invite-a", "hash-a", 100, 1_000);
    expect(
      repository.startDeviceJob(
        "hash-a",
        "session-a",
        "login-a",
        authorization("device-a", 900),
        200,
      ),
    ).toMatchObject({ ok: true, state: { login: { status: "pending" } } });
    expect(repository.inviteState("hash-a")).toMatchObject({
      invite: { sessionHash: "session-a", usedAtMs: 200 },
    });
    expect(
      repository.startDeviceJob(
        "hash-a",
        "session-other",
        "login-other",
        authorization("device-other", 900),
        201,
      ),
    ).toEqual({ ok: false, state: null });

    repository.failDeviceJob("login-a", "device-a");
    const retried = repository.startDeviceJob(
      "hash-a",
      "session-a",
      "login-retry",
      authorization("device-retry", 950),
      300,
    );
    expect(retried).toMatchObject({
      ok: true,
      state: {
        login: {
          deviceAuthId: "device-retry",
          loginId: "login-retry",
          status: "pending",
        },
      },
    });
    if (!retried.ok || retried.state.login === null) {
      throw new Error("device login did not retry");
    }
    repository.failDeviceJob(retried.state.login.loginId, "device-a");
    expect(repository.inviteState("hash-a")).toMatchObject({
      login: { status: "pending" },
    });
    repository.failDeviceJob(
      retried.state.login.loginId,
      retried.state.login.deviceAuthId,
    );
    expect(
      repository.startDeviceJob(
        "hash-a",
        "session-a",
        "login-expired",
        authorization("device-expired", 1_200),
        1_000,
      ),
    ).toMatchObject({ ok: false });

    repository.createInvite("invite-expiring", "hash-expiring", 100, 1_000);
    expect(
      repository.startDeviceJob(
        "hash-expiring",
        "session-expiring",
        "login-expiring",
        authorization("device-expiring", 250),
        200,
      ),
    ).toMatchObject({ ok: true, state: { login: { status: "pending" } } });
    expect(
      repository.startDeviceJob(
        "hash-expiring",
        "session-expiring",
        "login-expiring-retry",
        authorization("device-expiring-retry", 900),
        251,
      ),
    ).toMatchObject({
      ok: true,
      state: {
        login: {
          deviceAuthId: "device-expiring-retry",
          loginId: "login-expiring-retry",
          status: "pending",
        },
      },
    });

    repository.createInvite("invite-b", "hash-b", 100, 1_000);
    const started = repository.startDeviceJob(
      "hash-b",
      "session-b",
      "login-b",
      authorization("device-b", 900),
      200,
    );
    if (!started.ok || started.state.login === null) {
      throw new Error("device login did not start");
    }
    database.exec(`
      CREATE TRIGGER account_requires_completed_login
      BEFORE INSERT ON accounts
      WHEN NOT EXISTS (
        SELECT 1 FROM device_logins WHERE status = 'complete'
      )
      BEGIN
        SELECT RAISE(ABORT, 'device login not complete');
      END
    `);
    await repository.completeDeviceJob(
      started.state.login,
      tokens("account-b", "device"),
      300,
    );
    await expect(
      repository.completeDeviceJob(
        started.state.login,
        tokens("account-b", "duplicate"),
        350,
      ),
    ).resolves.toBeNull();
    await expect(repository.account("account-b")).resolves.toMatchObject({
      accessToken: "access-device",
      credentialRevision: 1,
    });
    expect(
      database
        .prepare("SELECT count(*) AS count FROM events WHERE kind = ?")
        .get("account added"),
    ).toEqual({ count: 1 });
    expect(
      repository.startDeviceJob(
        "hash-b",
        "session-b",
        "login-after-complete",
        authorization("device-after-complete", 950),
        400,
      ),
    ).toMatchObject({
      ok: false,
      state: { login: { status: "complete" } },
    });

    repository.createInvite("invite-short", "hash-short", 100, 250);
    const expiring = repository.startDeviceJob(
      "hash-short",
      "session-short",
      "login-short",
      authorization("device-short", 900),
      200,
    );
    expect(expiring).toMatchObject({
      ok: true,
      state: {
        login: { expiresAtMs: 250, nextPollAtMs: 250 },
      },
    });
    if (!expiring.ok || expiring.state.login === null) {
      throw new Error("expiring device login did not start");
    }
    await expect(
      repository.completeDeviceJob(
        expiring.state.login,
        tokens("account-expired", "device"),
        251,
      ),
    ).resolves.toBeNull();
    await expect(repository.account("account-expired")).resolves.toBeNull();
    expect(
      database
        .prepare(
          "SELECT last_revision FROM credential_generations WHERE account_id = ?",
        )
        .get("account-expired"),
    ).toBeUndefined();
    database.close();
  });
});

describe("state repository usage", () => {
  it("derives spent state and clears stale cooldowns from fresh usage", async () => {
    const { database, storage } = createStorage();
    const repository = new StateRepository(storage, createEncryptionKey());
    await addAccount(repository, "account-a", "one", 100);
    expect(repository.nextUsageWorkAt("usage poll failed", 120_000)).toBe(
      120_000,
    );
    repository.setCooldown("account-a", 10_000);

    const spent: UsageObservation = {
      bankedResets: 2,
      limitReached: null,
      observedAtMs: 200,
      primary: {
        observedAtMs: 200,
        resetsAtMs: 5_000,
        usedPercent: 100,
        windowMinutes: 300,
      },
      secondary: null,
    };
    repository.applyUsage("account-a", spent);
    expect(repository.routingAccounts()[0]).toMatchObject({
      cooldownUntilMs: 10_000,
      spent: true,
      primary: { usedPercent: 100 },
    });
    expect(repository.accountFacts().get("account-a")).toMatchObject({
      bankedResets: 2,
      usageCheckedAtMs: 200,
    });

    repository.applyUsage("account-a", {
      ...spent,
      limitReached: false,
      observedAtMs: 300,
      primary: {
        observedAtMs: 300,
        resetsAtMs: 5_000,
        usedPercent: 25,
        windowMinutes: 300,
      },
    });
    expect(repository.routingAccounts()[0]).toMatchObject({
      cooldownUntilMs: null,
      spent: false,
      primary: { seenAtMs: 300, usedPercent: 25 },
    });
    const view = (await repository.accountViews())[0];
    expect(view).toMatchObject({
      bankedResets: 2,
      cooldownUntilMs: null,
      id: "account-a",
      paused: false,
      reauthReason: null,
      usageCheckedAtMs: 300,
    });
    expect(view).not.toHaveProperty("account");
    expect(view).not.toHaveProperty("identity");
    expect(view).not.toHaveProperty("runtime");
    expect(repository.nextUsageWorkAt("usage poll failed", 120_000)).toBe(
      120_300,
    );
    database.close();
  });

  it("derives traffic and token totals from events", async () => {
    const { database, storage } = createStorage();
    const repository = new StateRepository(storage, createEncryptionKey());
    await addAccount(repository, "account-a", "one", 100);
    await addAccount(repository, "account-b", "two", 100);

    repository.recordEvent({
      accountId: "account-a",
      atMs: 200,
      detail: "http",
      kind: "route",
    });
    repository.recordEvent({
      accountId: "account-a",
      atMs: 201,
      detail: "websocket",
      kind: "route",
    });
    repository.recordEvent({
      accountId: "account-b",
      atMs: 202,
      detail: "http",
      kind: "route",
    });
    repository.recordEvent({
      accountId: "account-a",
      atMs: 203,
      kind: "rate limited",
    });
    repository.recordEvent({
      accountId: "account-b",
      atMs: 204,
      kind: "failover",
    });
    repository.recordEvent({
      atMs: 205,
      durationMs: 10,
      kind: "response answered",
    });
    repository.recordEvent({
      atMs: 206,
      durationMs: 30,
      kind: "response answered",
    });
    repository.recordEvent({
      atMs: 207,
      kind: "response usage",
      usage: {
        inputTokens: 100,
        inputTokensDetails: {
          cachedTokens: 40,
          cacheWriteTokens: 15,
        },
        outputTokens: 25,
      },
    });
    repository.recordEvent({
      atMs: 208,
      kind: "response usage",
      usage: {
        inputTokens: 20,
        inputTokensDetails: {
          cachedTokens: 5,
          cacheWriteTokens: 3,
        },
        outputTokens: 7,
      },
    });

    expect(repository.trafficTotals()).toEqual({
      averageFirstByteMilliseconds: 20,
      cacheWriteInputTokens: 18,
      cachedInputTokens: 45,
      failovers: 1,
      inputTokens: 120,
      outputTokens: 32,
      rateLimits: 1,
      turns: 3,
      websocketTurns: 1,
    });
    expect(repository.accountTraffic()).toEqual(
      new Map([
        ["account-a", { rateLimits: 1, turns: 2 }],
        ["account-b", { rateLimits: 0, turns: 1 }],
      ]),
    );
    database.close();
  });
});

describe("state repository websocket counts", () => {
  it("persists open counts and never drops below zero", async () => {
    const { database, storage } = createStorage();
    const encryptionKey = createEncryptionKey();
    const repository = new StateRepository(storage, encryptionKey);
    await addAccount(repository, "account-a", "one", 100);

    expect(repository.websocketOpened("account-a")).toBe(true);
    expect(repository.websocketOpened("account-a")).toBe(true);

    const restarted = new StateRepository(storage, encryptionKey);
    expect((await restarted.accountViews())[0]?.openWebSockets).toBe(2);
    expect(restarted.websocketClosed("account-a")).toBe(true);
    expect((await restarted.accountViews())[0]?.openWebSockets).toBe(1);
    expect(restarted.websocketClosed("account-a")).toBe(true);
    expect(restarted.websocketClosed("account-a")).toBe(true);
    expect((await restarted.accountViews())[0]?.openWebSockets).toBe(0);
    expect(restarted.websocketOpened("missing")).toBe(false);
    database.close();
  });
});
