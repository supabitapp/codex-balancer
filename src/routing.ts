import type {
  AccountStatus,
  ModelCatalog,
  ModelEntry,
  QuotaWindow,
  RoutingAccount,
  RoutingConstraints,
  RoutingDecision,
} from "./domain.js";
import { isRecord } from "./record.js";

const MIN_COOLDOWN_MS = 30_000;
const MAX_COOLDOWN_MS = 3_600_000;

const quotaWindowKnown = (window: QuotaWindow): boolean =>
  window.seenAtMs !== null;

const quotaKnown = (account: RoutingAccount): boolean =>
  quotaWindowKnown(account.primary) || quotaWindowKnown(account.secondary);

export const accountPressure = (account: RoutingAccount): number =>
  Math.max(account.primary.usedPercent, account.secondary.usedPercent);

const hasReauthFailure = (account: RoutingAccount): boolean =>
  account.reauthReason !== null && account.reauthReason !== "";

export const accountStatusAt = (
  account: RoutingAccount,
  nowMs: number,
): AccountStatus => {
  if (account.paused) {
    return "paused";
  }
  if (hasReauthFailure(account)) {
    return "needs_reauth";
  }
  if (
    account.spent ||
    (account.cooldownUntilMs !== null && nowMs < account.cooldownUntilMs)
  ) {
    return "cooling";
  }
  if (!quotaKnown(account)) {
    return "checking";
  }
  return "live";
};

export const accountAvailableAt = (
  account: RoutingAccount,
  nowMs: number,
): boolean =>
  !account.paused &&
  !hasReauthFailure(account) &&
  !account.spent &&
  quotaKnown(account) &&
  (account.cooldownUntilMs === null || nowMs > account.cooldownUntilMs);

const compareStrings = (left: string, right: string): number => {
  if (left < right) {
    return -1;
  }
  if (left > right) {
    return 1;
  }
  return 0;
};

const compareLastUsed = (left: number | null, right: number | null): number => {
  if (left === right) {
    return 0;
  }
  if (left === null) {
    return -1;
  }
  if (right === null) {
    return 1;
  }
  return left < right ? -1 : 1;
};

export const roomierThan = (
  candidate: RoutingAccount,
  current: RoutingAccount,
): boolean => {
  const candidatePressure = accountPressure(candidate);
  const currentPressure = accountPressure(current);
  if (Math.abs(candidatePressure - currentPressure) > 1) {
    return candidatePressure < currentPressure;
  }
  const lastUsed = compareLastUsed(
    candidate.lastUsedAtMs,
    current.lastUsedAtMs,
  );
  if (lastUsed !== 0) {
    return lastUsed < 0;
  }
  return compareStrings(candidate.id, current.id) < 0;
};

const accountAllowed = (
  allowedIds: ReadonlySet<string> | null,
  accountId: string,
): boolean => allowedIds === null || allowedIds.has(accountId);

const candidateEligible = (
  account: RoutingAccount,
  constraints: RoutingConstraints,
  nowMs: number,
): boolean =>
  !constraints.skippedIds.has(account.id) &&
  accountAllowed(constraints.allowedIds, account.id) &&
  accountAvailableAt(account, nowMs);

export const routeAccount = (
  accounts: readonly RoutingAccount[],
  constraints: RoutingConstraints,
  nowMs: number,
): RoutingDecision => {
  const candidates = [...accounts];
  if (constraints.requiredId !== null && constraints.requiredId !== "") {
    const account =
      candidates.find(
        (candidate) =>
          candidate.id === constraints.requiredId &&
          candidateEligible(candidate, constraints, nowMs),
      ) ?? null;
    return { account, candidates, nowMs };
  }

  if (
    constraints.preferredId !== null &&
    constraints.preferredId !== "" &&
    !constraints.skippedIds.has(constraints.preferredId) &&
    accountAllowed(constraints.allowedIds, constraints.preferredId)
  ) {
    const preferred = candidates.find(
      (candidate) =>
        candidate.id === constraints.preferredId &&
        accountAvailableAt(candidate, nowMs),
    );
    if (preferred !== undefined) {
      return { account: preferred, candidates, nowMs };
    }
  }

  let best: RoutingAccount | null = null;
  for (const candidate of candidates) {
    if (!candidateEligible(candidate, constraints, nowMs)) {
      continue;
    }
    if (best === null || roomierThan(candidate, best)) {
      best = candidate;
    }
  }
  return { account: best, candidates, nowMs };
};

export const modelSlug = (entry: ModelEntry): string => {
  const slug = entry.slug;
  return typeof slug === "string" ? slug.trim().toLowerCase() : "";
};

export const canonicalServiceTier = (serviceTier: string): string | null => {
  const canonical = serviceTier.trim().toLowerCase();
  if (canonical === "" || canonical === "auto" || canonical === "default") {
    return null;
  }
  return canonical === "fast" ? "priority" : canonical;
};

export const modelServiceTiers = (entry: ModelEntry): readonly string[] => {
  const values = entry.service_tiers;
  if (!Array.isArray(values)) {
    return [];
  }
  const tiers: string[] = [];
  for (const value of values) {
    if (!isRecord(value)) {
      continue;
    }
    const id = value.id;
    if (typeof id !== "string") {
      continue;
    }
    const tier = canonicalServiceTier(id);
    if (tier !== null) {
      tiers.push(tier);
    }
  }
  return tiers;
};

export const modelSupportsServiceTier = (
  entry: ModelEntry,
  serviceTier: string,
): boolean => modelServiceTiers(entry).includes(serviceTier);

export const allowedAccountIds = (
  catalog: ModelCatalog,
  accounts: readonly RoutingAccount[],
  model: string,
  serviceTier: string,
  nowMs: number,
): ReadonlySet<string> | null => {
  const normalizedModel = model.trim().toLowerCase();
  if (normalizedModel === "") {
    return null;
  }
  for (const account of accounts) {
    if (accountAvailableAt(account, nowMs) && !catalog.has(account.id)) {
      return null;
    }
  }

  const normalizedTier = canonicalServiceTier(serviceTier);
  const allowed = new Set<string>();
  for (const account of accounts) {
    const entry = catalog.get(account.id)?.get(normalizedModel);
    if (entry === undefined) {
      continue;
    }
    if (
      normalizedTier !== null &&
      !modelSupportsServiceTier(entry, normalizedTier)
    ) {
      continue;
    }
    allowed.add(account.id);
  }
  return allowed;
};

const integerHeader = (value: string | null): number | null => {
  if (value === null || !/^[+-]?\d+$/u.test(value)) {
    return null;
  }
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) ? parsed : null;
};

export const readQuotaWindow = (
  headers: Headers,
  prefix: string,
  nowMs: number,
): QuotaWindow | null => {
  const usedHeader = headers.get(`${prefix}-used-percent`);
  if (usedHeader === null || usedHeader === "") {
    return null;
  }
  const usedPercent = Number(usedHeader);
  if (Number.isNaN(usedPercent)) {
    return null;
  }
  const minutes = integerHeader(headers.get(`${prefix}-window-minutes`)) ?? 0;
  const resetSeconds = integerHeader(headers.get(`${prefix}-reset-at`));
  return {
    minutes,
    resetsAtMs: resetSeconds === null ? null : resetSeconds * 1000,
    seenAtMs: nowMs,
    usedPercent,
  };
};

export const backoffMs = (attempt: number): number =>
  Math.min(MIN_COOLDOWN_MS * 2 ** attempt, MAX_COOLDOWN_MS);

const resetAtFromHeaders = (headers: Headers, nowMs: number): number | null => {
  let usedPercent = -1;
  let resetAtMs: number | null = null;
  for (const prefix of ["x-codex-primary", "x-codex-secondary-primary"]) {
    const window = readQuotaWindow(headers, prefix, nowMs);
    if (window !== null && window.usedPercent > usedPercent) {
      usedPercent = window.usedPercent;
      resetAtMs = window.resetsAtMs;
    }
  }
  if (resetAtMs !== null) {
    return resetAtMs;
  }

  const retryAfter = headers.get("retry-after");
  const seconds = integerHeader(retryAfter);
  if (seconds !== null) {
    return nowMs + seconds * 1000;
  }
  if (retryAfter === null) {
    return null;
  }
  const date = Date.parse(retryAfter);
  return Number.isNaN(date) ? null : date;
};

export const rateLimitCooldownAt = (
  headers: Headers,
  attempt: number,
  nowMs: number,
): number => resetAtFromHeaders(headers, nowMs) ?? nowMs + backoffMs(attempt);
