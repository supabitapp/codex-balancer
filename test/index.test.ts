import { describe, expect, it, vi } from "vitest";

vi.mock("../src/state.js", () => ({
  BalancerState: class {
    readonly mocked = true;
  },
}));

import type { Env } from "../src/env.js";
import type { DashboardSnapshot } from "../src/domain.js";
import { handleRequest } from "../src/index.js";

const inviteToken = "a".repeat(43);
const sessionToken = "b".repeat(43);
const webSocketKey = btoa("0123456789abcdef");

interface TestState {
  adminStateCalls: number;
  dashboardCalls: number;
  dashboardSocketCalls: number;
  dashboardSocketHeaders: HeadersInit;
  dashboardSocketRequest: Request | undefined;
  dashboardSocketWebSocket:
    { close(code?: number, reason?: string): void } | undefined;
  deviceCalls: number;
  deviceCredentials: (readonly [string, string])[];
  healthCalls: number;
  inspectCalls: number;
  modelsCalls: number;
  statusCalls: number;
  statusCredentials: (readonly [string, string])[];
  adminState(): Promise<never>;
  dashboard(): Promise<DashboardSnapshot>;
  fetch(request: Request): Promise<Response>;
  health(): Promise<void>;
  inspectInvite(token: string): Promise<Readonly<{ expiresAt: string }> | null>;
  models(clientVersion: string): Promise<readonly Record<string, unknown>[]>;
  onboardingStatus(
    token: string,
    session: string,
  ): Promise<Readonly<{ status: "ready" }>>;
  startDeviceLogin(
    token: string,
    session: string,
  ): Promise<Readonly<{ status: "pending" }>>;
}

const testState = (): TestState => ({
  adminStateCalls: 0,
  dashboardCalls: 0,
  dashboardSocketCalls: 0,
  dashboardSocketHeaders: {},
  dashboardSocketRequest: undefined,
  dashboardSocketWebSocket: {
    close() {
      return;
    },
  },
  deviceCalls: 0,
  deviceCredentials: [],
  healthCalls: 0,
  inspectCalls: 0,
  modelsCalls: 0,
  statusCalls: 0,
  statusCredentials: [],
  adminState() {
    this.adminStateCalls += 1;
    return Promise.reject(new Error("admin state must stay private"));
  },
  dashboard() {
    this.dashboardCalls += 1;
    return Promise.resolve({
      accounts: [
        {
          alias: "account 1",
          bankedResets: null,
          openWebSockets: 0,
          plan: "team",
          rateLimits: 0,
          resetAt: null,
          status: "live",
          turns: 2,
          weeklyRemainingPercent: 80,
        },
      ],
      events: [
        {
          accountAlias: "account 1",
          at: "2026-08-09T12:00:00.000Z",
          detail: "rate limited",
          kind: "failover",
        },
      ],
      totals: {
        averageFirstByteMilliseconds: 120,
        cacheWriteInputTokens: 0,
        cachedInputTokens: 0,
        failovers: 0,
        inputTokens: 10,
        outputTokens: 5,
        rateLimits: 0,
        turns: 2,
        websocketTurns: 0,
      },
      updatedAt: "2026-08-09T12:00:00.000Z",
    });
  },
  fetch(request) {
    this.dashboardSocketCalls += 1;
    this.dashboardSocketRequest = request;
    const response = new Response(null, {
      headers: this.dashboardSocketHeaders,
    });
    Object.defineProperty(response, "status", { value: 101 });
    if (this.dashboardSocketWebSocket !== undefined) {
      Object.defineProperty(response, "webSocket", {
        value: this.dashboardSocketWebSocket,
      });
    }
    return Promise.resolve(response);
  },
  health() {
    this.healthCalls += 1;
    return Promise.resolve();
  },
  inspectInvite() {
    this.inspectCalls += 1;
    return Promise.resolve({ expiresAt: "2099-01-01T00:00:00.000Z" });
  },
  models() {
    this.modelsCalls += 1;
    return Promise.resolve([{ slug: "gpt" }]);
  },
  onboardingStatus(token, session) {
    this.statusCalls += 1;
    this.statusCredentials.push([token, session]);
    return Promise.resolve({ status: "ready" });
  },
  startDeviceLogin(token, session) {
    this.deviceCalls += 1;
    this.deviceCredentials.push([token, session]);
    return Promise.resolve({ status: "pending" });
  },
});

interface TestEnvironment {
  readonly assets: Request[];
  readonly env: Env;
}

const testEnvironment = (state: TestState): TestEnvironment => {
  const assets: Request[] = [];
  const env = {
    ACCESS_AUD: "access-audience",
    ACCESS_TEAM_DOMAIN: "team.cloudflareaccess.com",
    ASSETS: {
      fetch(request: Request): Promise<Response> {
        assets.push(request);
        return Promise.resolve(
          new Response("<!doctype html><title>asset</title>", {
            headers: { "content-type": "text/html; charset=utf-8" },
          }),
        );
      },
    },
    AUTH_BASE_URL: "https://auth.test",
    BALANCER_KEY: "secret-key",
    BALANCER_STATE: {
      getByName(): TestState {
        return state;
      },
    },
    GIT_SHA: "abc123",
    TOKEN_ENCRYPTION_KEY: "token-key",
    UPSTREAM_BASE_URL: "https://upstream.test/v1",
    USAGE_BASE_URL: "https://usage.test",
  } as unknown as Env;
  return { assets, env };
};

const request = (pathname: string, init: RequestInit = {}): Request =>
  new Request(`https://balancer.test${pathname}`, init);

const dashboardSocketHeaders = (): Headers =>
  new Headers({
    Connection: "Upgrade",
    "Sec-WebSocket-Key": webSocketKey,
    "Sec-WebSocket-Version": "13",
    Upgrade: "websocket",
  });

describe("Worker health", () => {
  it("reports storage only after the Durable Object answers", async () => {
    const state = testState();
    const { env } = testEnvironment(state);
    const response = await handleRequest(request("/healthz"), env);

    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({
      sha: "abc123",
      status: "ok",
      storage: "ok",
    });
    expect(state.healthCalls).toBe(1);
  });

  it("does not claim healthy storage after a state failure", async () => {
    const state = testState();
    state.health = () => Promise.reject(new Error("private database detail"));
    const { env } = testEnvironment(state);
    const response = await handleRequest(request("/healthz"), env);

    expect(response.status).toBe(500);
    expect(await response.text()).not.toContain("private database detail");
  });
});

describe("route authentication", () => {
  it("requires the bearer key before model state work", async () => {
    const state = testState();
    const { env } = testEnvironment(state);
    const denied = await handleRequest(request("/v1/models"), env);

    expect(denied.status).toBe(401);
    expect(await denied.json()).toEqual({
      error: {
        message: "missing or invalid bearer key",
        type: "balancer_error",
      },
    });
    expect(state.modelsCalls).toBe(0);

    const allowed = await handleRequest(
      request("/v1/models", {
        headers: { authorization: "Bearer secret-key" },
      }),
      env,
    );
    expect(allowed.status).toBe(200);
    expect(await allowed.json()).toEqual({
      data: [{ id: "gpt", object: "model", owned_by: "openai" }],
      object: "list",
    });
    expect(state.modelsCalls).toBe(1);
  });

  it("requires the bearer key for both response transports", async () => {
    const state = testState();
    const { env } = testEnvironment(state);

    for (const method of ["GET", "POST"]) {
      const response = await handleRequest(
        request("/v1/responses", { method }),
        env,
      );
      expect(response.status).toBe(401);
    }
  });

  it("authenticates before rejecting unsupported v1 methods", async () => {
    const state = testState();
    const { env } = testEnvironment(state);
    const denied = await handleRequest(
      request("/v1/models", { method: "POST" }),
      env,
    );
    const rejected = await handleRequest(
      request("/v1/models", {
        headers: { authorization: "Bearer secret-key" },
        method: "POST",
      }),
      env,
    );

    expect(denied.status).toBe(401);
    expect(rejected.status).toBe(405);
    expect(rejected.headers.get("allow")).toBe("GET");
  });

  it("keeps the public dashboard on its redacted state method", async () => {
    const state = testState();
    const { env } = testEnvironment(state);
    const response = await handleRequest(request("/stats"), env);
    const text = await response.text();

    expect(response.status).toBe(200);
    expect(state.dashboardCalls).toBe(1);
    expect(state.adminStateCalls).toBe(0);
    expect(text).toContain('"alias":"account 1"');
    expect(text).not.toContain("email");
    expect(text).not.toContain("accountId");
  });

  it("forwards only handshake data to the public dashboard socket", async () => {
    const state = testState();
    const { env } = testEnvironment(state);
    const response = await handleRequest(
      request("/dashboard/ws", {
        headers: new Headers({
          ...Object.fromEntries(dashboardSocketHeaders()),
          authorization: "Bearer private",
          "cf-access-jwt-assertion": "private",
          "cf-connecting-ip": "203.0.113.1",
          cookie: "private=value",
          "x-forwarded-for": "203.0.113.1",
        }),
      }),
      env,
    );

    expect(response.status).toBe(101);
    expect(state.dashboardSocketCalls).toBe(1);
    expect(state.dashboardSocketRequest?.headers.get("upgrade")).toBe(
      "websocket",
    );
    for (const name of [
      "authorization",
      "cf-access-jwt-assertion",
      "cf-connecting-ip",
      "cookie",
      "x-forwarded-for",
    ]) {
      expect(state.dashboardSocketRequest?.headers.get(name)).toBeNull();
    }
  });

  it("rejects malformed dashboard socket handshakes before state work", async () => {
    const state = testState();
    const { env } = testEnvironment(state);
    const missingConnection = dashboardSocketHeaders();
    missingConnection.delete("Connection");
    const wrongVersion = dashboardSocketHeaders();
    wrongVersion.set("Sec-WebSocket-Version", "12");
    const shortKey = dashboardSocketHeaders();
    shortKey.set("Sec-WebSocket-Key", btoa("short"));

    const upgradeRequired = await handleRequest(
      request("/dashboard/ws", { headers: missingConnection }),
      env,
    );
    const invalidVersion = await handleRequest(
      request("/dashboard/ws", { headers: wrongVersion }),
      env,
    );
    const invalidKey = await handleRequest(
      request("/dashboard/ws", { headers: shortKey }),
      env,
    );

    expect(upgradeRequired.status).toBe(426);
    expect(upgradeRequired.headers.get("connection")).toBe("Upgrade");
    expect(upgradeRequired.headers.get("upgrade")).toBe("websocket");
    expect(invalidVersion.status).toBe(400);
    expect(invalidKey.status).toBe(400);
    expect(state.dashboardSocketCalls).toBe(0);
  });

  it("accepts only an offered dashboard socket protocol", async () => {
    const state = testState();
    const { env } = testEnvironment(state);
    const headers = dashboardSocketHeaders();
    headers.set("Sec-WebSocket-Protocol", "dashboard.v1, dashboard.v2");
    state.dashboardSocketHeaders = {
      "Sec-WebSocket-Protocol": "dashboard.v2",
    };

    const accepted = await handleRequest(
      request("/dashboard/ws", { headers }),
      env,
    );
    state.dashboardSocketHeaders = {
      "Sec-WebSocket-Protocol": "private",
    };
    const rejected = await handleRequest(
      request("/dashboard/ws", { headers }),
      env,
    );

    expect(accepted.status).toBe(101);
    expect(accepted.headers.get("sec-websocket-protocol")).toBe("dashboard.v2");
    expect(rejected.status).toBe(503);
    expect(rejected.headers.get("sec-websocket-protocol")).toBeNull();
  });

  it("rejects private headers from the dashboard socket state", async () => {
    const state = testState();
    state.dashboardSocketHeaders = { "set-cookie": "secret=leak" };
    const { env } = testEnvironment(state);
    const response = await handleRequest(
      request("/dashboard/ws", {
        headers: dashboardSocketHeaders(),
      }),
      env,
    );

    expect(response.status).toBe(503);
    expect(response.headers.get("set-cookie")).toBeNull();
  });

  it("rejects a dashboard upgrade without a returned socket", async () => {
    const state = testState();
    state.dashboardSocketWebSocket = undefined;
    const { env } = testEnvironment(state);
    const response = await handleRequest(
      request("/dashboard/ws", { headers: dashboardSocketHeaders() }),
      env,
    );

    expect(response.status).toBe(503);
  });

  it("rejects admin pages and APIs before assets or state", async () => {
    const state = testState();
    const { assets, env } = testEnvironment(state);

    for (const pathname of [
      "/admin",
      "/admin.html",
      "/admin.js",
      "/admin/state",
    ]) {
      const response = await handleRequest(request(pathname), env);
      expect(response.status).toBe(401);
    }
    expect(assets).toHaveLength(0);
    expect(state.adminStateCalls).toBe(0);
  });
});

describe("invite onboarding", () => {
  it("validates preview GET without consuming the invite", async () => {
    const state = testState();
    const { env } = testEnvironment(state);
    const preview = await handleRequest(
      request(`/accounts?invite=${inviteToken}`),
      env,
    );

    expect(preview.status).toBe(303);
    expect(preview.headers.get("location")).toBe("/accounts");
    expect(preview.headers.get("location")).not.toContain(inviteToken);
    expect(await preview.text()).not.toContain(inviteToken);
    expect(preview.headers.get("set-cookie")).toMatch(
      new RegExp(
        `__Host-codex-balancer-invite=${inviteToken}\\.[A-Za-z0-9_-]{43}`,
        "u",
      ),
    );
    expect(preview.headers.get("set-cookie")).toContain("Secure");
    expect(preview.headers.get("set-cookie")).toContain("HttpOnly");
    expect(preview.headers.get("set-cookie")).toContain("SameSite=Lax");
    expect(state.inspectCalls).toBe(1);
    expect(state.statusCalls).toBe(0);
    expect(state.deviceCalls).toBe(0);

    const cookie = preview.headers.get("set-cookie")?.split(";", 1)[0];
    const status = await handleRequest(
      request("/accounts/status", {
        headers: { cookie: cookie ?? "" },
      }),
      env,
    );
    expect(await status.json()).toEqual({ status: "ready" });
    expect(state.statusCalls).toBe(1);
    expect(state.statusCredentials).toHaveLength(1);
    expect(state.statusCredentials[0]?.[0]).toBe(inviteToken);
    expect(state.statusCredentials[0]?.[1]).toMatch(/^[A-Za-z0-9_-]{43}$/u);
    expect(state.deviceCalls).toBe(0);
  });

  it("starts device login only on POST", async () => {
    const state = testState();
    const { env } = testEnvironment(state);
    const cookie = `__Host-codex-balancer-invite=${inviteToken}.${sessionToken}`;

    const get = await handleRequest(
      request("/accounts/device", { headers: { cookie } }),
      env,
    );
    expect(get.status).toBe(405);
    expect(state.deviceCalls).toBe(0);

    const post = await handleRequest(
      request("/accounts/device", {
        headers: { cookie },
        method: "POST",
      }),
      env,
    );
    const emptyPost = await handleRequest(
      request("/accounts/device", {
        body: "",
        headers: { cookie },
        method: "POST",
      }),
      env,
    );
    expect(await post.json()).toEqual({ status: "pending" });
    expect(await emptyPost.json()).toEqual({ status: "pending" });
    expect(state.deviceCalls).toBe(2);
    expect(state.deviceCredentials).toEqual([
      [inviteToken, sessionToken],
      [inviteToken, sessionToken],
    ]);
  });

  it("rejects invite cookies without a browser session", async () => {
    const state = testState();
    const { env } = testEnvironment(state);
    const cookie = `__Host-codex-balancer-invite=${inviteToken}`;
    const [status, device] = await Promise.all([
      handleRequest(request("/accounts/status", { headers: { cookie } }), env),
      handleRequest(
        request("/accounts/device", { headers: { cookie }, method: "POST" }),
        env,
      ),
    ]);

    expect(await status.json()).toEqual({ status: "expired" });
    expect(await device.json()).toEqual({ status: "expired" });
    expect(state.statusCalls).toBe(0);
    expect(state.deviceCalls).toBe(0);
  });

  it("rejects invite preview methods and device bodies before state changes", async () => {
    const state = testState();
    const { env } = testEnvironment(state);
    const preview = await handleRequest(
      request(`/accounts?invite=${inviteToken}`, { method: "HEAD" }),
      env,
    );
    const device = await handleRequest(
      request("/accounts/device", {
        body: "unexpected",
        headers: {
          cookie: `__Host-codex-balancer-invite=${inviteToken}.${sessionToken}`,
        },
        method: "POST",
      }),
      env,
    );

    expect(preview.status).toBe(405);
    expect(preview.headers.get("allow")).toBe("GET");
    expect(device.status).toBe(400);
    expect(state.inspectCalls).toBe(0);
    expect(state.deviceCalls).toBe(0);
  });

  it("blocks direct HTML asset paths", async () => {
    const state = testState();
    const { assets, env } = testEnvironment(state);
    const response = await handleRequest(request("/accounts.html"), env);

    expect(response.status).toBe(404);
    expect(assets).toHaveLength(0);
  });

  it("serves the shared browser module through the Worker", async () => {
    const state = testState();
    const { assets, env } = testEnvironment(state);
    const response = await handleRequest(request("/shared.js"), env);

    expect(response.status).toBe(200);
    expect(assets.map((asset) => new URL(asset.url).pathname)).toEqual([
      "/shared.js",
    ]);
  });
});
