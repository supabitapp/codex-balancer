import type {
  AccountGrant,
  AccountId,
  AffinityFailure,
  AffinityRef,
  ResponseUsage,
  RouteFailureKind,
  SelectAccountInput,
  TransportKind,
} from "./domain.js";

export type SelectAccountResult =
  | Readonly<{ grant: AccountGrant; ok: true }>
  | Readonly<{
      failure: AffinityFailure | "no_account";
      ok: false;
    }>;

export type RefreshAccountResult =
  Readonly<{ accessToken: string; ok: true }> | Readonly<{ ok: false }>;

type RateLimitHeaders = Readonly<Record<string, string>>;

export interface AccountObservation {
  readonly accountId: AccountId;
  readonly headers: RateLimitHeaders;
}

export interface AccountFailure {
  readonly accountId: AccountId;
  readonly attempt: number;
  readonly failedOver: boolean;
  readonly headers: RateLimitHeaders;
  readonly kind: RouteFailureKind;
}

export interface RecordedRoute {
  readonly accountId: AccountId;
  readonly bindings: readonly AffinityRef[];
  readonly counted: boolean;
  readonly headers: RateLimitHeaders;
  readonly transport: TransportKind;
  readonly turnState: string | null;
}

export interface TransportPort {
  answered(latencyMs: number): Promise<void>;
  claimResponseId(accountId: AccountId, responseId: string): Promise<void>;
  observeAccount(observation: AccountObservation): Promise<void>;
  recordFailure(failure: AccountFailure): Promise<void>;
  recordRoute(outcome: RecordedRoute): Promise<void>;
  recordUsage(usage: ResponseUsage): Promise<void>;
  refreshAccount(
    accountId: AccountId,
    rejectedAccessToken: string,
  ): Promise<RefreshAccountResult>;
  selectAccount(input: SelectAccountInput): Promise<SelectAccountResult>;
  websocketClosed(accountId: AccountId): Promise<void>;
  websocketOpened(accountId: AccountId): Promise<void>;
}

export const ignoreFailure = async (
  work: () => void | Promise<void>,
): Promise<void> => {
  try {
    await work();
  } catch {
    return;
  }
};

export const maxUpstreamRetries = 3;

const upstreamRetryBudgetMs = 5_000;
const upstreamWaitMs = 90_000;

export const upstreamRequestSignal = (signal: AbortSignal): AbortSignal =>
  AbortSignal.any([signal, AbortSignal.timeout(upstreamWaitMs)]);

export const upstreamRetryBackoffMs = (retry: number): number =>
  (upstreamRetryBudgetMs * 2 ** (retry - 1)) / (2 ** maxUpstreamRetries - 1);

export const upstreamRetryDelayMs = (
  retry: number,
  random = Math.random,
): number => upstreamRetryBackoffMs(retry) * (0.9 + random() * 0.2);

export const sleepWithAbort = async (
  delayMs: number,
  signal: AbortSignal,
): Promise<void> => {
  await new Promise<void>((resolve, reject) => {
    const rejection = (): Error =>
      signal.reason instanceof Error
        ? signal.reason
        : new Error("request aborted");
    if (signal.aborted) {
      reject(rejection());
      return;
    }
    const completed = (): void => {
      signal.removeEventListener("abort", aborted);
      resolve();
    };
    const aborted = (): void => {
      clearTimeout(timeout);
      reject(rejection());
    };
    const timeout = setTimeout(completed, delayMs);
    signal.addEventListener("abort", aborted, { once: true });
  });
};

export const observedHeaders = (headers: Headers): RateLimitHeaders => {
  const observed: Record<string, string> = {};
  for (const name of [
    "retry-after",
    "x-codex-primary-reset-at",
    "x-codex-primary-used-percent",
    "x-codex-primary-window-minutes",
    "x-codex-secondary-primary-reset-at",
    "x-codex-secondary-primary-used-percent",
    "x-codex-secondary-primary-window-minutes",
  ]) {
    const value = headers.get(name);
    if (value !== null) {
      observed[name] = value;
    }
  }
  return observed;
};
