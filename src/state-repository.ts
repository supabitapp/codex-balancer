import { affinityStorageKey, resolveAffinity } from "./affinity.js";
import type {
  AccountId,
  AffinityBinding,
  AffinityFailure,
  AffinityRef,
  AffinityResolution,
  DashboardTotals,
  ModelCatalog,
  ModelEntry,
  QuotaWindow,
  RequestAffinity,
  ResponseUsage,
  RoutingAccount,
  SelectAccountInput,
} from "./domain.js";
import { decryptSecret, decodeJwtPayload, encryptSecret } from "./crypto.js";
import {
  allowedAccountIds,
  modelSlug,
  readQuotaWindow,
  routeAccount,
} from "./routing.js";
import type {
  DeviceAuthorization,
  TokenSet,
  UsageObservation,
  UsageWindowObservation,
} from "./account-upstream.js";
import { asRecord } from "./record.js";

export interface StateStorage {
  readonly sql: SqlStorage;
  transactionSync<T>(closure: () => T): T;
}

export interface StoredAccount {
  readonly accessToken: string;
  readonly accountId: AccountId;
  readonly credentialRevision: number;
  readonly idToken: string;
  readonly lastRefreshAtMs: number;
  readonly paused: boolean;
  readonly refreshToken: string;
}

interface AccountIdentity {
  readonly accountId: AccountId;
  readonly email: string;
  readonly plan: string;
}

interface AccountFacts {
  readonly bankedResets: number | null;
  readonly openWebSockets: number;
  readonly usageCheckedAtMs: number | null;
}

export interface AccountView extends AccountFacts, RoutingAccount {
  readonly email: string;
  readonly plan: string;
}

type SelectionPreview =
  | Readonly<{
      accountId: AccountId;
      lastRefreshAtMs: number;
      ok: true;
      resolution: AffinityResolution;
    }>
  | Readonly<{ failure: AffinityFailure | "no_account"; ok: false }>;

type SelectionCommit =
  | Readonly<{ ok: true; resolution: AffinityResolution }>
  | Readonly<{
      failure: AffinityFailure | "no_account";
      ok: false;
      retry: boolean;
    }>;

interface StoredEvent {
  readonly accountId: AccountId;
  readonly atMs: number;
  readonly detail: string;
  readonly kind: string;
}

interface AccountTraffic {
  readonly rateLimits: number;
  readonly turns: number;
}

interface StoredInvite {
  readonly expiresAtMs: number;
  readonly id: string;
  readonly sessionHash: string | null;
  readonly usedAtMs: number | null;
}

type DeviceLoginStatus = "complete" | "failed" | "pending";

export interface StoredDeviceLogin {
  readonly deviceAuthId: string;
  readonly expiresAtMs: number;
  readonly loginId: string;
  readonly nextPollAtMs: number;
  readonly pollIntervalMs: number;
  readonly status: DeviceLoginStatus;
  readonly userCode: string;
  readonly verificationUrl: string;
}

export interface InviteState {
  readonly invite: StoredInvite;
  readonly login: StoredDeviceLogin | null;
}

type StartDeviceJobResult =
  | Readonly<{ ok: true; state: InviteState }>
  | Readonly<{ ok: false; state: InviteState | null }>;

interface ModelState {
  readonly clientVersion: string;
  readonly refreshedAtMs: number;
}

interface AccountRow extends Record<string, SqlStorageValue> {
  access_token: string;
  account_id: string;
  id_token: string;
  last_refresh_at_ms: number;
  paused: number;
  revision: number;
  refresh_token: string;
}

interface RoutingRow extends Record<string, SqlStorageValue> {
  account_id: string;
  cooldown_until_ms: number;
  last_used_at_ms: number | null;
  limit_reached: number | null;
  paused: number;
  primary_minutes: number | null;
  primary_observed_at_ms: number | null;
  primary_resets_at_ms: number | null;
  primary_used_percent: number | null;
  reauth_reason: string | null;
  secondary_minutes: number | null;
  secondary_observed_at_ms: number | null;
  secondary_resets_at_ms: number | null;
  secondary_used_percent: number | null;
}

interface AccountViewRow extends Record<string, SqlStorageValue> {
  account_id: string;
  id_token: string;
  revision: number;
}

interface BindingRow extends Record<string, SqlStorageValue> {
  abandoned_at_ms: number | null;
  account_id: string;
  created_at_ms: number;
  kind: AffinityRef["kind"];
  last_used_at_ms: number;
  value: string;
}

interface InviteRow extends Record<string, SqlStorageValue> {
  created_at_ms: number;
  expires_at_ms: number;
  invite_id: string;
  session_hash: string | null;
  used_at_ms: number | null;
}

interface DeviceLoginRow extends Record<string, SqlStorageValue> {
  device_auth_id: string;
  expires_at_ms: number;
  login_id: string;
  next_poll_at_ms: number;
  poll_interval_ms: number;
  status: DeviceLoginStatus;
  user_code: string;
  verification_url: string;
}

const unknownWindow = (): QuotaWindow => ({
  minutes: 0,
  resetsAtMs: null,
  seenAtMs: null,
  usedPercent: 0,
});

const windowFromRow = (
  usedPercent: number | null,
  minutes: number | null,
  resetsAtMs: number | null,
  observedAtMs: number | null,
): QuotaWindow =>
  usedPercent === null || observedAtMs === null
    ? unknownWindow()
    : {
        minutes: minutes ?? 0,
        resetsAtMs,
        seenAtMs: observedAtMs,
        usedPercent,
      };

const spentFromFacts = (
  limitReached: number | null,
  primary: QuotaWindow,
  secondary: QuotaWindow,
): boolean =>
  limitReached === null
    ? primary.usedPercent >= 100 || secondary.usedPercent >= 100
    : limitReached !== 0;

const accountIdentity = (idToken: string): AccountIdentity => {
  const payload = asRecord(decodeJwtPayload(idToken));
  const auth = asRecord(payload?.["https://api.openai.com/auth"]);
  const accountId = auth?.chatgpt_account_id;
  if (typeof accountId !== "string" || accountId === "") {
    throw new Error("credentials carry no chatgpt_account_id");
  }
  return {
    accountId,
    email: typeof payload?.email === "string" ? payload.email : "",
    plan:
      typeof auth?.chatgpt_plan_type === "string" ? auth.chatgpt_plan_type : "",
  };
};

const accountAssociatedData = (
  accountId: AccountId,
  credentialRevision: number,
  field: "access" | "id" | "refresh",
): string => `account:${accountId}:${String(credentialRevision)}:${field}`;

const encryptionVerifierAssociatedData = "state:encryption-verifier";
const encryptionVerifierPlaintext = "state encryption verified";

const uniqueRefs = (refs: readonly AffinityRef[]): readonly AffinityRef[] => {
  const seen = new Set<string>();
  return refs.filter((ref) => {
    const key = affinityStorageKey(ref);
    if (seen.has(key)) {
      return false;
    }
    seen.add(key);
    return true;
  });
};

export class StateRepository {
  readonly #storage: StateStorage;
  readonly #sql: SqlStorage;
  readonly #encryptionKey: string;

  constructor(storage: StateStorage, encryptionKey: string) {
    this.#storage = storage;
    this.#sql = storage.sql;
    this.#encryptionKey = encryptionKey;
  }

  health(): boolean {
    return this.#sql.exec<{ ok: number }>("SELECT 1 AS ok").one().ok === 1;
  }

  async validateEncryption(): Promise<void> {
    await this.#validateEncryptionVerifier();
    const row = this.#sql
      .exec<AccountRow>(
        `SELECT a.account_id, a.id_token, a.access_token, a.refresh_token,
          a.paused, a.last_refresh_at_ms, g.last_revision AS revision
        FROM accounts a
        JOIN credential_generations g ON g.account_id = a.account_id
        ORDER BY a.account_id LIMIT 1`,
      )
      .toArray()[0];
    if (row !== undefined) {
      await this.#decodeAccount(row);
    }
  }

  async account(accountId: AccountId): Promise<StoredAccount | null> {
    const row = this.#sql
      .exec<AccountRow>(
        `SELECT a.account_id, a.id_token, a.access_token, a.refresh_token,
          a.paused, a.last_refresh_at_ms, g.last_revision AS revision
        FROM accounts a
        JOIN credential_generations g ON g.account_id = a.account_id
        WHERE a.account_id = ?`,
        accountId,
      )
      .toArray()[0];
    return row === undefined ? null : this.#decodeAccount(row);
  }

  async accounts(): Promise<readonly StoredAccount[]> {
    return Promise.all(
      this.#sql
        .exec<AccountRow>(
          `SELECT a.account_id, a.id_token, a.access_token, a.refresh_token,
            a.paused, a.last_refresh_at_ms, g.last_revision AS revision
          FROM accounts a
          JOIN credential_generations g ON g.account_id = a.account_id
          ORDER BY a.account_id`,
        )
        .toArray()
        .map((row) => this.#decodeAccount(row)),
    );
  }

  async accountViews(): Promise<readonly AccountView[]> {
    const rows = this.#sql
      .exec<AccountViewRow>(
        `SELECT a.account_id, a.id_token, g.last_revision AS revision
        FROM accounts a
        JOIN credential_generations g ON g.account_id = a.account_id
        ORDER BY a.account_id`,
      )
      .toArray();
    const routing = this.routingAccounts();
    const byId = new Map(routing.map((value) => [value.id, value]));
    const factsById = this.accountFacts();
    const identities = await Promise.all(
      rows.map(async (row) => ({
        accountId: row.account_id,
        identity: accountIdentity(
          await decryptSecret(
            row.id_token,
            this.#encryptionKey,
            accountAssociatedData(row.account_id, row.revision, "id"),
          ),
        ),
      })),
    );
    return identities.map(({ accountId, identity }) => {
      const account = byId.get(accountId);
      const facts = factsById.get(accountId);
      if (account === undefined || facts === undefined) {
        throw new Error("account state is incomplete");
      }
      return {
        ...account,
        ...facts,
        email: identity.email,
        plan: identity.plan,
      };
    });
  }

  routingAccounts(): readonly RoutingAccount[] {
    const rows = this.#sql
      .exec<RoutingRow>(
        `SELECT
          a.account_id,
          a.paused,
          r.reauth_reason,
          r.cooldown_until_ms,
          r.limit_reached,
          p.used_percent AS primary_used_percent,
          p.window_minutes AS primary_minutes,
          p.resets_at_ms AS primary_resets_at_ms,
          p.observed_at_ms AS primary_observed_at_ms,
          s.used_percent AS secondary_used_percent,
          s.window_minutes AS secondary_minutes,
          s.resets_at_ms AS secondary_resets_at_ms,
          s.observed_at_ms AS secondary_observed_at_ms,
          max(t.at_ms) AS last_used_at_ms
        FROM accounts a
        JOIN account_runtime r ON r.account_id = a.account_id
        LEFT JOIN account_windows p
          ON p.account_id = a.account_id AND p.kind = 'primary'
        LEFT JOIN account_windows s
          ON s.account_id = a.account_id AND s.kind = 'secondary'
        LEFT JOIN events t
          ON t.account_id = a.account_id AND t.kind = 'route'
        GROUP BY a.account_id
        ORDER BY a.account_id`,
      )
      .toArray();
    return rows.map((row) => {
      const primary = windowFromRow(
        row.primary_used_percent,
        row.primary_minutes,
        row.primary_resets_at_ms,
        row.primary_observed_at_ms,
      );
      const secondary = windowFromRow(
        row.secondary_used_percent,
        row.secondary_minutes,
        row.secondary_resets_at_ms,
        row.secondary_observed_at_ms,
      );
      return {
        cooldownUntilMs:
          row.cooldown_until_ms === 0 ? null : row.cooldown_until_ms,
        id: row.account_id,
        lastUsedAtMs: row.last_used_at_ms,
        paused: row.paused !== 0,
        primary,
        reauthReason: row.reauth_reason,
        secondary,
        spent: spentFromFacts(row.limit_reached, primary, secondary),
      };
    });
  }

  accountFacts(): ReadonlyMap<AccountId, AccountFacts> {
    interface Row extends Record<string, SqlStorageValue> {
      account_id: string;
      banked_resets: number | null;
      open_websockets: number;
      usage_checked_at_ms: number | null;
    }
    return new Map(
      this.#sql
        .exec<Row>(
          `SELECT account_id, usage_checked_at_ms, banked_resets,
            open_websockets
          FROM account_runtime`,
        )
        .toArray()
        .map((row) => [
          row.account_id,
          {
            bankedResets: row.banked_resets,
            openWebSockets: row.open_websockets,
            usageCheckedAtMs: row.usage_checked_at_ms,
          },
        ]),
    );
  }

  reauthReason(accountId: AccountId): string | null {
    return (
      this.#sql
        .exec<{ reauth_reason: string | null }>(
          "SELECT reauth_reason FROM account_runtime WHERE account_id = ?",
          accountId,
        )
        .toArray()[0]?.reauth_reason ?? null
    );
  }

  modelCatalog(): ModelCatalog {
    interface Row extends Record<string, SqlStorageValue> {
      account_id: string;
      models_json: string;
    }
    const catalog = new Map<AccountId, ReadonlyMap<string, ModelEntry>>();
    for (const row of this.#sql
      .exec<Row>(
        `SELECT account_id, models_json FROM account_model_catalogs
        ORDER BY account_id`,
      )
      .toArray()) {
      const parsed = JSON.parse(row.models_json) as unknown;
      if (!Array.isArray(parsed)) {
        throw new Error("stored model catalog is invalid");
      }
      const entries = new Map<string, ModelEntry>();
      for (const value of parsed) {
        const entry = asRecord(value);
        if (entry === undefined) {
          continue;
        }
        const slug = modelSlug(entry);
        if (slug !== "") {
          entries.set(slug, entry);
        }
      }
      catalog.set(row.account_id, entries);
    }
    return catalog;
  }

  modelState(): ModelState | null {
    interface Row extends Record<string, SqlStorageValue> {
      client_version: string;
      refreshed_at_ms: number;
    }
    const row = this.#sql
      .exec<Row>(
        "SELECT client_version, refreshed_at_ms FROM model_catalog_state WHERE id = 1",
      )
      .toArray()[0];
    return row === undefined
      ? null
      : {
          clientVersion: row.client_version,
          refreshedAtMs: row.refreshed_at_ms,
        };
  }

  replaceModelCatalogs(
    activeIds: readonly AccountId[],
    fresh: ReadonlyMap<AccountId, readonly ModelEntry[]>,
    clientVersion: string,
    nowMs: number,
  ): void {
    this.#storage.transactionSync(() => {
      const active = new Set(activeIds);
      for (const row of this.#sql
        .exec<{ account_id: string }>(
          "SELECT account_id FROM account_model_catalogs",
        )
        .toArray()) {
        if (!active.has(row.account_id) || !fresh.has(row.account_id)) {
          this.#sql.exec(
            "DELETE FROM account_model_catalogs WHERE account_id = ?",
            row.account_id,
          );
        }
      }
      for (const [accountId, entries] of fresh) {
        if (!active.has(accountId)) {
          continue;
        }
        this.#sql.exec(
          `INSERT INTO account_model_catalogs (account_id, models_json)
          VALUES (?, ?) ON CONFLICT (account_id) DO UPDATE SET
          models_json = excluded.models_json`,
          accountId,
          JSON.stringify(entries),
        );
      }
      this.#sql.exec(
        `INSERT INTO model_catalog_state (id, client_version, refreshed_at_ms)
        VALUES (1, ?, ?) ON CONFLICT (id) DO UPDATE SET
        client_version = excluded.client_version,
        refreshed_at_ms = excluded.refreshed_at_ms`,
        clientVersion,
        nowMs,
      );
    });
  }

  invalidateModels(): void {
    this.#sql.exec(
      "UPDATE model_catalog_state SET refreshed_at_ms = 0 WHERE id = 1",
    );
  }

  previewSelection(input: SelectAccountInput, nowMs: number): SelectionPreview {
    return this.#storage.transactionSync(() =>
      this.#resolveSelection(input, nowMs, null),
    );
  }

  commitSelection(
    input: SelectAccountInput,
    expectedAccountId: AccountId,
    nowMs: number,
  ): SelectionCommit {
    return this.#storage.transactionSync(() => {
      const selected = this.#resolveSelection(input, nowMs, expectedAccountId);
      if (!selected.ok) {
        return { ...selected, retry: false };
      }
      if (selected.accountId !== expectedAccountId) {
        return { failure: "no_account", ok: false, retry: true };
      }
      if (
        selected.resolution.hard &&
        !this.#storeBindings(
          selected.resolution.bindings,
          selected.accountId,
          nowMs,
          false,
        )
      ) {
        return { failure: "conflict", ok: false, retry: true };
      }
      return { ok: true, resolution: selected.resolution };
    });
  }

  bind(
    refs: readonly AffinityRef[],
    accountId: AccountId,
    nowMs: number,
  ): boolean {
    return this.#storage.transactionSync(() =>
      this.#storeBindings(uniqueRefs(refs), accountId, nowMs, true),
    );
  }

  async updateRefreshedTokens(
    snapshot: StoredAccount,
    tokens: TokenSet,
    nowMs: number,
  ): Promise<boolean> {
    const identity = accountIdentity(tokens.idToken);
    if (identity.accountId !== snapshot.accountId) {
      throw new Error("refreshed credentials changed account identity");
    }
    const nextRevision = snapshot.credentialRevision + 1;
    const encrypted = await this.#encryptTokens(
      snapshot.accountId,
      nextRevision,
      tokens,
    );
    return this.#storage.transactionSync(() => {
      if (
        this.#activeCredentialRevision(snapshot.accountId) !==
          snapshot.credentialRevision ||
        !this.#advanceCredentialGeneration(
          snapshot.accountId,
          snapshot.credentialRevision,
          nextRevision,
        )
      ) {
        return false;
      }
      const changed = this.#sql.exec(
        `UPDATE accounts SET
          id_token = ?, access_token = ?, refresh_token = ?,
          last_refresh_at_ms = ?
        WHERE account_id = ?`,
        encrypted.idToken,
        encrypted.accessToken,
        encrypted.refreshToken,
        nowMs,
        snapshot.accountId,
      ).rowsWritten;
      if (changed !== 1) {
        throw new Error("credential generation changed without its account");
      }
      this.#sql.exec(
        "UPDATE account_runtime SET reauth_reason = NULL WHERE account_id = ?",
        snapshot.accountId,
      );
      return true;
    });
  }

  setReauthReason(
    accountId: AccountId,
    credentialRevision: number,
    reason: string,
  ): boolean {
    return this.#storage.transactionSync(() => {
      if (this.#activeCredentialRevision(accountId) !== credentialRevision) {
        return false;
      }
      return (
        this.#sql.exec(
          "UPDATE account_runtime SET reauth_reason = ? WHERE account_id = ?",
          reason,
          accountId,
        ).rowsWritten === 1
      );
    });
  }

  updateAccount(accountId: AccountId, paused: boolean): boolean {
    const changed = this.#sql.exec(
      "UPDATE accounts SET paused = ? WHERE account_id = ?",
      paused ? 1 : 0,
      accountId,
    ).rowsWritten;
    if (changed === 1) {
      this.invalidateModels();
    }
    return changed === 1;
  }

  deleteAccount(accountId: AccountId): boolean {
    const changed = this.#sql.exec(
      "DELETE FROM accounts WHERE account_id = ?",
      accountId,
    ).rowsWritten;
    if (changed === 1) {
      this.invalidateModels();
    }
    return changed === 1;
  }

  observeHeaders(accountId: AccountId, headers: Headers, nowMs: number): void {
    const windows = [
      ["primary", readQuotaWindow(headers, "x-codex-primary", nowMs)],
      [
        "secondary",
        readQuotaWindow(headers, "x-codex-secondary-primary", nowMs),
      ],
    ] as const;
    this.#storage.transactionSync(() => {
      for (const [kind, window] of windows) {
        if (window !== null) {
          this.#upsertWindow(accountId, kind, {
            observedAtMs: window.seenAtMs ?? nowMs,
            resetsAtMs: window.resetsAtMs,
            usedPercent: window.usedPercent,
            windowMinutes: window.minutes,
          });
        }
      }
    });
  }

  setCooldown(accountId: AccountId, cooldownUntilMs: number): void {
    this.#sql.exec(
      "UPDATE account_runtime SET cooldown_until_ms = ? WHERE account_id = ?",
      cooldownUntilMs,
      accountId,
    );
  }

  websocketOpened(accountId: AccountId): boolean {
    return (
      this.#sql.exec(
        `UPDATE account_runtime
        SET open_websockets = open_websockets + 1
        WHERE account_id = ?`,
        accountId,
      ).rowsWritten === 1
    );
  }

  websocketClosed(accountId: AccountId): boolean {
    return (
      this.#sql.exec(
        `UPDATE account_runtime
        SET open_websockets = max(open_websockets - 1, 0)
        WHERE account_id = ?`,
        accountId,
      ).rowsWritten === 1
    );
  }

  applyUsage(accountId: AccountId, observation: UsageObservation): void {
    this.#storage.transactionSync(() => {
      this.#sql.exec(
        `UPDATE account_runtime SET usage_checked_at_ms = ?,
          limit_reached = ?, banked_resets = ?
        WHERE account_id = ?`,
        observation.observedAtMs,
        observation.limitReached === null
          ? null
          : observation.limitReached
            ? 1
            : 0,
        observation.bankedResets,
        accountId,
      );
      if (observation.primary !== null) {
        this.#upsertWindow(accountId, "primary", observation.primary);
      }
      if (observation.secondary !== null) {
        this.#upsertWindow(accountId, "secondary", observation.secondary);
      }
      const routing = this.routingAccounts().find(
        (account) => account.id === accountId,
      );
      if (routing?.reauthReason === null && !routing.spent) {
        this.#sql.exec(
          "UPDATE account_runtime SET cooldown_until_ms = 0 WHERE account_id = ?",
          accountId,
        );
      }
    });
  }

  setBankedResets(accountId: AccountId, count: number): void {
    this.#sql.exec(
      "UPDATE account_runtime SET banked_resets = ? WHERE account_id = ?",
      count,
      accountId,
    );
  }

  recordEvent(input: {
    readonly accountId?: AccountId;
    readonly atMs: number;
    readonly detail?: string;
    readonly durationMs?: number;
    readonly kind: string;
    readonly usage?: ResponseUsage;
  }): void {
    const usage = input.usage;
    this.#sql.exec(
      `INSERT INTO events (
        at_ms, kind, account_id, detail, duration_ms, input_tokens,
        cached_tokens, cache_write_tokens, output_tokens
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      input.atMs,
      input.kind,
      input.accountId ?? "",
      input.detail ?? "",
      input.durationMs ?? 0,
      usage?.inputTokens ?? 0,
      usage?.inputTokensDetails.cachedTokens ?? 0,
      usage?.inputTokensDetails.cacheWriteTokens ?? 0,
      usage?.outputTokens ?? 0,
    );
  }

  trafficTotals(): DashboardTotals {
    interface Row extends Record<string, SqlStorageValue> {
      average_first_byte_ms: number | null;
      cache_write_input_tokens: number;
      cached_input_tokens: number;
      failovers: number;
      input_tokens: number;
      output_tokens: number;
      rate_limits: number;
      turns: number;
      websocket_turns: number;
    }
    const row = this.#sql
      .exec<Row>(
        `SELECT
          (SELECT count(*) FROM events WHERE kind = 'route') AS turns,
          (SELECT count(*) FROM events WHERE kind = 'route' AND detail = 'websocket') AS websocket_turns,
          (SELECT count(*) FROM events WHERE kind = 'failover') AS failovers,
          (SELECT count(*) FROM events WHERE kind = 'rate limited') AS rate_limits,
          (SELECT avg(duration_ms) FROM events WHERE kind = 'response answered') AS average_first_byte_ms,
          coalesce((SELECT sum(input_tokens) FROM events WHERE kind = 'response usage'), 0) AS input_tokens,
          coalesce((SELECT sum(cached_tokens) FROM events WHERE kind = 'response usage'), 0) AS cached_input_tokens,
          coalesce((SELECT sum(cache_write_tokens) FROM events WHERE kind = 'response usage'), 0) AS cache_write_input_tokens,
          coalesce((SELECT sum(output_tokens) FROM events WHERE kind = 'response usage'), 0) AS output_tokens`,
      )
      .one();
    return {
      averageFirstByteMilliseconds: row.average_first_byte_ms,
      cacheWriteInputTokens: row.cache_write_input_tokens,
      cachedInputTokens: row.cached_input_tokens,
      failovers: row.failovers,
      inputTokens: row.input_tokens,
      outputTokens: row.output_tokens,
      rateLimits: row.rate_limits,
      turns: row.turns,
      websocketTurns: row.websocket_turns,
    };
  }

  accountTraffic(): ReadonlyMap<AccountId, AccountTraffic> {
    interface Row extends Record<string, SqlStorageValue> {
      account_id: string;
      rate_limits: number;
      turns: number;
    }
    const rows = this.#sql
      .exec<Row>(
        `SELECT ids.account_id,
          coalesce(e.turns, 0) AS turns,
          coalesce(e.rate_limits, 0) AS rate_limits
        FROM (SELECT account_id FROM accounts) ids
        LEFT JOIN (
          SELECT account_id,
            sum(CASE WHEN kind = 'route' THEN 1 ELSE 0 END) AS turns,
            sum(CASE WHEN kind = 'rate limited' THEN 1 ELSE 0 END) AS rate_limits
          FROM events WHERE kind IN ('route', 'rate limited')
          GROUP BY account_id
        ) e ON e.account_id = ids.account_id`,
      )
      .toArray();
    return new Map(
      rows.map((row) => [
        row.account_id,
        {
          rateLimits: row.rate_limits,
          turns: row.turns,
        },
      ]),
    );
  }

  recentEvents(
    kinds: readonly string[],
    limit: number,
  ): readonly StoredEvent[] {
    interface Row extends Record<string, SqlStorageValue> {
      account_id: string;
      at_ms: number;
      detail: string;
      kind: string;
    }
    if (kinds.length === 0) {
      return [];
    }
    const placeholders = kinds.map(() => "?").join(", ");
    return this.#sql
      .exec<Row>(
        `SELECT at_ms, kind, account_id, detail
        FROM events WHERE kind IN (${placeholders})
        ORDER BY id DESC LIMIT ?`,
        ...kinds,
        limit,
      )
      .toArray()
      .map((row) => ({
        accountId: row.account_id,
        atMs: row.at_ms,
        detail: row.detail,
        kind: row.kind,
      }));
  }

  latestEventAtByAccount(kind: string): ReadonlyMap<AccountId, number> {
    interface Row extends Record<string, SqlStorageValue> {
      account_id: string;
      at_ms: number;
    }
    return new Map(
      this.#sql
        .exec<Row>(
          `SELECT account_id, max(at_ms) AS at_ms FROM events
          WHERE kind = ? AND account_id <> '' GROUP BY account_id`,
          kind,
        )
        .toArray()
        .map((row) => [row.account_id, row.at_ms]),
    );
  }

  nextUsageWorkAt(failureKind: string, intervalMs: number): number | null {
    return this.#sql
      .exec<{ next_at: number | null }>(
        `SELECT min(
          max(
            coalesce(r.usage_checked_at_ms, 0),
            coalesce(f.at_ms, 0)
          ) + ?
        ) AS next_at
        FROM account_runtime r
        LEFT JOIN (
          SELECT account_id, max(at_ms) AS at_ms
          FROM events WHERE kind = ? GROUP BY account_id
        ) f ON f.account_id = r.account_id`,
        intervalMs,
        failureKind,
      )
      .one().next_at;
  }

  createInvite(
    inviteId: string,
    tokenHash: string,
    createdAtMs: number,
    expiresAtMs: number,
  ): void {
    this.#sql.exec(
      `INSERT INTO account_invites (
        invite_id, token_hash, created_at_ms, expires_at_ms, used_at_ms,
        session_hash
      ) VALUES (?, ?, ?, ?, NULL, NULL)`,
      inviteId,
      tokenHash,
      createdAtMs,
      expiresAtMs,
    );
  }

  invites(limit = 100): readonly StoredInvite[] {
    return this.#sql
      .exec<InviteRow>(
        `SELECT invite_id, created_at_ms, expires_at_ms, used_at_ms,
          session_hash
        FROM account_invites ORDER BY created_at_ms DESC LIMIT ?`,
        limit,
      )
      .toArray()
      .map(this.#inviteFromRow);
  }

  inviteState(tokenHash: string): InviteState | null {
    const invite = this.#sql
      .exec<InviteRow>(
        `SELECT invite_id, created_at_ms, expires_at_ms, used_at_ms,
          session_hash
        FROM account_invites WHERE token_hash = ?`,
        tokenHash,
      )
      .toArray()[0];
    if (invite === undefined) {
      return null;
    }
    const login = this.#sql
      .exec<DeviceLoginRow>(
        `SELECT login_id, device_auth_id, user_code, poll_interval_ms,
          expires_at_ms, next_poll_at_ms, status, verification_url
        FROM device_logins WHERE invite_id = ?`,
        invite.invite_id,
      )
      .toArray()[0];
    return {
      invite: this.#inviteFromRow(invite),
      login: login === undefined ? null : this.#deviceLoginFromRow(login),
    };
  }

  startDeviceJob(
    tokenHash: string,
    sessionHash: string,
    loginId: string,
    authorization: DeviceAuthorization,
    nowMs: number,
  ): StartDeviceJobResult {
    return this.#storage.transactionSync(() => {
      let current = this.inviteState(tokenHash);
      if (current === null || current.invite.expiresAtMs <= nowMs) {
        return { ok: false, state: current };
      }
      if (
        current.invite.usedAtMs !== null &&
        current.invite.sessionHash !== sessionHash
      ) {
        return { ok: false, state: null };
      }
      if (
        current.login?.status === "pending" &&
        current.login.expiresAtMs <= nowMs
      ) {
        this.failDeviceJob(current.login.loginId, current.login.deviceAuthId);
        current = this.inviteState(tokenHash);
        if (current === null) {
          throw new Error("expired device login state disappeared");
        }
      }
      const first = current.invite.usedAtMs === null && current.login === null;
      const retry =
        current.invite.sessionHash === sessionHash &&
        current.login?.status === "failed";
      if (!first && !retry) {
        return { ok: false, state: current };
      }
      const expiresAtMs = Math.min(
        authorization.expiresAtMs,
        current.invite.expiresAtMs,
      );
      const nextPollAtMs = Math.min(
        nowMs + authorization.pollIntervalMs,
        expiresAtMs,
      );
      if (first) {
        const changed = this.#sql.exec(
          `UPDATE account_invites SET used_at_ms = ?, session_hash = ?
          WHERE invite_id = ? AND used_at_ms IS NULL AND session_hash IS NULL
            AND expires_at_ms > ?`,
          nowMs,
          sessionHash,
          current.invite.id,
          nowMs,
        ).rowsWritten;
        if (changed !== 1) {
          return { ok: false, state: this.inviteState(tokenHash) };
        }
        this.#sql.exec(
          `INSERT INTO device_logins (
            login_id, invite_id, device_auth_id, user_code, poll_interval_ms,
            expires_at_ms, next_poll_at_ms, status, verification_url
          ) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
          loginId,
          current.invite.id,
          authorization.deviceAuthId,
          authorization.userCode,
          authorization.pollIntervalMs,
          expiresAtMs,
          nextPollAtMs,
          authorization.verificationUrl,
        );
      } else {
        if (current.login === null) {
          throw new Error("failed device login disappeared");
        }
        const changed = this.#sql.exec(
          `UPDATE device_logins SET
            login_id = ?, device_auth_id = ?, user_code = ?, poll_interval_ms = ?,
            expires_at_ms = ?, next_poll_at_ms = ?, status = 'pending',
            verification_url = ?
          WHERE invite_id = ? AND status = 'failed' AND device_auth_id = ?`,
          loginId,
          authorization.deviceAuthId,
          authorization.userCode,
          authorization.pollIntervalMs,
          expiresAtMs,
          nextPollAtMs,
          authorization.verificationUrl,
          current.invite.id,
          current.login.deviceAuthId,
        ).rowsWritten;
        if (changed !== 1) {
          return { ok: false, state: this.inviteState(tokenHash) };
        }
      }
      const state = this.inviteState(tokenHash);
      if (state === null) {
        throw new Error("device login state disappeared");
      }
      return { ok: true, state };
    });
  }

  dueDeviceJobs(nowMs: number): readonly StoredDeviceLogin[] {
    return this.#sql
      .exec<DeviceLoginRow>(
        `SELECT login_id, device_auth_id, user_code, poll_interval_ms,
          expires_at_ms, next_poll_at_ms, status, verification_url
        FROM device_logins
        WHERE status = 'pending' AND next_poll_at_ms <= ?
        ORDER BY next_poll_at_ms`,
        nowMs,
      )
      .toArray()
      .map(this.#deviceLoginFromRow);
  }

  nextDevicePollAt(): number | null {
    const row = this.#sql
      .exec<{ next_at: number | null }>(
        `SELECT min(next_poll_at_ms) AS next_at
        FROM device_logins WHERE status = 'pending'`,
      )
      .one();
    return row.next_at;
  }

  deferDeviceJob(
    loginId: string,
    deviceAuthId: string,
    nextPollAtMs: number,
  ): void {
    this.#sql.exec(
      `UPDATE device_logins SET next_poll_at_ms = ?
      WHERE login_id = ? AND device_auth_id = ? AND status = 'pending'`,
      nextPollAtMs,
      loginId,
      deviceAuthId,
    );
  }

  failDeviceJob(loginId: string, deviceAuthId: string): void {
    this.#sql.exec(
      `UPDATE device_logins SET status = 'failed'
      WHERE login_id = ? AND device_auth_id = ? AND status = 'pending'`,
      loginId,
      deviceAuthId,
    );
  }

  async completeDeviceJob(
    login: StoredDeviceLogin,
    tokens: TokenSet,
    nowMs: number,
  ): Promise<AccountId | null> {
    const identity = accountIdentity(tokens.idToken);
    for (let attempt = 0; attempt < 4; attempt += 1) {
      const expectedRevision = this.#activeCredentialRevision(
        identity.accountId,
      );
      const expectedGeneration =
        this.#credentialGeneration(identity.accountId) ?? 0;
      const nextRevision = expectedGeneration + 1;
      const encrypted = await this.#encryptTokens(
        identity.accountId,
        nextRevision,
        tokens,
      );
      const result = this.#storage.transactionSync(() => {
        if (
          this.#activeCredentialRevision(identity.accountId) !==
            expectedRevision ||
          (this.#credentialGeneration(identity.accountId) ?? 0) !==
            expectedGeneration
        ) {
          return "retry" as const;
        }
        const completed = this.#sql.exec(
          `UPDATE device_logins SET status = 'complete'
          WHERE login_id = ? AND status = 'pending' AND device_auth_id = ?
            AND expires_at_ms > ? AND EXISTS (
              SELECT 1 FROM account_invites i
              WHERE i.invite_id = device_logins.invite_id
                AND i.expires_at_ms > ?
            )`,
          login.loginId,
          login.deviceAuthId,
          nowMs,
          nowMs,
        ).rowsWritten;
        if (completed !== 1) {
          return "finished" as const;
        }
        if (
          !this.#advanceCredentialGeneration(
            identity.accountId,
            expectedGeneration,
            nextRevision,
          )
        ) {
          throw new Error("credential generation changed during completion");
        }
        const rowsWritten =
          expectedRevision === null
            ? this.#sql.exec(
                `INSERT OR IGNORE INTO accounts (
                  account_id, id_token, access_token, refresh_token, paused,
                  last_refresh_at_ms
                ) VALUES (?, ?, ?, ?, 0, ?)`,
                identity.accountId,
                encrypted.idToken,
                encrypted.accessToken,
                encrypted.refreshToken,
                nowMs,
              ).rowsWritten
            : this.#sql.exec(
                `UPDATE accounts SET
                  id_token = ?, access_token = ?, refresh_token = ?, paused = 0,
                  last_refresh_at_ms = ?
                WHERE account_id = ?`,
                encrypted.idToken,
                encrypted.accessToken,
                encrypted.refreshToken,
                nowMs,
                identity.accountId,
              ).rowsWritten;
        if (rowsWritten !== 1) {
          throw new Error("credential generation changed without its account");
        }
        this.#resetAccountRuntime(identity.accountId);
        this.recordEvent({
          accountId: identity.accountId,
          atMs: nowMs,
          kind: "account added",
        });
        return "complete" as const;
      });
      if (result === "complete") {
        return identity.accountId;
      }
      if (result === "finished") {
        return null;
      }
    }
    throw new Error("account credentials changed during device login");
  }

  pruneInvites(beforeMs: number): void {
    this.#sql.exec(
      "DELETE FROM account_invites WHERE expires_at_ms < ?",
      beforeMs,
    );
  }

  pruneHistory(beforeMs: number): void {
    this.#sql.exec("DELETE FROM events WHERE at_ms < ?", beforeMs);
  }

  #activeCredentialRevision(accountId: AccountId): number | null {
    return (
      this.#sql
        .exec<{ last_revision: number }>(
          `SELECT g.last_revision FROM accounts a
          JOIN credential_generations g ON g.account_id = a.account_id
          WHERE a.account_id = ?`,
          accountId,
        )
        .toArray()[0]?.last_revision ?? null
    );
  }

  #credentialGeneration(accountId: AccountId): number | null {
    return (
      this.#sql
        .exec<{ last_revision: number }>(
          `SELECT last_revision FROM credential_generations
          WHERE account_id = ?`,
          accountId,
        )
        .toArray()[0]?.last_revision ?? null
    );
  }

  #advanceCredentialGeneration(
    accountId: AccountId,
    expectedRevision: number,
    nextRevision: number,
  ): boolean {
    if (expectedRevision === 0) {
      return (
        this.#sql.exec(
          `INSERT OR IGNORE INTO credential_generations (
            account_id, last_revision
          ) VALUES (?, ?)`,
          accountId,
          nextRevision,
        ).rowsWritten === 1
      );
    }
    return (
      this.#sql.exec(
        `UPDATE credential_generations SET last_revision = ?
        WHERE account_id = ? AND last_revision = ?`,
        nextRevision,
        accountId,
        expectedRevision,
      ).rowsWritten === 1
    );
  }

  async #validateEncryptionVerifier(): Promise<void> {
    let verifier = this.#sql
      .exec<{ verifier: string }>(
        "SELECT verifier FROM encryption_state WHERE id = 1",
      )
      .toArray()[0]?.verifier;
    if (verifier === undefined) {
      const candidate = await encryptSecret(
        encryptionVerifierPlaintext,
        this.#encryptionKey,
        encryptionVerifierAssociatedData,
      );
      this.#sql.exec(
        "INSERT OR IGNORE INTO encryption_state (id, verifier) VALUES (1, ?)",
        candidate,
      );
      verifier = this.#sql
        .exec<{ verifier: string }>(
          "SELECT verifier FROM encryption_state WHERE id = 1",
        )
        .one().verifier;
    }
    if (
      (await decryptSecret(
        verifier,
        this.#encryptionKey,
        encryptionVerifierAssociatedData,
      )) !== encryptionVerifierPlaintext
    ) {
      throw new Error("state encryption check failed");
    }
  }

  async #decodeAccount(row: AccountRow): Promise<StoredAccount> {
    const [idToken, accessToken, refreshToken] = await Promise.all([
      decryptSecret(
        row.id_token,
        this.#encryptionKey,
        accountAssociatedData(row.account_id, row.revision, "id"),
      ),
      decryptSecret(
        row.access_token,
        this.#encryptionKey,
        accountAssociatedData(row.account_id, row.revision, "access"),
      ),
      decryptSecret(
        row.refresh_token,
        this.#encryptionKey,
        accountAssociatedData(row.account_id, row.revision, "refresh"),
      ),
    ]);
    return {
      accessToken,
      accountId: row.account_id,
      credentialRevision: row.revision,
      idToken,
      lastRefreshAtMs: row.last_refresh_at_ms,
      paused: row.paused !== 0,
      refreshToken,
    };
  }

  async #encryptTokens(
    accountId: AccountId,
    credentialRevision: number,
    tokens: TokenSet,
  ): Promise<TokenSet> {
    await this.#validateEncryptionVerifier();
    const [idToken, accessToken, refreshToken] = await Promise.all([
      encryptSecret(
        tokens.idToken,
        this.#encryptionKey,
        accountAssociatedData(accountId, credentialRevision, "id"),
      ),
      encryptSecret(
        tokens.accessToken,
        this.#encryptionKey,
        accountAssociatedData(accountId, credentialRevision, "access"),
      ),
      encryptSecret(
        tokens.refreshToken,
        this.#encryptionKey,
        accountAssociatedData(accountId, credentialRevision, "refresh"),
      ),
    ]);
    return { accessToken, idToken, refreshToken };
  }

  #resolveSelection(
    input: SelectAccountInput,
    nowMs: number,
    expectedAccountId: AccountId | null,
  ): SelectionPreview {
    for (let retry = 0; retry < 3; retry += 1) {
      const accounts = this.routingAccounts();
      const bindings = this.#bindingsForRequest(input.affinity);
      let resolution = resolveAffinity(
        input.affinity,
        bindings,
        accounts,
        nowMs,
      );
      if (!this.#applyAbandonments(resolution.abandonments, nowMs)) {
        continue;
      }
      if (resolution.abandonments.length > 0) {
        continue;
      }
      if (!resolution.ok && input.requiredAccountId !== null) {
        resolution = resolveAffinity(
          input.affinity,
          bindings,
          accounts.filter((account) => account.id === input.requiredAccountId),
          nowMs,
        );
        if (!this.#applyAbandonments(resolution.abandonments, nowMs)) {
          continue;
        }
        if (resolution.abandonments.length > 0) {
          continue;
        }
      }
      if (!resolution.ok) {
        return { failure: resolution.failure, ok: false };
      }
      if (
        input.requiredAccountId !== null &&
        resolution.resolution.requiredAccountId !== null &&
        input.requiredAccountId !== resolution.resolution.requiredAccountId
      ) {
        return { failure: "conflict", ok: false };
      }
      const decision = routeAccount(
        accounts,
        {
          allowedIds: allowedAccountIds(
            this.modelCatalog(),
            accounts,
            input.model,
            input.serviceTier,
            nowMs,
          ),
          preferredId: resolution.resolution.preferredAccountId,
          requiredId:
            input.requiredAccountId ?? resolution.resolution.requiredAccountId,
          skippedIds: new Set(input.skipAccountIds),
        },
        nowMs,
      );
      if (decision.account === null) {
        return { failure: "no_account", ok: false };
      }
      if (
        expectedAccountId !== null &&
        decision.account.id !== expectedAccountId
      ) {
        return {
          accountId: decision.account.id,
          lastRefreshAtMs: this.#lastRefreshAt(decision.account.id),
          ok: true,
          resolution: resolution.resolution,
        };
      }
      return {
        accountId: decision.account.id,
        lastRefreshAtMs: this.#lastRefreshAt(decision.account.id),
        ok: true,
        resolution: resolution.resolution,
      };
    }
    return { failure: "conflict", ok: false };
  }

  #lastRefreshAt(accountId: AccountId): number {
    return this.#sql
      .exec<{ last_refresh_at_ms: number }>(
        "SELECT last_refresh_at_ms FROM accounts WHERE account_id = ?",
        accountId,
      )
      .one().last_refresh_at_ms;
  }

  #bindingsForRequest(
    request: RequestAffinity,
  ): ReadonlyMap<string, AffinityBinding> {
    const refs = uniqueRefs([
      ...(request.preferred === null ? [] : [request.preferred]),
      ...request.hard,
    ]);
    const output = new Map<string, AffinityBinding>();
    for (const ref of refs) {
      const row = this.#sql
        .exec<BindingRow>(
          `SELECT kind, value, account_id, created_at_ms,
            last_used_at_ms, abandoned_at_ms
          FROM bindings WHERE kind = ? AND value = ?`,
          ref.kind,
          ref.value,
        )
        .toArray()[0];
      if (row !== undefined) {
        output.set(affinityStorageKey(ref), {
          abandonedAtMs: row.abandoned_at_ms,
          accountId: row.account_id,
          createdAtMs: row.created_at_ms,
          lastUsedAtMs: row.last_used_at_ms,
        });
      }
    }
    return output;
  }

  #applyAbandonments(
    abandonments: ReturnType<typeof resolveAffinity>["abandonments"],
    nowMs: number,
  ): boolean {
    for (const abandonment of abandonments) {
      const changed = this.#sql.exec(
        `UPDATE bindings SET abandoned_at_ms = ?
        WHERE kind = ? AND value = ? AND account_id = ?
          AND last_used_at_ms = ? AND abandoned_at_ms IS NULL`,
        nowMs,
        abandonment.ref.kind,
        abandonment.ref.value,
        abandonment.accountId,
        abandonment.lastUsedAtMs,
      ).rowsWritten;
      if (changed !== 1) {
        return false;
      }
    }
    return true;
  }

  #storeBindings(
    refs: readonly AffinityRef[],
    accountId: AccountId,
    nowMs: number,
    touch: boolean,
  ): boolean {
    for (const ref of refs) {
      const row = this.#sql
        .exec<{ abandoned_at_ms: number | null; account_id: string }>(
          `SELECT account_id, abandoned_at_ms FROM bindings
          WHERE kind = ? AND value = ?`,
          ref.kind,
          ref.value,
        )
        .toArray()[0];
      if (row?.abandoned_at_ms === null && row.account_id !== accountId) {
        return false;
      }
    }
    for (const ref of refs) {
      this.#sql.exec(
        `INSERT INTO bindings (
          kind, value, account_id, created_at_ms, last_used_at_ms,
          abandoned_at_ms
        ) VALUES (?, ?, ?, ?, ?, NULL)
        ON CONFLICT (kind, value) DO UPDATE SET
          account_id = excluded.account_id,
          created_at_ms = CASE
            WHEN bindings.abandoned_at_ms IS NOT NULL
            THEN excluded.created_at_ms
            ELSE bindings.created_at_ms
          END,
          last_used_at_ms = CASE
            WHEN ? = 1 OR bindings.abandoned_at_ms IS NOT NULL
            THEN excluded.last_used_at_ms
            ELSE bindings.last_used_at_ms
          END,
          abandoned_at_ms = NULL`,
        ref.kind,
        ref.value,
        accountId,
        nowMs,
        nowMs,
        touch ? 1 : 0,
      );
    }
    return true;
  }

  #resetAccountRuntime(accountId: AccountId): void {
    this.#sql.exec(
      `INSERT INTO account_runtime (
        account_id, reauth_reason, cooldown_until_ms, usage_checked_at_ms,
        limit_reached, banked_resets
      ) VALUES (?, NULL, 0, NULL, NULL, NULL)
      ON CONFLICT (account_id) DO UPDATE SET
        reauth_reason = NULL,
        cooldown_until_ms = 0,
        usage_checked_at_ms = NULL,
        limit_reached = NULL,
        banked_resets = NULL`,
      accountId,
    );
    this.#sql.exec(
      "DELETE FROM account_windows WHERE account_id = ?",
      accountId,
    );
    this.#sql.exec(
      "DELETE FROM account_model_catalogs WHERE account_id = ?",
      accountId,
    );
    this.invalidateModels();
  }

  #upsertWindow(
    accountId: AccountId,
    kind: "primary" | "secondary",
    window: UsageWindowObservation,
  ): void {
    this.#sql.exec(
      `INSERT INTO account_windows (
        account_id, kind, used_percent, window_minutes, resets_at_ms,
        observed_at_ms
      ) VALUES (?, ?, ?, ?, ?, ?)
      ON CONFLICT (account_id, kind) DO UPDATE SET
        used_percent = excluded.used_percent,
        window_minutes = excluded.window_minutes,
        resets_at_ms = excluded.resets_at_ms,
        observed_at_ms = excluded.observed_at_ms`,
      accountId,
      kind,
      window.usedPercent,
      window.windowMinutes,
      window.resetsAtMs,
      window.observedAtMs,
    );
  }

  #inviteFromRow = (row: InviteRow): StoredInvite => ({
    expiresAtMs: row.expires_at_ms,
    id: row.invite_id,
    sessionHash: row.session_hash,
    usedAtMs: row.used_at_ms,
  });

  #deviceLoginFromRow = (row: DeviceLoginRow): StoredDeviceLogin => ({
    deviceAuthId: row.device_auth_id,
    expiresAtMs: row.expires_at_ms,
    loginId: row.login_id,
    nextPollAtMs: row.next_poll_at_ms,
    pollIntervalMs: row.poll_interval_ms,
    status: row.status,
    userCode: row.user_code,
    verificationUrl: row.verification_url,
  });
}
