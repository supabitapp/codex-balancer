import { reset, runInDurableObject } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { afterEach, describe, expect, it } from "vitest";

const webSocketKey = btoa("0123456789abcdef");

afterEach(async () => {
  await reset();
});

describe("state dashboard", () => {
  it("drops private event kinds from the public snapshot", async () => {
    const stub = env.BALANCER_STATE.getByName("global");
    await stub.health();
    await runInDurableObject(stub, (_instance, state) => {
      const insertEvent = (kind: string, atMs: number): void => {
        state.storage.sql.exec(
          `INSERT INTO events (
            at_ms, kind, account_id, detail, duration_ms, input_tokens,
            cached_tokens, cache_write_tokens, output_tokens
          ) VALUES (?, ?, '', 'private', 0, 0, 0, 0, 0)`,
          atMs,
          kind,
        );
      };
      insertEvent("rate limited", 300);
      for (let index = 0; index < 250; index += 1) {
        insertEvent(
          index % 2 === 0 ? "response answered" : "response usage",
          400 + index,
        );
      }
    });

    const snapshot = await stub.dashboard();

    expect(snapshot.events).toEqual([
      {
        accountAlias: "",
        at: new Date(300).toISOString(),
        detail: "",
        kind: "rate limited",
      },
    ]);
  });

  it("publishes totals derived from route and response events", async () => {
    const stub = env.BALANCER_STATE.getByName("global");
    await stub.health();
    await runInDurableObject(stub, (_instance, state) => {
      state.storage.sql.exec(`
        INSERT INTO events (
          at_ms, kind, account_id, detail, duration_ms, input_tokens,
          cached_tokens, cache_write_tokens, output_tokens
        ) VALUES
          (100, 'route', 'account-a', 'http', 0, 0, 0, 0, 0),
          (101, 'route', 'account-a', 'websocket', 0, 0, 0, 0, 0),
          (102, 'failover', 'account-a', 'rate_limited', 0, 0, 0, 0, 0),
          (103, 'rate limited', 'account-a', '', 0, 0, 0, 0, 0),
          (104, 'response answered', '', '', 10, 0, 0, 0, 0),
          (105, 'response answered', '', '', 30, 0, 0, 0, 0),
          (106, 'response usage', '', '', 0, 100, 40, 15, 25),
          (107, 'response usage', '', '', 0, 20, 5, 3, 7)
      `);
    });

    await expect(stub.dashboard()).resolves.toMatchObject({
      totals: {
        averageFirstByteMilliseconds: 20,
        cacheWriteInputTokens: 18,
        cachedInputTokens: 45,
        failovers: 1,
        inputTokens: 120,
        outputTokens: 32,
        rateLimits: 1,
        turns: 2,
        websocketTurns: 1,
      },
    });
  });

  it("does not broadcast private state changes", async () => {
    const stub = env.BALANCER_STATE.getByName("global");
    const response = await stub.fetch(
      new Request("https://balancer.test/dashboard/ws", {
        headers: {
          Connection: "Upgrade",
          "Sec-WebSocket-Key": webSocketKey,
          "Sec-WebSocket-Version": "13",
          Upgrade: "websocket",
        },
      }),
    );
    const socket = response.webSocket;
    expect(response.status).toBe(101);
    expect(socket).not.toBeNull();
    if (socket === null) {
      throw new Error("dashboard socket missing");
    }
    const messages: string[] = [];
    const firstMessage = new Promise<void>((resolve) => {
      socket.addEventListener("message", (event) => {
        messages.push(String(event.data));
        if (messages.length === 1) {
          resolve();
        }
      });
    });
    socket.accept();
    await firstMessage;

    await stub.answered(12);
    await stub.createInvite("https://balancer.test", {
      expiresInSeconds: 300,
    });
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(messages).toHaveLength(1);
    socket.close();
  });
});
