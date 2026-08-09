import { DurableObject } from "cloudflare:workers";

import {
  AccountUpstreamError,
  consumeResetCredit,
  exchangeDeviceCode,
  fetchModels,
  fetchResetCredits,
  fetchUsage,
  pollDeviceAuthorization,
  RefreshTokenError,
  refreshTokens,
  requestDeviceAuthorization,
} from "./account-upstream.js";
import { hashToken, randomToken } from "./crypto.js";
import type {
  AccountId,
  AdminSnapshot,
  CreateInviteInput,
  CreateInviteResult,
  DashboardEvent,
  DashboardSnapshot,
  InviteInspection,
  ModelEntry,
  OnboardingSnapshot,
  QuotaWindow,
  ResponseUsage,
  SelectAccountInput,
  UpdateAccountInput,
} from "./domain.js";
import type { Env } from "./env.js";
import {
  type AccountView,
  type InviteState,
  StateRepository,
  type StoredAccount,
  type StoredDeviceLogin,
} from "./state-repository.js";
import { initializeStateSchema } from "./state-schema.js";
import {
  accountStatusAt,
  backoffMs,
  canonicalServiceTier,
  rateLimitCooldownAt,
} from "./routing.js";
import { asRecord } from "./record.js";
import type {
  AccountFailure,
  AccountObservation,
  RecordedRoute,
  RefreshAccountResult,
  SelectAccountResult,
} from "./transport-port.js";
import { webSocketHandshakeFailure } from "./websocket-handshake.js";

const refreshAfterMs = 8 * 24 * 60 * 60 * 1000;
const usageIntervalMs = 2 * 60 * 1000;
const modelRefreshIntervalMs = 5 * 60 * 1000;
const resetCreditLeadMs = 5 * 60 * 1000;
const cleanupIntervalMs = 6 * 60 * 60 * 1000;
const historyRetentionMs = 90 * 24 * 60 * 60 * 1000;
const inviteRetentionMs = 7 * 24 * 60 * 60 * 1000;
const defaultInviteSeconds = 60 * 60;
const maximumInviteSeconds = 7 * 24 * 60 * 60;
const refreshTimeoutMs = 30_000;
const accountRequestTimeoutMs = 15_000;
const maximumDashboardSockets = 100;
const dashboardSocketTag = "dashboard";
const usageFailureKind = "usage poll failed";

const upstreamFetch = (
  input: string | URL,
  init?: RequestInit,
): Promise<Response> => fetch(input, init);

const headersFromRecord = (values: Readonly<Record<string, string>>): Headers =>
  new Headers(values);

const isoTime = (milliseconds: number): string =>
  new Date(milliseconds).toISOString();

const longestWindow = (
  primary: QuotaWindow,
  secondary: QuotaWindow,
): QuotaWindow | null => {
  const known = [primary, secondary].filter(
    (window) => window.seenAtMs !== null,
  );
  return known.reduce<QuotaWindow | null>(
    (longest, window) =>
      longest === null || window.minutes > longest.minutes ? window : longest,
    null,
  );
};

const weeklyRemainingPercent = (view: AccountView): number | null => {
  const weekly = longestWindow(view.primary, view.secondary);
  return weekly === null
    ? null
    : Math.min(Math.max(100 - weekly.usedPercent, 0), 100);
};

const nextResetAt = (view: AccountView, nowMs: number): number | null => {
  const future = [view.primary.resetsAtMs, view.secondary.resetsAtMs].filter(
    (value): value is number => value !== null && value > nowMs,
  );
  return future.length === 0 ? null : Math.min(...future);
};

const publicEventKinds = [
  "account added",
  "account removed",
  "account reset",
  "failover",
  "rate limited",
  "usage poll failed",
] as const;

const publicFailoverDetails = new Set([
  "disconnected",
  "empty_response",
  "invalid_handshake",
  "model_unsupported",
  "rate_limited",
  "server_failure",
  "unauthorized",
  "unreachable",
]);

const publicEventDetail = (kind: string, detail: string): string => {
  return kind === "failover" && publicFailoverDetails.has(detail)
    ? detail.replaceAll("_", " ")
    : "";
};

const onboardingSnapshot = (
  state: InviteState | null,
  nowMs: number,
): OnboardingSnapshot => {
  if (state === null) {
    return { status: "expired" };
  }
  if (state.login?.status === "complete") {
    return { status: "complete" };
  }
  if (state.invite.expiresAtMs <= nowMs) {
    return { status: "expired" };
  }
  if (state.login === null) {
    return state.invite.usedAtMs === null
      ? { expiresAt: isoTime(state.invite.expiresAtMs), status: "ready" }
      : { status: "expired" };
  }
  if (state.login.status === "failed") {
    return {
      error: "Sign-in did not complete.",
      expiresAt: isoTime(state.invite.expiresAtMs),
      status: "failed",
    };
  }
  if (state.login.expiresAtMs <= nowMs) {
    return {
      error: "Sign-in expired.",
      expiresAt: isoTime(state.invite.expiresAtMs),
      status: "failed",
    };
  }
  return {
    expiresAt: isoTime(state.login.expiresAtMs),
    status: "pending",
    userCode: state.login.userCode,
    verificationUrl: state.login.verificationUrl,
  };
};

const inviteStateForSession = (
  state: InviteState | null,
  sessionHash: string,
): InviteState | null =>
  state?.invite.usedAtMs === null || state?.invite.sessionHash === sessionHash
    ? state
    : null;

const mergeModelTiers = (
  target: Record<string, unknown>,
  source: ModelEntry,
): void => {
  const tiers: unknown[] = [];
  const seen = new Set<string>();
  for (const entry of [target, source]) {
    const values = entry.service_tiers;
    if (!Array.isArray(values)) {
      continue;
    }
    for (const value of values) {
      const tier = asRecord(value);
      const id =
        typeof tier?.id === "string" ? canonicalServiceTier(tier.id) : null;
      if (id === null || seen.has(id)) {
        continue;
      }
      seen.add(id);
      tiers.push(value);
    }
  }
  if (tiers.length > 0) {
    target.service_tiers = tiers;
  }
};

const mergedModels = (
  catalogs: ReturnType<StateRepository["modelCatalog"]>,
): readonly ModelEntry[] => {
  const merged = new Map<string, Record<string, unknown>>();
  for (const entries of catalogs.values()) {
    for (const [slug, entry] of entries) {
      const existing = merged.get(slug);
      if (existing === undefined) {
        merged.set(slug, { ...entry });
      } else {
        mergeModelTiers(existing, entry);
      }
    }
  }
  return [...merged.entries()]
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([, entry]) => entry);
};

export class BalancerState extends DurableObject<Env> {
  readonly #repository: StateRepository;
  readonly #refreshes = new Map<AccountId, Promise<RefreshAccountResult>>();
  #dashboardBroadcast: Promise<void> | null = null;
  #dashboardDirty = false;
  #modelsRefresh: Promise<void> | null = null;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    initializeStateSchema(ctx.storage.sql);
    this.#repository = new StateRepository(
      ctx.storage,
      env.TOKEN_ENCRYPTION_KEY,
    );
    ctx.waitUntil(this.#scheduleAlarm());
  }

  async health(): Promise<void> {
    if (!this.#repository.health()) {
      throw new Error("state storage is unavailable");
    }
    await this.#repository.validateEncryption();
    await this.ctx.storage.sync();
  }

  async selectAccount(input: SelectAccountInput): Promise<SelectAccountResult> {
    const skipped = new Set(input.skipAccountIds);
    for (let retry = 0; retry < 5; retry += 1) {
      const nowMs = Date.now();
      const selectionInput = { ...input, skipAccountIds: [...skipped] };
      const preview = this.#repository.previewSelection(selectionInput, nowMs);
      if (!preview.ok) {
        return preview;
      }
      if (nowMs - preview.lastRefreshAtMs > refreshAfterMs) {
        const refreshed = await this.#refreshAccount(
          preview.accountId,
          null,
          true,
        );
        if (!refreshed.ok) {
          skipped.add(preview.accountId);
          continue;
        }
      }
      const committed = this.#repository.commitSelection(
        selectionInput,
        preview.accountId,
        Date.now(),
      );
      if (!committed.ok) {
        if (committed.retry) {
          continue;
        }
        return { failure: committed.failure, ok: false };
      }
      const account = await this.#repository.account(preview.accountId);
      if (account === null) {
        continue;
      }
      return {
        grant: {
          accessToken: account.accessToken,
          accountId: account.accountId,
          resolution: committed.resolution,
        },
        ok: true,
      };
    }
    return { failure: "no_account", ok: false };
  }

  refreshAccount(
    accountId: AccountId,
    rejectedAccessToken: string,
  ): Promise<RefreshAccountResult> {
    return this.#refreshAccount(accountId, rejectedAccessToken, false);
  }

  async observeAccount(observation: AccountObservation): Promise<void> {
    this.#repository.observeHeaders(
      observation.accountId,
      headersFromRecord(observation.headers),
      Date.now(),
    );
    await this.#publish();
  }

  async recordFailure(failure: AccountFailure): Promise<void> {
    const nowMs = Date.now();
    const headers = headersFromRecord(failure.headers);
    this.#repository.observeHeaders(failure.accountId, headers, nowMs);
    if (failure.kind === "rate_limited") {
      this.#repository.setCooldown(
        failure.accountId,
        rateLimitCooldownAt(headers, failure.attempt, nowMs),
      );
      this.#repository.recordEvent({
        accountId: failure.accountId,
        atMs: nowMs,
        kind: "rate limited",
      });
    } else if (failure.kind !== "model_unsupported") {
      this.#repository.setCooldown(
        failure.accountId,
        nowMs + backoffMs(failure.attempt),
      );
    }
    if (failure.failedOver) {
      this.#repository.recordEvent({
        accountId: failure.accountId,
        atMs: nowMs,
        detail: failure.kind,
        kind: "failover",
      });
    }
    await this.#publish();
  }

  async recordRoute(outcome: RecordedRoute): Promise<void> {
    const nowMs = Date.now();
    const bindings = [
      ...outcome.bindings,
      ...(outcome.turnState === null
        ? []
        : [{ kind: "turn_state" as const, value: outcome.turnState }]),
    ];
    if (!this.#repository.bind(bindings, outcome.accountId, nowMs)) {
      throw new Error("affinity binding conflict");
    }
    this.#repository.observeHeaders(
      outcome.accountId,
      headersFromRecord(outcome.headers),
      nowMs,
    );
    if (outcome.counted) {
      this.#repository.recordEvent({
        accountId: outcome.accountId,
        atMs: nowMs,
        detail: outcome.transport,
        kind: "route",
      });
    }
    await this.#publish();
  }

  async recordUsage(usage: ResponseUsage): Promise<void> {
    if (
      usage.inputTokens === 0 &&
      usage.outputTokens === 0 &&
      usage.inputTokensDetails.cachedTokens === 0 &&
      usage.inputTokensDetails.cacheWriteTokens === 0
    ) {
      return;
    }
    this.#repository.recordEvent({
      atMs: Date.now(),
      kind: "response usage",
      usage,
    });
    await this.#publish();
  }

  answered(latencyMs: number): void {
    if (!Number.isFinite(latencyMs) || latencyMs < 0) {
      return;
    }
    this.#repository.recordEvent({
      atMs: Date.now(),
      durationMs: latencyMs,
      kind: "response answered",
    });
  }

  async claimResponseId(
    accountId: AccountId,
    responseId: string,
  ): Promise<void> {
    if (
      responseId !== "" &&
      !this.#repository.bind(
        [{ kind: "response", value: responseId }],
        accountId,
        Date.now(),
      )
    ) {
      throw new Error("response binding conflict");
    }
    await this.ctx.storage.sync();
  }

  async websocketOpened(accountId: AccountId): Promise<void> {
    this.#repository.websocketOpened(accountId);
    await this.#publish();
  }

  async websocketClosed(accountId: AccountId): Promise<void> {
    this.#repository.websocketClosed(accountId);
    await this.#publish();
  }

  async models(clientVersion: string): Promise<readonly ModelEntry[]> {
    const storedVersion = this.#repository.modelState()?.clientVersion ?? "";
    const version = clientVersion === "" ? storedVersion : clientVersion;
    await this.#refreshModels(version, Date.now());
    return mergedModels(this.#repository.modelCatalog());
  }

  async dashboard(): Promise<DashboardSnapshot> {
    const nowMs = Date.now();
    const views = await this.#repository.accountViews();
    const traffic = this.#repository.accountTraffic();
    const totals = this.#repository.trafficTotals();
    const aliases = new Map(
      views.map((view, index) => [view.id, `Account ${String(index + 1)}`]),
    );
    const accounts = views.map((view) => {
      const accountTraffic = traffic.get(view.id);
      const resetAt = nextResetAt(view, nowMs);
      return {
        alias: aliases.get(view.id) ?? "",
        bankedResets: view.bankedResets,
        openWebSockets: view.openWebSockets,
        plan: view.plan,
        rateLimits: accountTraffic?.rateLimits ?? 0,
        resetAt: resetAt === null ? null : isoTime(resetAt),
        status: accountStatusAt(view, nowMs),
        turns: accountTraffic?.turns ?? 0,
        weeklyRemainingPercent: weeklyRemainingPercent(view),
      };
    });
    const events: DashboardEvent[] = this.#repository
      .recentEvents(publicEventKinds, 100)
      .map((event) => ({
        accountAlias: aliases.get(event.accountId) ?? "",
        at: isoTime(event.atMs),
        detail: publicEventDetail(event.kind, event.detail),
        kind: event.kind,
      }));
    return {
      accounts,
      events,
      totals,
      updatedAt: isoTime(nowMs),
    };
  }

  async inspectInvite(token: string): Promise<InviteInspection | null> {
    const state = this.#repository.inviteState(await hashToken(token));
    if (state?.invite.usedAtMs !== null) {
      return null;
    }
    return state.invite.expiresAtMs <= Date.now()
      ? null
      : { expiresAt: isoTime(state.invite.expiresAtMs) };
  }

  async startDeviceLogin(
    token: string,
    sessionToken: string,
  ): Promise<OnboardingSnapshot> {
    const nowMs = Date.now();
    const [tokenHash, sessionHash] = await Promise.all([
      hashToken(token),
      hashToken(sessionToken),
    ]);
    const current = inviteStateForSession(
      this.#repository.inviteState(tokenHash),
      sessionHash,
    );
    const snapshot = onboardingSnapshot(current, nowMs);
    const first = current?.invite.usedAtMs === null && current.login === null;
    const retry =
      current?.login?.status === "failed" ||
      (current?.login?.status === "pending" &&
        current.login.expiresAtMs <= nowMs);
    if (snapshot.status === "expired" || (!first && !retry)) {
      return snapshot;
    }
    let authorization;
    try {
      authorization = await requestDeviceAuthorization(
        upstreamFetch,
        this.env.AUTH_BASE_URL,
        nowMs,
        AbortSignal.timeout(accountRequestTimeoutMs),
      );
    } catch {
      return { error: "Could not start sign-in.", status: "failed" };
    }
    const started = this.#repository.startDeviceJob(
      tokenHash,
      sessionHash,
      randomToken(18),
      authorization,
      nowMs,
    );
    await this.#scheduleAlarm();
    return onboardingSnapshot(started.state, Date.now());
  }

  async onboardingStatus(
    token: string,
    sessionToken: string,
  ): Promise<OnboardingSnapshot> {
    const [tokenHash, sessionHash] = await Promise.all([
      hashToken(token),
      hashToken(sessionToken),
    ]);
    return onboardingSnapshot(
      inviteStateForSession(
        this.#repository.inviteState(tokenHash),
        sessionHash,
      ),
      Date.now(),
    );
  }

  async createInvite(
    baseUrl: string,
    input: CreateInviteInput,
  ): Promise<CreateInviteResult> {
    const expiresInSeconds = input.expiresInSeconds ?? defaultInviteSeconds;
    if (
      !Number.isSafeInteger(expiresInSeconds) ||
      expiresInSeconds <= 0 ||
      expiresInSeconds > maximumInviteSeconds
    ) {
      throw new RangeError("invalid invite expiry");
    }
    const token = randomToken();
    const nowMs = Date.now();
    const expiresAtMs = nowMs + expiresInSeconds * 1000;
    this.#repository.createInvite(
      randomToken(18),
      await hashToken(token),
      nowMs,
      expiresAtMs,
    );
    const url = new URL("/accounts", baseUrl);
    url.searchParams.set("invite", token);
    return { expiresAt: isoTime(expiresAtMs), url: url.href };
  }

  async adminState(): Promise<AdminSnapshot> {
    const nowMs = Date.now();
    const views = await this.#repository.accountViews();
    return {
      accounts: views.map((view) => {
        const resetAt = nextResetAt(view, nowMs);
        return {
          bankedResets: view.bankedResets,
          email: view.email,
          id: view.id,
          paused: view.paused,
          plan: view.plan,
          resetAt: resetAt === null ? null : isoTime(resetAt),
          status: accountStatusAt(view, nowMs),
          weeklyRemainingPercent: weeklyRemainingPercent(view),
        };
      }),
      invites: this.#repository.invites().map((invite) => ({
        expiresAt: isoTime(invite.expiresAtMs),
        id: invite.id,
        usedAt: invite.usedAtMs === null ? null : isoTime(invite.usedAtMs),
      })),
    };
  }

  async updateAccount(
    accountId: AccountId,
    input: UpdateAccountInput,
  ): Promise<void> {
    this.#repository.updateAccount(accountId, input.paused);
    await this.#publish();
  }

  async deleteAccount(accountId: AccountId): Promise<void> {
    if (this.#repository.deleteAccount(accountId)) {
      this.#repository.recordEvent({
        accountId,
        atMs: Date.now(),
        kind: "account removed",
      });
    }
    await this.#publish();
  }

  async dashboardSocket(request: Request): Promise<Response> {
    const failure = webSocketHandshakeFailure(request);
    if (failure !== null) {
      return new Response(failure.message, {
        headers: failure.headers,
        status: failure.status,
      });
    }
    if (
      this.ctx.getWebSockets(dashboardSocketTag).length >=
      maximumDashboardSockets
    ) {
      return new Response("dashboard stream limit reached", { status: 503 });
    }
    const snapshot = await this.dashboard();
    const pair = new WebSocketPair();
    const client = pair[0];
    const server = pair[1];
    this.ctx.acceptWebSocket(server, [dashboardSocketTag]);
    server.send(JSON.stringify(snapshot));
    return new Response(null, { status: 101, webSocket: client });
  }

  override fetch(request: Request): Promise<Response> {
    return this.dashboardSocket(request);
  }

  override webSocketMessage(socket: WebSocket): void {
    socket.close(1008, "read only");
  }

  override webSocketError(socket: WebSocket): void {
    socket.close(1011, "stream error");
  }

  override async alarm(): Promise<void> {
    const nowMs = Date.now();
    let publicChanged = false;
    try {
      for (const login of this.#repository.dueDeviceJobs(nowMs)) {
        if (await this.#pollDeviceLogin(login, nowMs)) {
          publicChanged = true;
        }
      }
      const failures =
        this.#repository.latestEventAtByAccount(usageFailureKind);
      for (const view of await this.#repository.accountViews()) {
        const lastAttempt = Math.max(
          view.usageCheckedAtMs ?? 0,
          failures.get(view.id) ?? 0,
        );
        if (nowMs - lastAttempt >= usageIntervalMs) {
          await this.#pollUsage(view.id, nowMs);
          publicChanged = true;
        }
      }
      const modelState = this.#repository.modelState();
      if (
        modelState !== null &&
        nowMs - modelState.refreshedAtMs >= modelRefreshIntervalMs
      ) {
        await this.#refreshModels(modelState.clientVersion, nowMs);
      }
      this.#repository.pruneInvites(nowMs - inviteRetentionMs);
      this.#repository.pruneHistory(nowMs - historyRetentionMs);
      if (publicChanged) {
        await this.#queueDashboardBroadcast();
      }
    } finally {
      await this.#scheduleAlarm(Date.now(), true);
    }
  }

  async #refreshAccount(
    accountId: AccountId,
    rejectedAccessToken: string | null,
    force: boolean,
  ): Promise<RefreshAccountResult> {
    const current = await this.#repository.account(accountId);
    if (current === null) {
      return { ok: false };
    }
    const reauthReason = this.#repository.reauthReason(accountId);
    if (reauthReason !== null) {
      return { ok: false };
    }
    if (!force && rejectedAccessToken !== current.accessToken) {
      return { accessToken: current.accessToken, ok: true };
    }
    const existing = this.#refreshes.get(accountId);
    if (existing !== undefined) {
      return existing;
    }
    const pending = this.#performRefresh(current);
    this.#refreshes.set(accountId, pending);
    try {
      return await pending;
    } finally {
      if (this.#refreshes.get(accountId) === pending) {
        this.#refreshes.delete(accountId);
      }
    }
  }

  async #performRefresh(
    snapshot: StoredAccount,
  ): Promise<RefreshAccountResult> {
    try {
      const refreshed = await refreshTokens(
        upstreamFetch,
        this.env.AUTH_BASE_URL,
        snapshot.refreshToken,
        AbortSignal.timeout(refreshTimeoutMs),
      );
      const changed = await this.#repository.updateRefreshedTokens(
        snapshot,
        {
          accessToken: refreshed.accessToken,
          idToken: refreshed.idToken ?? snapshot.idToken,
          refreshToken: refreshed.refreshToken ?? snapshot.refreshToken,
        },
        Date.now(),
      );
      const current = await this.#repository.account(snapshot.accountId);
      if (
        current !== null &&
        (changed || current.credentialRevision !== snapshot.credentialRevision)
      ) {
        await this.#publish();
        return { accessToken: current.accessToken, ok: true };
      }
      return { ok: false };
    } catch (error) {
      const current = await this.#repository.account(snapshot.accountId);
      if (
        current !== null &&
        current.credentialRevision !== snapshot.credentialRevision
      ) {
        return { accessToken: current.accessToken, ok: true };
      }
      if (error instanceof RefreshTokenError && error.permanent) {
        this.#repository.setReauthReason(
          snapshot.accountId,
          snapshot.credentialRevision,
          error.reason,
        );
        await this.#publish();
      }
      return { ok: false };
    }
  }

  async #accountRequest<T>(
    accountId: AccountId,
    request: (account: StoredAccount) => Promise<T>,
  ): Promise<T> {
    const first = await this.#repository.account(accountId);
    if (first === null) {
      throw new Error("account no longer exists");
    }
    try {
      return await request(first);
    } catch (error) {
      if (!(error instanceof AccountUpstreamError) || error.status !== 401) {
        throw error;
      }
      const refreshed = await this.refreshAccount(accountId, first.accessToken);
      if (!refreshed.ok) {
        throw error;
      }
      const current = await this.#repository.account(accountId);
      if (current === null) {
        throw new Error("account no longer exists", { cause: error });
      }
      return request(current);
    }
  }

  async #pollUsage(accountId: AccountId, nowMs: number): Promise<void> {
    try {
      const observation = await this.#accountRequest(accountId, (account) =>
        fetchUsage(
          upstreamFetch,
          this.env.USAGE_BASE_URL,
          { accessToken: account.accessToken, accountId },
          nowMs,
          AbortSignal.timeout(accountRequestTimeoutMs),
        ),
      );
      this.#repository.applyUsage(accountId, observation);
      if ((observation.bankedResets ?? 0) > 0) {
        await this.#processResetCredits(accountId, nowMs);
      }
    } catch {
      this.#repository.recordEvent({
        accountId,
        atMs: nowMs,
        kind: usageFailureKind,
      });
    }
  }

  async #processResetCredits(
    accountId: AccountId,
    nowMs: number,
  ): Promise<void> {
    const credits = await this.#accountRequest(accountId, (account) =>
      fetchResetCredits(
        upstreamFetch,
        this.env.USAGE_BASE_URL,
        {
          accessToken: account.accessToken,
          accountId,
        },
        AbortSignal.timeout(accountRequestTimeoutMs),
      ),
    );
    this.#repository.setBankedResets(accountId, credits.availableCount);
    const expiring = credits.credits
      .filter(
        (credit) =>
          credit.resetType === "codex_rate_limits" &&
          credit.status === "available" &&
          credit.expiresAtMs !== null &&
          credit.expiresAtMs > nowMs &&
          credit.expiresAtMs <= nowMs + resetCreditLeadMs,
      )
      .sort(
        (left, right) =>
          (left.expiresAtMs ?? Number.MAX_SAFE_INTEGER) -
          (right.expiresAtMs ?? Number.MAX_SAFE_INTEGER),
      )[0];
    if (expiring === undefined) {
      return;
    }
    const result = await this.#accountRequest(accountId, (account) =>
      consumeResetCredit(
        upstreamFetch,
        this.env.USAGE_BASE_URL,
        { accessToken: account.accessToken, accountId },
        expiring.id,
        AbortSignal.timeout(accountRequestTimeoutMs),
      ),
    );
    if (result.code === "reset" || result.code === "already_redeemed") {
      this.#repository.recordEvent({
        accountId,
        atMs: nowMs,
        detail: result.code,
        kind: "account reset",
      });
    }
    if (result.code !== "nothing_to_reset") {
      const observation = await this.#accountRequest(accountId, (account) =>
        fetchUsage(
          upstreamFetch,
          this.env.USAGE_BASE_URL,
          { accessToken: account.accessToken, accountId },
          Date.now(),
          AbortSignal.timeout(accountRequestTimeoutMs),
        ),
      );
      this.#repository.applyUsage(accountId, observation);
    }
  }

  async #pollDeviceLogin(
    login: StoredDeviceLogin,
    nowMs: number,
  ): Promise<boolean> {
    if (login.expiresAtMs <= nowMs) {
      this.#repository.failDeviceJob(login.loginId, login.deviceAuthId);
      return false;
    }
    try {
      const result = await pollDeviceAuthorization(
        upstreamFetch,
        this.env.AUTH_BASE_URL,
        login.deviceAuthId,
        login.userCode,
        AbortSignal.timeout(accountRequestTimeoutMs),
      );
      if (result.status === "pending") {
        this.#repository.deferDeviceJob(
          login.loginId,
          login.deviceAuthId,
          Math.min(nowMs + login.pollIntervalMs, login.expiresAtMs),
        );
        return false;
      }
      const tokens = await exchangeDeviceCode(
        upstreamFetch,
        this.env.AUTH_BASE_URL,
        result.authorizationCode,
        result.codeVerifier,
        AbortSignal.timeout(accountRequestTimeoutMs),
      );
      return (
        (await this.#repository.completeDeviceJob(
          login,
          tokens,
          Date.now(),
        )) !== null
      );
    } catch {
      this.#repository.failDeviceJob(login.loginId, login.deviceAuthId);
      return false;
    }
  }

  async #refreshModels(clientVersion: string, nowMs: number): Promise<void> {
    if (clientVersion === "") {
      return;
    }
    const state = this.#repository.modelState();
    if (
      state !== null &&
      state.clientVersion === clientVersion &&
      nowMs - state.refreshedAtMs < modelRefreshIntervalMs
    ) {
      return;
    }
    if (this.#modelsRefresh !== null) {
      await this.#modelsRefresh;
      return this.#refreshModels(clientVersion, Date.now());
    }
    const pending = this.#performModelRefresh(clientVersion);
    this.#modelsRefresh = pending;
    try {
      await pending;
    } finally {
      if (this.#modelsRefresh === pending) {
        this.#modelsRefresh = null;
      }
    }
  }

  async #performModelRefresh(clientVersion: string): Promise<void> {
    const accounts = (await this.#repository.accounts()).filter(
      (account) => !account.paused,
    );
    const fresh = new Map<AccountId, readonly ModelEntry[]>();
    for (const account of accounts) {
      try {
        const models = await this.#accountRequest(
          account.accountId,
          (current) =>
            fetchModels(
              upstreamFetch,
              this.env.UPSTREAM_BASE_URL,
              {
                accessToken: current.accessToken,
                accountId: current.accountId,
              },
              clientVersion,
              AbortSignal.timeout(accountRequestTimeoutMs),
            ),
        );
        fresh.set(account.accountId, models);
      } catch {
        continue;
      }
    }
    this.#repository.replaceModelCatalogs(
      accounts.map((account) => account.accountId),
      fresh,
      clientVersion,
      Date.now(),
    );
  }

  async #publish(): Promise<void> {
    await this.#scheduleAlarm();
    this.ctx.waitUntil(this.#queueDashboardBroadcast());
  }

  #queueDashboardBroadcast(): Promise<void> {
    this.#dashboardDirty = true;
    if (this.#dashboardBroadcast !== null) {
      return this.#dashboardBroadcast;
    }
    const pending = (async () => {
      while (this.#dashboardDirty) {
        this.#dashboardDirty = false;
        await this.#broadcastDashboard();
      }
    })().finally(() => {
      this.#dashboardBroadcast = null;
      if (this.#dashboardDirty) {
        this.ctx.waitUntil(this.#queueDashboardBroadcast());
      }
    });
    this.#dashboardBroadcast = pending;
    return pending;
  }

  async #broadcastDashboard(): Promise<void> {
    const sockets = this.ctx.getWebSockets(dashboardSocketTag);
    if (sockets.length === 0) {
      return;
    }
    const payload = JSON.stringify(await this.dashboard());
    for (const socket of sockets) {
      try {
        socket.send(payload);
      } catch {
        socket.close(1011, "stream error");
      }
    }
  }

  async #scheduleAlarm(nowMs = Date.now(), replace = false): Promise<void> {
    const candidates = [nowMs + cleanupIntervalMs];
    const accountWork = this.#repository.nextUsageWorkAt(
      usageFailureKind,
      usageIntervalMs,
    );
    if (accountWork !== null) {
      candidates.push(accountWork);
    }
    const devicePoll = this.#repository.nextDevicePollAt();
    if (devicePoll !== null) {
      candidates.push(devicePoll);
    }
    const models = this.#repository.modelState();
    if (models !== null && models.clientVersion !== "") {
      candidates.push(models.refreshedAtMs + modelRefreshIntervalMs);
    }
    const next = Math.max(nowMs + 1000, Math.min(...candidates));
    const current = await this.ctx.storage.getAlarm();
    if (replace || current === null || next < current) {
      await this.ctx.storage.setAlarm(next);
    }
  }
}
