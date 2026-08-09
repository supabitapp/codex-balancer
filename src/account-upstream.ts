import { asRecord } from "./record.js";

const oauthClientId = "app_EMoamEEZ73f0CkXaXp7hrann";
const maximumErrorBytes = 4 << 10;
const maximumModelBytes = 16 << 20;

export interface TokenSet {
  readonly accessToken: string;
  readonly idToken: string;
  readonly refreshToken: string;
}

interface RefreshedTokenSet {
  readonly accessToken: string;
  readonly idToken?: string;
  readonly refreshToken?: string;
}

export interface DeviceAuthorization {
  readonly deviceAuthId: string;
  readonly expiresAtMs: number;
  readonly pollIntervalMs: number;
  readonly userCode: string;
  readonly verificationUrl: string;
}

type DevicePollResult =
  | { readonly status: "pending" }
  | {
      readonly authorizationCode: string;
      readonly codeVerifier: string;
      readonly status: "complete";
    };

export interface UsageWindowObservation {
  readonly observedAtMs: number;
  readonly resetsAtMs: number | null;
  readonly usedPercent: number;
  readonly windowMinutes: number;
}

export interface UsageObservation {
  readonly bankedResets: number | null;
  readonly limitReached: boolean | null;
  readonly observedAtMs: number;
  readonly primary: UsageWindowObservation | null;
  readonly secondary: UsageWindowObservation | null;
}

interface ResetCredit {
  readonly expiresAtMs: number | null;
  readonly id: string;
  readonly resetType: string;
  readonly status: string;
}

interface ResetCredits {
  readonly availableCount: number;
  readonly credits: readonly ResetCredit[];
}

interface ResetCreditResult {
  readonly code: string;
  readonly windowsReset: number;
}

interface AccountCredentials {
  readonly accessToken: string;
  readonly accountId: string;
}

export class AccountUpstreamError extends Error {
  readonly body: string;
  readonly status: number;

  constructor(message: string, status: number, body = "") {
    super(message);
    this.name = "AccountUpstreamError";
    this.body = body;
    this.status = status;
  }
}

export class RefreshTokenError extends AccountUpstreamError {
  readonly permanent: boolean;
  readonly reason: string;

  constructor(status: number, reason: string, permanent: boolean) {
    super("token refresh failed", status);
    this.name = "RefreshTokenError";
    this.permanent = permanent;
    this.reason = reason;
  }
}

export type AccountFetcher = (
  input: string | URL,
  init?: RequestInit,
) => Promise<Response>;

const trimBaseUrl = (value: string): string => value.replace(/\/+$/u, "");

const readLimited = async (
  response: Response,
  maximumBytes: number,
): Promise<Uint8Array<ArrayBuffer>> => {
  if (response.body === null) {
    return new Uint8Array();
  }
  const reader = response.body.getReader();
  const chunks: Uint8Array<ArrayBuffer>[] = [];
  let size = 0;
  try {
    for (
      let result = await reader.read();
      !result.done;
      result = await reader.read()
    ) {
      if (!(result.value instanceof Uint8Array)) {
        throw new Error("upstream response body has an invalid chunk");
      }
      const chunk = Uint8Array.from(result.value);
      size += chunk.byteLength;
      if (size > maximumBytes) {
        await reader.cancel();
        throw new Error("upstream response body is too large");
      }
      chunks.push(chunk);
    }
  } finally {
    reader.releaseLock();
  }
  const output = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    output.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return output;
};

const readTextLimited = async (
  response: Response,
  maximumBytes: number,
): Promise<string> =>
  new TextDecoder().decode(await readLimited(response, maximumBytes));

const readJsonLimited = async (
  response: Response,
  maximumBytes: number,
): Promise<unknown> =>
  JSON.parse(await readTextLimited(response, maximumBytes)) as unknown;

const requiredString = (
  value: unknown,
  field: string,
  allowEmpty = false,
): string => {
  if (typeof value !== "string" || (!allowEmpty && value.trim() === "")) {
    throw new Error(`upstream response omitted ${field}`);
  }
  return value;
};

const optionalString = (value: unknown): string | undefined =>
  typeof value === "string" && value !== "" ? value : undefined;

const integer = (value: unknown, field: string): number => {
  if (!Number.isSafeInteger(value)) {
    throw new Error(`upstream response has invalid ${field}`);
  }
  return value as number;
};

const postJson = (
  fetcher: AccountFetcher,
  url: string,
  value: unknown,
  signal?: AbortSignal,
): Promise<Response> =>
  fetcher(url, {
    body: JSON.stringify(value),
    headers: { "Content-Type": "application/json" },
    method: "POST",
    ...(signal === undefined ? {} : { signal }),
  });

export const refreshTokens = async (
  fetcher: AccountFetcher,
  authBaseUrl: string,
  refreshToken: string,
  signal?: AbortSignal,
): Promise<RefreshedTokenSet> => {
  const response = await postJson(
    fetcher,
    `${trimBaseUrl(authBaseUrl)}/oauth/token`,
    {
      client_id: oauthClientId,
      grant_type: "refresh_token",
      refresh_token: refreshToken,
    },
    signal,
  );
  if (!response.ok) {
    const body = await readTextLimited(response, maximumErrorBytes);
    const reasons = [
      "refresh_token_expired",
      "refresh_token_reused",
      "refresh_token_invalidated",
      "invalid_grant",
    ];
    const reason = reasons.find((candidate) => body.includes(candidate));
    throw new RefreshTokenError(
      response.status,
      reason ?? `status_${String(response.status)}`,
      reason !== undefined || response.status === 401,
    );
  }
  const payload = asRecord(await readJsonLimited(response, maximumErrorBytes));
  if (payload === undefined) {
    throw new Error("token response is not an object");
  }
  const idToken = optionalString(payload.id_token);
  const refresh = optionalString(payload.refresh_token);
  return {
    accessToken: requiredString(payload.access_token, "access_token"),
    ...(idToken === undefined ? {} : { idToken }),
    ...(refresh === undefined ? {} : { refreshToken: refresh }),
  };
};

export const requestDeviceAuthorization = async (
  fetcher: AccountFetcher,
  authBaseUrl: string,
  nowMs: number,
  signal?: AbortSignal,
): Promise<DeviceAuthorization> => {
  const baseUrl = trimBaseUrl(authBaseUrl);
  const response = await postJson(
    fetcher,
    `${baseUrl}/api/accounts/deviceauth/usercode`,
    { client_id: oauthClientId },
    signal,
  );
  if (!response.ok) {
    await response.body?.cancel();
    throw new AccountUpstreamError(
      response.status === 404
        ? "device code login is not enabled"
        : "device code request failed",
      response.status,
    );
  }
  const payload = asRecord(await readJsonLimited(response, maximumErrorBytes));
  if (payload === undefined) {
    throw new Error("device code response is not an object");
  }
  const interval = Number.parseInt(
    requiredString(payload.interval, "interval"),
    10,
  );
  if (!Number.isSafeInteger(interval) || interval <= 0) {
    throw new Error("device code response has invalid interval");
  }
  return {
    deviceAuthId: requiredString(payload.device_auth_id, "device_auth_id"),
    expiresAtMs: nowMs + 15 * 60 * 1000,
    pollIntervalMs: interval * 1000,
    userCode: requiredString(payload.user_code, "user_code"),
    verificationUrl: `${baseUrl}/codex/device`,
  };
};

export const pollDeviceAuthorization = async (
  fetcher: AccountFetcher,
  authBaseUrl: string,
  deviceAuthId: string,
  userCode: string,
  signal?: AbortSignal,
): Promise<DevicePollResult> => {
  const response = await postJson(
    fetcher,
    `${trimBaseUrl(authBaseUrl)}/api/accounts/deviceauth/token`,
    { device_auth_id: deviceAuthId, user_code: userCode },
    signal,
  );
  if (response.status === 403 || response.status === 404) {
    await response.body?.cancel();
    return { status: "pending" };
  }
  if (!response.ok) {
    await response.body?.cancel();
    throw new AccountUpstreamError(
      "device authorization failed",
      response.status,
    );
  }
  const payload = asRecord(await readJsonLimited(response, maximumErrorBytes));
  if (payload === undefined) {
    throw new Error("device authorization response is not an object");
  }
  return {
    authorizationCode: requiredString(
      payload.authorization_code,
      "authorization_code",
    ),
    codeVerifier: requiredString(payload.code_verifier, "code_verifier"),
    status: "complete",
  };
};

export const exchangeDeviceCode = async (
  fetcher: AccountFetcher,
  authBaseUrl: string,
  authorizationCode: string,
  codeVerifier: string,
  signal?: AbortSignal,
): Promise<TokenSet> => {
  const baseUrl = trimBaseUrl(authBaseUrl);
  const form = new URLSearchParams({
    client_id: oauthClientId,
    code: authorizationCode,
    code_verifier: codeVerifier,
    grant_type: "authorization_code",
    redirect_uri: `${baseUrl}/deviceauth/callback`,
  });
  const response = await fetcher(`${baseUrl}/oauth/token`, {
    body: form.toString(),
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    method: "POST",
    ...(signal === undefined ? {} : { signal }),
  });
  if (!response.ok) {
    const body = await readTextLimited(response, maximumErrorBytes);
    throw new AccountUpstreamError(
      "device code exchange failed",
      response.status,
      body,
    );
  }
  const payload = asRecord(await readJsonLimited(response, maximumErrorBytes));
  if (payload === undefined) {
    throw new Error("token response is not an object");
  }
  return {
    accessToken: requiredString(payload.access_token, "access_token"),
    idToken: requiredString(payload.id_token, "id_token"),
    refreshToken: requiredString(payload.refresh_token, "refresh_token"),
  };
};

const accountFetch = (
  fetcher: AccountFetcher,
  url: string,
  credentials: AccountCredentials,
  method: "GET" | "POST",
  body?: string,
  signal?: AbortSignal,
): Promise<Response> =>
  fetcher(url, {
    ...(body === undefined ? {} : { body }),
    headers: {
      Accept: "application/json",
      Authorization: `Bearer ${credentials.accessToken}`,
      "chatgpt-account-id": credentials.accountId,
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    method,
    ...(signal === undefined ? {} : { signal }),
  });

const usageWindow = (
  value: unknown,
  observedAtMs: number,
): UsageWindowObservation | null => {
  const payload = asRecord(value);
  if (payload === undefined || typeof payload.used_percent !== "number") {
    return null;
  }
  const seconds = integer(payload.limit_window_seconds, "limit_window_seconds");
  const resetAt = integer(payload.reset_at, "reset_at");
  return {
    observedAtMs,
    resetsAtMs: resetAt > 0 ? resetAt * 1000 : null,
    usedPercent: payload.used_percent,
    windowMinutes: Math.trunc(seconds / 60),
  };
};

export const fetchUsage = async (
  fetcher: AccountFetcher,
  usageBaseUrl: string,
  credentials: AccountCredentials,
  nowMs: number,
  signal?: AbortSignal,
): Promise<UsageObservation> => {
  const response = await accountFetch(
    fetcher,
    `${trimBaseUrl(usageBaseUrl)}/usage`,
    credentials,
    "GET",
    undefined,
    signal,
  );
  if (!response.ok) {
    await response.body?.cancel();
    throw new AccountUpstreamError("usage request failed", response.status);
  }
  const payload = asRecord(await readJsonLimited(response, maximumErrorBytes));
  const rateLimit = asRecord(payload?.rate_limit);
  if (payload === undefined || rateLimit === undefined) {
    throw new Error("usage response omitted rate_limit");
  }
  const credits = asRecord(payload.rate_limit_reset_credits);
  const count = credits?.available_count;
  const limitReached = rateLimit.limit_reached;
  if (limitReached !== undefined && typeof limitReached !== "boolean") {
    throw new Error("usage response has invalid limit_reached");
  }
  return {
    bankedResets:
      count === undefined ? null : integer(count, "available_count"),
    limitReached: limitReached ?? null,
    observedAtMs: nowMs,
    primary: usageWindow(rateLimit.primary_window, nowMs),
    secondary: usageWindow(rateLimit.secondary_window, nowMs),
  };
};

export const fetchResetCredits = async (
  fetcher: AccountFetcher,
  usageBaseUrl: string,
  credentials: AccountCredentials,
  signal?: AbortSignal,
): Promise<ResetCredits> => {
  const response = await accountFetch(
    fetcher,
    `${trimBaseUrl(usageBaseUrl)}/rate-limit-reset-credits`,
    credentials,
    "GET",
    undefined,
    signal,
  );
  if (!response.ok) {
    await response.body?.cancel();
    throw new AccountUpstreamError(
      "reset credits request failed",
      response.status,
    );
  }
  const payload = asRecord(await readJsonLimited(response, maximumErrorBytes));
  if (payload === undefined || !Array.isArray(payload.credits)) {
    throw new Error("reset credits response is invalid");
  }
  const credits = payload.credits.map((value): ResetCredit => {
    const credit = asRecord(value);
    if (credit === undefined) {
      throw new Error("reset credit is not an object");
    }
    const expiresAt = optionalString(credit.expires_at);
    const expiresAtMs = expiresAt === undefined ? null : Date.parse(expiresAt);
    if (expiresAtMs !== null && !Number.isFinite(expiresAtMs)) {
      throw new Error("reset credit has invalid expires_at");
    }
    return {
      expiresAtMs,
      id: requiredString(credit.id, "credit id"),
      resetType: requiredString(credit.reset_type, "reset_type"),
      status: requiredString(credit.status, "status"),
    };
  });
  return {
    availableCount: integer(payload.available_count, "available_count"),
    credits,
  };
};

export const consumeResetCredit = async (
  fetcher: AccountFetcher,
  usageBaseUrl: string,
  credentials: AccountCredentials,
  creditId: string,
  signal?: AbortSignal,
): Promise<ResetCreditResult> => {
  const response = await accountFetch(
    fetcher,
    `${trimBaseUrl(usageBaseUrl)}/rate-limit-reset-credits/consume`,
    credentials,
    "POST",
    JSON.stringify({ credit_id: creditId, redeem_request_id: creditId }),
    signal,
  );
  if (!response.ok) {
    await response.body?.cancel();
    throw new AccountUpstreamError(
      "reset credit consume failed",
      response.status,
    );
  }
  const payload = asRecord(await readJsonLimited(response, maximumErrorBytes));
  if (payload === undefined) {
    throw new Error("reset credit response is not an object");
  }
  const code = requiredString(payload.code, "code");
  if (
    !["reset", "nothing_to_reset", "no_credit", "already_redeemed"].includes(
      code,
    )
  ) {
    throw new Error(`reset credit response has invalid code ${code}`);
  }
  return {
    code,
    windowsReset: integer(payload.windows_reset, "windows_reset"),
  };
};

export const fetchModels = async (
  fetcher: AccountFetcher,
  upstreamBaseUrl: string,
  credentials: AccountCredentials,
  clientVersion: string,
  signal?: AbortSignal,
): Promise<readonly Record<string, unknown>[]> => {
  const url = new URL(`${trimBaseUrl(upstreamBaseUrl)}/models`);
  url.searchParams.set("client_version", clientVersion);
  const response = await accountFetch(
    fetcher,
    url.href,
    credentials,
    "GET",
    undefined,
    signal,
  );
  if (!response.ok) {
    const body = await readTextLimited(response, maximumErrorBytes);
    throw new AccountUpstreamError(
      "models request failed",
      response.status,
      body,
    );
  }
  const payload = asRecord(await readJsonLimited(response, maximumModelBytes));
  if (payload === undefined || !Array.isArray(payload.models)) {
    throw new Error("models response omitted models");
  }
  return payload.models.flatMap((value) => {
    const model = asRecord(value);
    return model !== undefined &&
      typeof model.slug === "string" &&
      model.slug.trim() !== ""
      ? [model]
      : [];
  });
};
