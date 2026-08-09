import { describe, expect, it } from "vitest";

import type {
  AffinityBinding,
  AffinityKind,
  AffinityRef,
  RequestAffinity,
  RoutingAccount,
} from "../src/domain.js";
import {
  affinityOwnerAbandonable,
  affinityStorageKey,
  extractRequestAffinity,
  resolveAffinity,
  shouldAbandonAffinity,
} from "../src/affinity.js";

const nowMs = Date.UTC(2026, 7, 9, 12);
const hourMs = 60 * 60 * 1_000;

const ref = (kind: AffinityKind, value: string): AffinityRef => ({
  kind,
  value,
});

const account = (
  id: string,
  overrides: Partial<RoutingAccount> = {},
): RoutingAccount => ({
  id,
  paused: false,
  reauthReason: null,
  cooldownUntilMs: null,
  spent: false,
  primary: {
    usedPercent: 0,
    minutes: 300,
    resetsAtMs: null,
    seenAtMs: nowMs,
  },
  secondary: {
    usedPercent: 0,
    minutes: 10_080,
    resetsAtMs: null,
    seenAtMs: nowMs,
  },
  lastUsedAtMs: null,
  ...overrides,
});

const binding = (accountId: string, lastUsedAtMs = nowMs): AffinityBinding => ({
  accountId,
  createdAtMs: lastUsedAtMs,
  lastUsedAtMs,
  abandonedAtMs: null,
});

const bindingMap = (
  ...entries: readonly (readonly [AffinityRef, AffinityBinding])[]
): ReadonlyMap<string, AffinityBinding> =>
  new Map(
    entries.map(([affinity, owner]) => [affinityStorageKey(affinity), owner]),
  );

const request = (
  preferred: AffinityRef | null,
  hard: readonly AffinityRef[] = [],
  requireUnambiguous = false,
): RequestAffinity => ({ preferred, hard, requireUnambiguous });

describe("affinity extraction", () => {
  it("uses body turn state before the header and drops soft affinity", () => {
    const headers = new Headers({
      "x-codex-turn-state": "header-turn",
      session_id: "session",
    });

    expect(
      extractRequestAffinity(headers, {
        client_metadata: { "x-codex-turn-state": " body-turn " },
        prompt_cache_key: "cache",
        previous_response_id: " response ",
        conversation: " conversation ",
        input: [
          {
            role: "user",
            content: [
              { type: "input_file", file_id: " file-a " },
              { type: "input_file", file_id: "file-b" },
            ],
          },
        ],
      }),
    ).toEqual({
      preferred: null,
      hard: [
        ref("turn_state", "body-turn"),
        ref("response", "response"),
        ref("conversation", "conversation"),
        ref("file", "file-a"),
        ref("file", "file-b"),
      ],
      requireUnambiguous: true,
    });
  });

  it("uses the turn header before session and prompt cache affinity", () => {
    expect(
      extractRequestAffinity(
        new Headers({
          "x-codex-turn-state": " turn ",
          session_id: "session",
        }),
        { prompt_cache_key: "cache", input: [] },
      ),
    ).toEqual(request(null, [ref("turn_state", "turn")]));
  });

  it("honors every session header in Go precedence order", () => {
    const names = [
      "session_id",
      "session-id",
      "x-codex-session-id",
      "x-codex-conversation-id",
      "thread-id",
    ] as const;

    for (const [index, expectedName] of names.entries()) {
      const headers = new Headers(
        names.slice(index).map((name) => [name, ` ${name}-value `]),
      );

      expect(
        extractRequestAffinity(headers, {
          prompt_cache_key: "cache",
          input: [],
        }),
      ).toEqual(request(ref("session", `${expectedName}-value`)));
    }
  });

  it("uses snake-case prompt cache before camel-case prompt cache", () => {
    expect(
      extractRequestAffinity(new Headers(), {
        prompt_cache_key: " snake ",
        promptCacheKey: "camel",
        input: [],
      }),
    ).toEqual(request(ref("prompt_cache", "snake")));

    expect(
      extractRequestAffinity(new Headers(), {
        prompt_cache_key: " ",
        promptCacheKey: " camel ",
        input: [],
      }),
    ).toEqual(request(ref("prompt_cache", "camel")));
  });

  it("finds nested input files and preserves repeated evidence", () => {
    expect(
      extractRequestAffinity(new Headers(), {
        input: [
          {
            content: [
              { type: "input_file", file_id: "file-a" },
              {
                nested: [
                  { type: "input_file", file_id: "file-b" },
                  { type: "input_file", file_id: "file-a" },
                ],
              },
            ],
          },
        ],
      }),
    ).toEqual(
      request(null, [
        ref("file", "file-a"),
        ref("file", "file-b"),
        ref("file", "file-a"),
      ]),
    );
  });

  it("requires an owner for non-string conversations only", () => {
    expect(
      extractRequestAffinity(new Headers(), { conversation: 42, input: [] }),
    ).toEqual(request(null, [], true));
    expect(
      extractRequestAffinity(new Headers(), { conversation: "  ", input: [] }),
    ).toEqual(request(null));
    expect(
      extractRequestAffinity(new Headers(), { conversation: null, input: [] }),
    ).toEqual(request(null));
  });
});

describe("affinity resolution", () => {
  const accountA = account("account-a");
  const accountB = account("account-b");
  const accounts = [accountA, accountB];
  const session = ref("session", "session");
  const turn = ref("turn_state", "turn");
  const response = ref("response", "response");

  it("resolves soft ownership without making it required", () => {
    expect(
      resolveAffinity(
        request(session),
        bindingMap([session, binding(accountA.id)]),
        accounts,
        nowMs,
      ),
    ).toEqual({
      ok: true,
      resolution: {
        requiredAccountId: null,
        preferredAccountId: accountA.id,
        bindings: [session],
        hard: false,
      },
      abandonments: [],
    });
  });

  it("uses the hard owner and drops the soft binding", () => {
    expect(
      resolveAffinity(
        request(session, [response]),
        bindingMap(
          [session, binding(accountA.id)],
          [response, binding(accountB.id)],
        ),
        accounts,
        nowMs,
      ),
    ).toEqual({
      ok: true,
      resolution: {
        requiredAccountId: accountB.id,
        preferredAccountId: accountA.id,
        bindings: [response],
        hard: true,
      },
      abandonments: [],
    });
  });

  it("rejects hard references owned by different accounts", () => {
    expect(
      resolveAffinity(
        request(null, [turn, response]),
        bindingMap(
          [turn, binding(accountA.id)],
          [response, binding(accountB.id)],
        ),
        accounts,
        nowMs,
      ),
    ).toEqual({ ok: false, failure: "conflict", abandonments: [] });
  });

  it("rejects a missing previous response", () => {
    expect(
      resolveAffinity(
        request(null, [ref("response", "missing")]),
        bindingMap(),
        accounts,
        nowMs,
      ),
    ).toEqual({
      ok: false,
      failure: "owner_unavailable",
      abandonments: [],
    });
  });

  it("drops unknown files but rejects partial file ownership", () => {
    const known = ref("file", "known");
    const unknown = ref("file", "unknown");

    expect(
      resolveAffinity(request(null, [unknown]), bindingMap(), accounts, nowMs),
    ).toEqual({
      ok: true,
      resolution: {
        requiredAccountId: null,
        preferredAccountId: null,
        bindings: [],
        hard: false,
      },
      abandonments: [],
    });
    expect(
      resolveAffinity(
        request(null, [known, unknown]),
        bindingMap([known, binding(accountA.id)]),
        accounts,
        nowMs,
      ),
    ).toEqual({
      ok: false,
      failure: "owner_unavailable",
      abandonments: [],
    });
  });

  it("requires the sole account for an unknown turn", () => {
    const unknown = ref("turn_state", "unknown");

    expect(
      resolveAffinity(request(null, [unknown]), bindingMap(), accounts, nowMs),
    ).toEqual({
      ok: false,
      failure: "owner_unavailable",
      abandonments: [],
    });
    expect(
      resolveAffinity(
        request(null, [unknown]),
        bindingMap(),
        [accountA],
        nowMs,
      ),
    ).toEqual({
      ok: true,
      resolution: {
        requiredAccountId: accountA.id,
        preferredAccountId: null,
        bindings: [unknown],
        hard: true,
      },
      abandonments: [],
    });
  });

  it("keeps an unknown conversation ambiguous despite another hard owner", () => {
    const conversation = ref("conversation", "unknown");
    const file = ref("file", "owned");

    expect(
      resolveAffinity(
        request(null, [conversation, file], true),
        bindingMap([file, binding(accountA.id)]),
        accounts,
        nowMs,
      ),
    ).toEqual({ ok: false, failure: "ambiguous", abandonments: [] });
  });

  it("uses a known turn or the sole account for an unknown conversation", () => {
    const conversation = ref("conversation", "unknown");

    expect(
      resolveAffinity(
        request(null, [turn, conversation], true),
        bindingMap([turn, binding(accountA.id)]),
        accounts,
        nowMs,
      ),
    ).toEqual({
      ok: true,
      resolution: {
        requiredAccountId: accountA.id,
        preferredAccountId: null,
        bindings: [turn, conversation],
        hard: true,
      },
      abandonments: [],
    });
    expect(
      resolveAffinity(
        request(null, [conversation], true),
        bindingMap(),
        [accountA],
        nowMs,
      ),
    ).toEqual({
      ok: true,
      resolution: {
        requiredAccountId: accountA.id,
        preferredAccountId: null,
        bindings: [conversation],
        hard: true,
      },
      abandonments: [],
    });
  });

  it("abandons stale removed turn owners but not response owners", () => {
    const removedTurn = ref("turn_state", "removed-turn");
    const removedResponse = ref("response", "removed-response");
    const stale = binding("removed-account", nowMs - 2 * hourMs);

    expect(
      resolveAffinity(
        request(null, [removedTurn]),
        bindingMap([removedTurn, stale]),
        accounts,
        nowMs,
      ),
    ).toEqual({
      ok: true,
      resolution: {
        requiredAccountId: null,
        preferredAccountId: null,
        bindings: [removedTurn],
        hard: true,
      },
      abandonments: [
        {
          ref: removedTurn,
          accountId: "removed-account",
          lastUsedAtMs: stale.lastUsedAtMs,
        },
      ],
    });
    expect(
      resolveAffinity(
        request(null, [removedResponse]),
        bindingMap([removedResponse, stale]),
        accounts,
        nowMs,
      ),
    ).toEqual({
      ok: false,
      failure: "owner_unavailable",
      abandonments: [],
    });
  });

  it("carries a stale abandonment plan through a later failure", () => {
    const staleTurn = ref("turn_state", "stale-turn");
    const missingResponse = ref("response", "missing-response");
    const stale = binding("removed-account", nowMs - 2 * hourMs);

    expect(
      resolveAffinity(
        request(null, [staleTurn, missingResponse]),
        bindingMap([staleTurn, stale]),
        accounts,
        nowMs,
      ),
    ).toEqual({
      ok: false,
      failure: "owner_unavailable",
      abandonments: [
        {
          ref: staleTurn,
          accountId: "removed-account",
          lastUsedAtMs: stale.lastUsedAtMs,
        },
      ],
    });
  });
});

describe("affinity abandonment", () => {
  const turn = ref("turn_state", "turn");
  const response = ref("response", "response");
  const stale = binding("account-a", nowMs - hourMs);

  it("abandons only stale turn and conversation bindings", () => {
    expect(shouldAbandonAffinity(turn, stale, null, nowMs)).toBe(true);
    expect(
      shouldAbandonAffinity(
        ref("conversation", "conversation"),
        stale,
        null,
        nowMs,
      ),
    ).toBe(true);
    expect(shouldAbandonAffinity(response, stale, null, nowMs)).toBe(false);
    expect(
      shouldAbandonAffinity(
        turn,
        binding("account-a", nowMs - hourMs + 1),
        null,
        nowMs,
      ),
    ).toBe(false);
  });

  it("abandons paused, broken, and fully reset spent owners", () => {
    expect(
      affinityOwnerAbandonable(account("paused", { paused: true }), nowMs),
    ).toBe(true);
    expect(
      affinityOwnerAbandonable(
        account("broken", { reauthReason: "refresh failed" }),
        nowMs,
      ),
    ).toBe(true);
    expect(
      affinityOwnerAbandonable(
        account("spent", {
          spent: true,
          primary: {
            usedPercent: 100,
            minutes: 300,
            resetsAtMs: nowMs,
            seenAtMs: nowMs,
          },
          secondary: {
            usedPercent: 100,
            minutes: 10_080,
            resetsAtMs: nowMs - 1,
            seenAtMs: nowMs,
          },
        }),
        nowMs,
      ),
    ).toBe(true);
  });

  it("keeps active and not-yet-reset spent owners", () => {
    expect(affinityOwnerAbandonable(account("active"), nowMs)).toBe(false);
    expect(
      affinityOwnerAbandonable(
        account("spent", {
          spent: true,
          primary: {
            usedPercent: 100,
            minutes: 300,
            resetsAtMs: nowMs + 1,
            seenAtMs: nowMs,
          },
        }),
        nowMs,
      ),
    ).toBe(false);
  });
});
