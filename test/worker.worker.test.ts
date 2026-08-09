import {
  evictDurableObject,
  listDurableObjectIds,
  reset,
  runInDurableObject,
} from "cloudflare:test";
import { env, exports } from "cloudflare:workers";
import { afterEach, describe, expect, it } from "vitest";

import { hashToken } from "../src/crypto.js";

const webSocketKey = btoa("0123456789abcdef");

afterEach(async () => {
  await reset();
});

describe("Worker runtime", () => {
  it("serves health through the singleton SQLite Durable Object", async () => {
    const response = await exports.default.fetch(
      new Request("https://balancer.test/healthz"),
    );

    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({
      sha: "test-sha",
      status: "ok",
      storage: "ok",
    });

    const expectedId = env.BALANCER_STATE.idFromName("global").toString();
    const ids = await listDurableObjectIds(env.BALANCER_STATE);
    expect(ids.map((id) => id.toString())).toEqual([expectedId]);

    const stub = env.BALANCER_STATE.getByName("global");
    const schemaReady = await runInDurableObject(stub, (_instance, state) => {
      const row = state.storage.sql
        .exec<{ name: string }>(
          "SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'accounts'",
        )
        .one();
      return row.name === "accounts";
    });
    expect(schemaReady).toBe(true);
  });

  it("rejects malformed dashboard socket handshakes", async () => {
    const stub = env.BALANCER_STATE.getByName("global");
    const missingConnection = await stub.fetch(
      new Request("https://balancer.test/dashboard/ws", {
        headers: {
          "Sec-WebSocket-Key": webSocketKey,
          "Sec-WebSocket-Version": "13",
          Upgrade: "websocket",
        },
      }),
    );
    const invalidKey = await stub.fetch(
      new Request("https://balancer.test/dashboard/ws", {
        headers: {
          Connection: "Upgrade",
          "Sec-WebSocket-Key": btoa("short"),
          "Sec-WebSocket-Version": "13",
          Upgrade: "websocket",
        },
      }),
    );

    expect(missingConnection.status).toBe(426);
    expect(missingConnection.headers.get("upgrade")).toBe("websocket");
    expect(invalidKey.status).toBe(400);
    const sockets = await runInDurableObject(stub, (_instance, state) =>
      state.getWebSockets("dashboard"),
    );
    expect(sockets).toHaveLength(0);
  });

  it("opens the dashboard socket through the Worker", async () => {
    const response = await exports.default.fetch(
      new Request("https://balancer.test/dashboard/ws", {
        headers: {
          Connection: "Upgrade",
          "Sec-WebSocket-Extensions":
            "permessage-deflate; client_max_window_bits",
          "Sec-WebSocket-Key": webSocketKey,
          "Sec-WebSocket-Version": "13",
          Upgrade: "websocket",
        },
      }),
    );

    expect(response.status).toBe(101);
    expect(response.webSocket).not.toBeNull();
    response.webSocket?.accept();
    response.webSocket?.close();
  });

  it("keeps an unused invite after Durable Object eviction", async () => {
    const stub = env.BALANCER_STATE.getByName("global");
    const invite = await stub.createInvite("https://balancer.test", {
      expiresInSeconds: 300,
    });
    const token = new URL(invite.url).searchParams.get("invite");
    expect(token).toMatch(/^[A-Za-z0-9_-]{43}$/u);
    if (token === null) {
      throw new Error("invite token missing");
    }

    const storedBeforeEviction = await runInDurableObject(
      stub,
      (_instance, state) =>
        state.storage.sql
          .exec<{ count: number }>(
            "SELECT COUNT(*) AS count FROM account_invites",
          )
          .one().count,
    );
    expect(storedBeforeEviction).toBe(1);

    await evictDurableObject(stub);

    const inspection = await stub.inspectInvite(token);
    expect(inspection).not.toBeNull();
    expect(inspection?.expiresAt).toBe(invite.expiresAt);

    const page = await exports.default.fetch(
      new Request(invite.url, { redirect: "manual" }),
    );
    expect(page.status).toBe(303);
    expect(page.headers.get("location")).toBe("/accounts");
    const cookie = page.headers.get("set-cookie");
    expect(cookie).toContain(`__Host-codex-balancer-invite=${token}`);
    expect(cookie).toContain("Secure");
    expect(cookie).toContain("HttpOnly");

    const status = await exports.default.fetch(
      new Request("https://balancer.test/accounts/status", {
        headers: { cookie: cookie ?? "" },
      }),
    );
    expect(status.status).toBe(200);
    expect(await status.json()).toMatchObject({ status: "ready" });

    await evictDurableObject(stub);

    const state = await stub.adminState();
    expect(state.invites).toHaveLength(1);
    expect(state.invites[0]?.usedAt).toBeNull();
  });

  it("binds a consumed invite to the browser session that used it", async () => {
    const stub = env.BALANCER_STATE.getByName("global");
    const invite = await stub.createInvite("https://balancer.test", {
      expiresInSeconds: 300,
    });
    const [firstPage, secondPage] = await Promise.all([
      exports.default.fetch(new Request(invite.url, { redirect: "manual" })),
      exports.default.fetch(new Request(invite.url, { redirect: "manual" })),
    ]);
    const firstCookie = firstPage.headers.get("set-cookie")?.split(";", 1)[0];
    const secondCookie = secondPage.headers.get("set-cookie")?.split(";", 1)[0];
    const firstValue = firstCookie?.split("=", 2)[1];
    const secondValue = secondCookie?.split("=", 2)[1];
    const firstSession = firstValue?.split(".", 2)[1];
    const secondSession = secondValue?.split(".", 2)[1];
    expect(firstSession).toMatch(/^[A-Za-z0-9_-]{43}$/u);
    expect(secondSession).toMatch(/^[A-Za-z0-9_-]{43}$/u);
    expect(secondSession).not.toBe(firstSession);
    if (firstSession === undefined) {
      throw new Error("first invite session missing");
    }

    const nowMs = Date.now();
    const sessionHash = await hashToken(firstSession);
    await runInDurableObject(stub, (_instance, state) => {
      state.storage.sql.exec(
        `UPDATE account_invites SET used_at_ms = ?, session_hash = ?`,
        nowMs,
        sessionHash,
      );
      state.storage.sql.exec(
        `INSERT INTO device_logins (
          login_id, invite_id, device_auth_id, user_code, poll_interval_ms,
          expires_at_ms, next_poll_at_ms, status, verification_url
        ) SELECT 'login-a', invite_id, 'device-a', 'CODE-A', 5000,
          ?, ?, 'pending', 'https://example.com/device'
        FROM account_invites`,
        nowMs + 60_000,
        nowMs + 5_000,
      );
    });

    const [firstStatus, secondStatus, replay] = await Promise.all([
      exports.default.fetch(
        new Request("https://balancer.test/accounts/status", {
          headers: { cookie: firstCookie ?? "" },
        }),
      ),
      exports.default.fetch(
        new Request("https://balancer.test/accounts/status", {
          headers: { cookie: secondCookie ?? "" },
        }),
      ),
      exports.default.fetch(new Request(invite.url, { redirect: "manual" })),
    ]);

    expect(await firstStatus.json()).toMatchObject({
      status: "pending",
      userCode: "CODE-A",
    });
    expect(await secondStatus.json()).toEqual({ status: "expired" });
    expect(replay.status).toBe(404);
    expect(replay.headers.get("set-cookie")).toBeNull();
  });
});
