import { describe, expect, it } from "vitest";

import type {
  AccountGrant,
  AccountId,
  ResponseUsage,
  SelectAccountInput,
} from "../src/domain.js";
import type {
  AccountFailure,
  AccountObservation,
  RecordedRoute,
  RefreshAccountResult,
  SelectAccountResult,
  TransportPort,
} from "../src/transport-port.js";
import {
  maxWebSocketFrameBytes,
  proxyWebSocketResponse,
  type WebSocketProxyOptions,
} from "../src/websocket-proxy.js";

type Frame = Parameters<WebSocket["send"]>[0];

class FakeWebSocket extends EventTarget {
  accepted = false;
  acceptOptions: WebSocketAcceptOptions | undefined;
  binaryType: "arraybuffer" | "blob" = "arraybuffer";
  readonly closes: Readonly<{
    code: number | undefined;
    reason: string | undefined;
  }>[] = [];
  extensions: string | null = null;
  protocol: string | null = null;
  readyState = 1;
  readonly sent: Frame[] = [];
  url: string | null = null;

  accept(options?: WebSocketAcceptOptions): void {
    this.accepted = true;
    this.acceptOptions = options;
  }

  close(code?: number, reason?: string): void {
    this.closes.push({ code, reason });
    this.readyState = 3;
  }

  emitClose(code = 1006): void {
    this.readyState = 3;
    const event = new Event("close");
    Object.defineProperties(event, {
      code: { value: code },
      reason: { value: "" },
      wasClean: { value: code === 1000 },
    });
    this.dispatchEvent(event);
  }

  emitMessage(data: Frame): void {
    const event = new Event("message");
    Object.defineProperty(event, "data", { value: data });
    this.dispatchEvent(event);
  }

  send(message: Frame): void {
    this.sent.push(message);
  }
}

interface FakePair {
  readonly client: FakeWebSocket;
  readonly server: FakeWebSocket;
  readonly value: { 0: WebSocket; 1: WebSocket };
}

const fakePair = (): FakePair => {
  const client = new FakeWebSocket();
  const server = new FakeWebSocket();
  return {
    client,
    server,
    value: {
      0: client as unknown as WebSocket,
      1: server as unknown as WebSocket,
    },
  };
};

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
  readonly closed: AccountId[] = [];
  readonly failures: AccountFailure[] = [];
  readonly observations: AccountObservation[] = [];
  readonly opened: AccountId[] = [];
  refresh: RefreshAccountResult = { ok: false };
  readonly refreshes: (readonly [AccountId, string])[] = [];
  readonly responseIds: (readonly [AccountId, string])[] = [];
  readonly routes: RecordedRoute[] = [];
  readonly selections: SelectAccountInput[] = [];
  selectResults: SelectAccountResult[] = [];
  readonly usage: ResponseUsage[] = [];

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
    this.closed.push(accountId);
    return Promise.resolve();
  }

  websocketOpened(accountId: AccountId): Promise<void> {
    this.opened.push(accountId);
    return Promise.resolve();
  }
}

const socketResponse = (
  webSocket: FakeWebSocket,
  headers: HeadersInit = {},
): Response => {
  const response = new Response(null, { headers });
  Object.defineProperties(response, {
    status: { value: 101 },
    webSocket: { value: webSocket },
  });
  return response;
};

const upgradeResponse = (webSocket: WebSocket, headers: Headers): Response => {
  const response = new Response(null, { headers });
  Object.defineProperties(response, {
    status: { value: 101 },
    webSocket: { value: webSocket },
  });
  return response;
};

const options = (
  port: TestPort,
  fetcher: NonNullable<WebSocketProxyOptions["fetcher"]>,
  pair: FakePair,
  overrides: Partial<WebSocketProxyOptions> = {},
): WebSocketProxyOptions => ({
  fetcher,
  now: () => 100,
  pairFactory: () => pair.value,
  port,
  random: () => 0.5,
  sleep: () => Promise.resolve(),
  upgradeResponse,
  upstreamBaseUrl: "https://upstream.test/v1",
  ...overrides,
});

const request = (mutate?: (headers: Headers) => void): Request => {
  const headers = new Headers({
    authorization: "Bearer client",
    connection: "keep-alive, Upgrade",
    cookie: "secret=value",
    "cf-access-jwt-assertion": "access",
    "cf-connecting-ip": "203.0.113.1",
    forwarded: "for=203.0.113.1",
    "openai-beta": "responses=experimental, custom=1",
    "sec-websocket-key": "dGhlIHNhbXBsZSBub25jZQ==",
    "sec-websocket-protocol": "responses",
    "sec-websocket-version": "13",
    "session-id": "thread",
    upgrade: "websocket",
    "x-forwarded-for": "203.0.113.1",
  });
  mutate?.(headers);
  return new Request("https://balancer.test/v1/responses", { headers });
};

const drain = async (): Promise<void> => {
  await new Promise<void>((resolve) => setTimeout(resolve, 0));
  await Promise.resolve();
};

const frameStrings = (socket: FakeWebSocket): string[] =>
  socket.sent.filter((frame): frame is string => typeof frame === "string");

const nextSocketResponse = (sockets: FakeWebSocket[]): Promise<Response> => {
  const socket = sockets.shift();
  return socket === undefined
    ? Promise.reject(new Error("no fake upstream socket"))
    : Promise.resolve(socketResponse(socket));
};

const connect = (
  port: TestPort,
  upstream: FakeWebSocket,
  pair: FakePair,
): Promise<Response> => {
  port.selectResults.push({ grant: grant("account-a"), ok: true });
  return proxyWebSocketResponse(
    request(),
    options(port, () => Promise.resolve(socketResponse(upstream)), pair),
  );
};

describe("WebSocket proxy", () => {
  it("rejects invalid downstream handshakes before selecting an account", async () => {
    const cases: (readonly [(headers: Headers) => void, number])[] = [
      [
        (headers) => {
          headers.delete("upgrade");
        },
        426,
      ],
      [
        (headers) => {
          headers.delete("connection");
        },
        426,
      ],
      [
        (headers) => {
          headers.set("sec-websocket-version", "12");
        },
        400,
      ],
      [
        (headers) => {
          headers.delete("sec-websocket-key");
        },
        400,
      ],
    ];

    for (const [mutate, status] of cases) {
      const port = new TestPort();
      const pair = fakePair();
      let fetches = 0;
      const response = await proxyWebSocketResponse(
        request(mutate),
        options(
          port,
          () => {
            fetches += 1;
            return Promise.resolve(socketResponse(new FakeWebSocket()));
          },
          pair,
        ),
      );

      expect(response.status).toBe(status);
      expect(fetches).toBe(0);
      expect(port.selections).toHaveLength(0);
    }
  });

  it("uses safe selected-account headers for the upstream handshake", async () => {
    const port = new TestPort();
    port.selectResults.push({ grant: grant("account-a"), ok: true });
    const pair = fakePair();
    const upstream = new FakeWebSocket();
    let url = "";
    let headers = new Headers();
    const response = await proxyWebSocketResponse(
      request(),
      options(
        port,
        (input, init) => {
          url = input instanceof Request ? input.url : String(input);
          headers = new Headers(init?.headers);
          return Promise.resolve(
            socketResponse(upstream, { "x-upstream-public": "yes" }),
          );
        },
        pair,
      ),
    );

    expect(response.status).toBe(101);
    expect(response.webSocket).toBe(pair.value[0]);
    expect(url).toBe("https://upstream.test/v1/responses");
    expect(headers.get("authorization")).toBe("Bearer token-account-a");
    expect(headers.get("chatgpt-account-id")).toBe("account-a");
    expect(headers.get("upgrade")).toBe("websocket");
    expect(headers.get("openai-beta")).toBe(
      "custom=1, responses_websockets=2026-02-06",
    );
    for (const name of [
      "cookie",
      "cf-access-jwt-assertion",
      "cf-connecting-ip",
      "forwarded",
      "sec-websocket-key",
      "sec-websocket-protocol",
      "sec-websocket-version",
      "x-forwarded-for",
    ]) {
      expect(headers.get(name)).toBeNull();
    }
    expect(pair.server.accepted).toBe(true);
    expect(pair.server.acceptOptions).toEqual({ allowHalfOpen: true });
    expect(upstream.accepted).toBe(true);
    expect(response.headers.get("x-upstream-public")).toBe("yes");
    expect(port.opened).toEqual(["account-a"]);
  });

  it("rejects protocols and extensions the proxy did not offer", async () => {
    for (const headers of [
      { "Sec-WebSocket-Protocol": "private" },
      { "Sec-WebSocket-Extensions": "permessage-deflate" },
    ]) {
      const port = new TestPort();
      port.selectResults.push({ grant: grant("account-a"), ok: true });
      const pair = fakePair();
      const upstream = new FakeWebSocket();
      const response = await proxyWebSocketResponse(
        request(),
        options(
          port,
          () => Promise.resolve(socketResponse(upstream, headers)),
          pair,
        ),
      );

      expect(response.status).toBe(503);
      expect(upstream.accepted).toBe(true);
      expect(upstream.closes).toEqual([
        { code: 1002, reason: "invalid handshake" },
      ]);
      expect(port.failures).toMatchObject([
        { accountId: "account-a", kind: "invalid_handshake" },
      ]);
    }
  });

  it("retries one account's failed handshakes before failing over", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-b"), ok: true },
    );
    const pair = fakePair();
    const upstream = new FakeWebSocket();
    const accounts: string[] = [];
    const delays: number[] = [];
    const response = await proxyWebSocketResponse(
      request(),
      options(
        port,
        (_input, init) => {
          const account = new Headers(init?.headers).get("chatgpt-account-id");
          accounts.push(account ?? "");
          return Promise.resolve(
            account === "account-a"
              ? new Response("failed", { status: 503 })
              : socketResponse(upstream),
          );
        },
        pair,
        {
          sleep: (delay) => {
            delays.push(delay);
            return Promise.resolve();
          },
        },
      ),
    );

    expect(response.status).toBe(101);
    expect(accounts).toEqual([
      "account-a",
      "account-a",
      "account-a",
      "account-a",
      "account-b",
    ]);
    expect(delays).toHaveLength(3);
    expect(delays.reduce((sum, delay) => sum + delay, 0)).toBeCloseTo(5_000);
    expect(port.failures).toEqual([
      expect.objectContaining({
        accountId: "account-a",
        failedOver: true,
        kind: "server_failure",
      }),
    ]);
    expect(port.opened).toEqual(["account-b"]);
  });

  it("refreshes a rejected handshake token on the same account", async () => {
    const port = new TestPort();
    port.selectResults.push({ grant: grant("account-a"), ok: true });
    port.refresh = { accessToken: "token-refreshed", ok: true };
    const pair = fakePair();
    const upstream = new FakeWebSocket();
    const tokens: string[] = [];
    const response = await proxyWebSocketResponse(
      request(),
      options(
        port,
        (_input, init) => {
          tokens.push(new Headers(init?.headers).get("authorization") ?? "");
          return Promise.resolve(
            tokens.length === 1
              ? new Response("unauthorized", { status: 401 })
              : socketResponse(upstream),
          );
        },
        pair,
      ),
    );

    expect(response.status).toBe(101);
    expect(tokens).toEqual([
      "Bearer token-account-a",
      "Bearer token-refreshed",
    ]);
    expect(port.refreshes).toEqual([["account-a", "token-account-a"]]);
    expect(port.selections).toHaveLength(1);
    expect(port.failures).toHaveLength(0);
  });

  it("stops after three handshakes reject tokens that cannot refresh", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-b"), ok: true },
      { grant: grant("account-c"), ok: true },
      { grant: grant("account-d"), ok: true },
    );
    const pair = fakePair();
    let fetches = 0;
    const response = await proxyWebSocketResponse(
      request(),
      options(
        port,
        () => {
          fetches += 1;
          return Promise.resolve(new Response("unauthorized", { status: 401 }));
        },
        pair,
      ),
    );

    expect(response.status).toBe(503);
    expect(fetches).toBe(3);
    expect(port.selections).toHaveLength(3);
    expect(port.refreshes).toHaveLength(3);
    expect(port.failures).toHaveLength(3);
  });

  it("refreshes a pre-visible unauthorized turn on the same account", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-a"), ok: true },
    );
    port.refresh = { accessToken: "token-refreshed", ok: true };
    const pair = fakePair();
    const upstreamA = new FakeWebSocket();
    const upstreamRefreshed = new FakeWebSocket();
    const sockets = [upstreamA, upstreamRefreshed];
    const tokens: string[] = [];
    await proxyWebSocketResponse(
      request(),
      options(
        port,
        (_input, init) => {
          tokens.push(new Headers(init?.headers).get("authorization") ?? "");
          return nextSocketResponse(sockets);
        },
        pair,
      ),
    );
    const turn = JSON.stringify({ input: [], type: "response.create" });
    pair.server.emitMessage(turn);
    await drain();

    upstreamA.emitMessage(
      JSON.stringify({
        error: { code: "unauthorized" },
        status: 401,
        type: "error",
      }),
    );
    await drain();

    expect(tokens).toEqual([
      "Bearer token-account-a",
      "Bearer token-refreshed",
    ]);
    expect(port.refreshes).toEqual([["account-a", "token-account-a"]]);
    expect(frameStrings(upstreamRefreshed)).toEqual([turn]);
    expect(frameStrings(pair.server)).toHaveLength(0);
    expect(port.failures).toHaveLength(0);
  });

  it.each(["downstream", "upstream"] as const)(
    "closes both sockets when a %s frame exceeds 32 MiB",
    async (source) => {
      const port = new TestPort();
      const pair = fakePair();
      const upstream = new FakeWebSocket();
      await connect(port, upstream, pair);

      const frame = new Uint8Array(maxWebSocketFrameBytes + 1);
      if (source === "downstream") {
        pair.server.emitMessage(frame);
      } else {
        upstream.emitMessage(frame);
      }
      await drain();

      expect(upstream.sent).toHaveLength(0);
      expect(pair.server.sent).toHaveLength(0);
      expect(pair.server.closes[0]?.code).toBe(1009);
      expect(upstream.closes[0]?.code).toBe(1009);
      expect(port.closed).toEqual(["account-a"]);
    },
  );

  it("routes response.create and records the accepted response", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-a"), ok: true },
    );
    const pair = fakePair();
    const upstream = new FakeWebSocket();
    await proxyWebSocketResponse(
      request(),
      options(port, () => Promise.resolve(socketResponse(upstream)), pair),
    );
    const turn = JSON.stringify({
      input: [],
      model: "gpt-route",
      service_tier: "fast",
      type: "response.create",
    });

    pair.server.emitMessage(turn);
    await drain();
    expect(frameStrings(upstream)).toEqual([turn]);

    const created = JSON.stringify({
      headers: {
        "x-codex-primary-used-percent": 25,
        "x-codex-turn-state": "turn-next",
      },
      response: { id: "response-a" },
      type: "response.created",
    });
    upstream.emitMessage(created);
    await drain();

    expect(frameStrings(pair.server)).toEqual([created]);
    expect(port.routes).toEqual([
      {
        accountId: "account-a",
        bindings: [{ kind: "session", value: "thread" }],
        counted: true,
        headers: { "x-codex-primary-used-percent": "25" },
        transport: "websocket",
        turnState: "turn-next",
      },
    ]);
    expect(port.selections[1]).toMatchObject({
      model: "gpt-route",
      serviceTier: "fast",
    });
    expect(port.responseIds).toEqual([["account-a", "response-a"]]);
    expect(port.answeredLatencies).toEqual([0]);
    expect(port.observations).toContainEqual({
      accountId: "account-a",
      headers: { "x-codex-primary-used-percent": "25" },
    });
  });

  it("binds non-generating turns without answer or usage counts", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-a"), ok: true },
    );
    const pair = fakePair();
    const upstream = new FakeWebSocket();
    await proxyWebSocketResponse(
      request(),
      options(
        port,
        () =>
          Promise.resolve(
            socketResponse(upstream, {
              "x-codex-turn-state": "handshake-turn",
            }),
          ),
        pair,
      ),
    );
    pair.server.emitMessage(
      JSON.stringify({ generate: false, type: "response.create" }),
    );
    await drain();
    upstream.emitMessage(
      JSON.stringify({
        response: { id: "response-a" },
        type: "response.created",
      }),
    );
    upstream.emitMessage(
      JSON.stringify({
        response: { usage: { output_tokens: 7 } },
        type: "response.completed",
      }),
    );
    await drain();

    expect(port.routes).toHaveLength(1);
    expect(port.routes[0]).toMatchObject({
      counted: false,
      turnState: "handshake-turn",
    });
    expect(port.routes[0]?.bindings).toContainEqual({
      kind: "turn_state",
      value: "handshake-turn",
    });
    expect(port.responseIds).toEqual([["account-a", "response-a"]]);
    expect(port.answeredLatencies).toEqual([]);
    expect(port.usage).toEqual([]);
  });

  it("records terminal WebSocket usage", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-a"), ok: true },
    );
    const pair = fakePair();
    const upstream = new FakeWebSocket();
    await proxyWebSocketResponse(
      request(),
      options(port, () => Promise.resolve(socketResponse(upstream)), pair),
    );
    pair.server.emitMessage(
      JSON.stringify({
        model: "request-model",
        service_tier: "request-tier",
        type: "response.create",
      }),
    );
    await drain();
    upstream.emitMessage(
      JSON.stringify({
        response: {
          model: "response-model",
          service_tier: "response-tier",
          usage: {
            input_tokens: 20,
            input_tokens_details: {
              cached_tokens: 8,
              cache_write_tokens: 3,
            },
            output_tokens: 4,
          },
        },
        type: "response.completed",
      }),
    );
    await drain();

    expect(port.usage).toEqual([
      {
        inputTokens: 20,
        inputTokensDetails: { cachedTokens: 8, cacheWriteTokens: 3 },
        outputTokens: 4,
      },
    ]);
  });

  it("replays one pre-visible turn on another account", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-b"), ok: true },
    );
    const pair = fakePair();
    const upstreamA = new FakeWebSocket();
    const upstreamB = new FakeWebSocket();
    const sockets = [upstreamA, upstreamB];
    await proxyWebSocketResponse(
      request(),
      options(port, () => nextSocketResponse(sockets), pair),
    );
    const turn = JSON.stringify({ input: [], type: "response.create" });
    pair.server.emitMessage(turn);
    await drain();

    upstreamA.emitMessage(
      JSON.stringify({
        error: { code: "upstream_error" },
        status: 502,
        type: "error",
      }),
    );
    await drain();

    expect(frameStrings(upstreamA)).toEqual([turn]);
    expect(frameStrings(upstreamB)).toEqual([turn]);
    expect(frameStrings(pair.server)).toHaveLength(0);
    expect(port.failures).toContainEqual(
      expect.objectContaining({
        accountId: "account-a",
        failedOver: true,
        kind: "server_failure",
      }),
    );
    expect(port.opened).toEqual(["account-a", "account-b"]);
    expect(port.closed).toEqual(["account-a"]);
  });

  it.each([
    {
      failure: JSON.stringify({
        error: { code: "upstream_error" },
        status: 502,
        type: "error",
      }),
      name: "semantic output",
      visible: JSON.stringify({
        delta: "visible",
        type: "response.output_text.delta",
      }),
    },
    {
      failure: JSON.stringify({
        error: { code: "upstream_error" },
        id: "event-a",
        status: 502,
        type: "error",
      }),
      name: "response identity",
      visible: null,
    },
  ])("does not replay after $name", async ({ visible, failure }) => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-b"), ok: true },
    );
    const pair = fakePair();
    const upstream = new FakeWebSocket();
    let fetches = 0;
    await proxyWebSocketResponse(
      request(),
      options(
        port,
        () => {
          fetches += 1;
          return Promise.resolve(socketResponse(upstream));
        },
        pair,
      ),
    );
    pair.server.emitMessage(
      JSON.stringify({ input: [], type: "response.create" }),
    );
    await drain();

    if (visible !== null) {
      upstream.emitMessage(visible);
      await drain();
    }
    upstream.emitMessage(failure);
    await drain();

    expect(fetches).toBe(1);
    expect(frameStrings(pair.server)).toEqual(
      visible === null ? [failure] : [visible, failure],
    );
  });

  it("replays a rate limit even when the event has response identity", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-b"), ok: true },
    );
    const pair = fakePair();
    const upstreamA = new FakeWebSocket();
    const upstreamB = new FakeWebSocket();
    const sockets = [upstreamA, upstreamB];
    await proxyWebSocketResponse(
      request(),
      options(port, () => nextSocketResponse(sockets), pair),
    );
    const turn = JSON.stringify({ input: [], type: "response.create" });
    pair.server.emitMessage(turn);
    await drain();

    upstreamA.emitMessage(
      JSON.stringify({
        response: {
          error: { code: "usage_limit_reached" },
          id: "response-a",
        },
        type: "response.failed",
      }),
    );
    await drain();

    expect(frameStrings(upstreamB)).toEqual([turn]);
    expect(frameStrings(pair.server)).toHaveLength(0);
    expect(port.failures).toContainEqual(
      expect.objectContaining({
        accountId: "account-a",
        failedOver: true,
        kind: "rate_limited",
      }),
    );
  });

  it("keeps a hard turn on its account after a server failure", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-a", true), ok: true },
    );
    const pair = fakePair();
    const upstream = new FakeWebSocket();
    let fetches = 0;
    await proxyWebSocketResponse(
      request(),
      options(
        port,
        () => {
          fetches += 1;
          return Promise.resolve(socketResponse(upstream));
        },
        pair,
      ),
    );
    pair.server.emitMessage(JSON.stringify({ type: "response.create" }));
    await drain();
    const failure = JSON.stringify({ status: 502, type: "error" });
    upstream.emitMessage(failure);
    await drain();

    expect(fetches).toBe(1);
    expect(frameStrings(pair.server)).toEqual([failure]);
    expect(port.failures).toContainEqual(
      expect.objectContaining({
        accountId: "account-a",
        failedOver: false,
        kind: "server_failure",
      }),
    );
  });

  it("replays a pre-visible turn after an upstream disconnect", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-b"), ok: true },
    );
    const pair = fakePair();
    const upstreamA = new FakeWebSocket();
    const upstreamB = new FakeWebSocket();
    const sockets = [upstreamA, upstreamB];
    await proxyWebSocketResponse(
      request(),
      options(port, () => nextSocketResponse(sockets), pair),
    );
    const turn = JSON.stringify({ type: "response.create" });
    pair.server.emitMessage(turn);
    await drain();
    upstreamA.emitClose();
    await drain();

    expect(frameStrings(upstreamB)).toEqual([turn]);
    expect(pair.server.closes).toHaveLength(0);
    expect(port.failures).toContainEqual(
      expect.objectContaining({
        accountId: "account-a",
        failedOver: true,
        kind: "disconnected",
      }),
    );
  });

  it("does not replay while more than one turn is outstanding", async () => {
    const port = new TestPort();
    port.selectResults.push(
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-a"), ok: true },
      { grant: grant("account-b"), ok: true },
    );
    const pair = fakePair();
    const upstream = new FakeWebSocket();
    let fetches = 0;
    await proxyWebSocketResponse(
      request(),
      options(
        port,
        () => {
          fetches += 1;
          return Promise.resolve(socketResponse(upstream));
        },
        pair,
      ),
    );
    pair.server.emitMessage(
      JSON.stringify({ input: ["one"], type: "response.create" }),
    );
    pair.server.emitMessage(
      JSON.stringify({ input: ["two"], type: "response.create" }),
    );
    await drain();
    const failure = JSON.stringify({
      error: { code: "upstream_error" },
      status: 502,
      type: "error",
    });

    upstream.emitMessage(failure);
    await drain();

    expect(fetches).toBe(1);
    expect(frameStrings(upstream)).toHaveLength(2);
    expect(frameStrings(pair.server)).toEqual([failure]);
    expect(port.selections[1]?.requiredAccountId).toBeNull();
    expect(port.selections[2]?.requiredAccountId).toBe("account-a");
  });

  it("closes the upstream socket and records cleanup", async () => {
    const port = new TestPort();
    const pair = fakePair();
    const upstream = new FakeWebSocket();
    await connect(port, upstream, pair);

    pair.server.emitClose(1000);
    await drain();

    expect(upstream.closes).toHaveLength(1);
    expect(port.closed).toEqual(["account-a"]);
  });
});
