import { describe, expect, it } from "vitest";

import type {
  AccountGrant,
  AccountId,
  ResponseUsage,
  SelectAccountInput,
} from "../src/domain.js";
import {
  proxyHttpResponse,
  upstreamRetryBackoffMs,
  type HttpProxyOptions,
} from "../src/http-proxy.js";
import type {
  AccountFailure,
  AccountObservation,
  RecordedRoute,
  RefreshAccountResult,
  SelectAccountResult,
  TransportPort,
} from "../src/transport-port.js";

const encoder = new TextEncoder();

const grant = (accountId: string, hard = false): AccountGrant => ({
  accessToken: `token-${accountId}`,
  accountId,
  resolution: {
    bindings: [{ kind: hard ? "turn_state" : "session", value: "thread" }],
    hard,
    preferredAccountId: hard ? null : accountId,
    requiredAccountId: hard ? accountId : null,
  },
});

class TestPort implements TransportPort {
  readonly answeredLatencies: number[] = [];
  readonly failures: AccountFailure[] = [];
  readonly observations: AccountObservation[] = [];
  readonly responseIds: (readonly [AccountId, string])[] = [];
  readonly routes: RecordedRoute[] = [];
  readonly selections: SelectAccountInput[] = [];
  readonly usage: ResponseUsage[] = [];
  readonly websocketAccounts: AccountId[] = [];
  refresh: RefreshAccountResult = { ok: false };
  refreshes: (readonly [AccountId, string])[] = [];
  selectResults: SelectAccountResult[] = [];

  answered(latencyMs: number): Promise<void> {
    this.answeredLatencies.push(latencyMs);
    return Promise.resolve();
  }

  claimResponseId(accountId: AccountId, responseId: string): Promise<void> {
    this.responseIds.push([accountId, responseId]);
    return Promise.resolve();
  }

  observeAccount(observation: AccountObservation): Promise<void> {
    this.observations.push(observation);
    return Promise.resolve();
  }

  recordFailure(failure: AccountFailure): Promise<void> {
    this.failures.push(failure);
    return Promise.resolve();
  }

  recordRoute(outcome: RecordedRoute): Promise<void> {
    this.routes.push(outcome);
    return Promise.resolve();
  }

  recordUsage(usage: ResponseUsage): Promise<void> {
    this.usage.push(usage);
    return Promise.resolve();
  }

  refreshAccount(
    accountId: AccountId,
    rejectedAccessToken: string,
  ): Promise<RefreshAccountResult> {
    this.refreshes.push([accountId, rejectedAccessToken]);
    return Promise.resolve(this.refresh);
  }

  selectAccount(input: SelectAccountInput): Promise<SelectAccountResult> {
    this.selections.push(input);
    return Promise.resolve(
      this.selectResults.shift() ?? { failure: "no_account", ok: false },
    );
  }

  websocketClosed(accountId: AccountId): Promise<void> {
    this.websocketAccounts.push(accountId);
    return Promise.resolve();
  }

  websocketOpened(accountId: AccountId): Promise<void> {
    this.websocketAccounts.push(accountId);
    return Promise.resolve();
  }
}

const sseResponse = (id: string): Response => {
  const response = { id };
  return new Response(
    `data: ${JSON.stringify({ response, type: "response.created" })}\n\n`,
    { headers: { "content-type": "text/event-stream" } },
  );
};

const options = (
  port: TestPort,
  fetcher: NonNullable<HttpProxyOptions["fetcher"]>,
  overrides: Partial<HttpProxyOptions> = {},
): HttpProxyOptions => ({
  fetcher,
  now: () => 100,
  port,
  random: () => 0.5,
  sleep: () => Promise.resolve(),
  upstreamBaseUrl: "https://upstream.test/v1",
  ...overrides,
});

const request = (body = '{"model":"gpt","input":[]}'): Request =>
  new Request("https://balancer.test/v1/responses", {
    body,
    headers: {
      authorization: "Bearer client",
      "content-type": "application/json",
      cookie: "secret=value",
      "session-id": "thread",
    },
    method: "POST",
  });

describe("HTTP proxy", () => {
  it("forwards exact bytes with selected credentials and records the stream", async () => {
    const port = new TestPort();
    port.selectResults.push({ grant: grant("account-a"), ok: true });
    const bodies: Uint8Array[] = [];
    const headers: Headers[] = [];
    const response = await proxyHttpResponse(
      request(),
      options(port, (_input, init) => {
        bodies.push(new Uint8Array(init?.body as Uint8Array));
        headers.push(new Headers(init?.headers));
        return Promise.resolve(sseResponse("response-a"));
      }),
    );

    expect(response.status).toBe(200);
    expect(await response.text()).toContain("response-a");
    expect(new TextDecoder().decode(bodies[0])).toBe(
      '{"model":"gpt","input":[]}',
    );
    expect(headers[0]?.get("authorization")).toBe("Bearer token-account-a");
    expect(headers[0]?.get("cookie")).toBeNull();
    expect(port.routes).toHaveLength(1);
    expect(port.selections[0]?.requiredAccountId).toBeNull();
    expect(port.responseIds).toEqual([["account-a", "response-a"]]);
    expect(port.answeredLatencies).toEqual([0]);
  });

  it("retries server failures on one account before failing over", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-b"), ok: true },
    );
    const calls: string[] = [];
    const delays: number[] = [];
    const response = await proxyHttpResponse(
      request(),
      options(
        port,
        (_input, init) => {
          const account = new Headers(init?.headers).get("chatgpt-account-id");
          calls.push(account ?? "");
          return Promise.resolve(
            account === "account-a"
              ? new Response("failed", { status: 503 })
              : sseResponse("response-b"),
          );
        },
        {
          sleep: (delay) => {
            delays.push(delay);
            return Promise.resolve();
          },
        },
      ),
    );

    expect(await response.text()).toContain("response-b");
    expect(calls).toEqual([
      "account-a",
      "account-a",
      "account-a",
      "account-a",
      "account-b",
    ]);
    expect(delays).toEqual([
      upstreamRetryBackoffMs(1),
      upstreamRetryBackoffMs(2),
      upstreamRetryBackoffMs(3),
    ]);
    expect(delays.reduce((sum, delay) => sum + delay, 0)).toBeCloseTo(5_000);
    expect(port.failures).toEqual([
      expect.objectContaining({
        accountId: "account-a",
        failedOver: true,
        kind: "server_failure",
      }),
    ]);
  });

  it("retries an empty successful response only for soft affinity", async () => {
    const softPort = new TestPort();
    softPort.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-b"), ok: true },
    );
    let softCalls = 0;
    const soft = await proxyHttpResponse(
      request(),
      options(softPort, () => {
        softCalls += 1;
        return Promise.resolve(
          softCalls === 1
            ? new Response(null, { status: 200 })
            : sseResponse("response-b"),
        );
      }),
    );
    expect(await soft.text()).toContain("response-b");
    expect(softPort.failures[0]).toEqual(
      expect.objectContaining({
        failedOver: true,
        kind: "empty_response",
      }),
    );

    const hardPort = new TestPort();
    hardPort.selectResults.push({ grant: grant("account-a", true), ok: true });
    const hard = await proxyHttpResponse(
      request(),
      options(hardPort, () =>
        Promise.resolve(new Response(null, { status: 200 })),
      ),
    );
    expect(hard.status).toBe(502);
    expect(hardPort.selections).toHaveLength(1);
    expect(hardPort.failures[0]?.failedOver).toBe(false);
  });

  it("refreshes a rejected token and resends on the same account", async () => {
    const port = new TestPort();
    port.selectResults.push({ grant: grant("account-a"), ok: true });
    port.refresh = { accessToken: "token-refreshed", ok: true };
    const tokens: string[] = [];
    const response = await proxyHttpResponse(
      request(),
      options(port, (_input, init) => {
        const token = new Headers(init?.headers).get("authorization") ?? "";
        tokens.push(token);
        return Promise.resolve(
          tokens.length === 1
            ? new Response("unauthorized", { status: 401 })
            : sseResponse("response-a"),
        );
      }),
    );

    expect(await response.text()).toContain("response-a");
    expect(tokens).toEqual([
      "Bearer token-account-a",
      "Bearer token-refreshed",
    ]);
    expect(port.refreshes).toEqual([["account-a", "token-account-a"]]);
    expect(port.selections).toHaveLength(1);
    expect(port.failures).toHaveLength(0);
  });

  it("stops after three accounts reject tokens that cannot refresh", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-b"), ok: true },
      { grant: grant("account-c"), ok: true },
      { grant: grant("account-d"), ok: true },
    );
    let fetches = 0;
    const response = await proxyHttpResponse(
      request(),
      options(port, () => {
        fetches += 1;
        return Promise.resolve(new Response("unauthorized", { status: 401 }));
      }),
    );

    expect(response.status).toBe(503);
    expect(fetches).toBe(3);
    expect(port.selections).toHaveLength(3);
    expect(port.refreshes).toHaveLength(3);
    expect(port.failures).toHaveLength(3);
  });

  it("uses only one replacement for the exact unsupported-model error", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-b"), ok: true },
    );
    const accounts: string[] = [];
    const response = await proxyHttpResponse(
      request('{"model":"gpt-route","input":[]}'),
      options(port, (_input, init) => {
        const account = new Headers(init?.headers).get("chatgpt-account-id");
        accounts.push(account ?? "");
        return Promise.resolve(
          new Response(
            JSON.stringify({
              error: {
                code: "invalid_request_error",
                message:
                  "The 'gpt-route' model is not supported when using Codex with a ChatGPT account.",
              },
            }),
            { status: 400 },
          ),
        );
      }),
    );

    expect(response.status).toBe(400);
    expect(accounts).toEqual(["account-a", "account-b"]);
    expect(port.failures).toEqual([
      expect.objectContaining({
        accountId: "account-a",
        failedOver: true,
        kind: "model_unsupported",
      }),
    ]);
  });

  it("does not replay after the first upstream body byte", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-b"), ok: true },
    );
    let pulls = 0;
    let fetches = 0;
    const response = await proxyHttpResponse(
      request(),
      options(port, () => {
        fetches += 1;
        return Promise.resolve(
          new Response(
            new ReadableStream<Uint8Array>({
              pull(controller) {
                pulls += 1;
                if (pulls === 1) {
                  controller.enqueue(
                    encoder.encode(
                      'data: {"type":"response.created","response":{"id":"response-a"}}\n\n',
                    ),
                  );
                  return;
                }
                controller.error(new Error("cut short"));
              },
            }),
          ),
        );
      }),
    );

    await expect(response.text()).rejects.toThrow("cut short");
    expect(fetches).toBe(1);
    expect(port.selections).toHaveLength(1);
    expect(port.responseIds).toEqual([["account-a", "response-a"]]);
  });
});
