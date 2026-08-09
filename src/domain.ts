export type AccountId = string;

export type AccountStatus =
  "live" | "checking" | "cooling" | "paused" | "needs_reauth";

export interface QuotaWindow {
  readonly usedPercent: number;
  readonly minutes: number;
  readonly resetsAtMs: number | null;
  readonly seenAtMs: number | null;
}

export interface RoutingAccount {
  readonly id: AccountId;
  readonly paused: boolean;
  readonly reauthReason: string | null;
  readonly cooldownUntilMs: number | null;
  readonly spent: boolean;
  readonly primary: QuotaWindow;
  readonly secondary: QuotaWindow;
  readonly lastUsedAtMs: number | null;
}

export interface RoutingConstraints {
  readonly requiredId: AccountId | null;
  readonly preferredId: AccountId | null;
  readonly skippedIds: ReadonlySet<AccountId>;
  readonly allowedIds: ReadonlySet<AccountId> | null;
}

export interface RoutingDecision {
  readonly account: RoutingAccount | null;
  readonly candidates: readonly RoutingAccount[];
  readonly nowMs: number;
}

export type AffinityKind =
  | "session"
  | "prompt_cache"
  | "turn_state"
  | "response"
  | "conversation"
  | "file";

export interface AffinityRef {
  readonly kind: AffinityKind;
  readonly value: string;
}

export interface RequestAffinity {
  readonly preferred: AffinityRef | null;
  readonly hard: readonly AffinityRef[];
  readonly requireUnambiguous: boolean;
}

export interface AffinityBinding {
  readonly accountId: AccountId;
  readonly createdAtMs: number;
  readonly lastUsedAtMs: number;
  readonly abandonedAtMs: number | null;
}

export interface AffinityResolution {
  readonly requiredAccountId: AccountId | null;
  readonly preferredAccountId: AccountId | null;
  readonly bindings: readonly AffinityRef[];
  readonly hard: boolean;
}

export type AffinityFailure = "conflict" | "owner_unavailable" | "ambiguous";

export interface AffinityAbandonment {
  readonly ref: AffinityRef;
  readonly accountId: AccountId;
  readonly lastUsedAtMs: number;
}

export type AffinityResolutionResult =
  | Readonly<{
      ok: true;
      resolution: AffinityResolution;
      abandonments: readonly AffinityAbandonment[];
    }>
  | Readonly<{
      ok: false;
      failure: AffinityFailure;
      abandonments: readonly AffinityAbandonment[];
    }>;

export type ModelEntry = Readonly<Record<string, unknown>>;

export type ModelCatalog = ReadonlyMap<
  AccountId,
  ReadonlyMap<string, ModelEntry>
>;

export type TransportKind = "http" | "websocket";

export interface SelectAccountInput {
  readonly affinity: RequestAffinity;
  readonly model: string;
  readonly requiredAccountId: AccountId | null;
  readonly serviceTier: string;
  readonly skipAccountIds: readonly AccountId[];
  readonly attempt: number;
  readonly transport: TransportKind;
}

export interface AccountGrant {
  readonly accountId: AccountId;
  readonly accessToken: string;
  readonly resolution: AffinityResolution;
}

interface InputTokenDetails {
  readonly cachedTokens: number;
  readonly cacheWriteTokens: number;
}

export interface ResponseUsage {
  readonly inputTokens: number;
  readonly outputTokens: number;
  readonly inputTokensDetails: InputTokenDetails;
}

export type RouteFailureKind =
  | "unreachable"
  | "unauthorized"
  | "rate_limited"
  | "server_failure"
  | "model_unsupported"
  | "empty_response"
  | "disconnected"
  | "invalid_handshake";

export interface DashboardTotals {
  readonly averageFirstByteMilliseconds: number | null;
  readonly cacheWriteInputTokens: number;
  readonly turns: number;
  readonly websocketTurns: number;
  readonly failovers: number;
  readonly rateLimits: number;
  readonly inputTokens: number;
  readonly cachedInputTokens: number;
  readonly outputTokens: number;
}

interface DashboardAccount {
  readonly alias: string;
  readonly plan: string;
  readonly status: AccountStatus;
  readonly weeklyRemainingPercent: number | null;
  readonly bankedResets: number | null;
  readonly resetAt: string | null;
  readonly turns: number;
  readonly openWebSockets: number;
  readonly rateLimits: number;
}

export interface DashboardEvent {
  readonly at: string;
  readonly kind: string;
  readonly accountAlias: string;
  readonly detail: string;
}

export interface DashboardSnapshot {
  readonly updatedAt: string;
  readonly totals: DashboardTotals;
  readonly accounts: readonly DashboardAccount[];
  readonly events: readonly DashboardEvent[];
}

interface AdminAccount {
  readonly id: AccountId;
  readonly email: string;
  readonly plan: string;
  readonly paused: boolean;
  readonly status: AccountStatus;
  readonly weeklyRemainingPercent: number | null;
  readonly bankedResets: number | null;
  readonly resetAt: string | null;
}

interface AdminInvite {
  readonly id: string;
  readonly expiresAt: string;
  readonly usedAt: string | null;
}

export interface AdminSnapshot {
  readonly accounts: readonly AdminAccount[];
  readonly invites: readonly AdminInvite[];
}

export interface CreateInviteInput {
  readonly expiresInSeconds?: number;
}

export interface CreateInviteResult {
  readonly url: string;
  readonly expiresAt: string;
}

export interface UpdateAccountInput {
  readonly paused: boolean;
}

type OnboardingStatus = "ready" | "pending" | "complete" | "failed" | "expired";

export interface OnboardingSnapshot {
  readonly status: OnboardingStatus;
  readonly expiresAt?: string;
  readonly verificationUrl?: string;
  readonly userCode?: string;
  readonly error?: string;
}

export interface InviteInspection {
  readonly expiresAt: string;
}
