import { describe, expect, it } from "vitest";

import type {
  ModelCatalog,
  ModelEntry,
  QuotaWindow,
  RoutingAccount,
  RoutingConstraints,
} from "../src/domain.js";
import {
  accountAvailableAt,
  accountPressure,
  accountStatusAt,
  allowedAccountIds,
  canonicalServiceTier,
  modelServiceTiers,
  modelSupportsServiceTier,
  roomierThan,
  routeAccount,
} from "../src/routing.js";

const nowMs = 1_000_000;

const quotaWindow = (
  usedPercent = 0,
  seenAtMs: number | null = nowMs,
): QuotaWindow => ({
  usedPercent,
  minutes: 300,
  resetsAtMs: nowMs + 60_000,
  seenAtMs,
});

const account = (
  id: string,
  usedPercent = 0,
  overrides: Partial<RoutingAccount> = {},
): RoutingAccount => ({
  id,
  paused: false,
  reauthReason: null,
  cooldownUntilMs: null,
  spent: false,
  primary: quotaWindow(usedPercent),
  secondary: quotaWindow(usedPercent),
  lastUsedAtMs: null,
  ...overrides,
});

const constraints = (
  overrides: Partial<RoutingConstraints> = {},
): RoutingConstraints => ({
  requiredId: null,
  preferredId: null,
  skippedIds: new Set(),
  allowedIds: null,
  ...overrides,
});

const model = (slug: string, ...serviceTiers: string[]): ModelEntry => ({
  slug,
  ...(serviceTiers.length === 0
    ? {}
    : {
        service_tiers: serviceTiers.map((id) => ({ id })),
      }),
});

describe("account routing state", () => {
  it("uses the highest observed quota pressure", () => {
    const candidate = account("account-a", 0, {
      primary: quotaWindow(18),
      secondary: quotaWindow(73),
    });

    expect(accountPressure(candidate)).toBe(73);
  });

  it.each([
    {
      name: "paused before every other state",
      overrides: {
        paused: true,
        reauthReason: "invalid_grant",
        spent: true,
        primary: quotaWindow(0, null),
        secondary: quotaWindow(0, null),
      },
      expected: "paused",
    },
    {
      name: "reauth before cooling or checking",
      overrides: {
        reauthReason: "invalid_grant",
        spent: true,
        primary: quotaWindow(0, null),
        secondary: quotaWindow(0, null),
      },
      expected: "needs_reauth",
    },
    {
      name: "spent",
      overrides: { spent: true },
      expected: "cooling",
    },
    {
      name: "future cooldown",
      overrides: { cooldownUntilMs: nowMs + 1 },
      expected: "cooling",
    },
    {
      name: "unknown quota",
      overrides: {
        primary: quotaWindow(0, null),
        secondary: quotaWindow(0, null),
      },
      expected: "checking",
    },
    {
      name: "known quota",
      overrides: {},
      expected: "live",
    },
  ])("reports $name", ({ overrides, expected }) => {
    expect(accountStatusAt(account("account-a", 0, overrides), nowMs)).toBe(
      expected,
    );
  });

  it.each([
    { name: "live", overrides: {}, expected: true },
    { name: "paused", overrides: { paused: true }, expected: false },
    {
      name: "needs reauth",
      overrides: { reauthReason: "invalid_grant" },
      expected: false,
    },
    { name: "spent", overrides: { spent: true }, expected: false },
    {
      name: "cooling",
      overrides: { cooldownUntilMs: nowMs + 1 },
      expected: false,
    },
    {
      name: "unknown quota",
      overrides: {
        primary: quotaWindow(0, null),
        secondary: quotaWindow(0, null),
      },
      expected: false,
    },
  ])("marks $name availability", ({ overrides, expected }) => {
    expect(accountAvailableAt(account("account-a", 0, overrides), nowMs)).toBe(
      expected,
    );
  });

  it("keeps the exact cooldown boundary unavailable after status turns live", () => {
    const candidate = account("account-a", 0, {
      cooldownUntilMs: nowMs,
    });

    expect(accountStatusAt(candidate, nowMs)).toBe("live");
    expect(accountAvailableAt(candidate, nowMs)).toBe(false);
    expect(accountAvailableAt(candidate, nowMs + 1)).toBe(true);
  });
});

describe("account selection", () => {
  const accountA = account("account-a", 80, {
    lastUsedAtMs: nowMs - 60_000,
  });
  const accountB = account("account-b", 10, {
    lastUsedAtMs: nowMs - 1_000,
  });
  const accounts = [accountA, accountB];

  it("honors hard and soft owners", () => {
    expect(routeAccount(accounts, constraints(), nowMs).account?.id).toBe(
      "account-b",
    );
    expect(
      routeAccount(accounts, constraints({ requiredId: "account-a" }), nowMs)
        .account?.id,
    ).toBe("account-a");
    expect(
      routeAccount(accounts, constraints({ preferredId: "account-a" }), nowMs)
        .account?.id,
    ).toBe("account-a");
  });

  it("fails hard ownership and spills soft ownership", () => {
    const unavailableOverrides: readonly Partial<RoutingAccount>[] = [
      { spent: true },
      { cooldownUntilMs: nowMs + 1 },
      { paused: true },
      { reauthReason: "invalid_grant" },
      {
        primary: quotaWindow(0, null),
        secondary: quotaWindow(0, null),
      },
    ];

    for (const overrides of unavailableOverrides) {
      const unavailable = [account("account-a", 80, overrides), accountB];

      expect(
        routeAccount(
          unavailable,
          constraints({ requiredId: "account-a" }),
          nowMs,
        ).account,
      ).toBeNull();
      expect(
        routeAccount(
          unavailable,
          constraints({ preferredId: "account-a" }),
          nowMs,
        ).account?.id,
      ).toBe("account-b");
    }
    expect(
      routeAccount(accounts, constraints({ requiredId: "missing" }), nowMs)
        .account,
    ).toBeNull();
    expect(
      routeAccount(accounts, constraints({ preferredId: "missing" }), nowMs)
        .account?.id,
    ).toBe("account-b");
  });

  it("does not retry skipped owners", () => {
    const skippedIds = new Set(["account-a"]);

    expect(
      routeAccount(
        accounts,
        constraints({ requiredId: "account-a", skippedIds }),
        nowMs,
      ).account,
    ).toBeNull();
    expect(
      routeAccount(
        accounts,
        constraints({ preferredId: "account-a", skippedIds }),
        nowMs,
      ).account?.id,
    ).toBe("account-b");
  });

  it("distinguishes no model filter from an empty allowlist", () => {
    expect(
      routeAccount(
        accounts,
        constraints({ skippedIds: new Set(), allowedIds: null }),
        nowMs,
      ).account?.id,
    ).toBe("account-b");
    expect(
      routeAccount(
        accounts,
        constraints({ skippedIds: new Set(), allowedIds: new Set() }),
        nowMs,
      ).account,
    ).toBeNull();
    expect(
      routeAccount(
        accounts,
        constraints({
          preferredId: "account-b",
          allowedIds: new Set(["account-a"]),
        }),
        nowMs,
      ).account?.id,
    ).toBe("account-a");
    expect(
      routeAccount(
        accounts,
        constraints({
          requiredId: "account-b",
          allowedIds: new Set(["account-a"]),
        }),
        nowMs,
      ).account,
    ).toBeNull();
  });

  it("returns the full candidate snapshot in source order", () => {
    const decision = routeAccount(accounts, constraints(), nowMs);

    expect(decision.candidates).toEqual(accounts);
    expect(decision.nowMs).toBe(nowMs);
  });
});

describe("roominess ordering", () => {
  it("uses pressure only when the difference exceeds one point", () => {
    const lowerPressure = account("account-b", 10, {
      lastUsedAtMs: nowMs,
    });
    const older = account("account-a", 11.000_001, {
      lastUsedAtMs: nowMs - 100_000,
    });

    expect(roomierThan(lowerPressure, older)).toBe(true);
    expect(roomierThan(older, lowerPressure)).toBe(false);
  });

  it("uses least recently used and then id within one point", () => {
    const recentLowPressure = account("account-b", 10, {
      lastUsedAtMs: nowMs,
    });
    const olderHighPressure = account("account-a", 11, {
      lastUsedAtMs: nowMs - 1,
    });
    const sameUseHigherId = account("account-z", 10.5, {
      lastUsedAtMs: nowMs - 1,
    });

    expect(roomierThan(olderHighPressure, recentLowPressure)).toBe(true);
    expect(roomierThan(recentLowPressure, olderHighPressure)).toBe(false);
    expect(roomierThan(olderHighPressure, sameUseHigherId)).toBe(true);
    expect(roomierThan(sameUseHigherId, olderHighPressure)).toBe(false);
  });

  it("treats a missing last-use time as the oldest", () => {
    const neverUsed = account("account-z", 10, { lastUsedAtMs: null });
    const used = account("account-a", 10, { lastUsedAtMs: 1 });

    expect(roomierThan(neverUsed, used)).toBe(true);
  });

  it("preserves sequential fold order for the non-transitive comparison", () => {
    const highOld = account("high-old", 1.6, { lastUsedAtMs: 100 });
    const middle = account("middle", 0.8, { lastUsedAtMs: 200 });
    const lowRecent = account("low-recent", 0, { lastUsedAtMs: 300 });

    expect(
      routeAccount([highOld, middle, lowRecent], constraints(), nowMs).account
        ?.id,
    ).toBe("low-recent");
    expect(
      routeAccount([lowRecent, middle, highOld], constraints(), nowMs).account
        ?.id,
    ).toBe("high-old");
  });
});

describe("model routing", () => {
  const accountA = account("account-a");
  const accountB = account("account-b");
  const accounts = [accountA, accountB];
  const accountAModels = new Map([["gpt-5.6-terra", model("gpt-5.6-terra")]]);
  const accountBModels = new Map([
    ["gpt-5.6-sol", model("gpt-5.6-sol", "priority", "default")],
  ]);
  const catalog: ModelCatalog = new Map([
    ["account-a", accountAModels],
    ["account-b", accountBModels],
  ]);

  it("filters only after every available account has catalog coverage", () => {
    expect(
      allowedAccountIds(catalog, accounts, " GPT-5.6-SOL ", "", nowMs),
    ).toEqual(new Set(["account-b"]));

    const incomplete: ModelCatalog = new Map([["account-a", accountAModels]]);

    expect(
      allowedAccountIds(incomplete, accounts, "gpt-5.6-sol", "", nowMs),
    ).toBeNull();
  });

  it("uses availability only to decide whether coverage is complete", () => {
    const incomplete: ModelCatalog = new Map([["account-a", accountAModels]]);
    const withUnavailableAccount = [
      accountA,
      account("account-b", 0, {
        reauthReason: "invalid_grant",
      }),
    ];

    expect(
      allowedAccountIds(
        incomplete,
        withUnavailableAccount,
        "gpt-5.6-terra",
        "",
        nowMs,
      ),
    ).toEqual(new Set(["account-a"]));
    expect(
      allowedAccountIds(
        catalog,
        withUnavailableAccount,
        "gpt-5.6-sol",
        "",
        nowMs,
      ),
    ).toEqual(new Set(["account-b"]));
  });

  it("returns no filter when the request has no model", () => {
    expect(
      allowedAccountIds(new Map(), accounts, "  ", "priority", nowMs),
    ).toBeNull();
  });

  it("returns an empty allowlist for a known unsupported model", () => {
    expect(allowedAccountIds(catalog, accounts, "missing", "", nowMs)).toEqual(
      new Set(),
    );
  });

  it.each([
    ["", null],
    [" auto ", null],
    ["DEFAULT", null],
    ["fast", "priority"],
    [" Priority ", "priority"],
    ["flex", "flex"],
  ])("canonicalizes service tier %j as %j", (input, expected) => {
    expect(canonicalServiceTier(input)).toBe(expected);
  });

  it("filters explicit service tiers and treats default tiers as unfiltered", () => {
    for (const tier of ["priority", "fast"]) {
      expect(
        allowedAccountIds(catalog, accounts, "gpt-5.6-sol", tier, nowMs),
      ).toEqual(new Set(["account-b"]));
    }
    for (const tier of ["", "auto", "default"]) {
      expect(
        allowedAccountIds(catalog, accounts, "gpt-5.6-sol", tier, nowMs),
      ).toEqual(new Set(["account-b"]));
    }
    expect(
      allowedAccountIds(catalog, accounts, "gpt-5.6-sol", "flex", nowMs),
    ).toEqual(new Set());
  });

  it("reads, normalizes, and checks declared model service tiers", () => {
    const entry = model("gpt-5.6-sol", " FAST ", "default", "Priority");

    expect(modelServiceTiers(entry)).toEqual(["priority", "priority"]);
    expect(modelSupportsServiceTier(entry, "priority")).toBe(true);
    expect(modelSupportsServiceTier(entry, "fast")).toBe(false);
    expect(modelSupportsServiceTier(entry, "default")).toBe(false);
  });
});
