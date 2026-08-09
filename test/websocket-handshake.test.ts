import { describe, expect, it } from "vitest";

import {
  selectedWebSocketProtocolAllowed,
  webSocketHandshakeFailure,
} from "../src/websocket-handshake.js";

const webSocketKey = btoa("0123456789abcdef");

const request = (mutate?: (headers: Headers) => void): Request => {
  const headers = new Headers({
    Connection: "keep-alive, Upgrade",
    "Sec-WebSocket-Key": webSocketKey,
    "Sec-WebSocket-Version": "13",
    Upgrade: "websocket",
  });
  mutate?.(headers);
  return new Request("https://balancer.test/dashboard/ws", { headers });
};

describe("WebSocket handshake", () => {
  it("accepts a complete upgrade request", () => {
    expect(webSocketHandshakeFailure(request())).toBeNull();
  });

  it("requires the upgrade connection token", () => {
    const failure = webSocketHandshakeFailure(
      request((headers) => {
        headers.set("Connection", "keep-alive");
      }),
    );

    expect(failure).toEqual({
      headers: { Connection: "Upgrade", Upgrade: "websocket" },
      message: "websocket upgrade required",
      status: 426,
    });
  });

  it.each([
    [
      "invalid version",
      (headers: Headers) => {
        headers.set("Sec-WebSocket-Version", "12");
      },
    ],
    [
      "missing key",
      (headers: Headers) => {
        headers.delete("Sec-WebSocket-Key");
      },
    ],
    [
      "short key",
      (headers: Headers) => {
        headers.set("Sec-WebSocket-Key", btoa("short"));
      },
    ],
    [
      "malformed key",
      (headers: Headers) => {
        headers.set("Sec-WebSocket-Key", "not base64");
      },
    ],
  ])("rejects %s", (_name, mutate) => {
    expect(webSocketHandshakeFailure(request(mutate))).toMatchObject({
      status: 400,
    });
  });

  it("accepts only a protocol offered by the client", () => {
    const offered = request((headers) => {
      headers.set("Sec-WebSocket-Protocol", "dashboard.v1, dashboard.v2");
    });

    expect(
      selectedWebSocketProtocolAllowed(
        offered,
        new Response(null, {
          headers: { "Sec-WebSocket-Protocol": "dashboard.v2" },
        }),
      ),
    ).toBe(true);
    expect(
      selectedWebSocketProtocolAllowed(
        offered,
        new Response(null, {
          headers: { "Sec-WebSocket-Protocol": "private" },
        }),
      ),
    ).toBe(false);
    expect(
      selectedWebSocketProtocolAllowed(
        offered,
        new Response(null, {
          headers: { "Sec-WebSocket-Protocol": "dashboard.v1, dashboard.v2" },
        }),
      ),
    ).toBe(false);
  });
});
