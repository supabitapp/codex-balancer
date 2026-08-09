import type {
  AffinityAbandonment,
  AffinityBinding,
  AffinityFailure,
  AffinityRef,
  AffinityResolutionResult,
  RequestAffinity,
  RoutingAccount,
} from "./domain.js";
import { isRecord } from "./record.js";

const HARD_AFFINITY_ABANDON_AFTER_MS = 3_600_000;

const nonEmptyString = (value: unknown): string | null => {
  if (typeof value !== "string") {
    return null;
  }
  const text = value.trim();
  return text === "" ? null : text;
};

const validAffinityRef = (ref: AffinityRef | null): ref is AffinityRef =>
  ref !== null && ref.value !== "";

export const affinityStorageKey = (ref: AffinityRef): string =>
  `${ref.kind}\n${ref.value}`;

const isHardAffinity = (ref: AffinityRef): boolean => {
  switch (ref.kind) {
    case "turn_state":
    case "response":
    case "conversation":
    case "file":
      return true;
    case "session":
    case "prompt_cache":
      return false;
  }
};

const isAbandonableAffinity = (ref: AffinityRef): boolean =>
  ref.kind === "turn_state" || ref.kind === "conversation";

const firstHeader = (headers: Headers, ...names: readonly string[]): string => {
  for (const name of names) {
    const value = headers.get(name)?.trim() ?? "";
    if (value !== "") {
      return value;
    }
  }
  return "";
};

const sessionAffinity = (headers: Headers): AffinityRef | null => {
  const value = firstHeader(
    headers,
    "session_id",
    "session-id",
    "x-codex-session-id",
    "x-codex-conversation-id",
    "thread-id",
  );
  return value === "" ? null : { kind: "session", value };
};

export const turnStateAffinity = (headers: Headers): AffinityRef | null => {
  const value = firstHeader(headers, "x-codex-turn-state");
  return value === "" ? null : { kind: "turn_state", value };
};

const clientTurnStateAffinity = (
  payload: Readonly<Record<string, unknown>>,
): AffinityRef | null => {
  const metadata = payload.client_metadata;
  if (!isRecord(metadata)) {
    return null;
  }
  const value = nonEmptyString(metadata["x-codex-turn-state"]);
  return value === null ? null : { kind: "turn_state", value };
};

const inputFileIds = (input: unknown): readonly string[] => {
  const ids: string[] = [];
  const visit = (value: unknown): void => {
    if (Array.isArray(value)) {
      for (const item of value) {
        visit(item);
      }
      return;
    }
    if (!isRecord(value)) {
      return;
    }
    if (nonEmptyString(value.type) === "input_file") {
      const id = nonEmptyString(value.file_id);
      if (id !== null) {
        ids.push(id);
      }
    }
    for (const item of Object.values(value)) {
      visit(item);
    }
  };
  visit(input);
  return ids;
};

const requestPayload = (
  payload: unknown,
): Readonly<Record<string, unknown>> => {
  if (payload === null || payload === undefined) {
    return {};
  }
  if (!isRecord(payload)) {
    throw new TypeError("affinity payload must be an object");
  }
  return payload;
};

export const extractRequestAffinity = (
  headers: Headers,
  rawPayload: unknown,
): RequestAffinity => {
  const payload = requestPayload(rawPayload);
  let preferred: AffinityRef | null = null;
  const hard: AffinityRef[] = [];

  const turnState =
    clientTurnStateAffinity(payload) ?? turnStateAffinity(headers);
  if (turnState !== null) {
    hard.push(turnState);
  } else {
    const session = sessionAffinity(headers);
    if (session !== null) {
      preferred = session;
    } else {
      const cache =
        nonEmptyString(payload.prompt_cache_key) ??
        nonEmptyString(payload.promptCacheKey);
      if (cache !== null) {
        preferred = { kind: "prompt_cache", value: cache };
      }
    }
  }

  const previousResponse = nonEmptyString(payload.previous_response_id);
  if (previousResponse !== null) {
    hard.push({ kind: "response", value: previousResponse });
  }

  let requireUnambiguous = false;
  const conversation = nonEmptyString(payload.conversation);
  if (conversation !== null) {
    hard.push({ kind: "conversation", value: conversation });
    requireUnambiguous = true;
  } else if (
    Object.hasOwn(payload, "conversation") &&
    payload.conversation !== null &&
    typeof payload.conversation !== "string"
  ) {
    requireUnambiguous = true;
  }

  for (const fileId of inputFileIds(payload.input)) {
    hard.push({ kind: "file", value: fileId });
  }
  return { hard, preferred, requireUnambiguous };
};

const affinityBindings = (request: RequestAffinity): readonly AffinityRef[] => {
  const seen = new Set<string>();
  const bindings: AffinityRef[] = [];
  const refs =
    request.preferred === null
      ? request.hard
      : [request.preferred, ...request.hard];
  for (const ref of refs) {
    if (!validAffinityRef(ref)) {
      continue;
    }
    const key = affinityStorageKey(ref);
    if (seen.has(key)) {
      continue;
    }
    seen.add(key);
    bindings.push(ref);
  }
  return bindings;
};

const hardAffinityRefs = (
  refs: readonly AffinityRef[],
): readonly AffinityRef[] => refs.filter(isHardAffinity);

export const affinityOwnerAbandonable = (
  account: RoutingAccount,
  nowMs: number,
): boolean => {
  if (
    account.paused ||
    (account.reauthReason !== null && account.reauthReason !== "")
  ) {
    return true;
  }
  if (!account.spent) {
    return false;
  }
  const primaryPending =
    account.primary.resetsAtMs !== null && account.primary.resetsAtMs > nowMs;
  const secondaryPending =
    account.secondary.resetsAtMs !== null &&
    account.secondary.resetsAtMs > nowMs;
  return !primaryPending && !secondaryPending;
};

export const shouldAbandonAffinity = (
  ref: AffinityRef,
  binding: AffinityBinding,
  owner: RoutingAccount | null,
  nowMs: number,
): boolean =>
  binding.accountId !== "" &&
  binding.abandonedAtMs === null &&
  isAbandonableAffinity(ref) &&
  nowMs - binding.lastUsedAtMs >= HARD_AFFINITY_ABANDON_AFTER_MS &&
  (owner === null || affinityOwnerAbandonable(owner, nowMs));

interface EffectiveBinding {
  readonly ownerId: string | null;
  readonly abandoned: boolean;
}

const failedResolution = (
  failure: AffinityFailure,
  abandonments: readonly AffinityAbandonment[],
): AffinityResolutionResult => ({ abandonments, failure, ok: false });

export const resolveAffinity = (
  request: RequestAffinity,
  bindings: ReadonlyMap<string, AffinityBinding>,
  accounts: readonly RoutingAccount[],
  nowMs: number,
): AffinityResolutionResult => {
  const accountsById = new Map(
    accounts.map((account) => [account.id, account]),
  );
  const abandonments: AffinityAbandonment[] = [];
  const effectiveBindings = new Map<string, EffectiveBinding>();
  const effectiveBinding = (ref: AffinityRef): EffectiveBinding => {
    const key = affinityStorageKey(ref);
    const cached = effectiveBindings.get(key);
    if (cached !== undefined) {
      return cached;
    }
    const binding = bindings.get(key);
    if (binding === undefined) {
      const missing = { abandoned: false, ownerId: null } as const;
      effectiveBindings.set(key, missing);
      return missing;
    }
    if (binding.abandonedAtMs !== null) {
      const abandoned = { abandoned: true, ownerId: null } as const;
      effectiveBindings.set(key, abandoned);
      return abandoned;
    }
    const owner = accountsById.get(binding.accountId) ?? null;
    if (shouldAbandonAffinity(ref, binding, owner, nowMs)) {
      abandonments.push({
        accountId: binding.accountId,
        lastUsedAtMs: binding.lastUsedAtMs,
        ref,
      });
      const abandoned = { abandoned: true, ownerId: null } as const;
      effectiveBindings.set(key, abandoned);
      return abandoned;
    }
    const active = {
      abandoned: false,
      ownerId: binding.accountId === "" ? null : binding.accountId,
    } as const;
    effectiveBindings.set(key, active);
    return active;
  };

  const preferredBinding =
    request.preferred === null
      ? null
      : effectiveBinding(request.preferred).ownerId;
  const owners = new Set<string>();
  const refOwners = new Map<string, string | null>();
  let fileCount = 0;
  let ownedFileCount = 0;
  let conversationKnown = false;
  let turnKnown = false;
  let conversationAbandoned = false;
  let unknownTurn = false;
  let nonFileHard = false;

  for (const ref of request.hard) {
    const binding = effectiveBinding(ref);
    const ownerId = binding.ownerId;
    refOwners.set(affinityStorageKey(ref), ownerId);
    if (ref.kind === "conversation" && ownerId !== null) {
      conversationKnown = true;
    }
    if (ref.kind === "conversation" && binding.abandoned) {
      conversationAbandoned = true;
    }
    if (ref.kind === "turn_state" && ownerId !== null) {
      turnKnown = true;
    } else if (ref.kind === "turn_state" && !binding.abandoned) {
      unknownTurn = true;
    }
    if (ref.kind === "file") {
      fileCount += 1;
      if (ownerId !== null) {
        ownedFileCount += 1;
      }
    } else {
      nonFileHard = true;
    }
    if (ownerId !== null) {
      owners.add(ownerId);
    } else if (ref.kind === "response") {
      return failedResolution("owner_unavailable", abandonments);
    }
  }

  if (ownedFileCount > 0 && ownedFileCount !== fileCount) {
    return failedResolution("owner_unavailable", abandonments);
  }
  if (owners.size > 1) {
    return failedResolution("conflict", abandonments);
  }

  let requiredAccountId: string | null = null;
  for (const ownerId of owners) {
    if (!accountsById.has(ownerId)) {
      return failedResolution("owner_unavailable", abandonments);
    }
    requiredAccountId = ownerId;
  }

  if (unknownTurn && requiredAccountId === null) {
    const onlyAccount = accounts.length === 1 ? accounts[0] : undefined;
    if (onlyAccount === undefined) {
      return failedResolution("owner_unavailable", abandonments);
    }
    requiredAccountId = onlyAccount.id;
  }
  if (
    request.requireUnambiguous &&
    !conversationKnown &&
    !turnKnown &&
    !conversationAbandoned
  ) {
    const onlyAccount = accounts.length === 1 ? accounts[0] : undefined;
    if (onlyAccount === undefined) {
      return failedResolution("ambiguous", abandonments);
    }
    requiredAccountId = onlyAccount.id;
  }

  const hard = nonFileHard || ownedFileCount > 0 || request.requireUnambiguous;
  let outputBindings = affinityBindings(request);
  if (hard) {
    outputBindings = hardAffinityRefs(outputBindings);
  }
  outputBindings = outputBindings.filter(
    (ref) =>
      ref.kind !== "file" ||
      (refOwners.get(affinityStorageKey(ref)) ?? null) !== null,
  );
  return {
    abandonments,
    ok: true,
    resolution: {
      bindings: outputBindings,
      hard,
      preferredAccountId: preferredBinding,
      requiredAccountId,
    },
  };
};
