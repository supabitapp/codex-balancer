import { describe, expect, it } from "vitest";

import {
  downstreamHeaders,
  downstreamWebSocketHeaders,
  upstreamHttpHeaders,
  upstreamWebSocketHeaders,
} from "../src/headers.js";

describe("upstream request headers", () => {
  it("forwards only the safe HTTP protocol headers", () => {
    const inbound = new Headers({
      accept: "text/event-stream",
      authorization: "Bearer client",
      "cf-access-jwt-assertion": "access",
      "cf-connecting-ip": "203.0.113.1",
      "content-encoding": "zstd",
      "content-type": "application/json",
      cookie: "secret=value",
      forwarded: "for=203.0.113.1",
      "openai-beta": "custom=1",
      "session-id": "session",
      "x-codex-turn-state": "turn",
      "x-forwarded-for": "203.0.113.1",
      "x-unknown": "value",
    });

    const headers = upstreamHttpHeaders(inbound, "token", "account");

    expect(Object.fromEntries(headers)).toEqual({
      accept: "text/event-stream",
      authorization: "Bearer token",
      "chatgpt-account-id": "account",
      "content-encoding": "zstd",
      "content-type": "application/json",
      "openai-beta": "custom=1",
      "session-id": "session",
      "x-codex-turn-state": "turn",
    });
  });

  it("normalizes the WebSocket beta and transport headers", () => {
    const inbound = new Headers({
      accept: "text/event-stream",
      "content-type": "application/json",
      "openai-beta": "responses=experimental, custom=1",
      "sec-websocket-protocol": "responses",
      "session-id": "session",
    });

    const headers = upstreamWebSocketHeaders(inbound, "token", "account");

    expect(Object.fromEntries(headers)).toEqual({
      authorization: "Bearer token",
      "chatgpt-account-id": "account",
      "openai-beta": "custom=1, responses_websockets=2026-02-06",
      "session-id": "session",
    });
  });
});

describe("downstream response headers", () => {
  it("removes hop-by-hop, credential, and connection-named headers", () => {
    const headers = downstreamHeaders(
      new Headers({
        authorization: "Bearer upstream",
        "chatgpt-account-id": "account",
        connection: "keep-alive, x-private",
        "content-length": "10",
        "content-type": "text/event-stream",
        "set-cookie": "session=secret",
        "x-private": "private",
        "x-public": "public",
      }),
    );

    expect(Object.fromEntries(headers)).toEqual({
      "content-type": "text/event-stream",
      "x-public": "public",
    });
  });

  it("removes upstream WebSocket handshake values", () => {
    const headers = downstreamWebSocketHeaders(
      new Headers({
        "sec-websocket-accept": "upstream",
        "sec-websocket-protocol": "responses",
        "x-public": "public",
      }),
    );

    expect(Object.fromEntries(headers)).toEqual({ "x-public": "public" });
  });
});
