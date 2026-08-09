import { describe, expect, it } from "vitest";

import {
  consumeResetCredit,
  exchangeDeviceCode,
  fetchModels,
  fetchResetCredits,
  fetchUsage,
  pollDeviceAuthorization,
  RefreshTokenError,
  refreshTokens,
  requestDeviceAuthorization,
  type AccountFetcher,
} from "../src/account-upstream.js";

interface FetchCall {
  readonly init: RequestInit | undefined;
  readonly url: string;
}

const queuedFetcher = (...responses: Response[]) => {
  const calls: FetchCall[] = [];
  const fetcher: AccountFetcher = (input, init) => {
    calls.push({ init, url: input.toString() });
    const response = responses.shift();
    if (response === undefined) {
      return Promise.reject(new Error("unexpected fetch"));
    }
    return Promise.resolve(response);
  };
  return { calls, fetcher };
};

const requestBody = (call: FetchCall): string => {
  const body = call.init?.body;
  if (typeof body !== "string") {
    throw new Error("request body is not a string");
  }
  return body;
};

const fetchCall = (calls: readonly FetchCall[], index: number): FetchCall => {
  const call = calls[index];
  if (call === undefined) {
    throw new Error(`fetch call ${String(index)} is missing`);
  }
  return call;
};

const credentials = {
  accessToken: "access-token",
  accountId: "account-1",
};

describe("token exchange", () => {
  it("refreshes rotating tokens", async () => {
    const { calls, fetcher } = queuedFetcher(
      Response.json({
        access_token: "new-access",
        id_token: "new-id",
        refresh_token: "new-refresh",
      }),
    );

    await expect(
      refreshTokens(fetcher, "https://auth.example/", "old-refresh"),
    ).resolves.toEqual({
      accessToken: "new-access",
      idToken: "new-id",
      refreshToken: "new-refresh",
    });
    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toBe("https://auth.example/oauth/token");
    expect(JSON.parse(requestBody(fetchCall(calls, 0)))).toEqual({
      client_id: "app_EMoamEEZ73f0CkXaXp7hrann",
      grant_type: "refresh_token",
      refresh_token: "old-refresh",
    });
  });

  it("classifies permanent refresh failures", async () => {
    const { fetcher } = queuedFetcher(
      new Response('{"error":"refresh_token_reused"}', { status: 400 }),
    );

    const error = await refreshTokens(
      fetcher,
      "https://auth.example",
      "old-refresh",
    ).catch((failure: unknown) => failure);

    expect(error).toBeInstanceOf(RefreshTokenError);
    expect(error).toMatchObject({
      permanent: true,
      reason: "refresh_token_reused",
      status: 400,
    });
  });
});

describe("device authorization", () => {
  it("requests, polls, and exchanges one device login", async () => {
    const nowMs = Date.UTC(2026, 7, 9, 12);
    const { calls, fetcher } = queuedFetcher(
      Response.json({
        device_auth_id: "device-1",
        interval: "5",
        user_code: "ABCD-EFGH",
      }),
      new Response(null, { status: 403 }),
      Response.json({
        authorization_code: "authorization-code",
        code_verifier: "code-verifier",
      }),
      Response.json({
        access_token: "access-token",
        id_token: "id-token",
        refresh_token: "refresh-token",
      }),
    );

    await expect(
      requestDeviceAuthorization(fetcher, "https://auth.example", nowMs),
    ).resolves.toEqual({
      deviceAuthId: "device-1",
      expiresAtMs: nowMs + 15 * 60 * 1000,
      pollIntervalMs: 5000,
      userCode: "ABCD-EFGH",
      verificationUrl: "https://auth.example/codex/device",
    });
    await expect(
      pollDeviceAuthorization(
        fetcher,
        "https://auth.example",
        "device-1",
        "ABCD-EFGH",
      ),
    ).resolves.toEqual({ status: "pending" });
    await expect(
      pollDeviceAuthorization(
        fetcher,
        "https://auth.example",
        "device-1",
        "ABCD-EFGH",
      ),
    ).resolves.toEqual({
      authorizationCode: "authorization-code",
      codeVerifier: "code-verifier",
      status: "complete",
    });
    await expect(
      exchangeDeviceCode(
        fetcher,
        "https://auth.example",
        "authorization-code",
        "code-verifier",
      ),
    ).resolves.toEqual({
      accessToken: "access-token",
      idToken: "id-token",
      refreshToken: "refresh-token",
    });
    expect(calls[3]?.init?.headers).toEqual({
      "Content-Type": "application/x-www-form-urlencoded",
    });
    expect(
      new URLSearchParams(requestBody(fetchCall(calls, 3))).get("redirect_uri"),
    ).toBe("https://auth.example/deviceauth/callback");
  });
});

describe("account observations", () => {
  it("reads raw quota facts", async () => {
    const nowMs = Date.UTC(2026, 7, 9, 12);
    const { calls, fetcher } = queuedFetcher(
      Response.json({
        rate_limit: {
          limit_reached: false,
          primary_window: {
            limit_window_seconds: 18_000,
            reset_at: 1_800_000_000,
            used_percent: 20,
          },
          secondary_window: {
            limit_window_seconds: 604_800,
            reset_at: 1_800_604_800,
            used_percent: 60,
          },
        },
        rate_limit_reset_credits: { available_count: 2 },
      }),
    );

    await expect(
      fetchUsage(fetcher, "https://usage.example/", credentials, nowMs),
    ).resolves.toEqual({
      bankedResets: 2,
      limitReached: false,
      observedAtMs: nowMs,
      primary: {
        observedAtMs: nowMs,
        resetsAtMs: 1_800_000_000_000,
        usedPercent: 20,
        windowMinutes: 300,
      },
      secondary: {
        observedAtMs: nowMs,
        resetsAtMs: 1_800_604_800_000,
        usedPercent: 60,
        windowMinutes: 10_080,
      },
    });
    expect(calls[0]?.init?.headers).toMatchObject({
      Authorization: "Bearer access-token",
      "chatgpt-account-id": "account-1",
    });
  });

  it("lists and consumes reset credits", async () => {
    const { fetcher } = queuedFetcher(
      Response.json({
        available_count: 1,
        credits: [
          {
            expires_at: "2026-08-09T12:04:00Z",
            id: "credit-1",
            reset_type: "codex_rate_limits",
            status: "available",
          },
        ],
      }),
      Response.json({ code: "reset", windows_reset: 2 }),
    );

    await expect(
      fetchResetCredits(fetcher, "https://usage.example", credentials),
    ).resolves.toEqual({
      availableCount: 1,
      credits: [
        {
          expiresAtMs: Date.parse("2026-08-09T12:04:00Z"),
          id: "credit-1",
          resetType: "codex_rate_limits",
          status: "available",
        },
      ],
    });
    await expect(
      consumeResetCredit(
        fetcher,
        "https://usage.example",
        credentials,
        "credit-1",
      ),
    ).resolves.toEqual({ code: "reset", windowsReset: 2 });
  });

  it("filters malformed model entries", async () => {
    const { calls, fetcher } = queuedFetcher(
      Response.json({
        models: [{ slug: "gpt-5" }, { slug: "" }, null, { name: "missing" }],
      }),
    );

    await expect(
      fetchModels(
        fetcher,
        "https://upstream.example/base/",
        credentials,
        "1.2.3",
      ),
    ).resolves.toEqual([{ slug: "gpt-5" }]);
    expect(calls[0]?.url).toBe(
      "https://upstream.example/base/models?client_version=1.2.3",
    );
  });
});
