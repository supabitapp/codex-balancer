import { describe, expect, it } from "vitest";

import {
  accountModelUnsupported,
  parseJson,
  parseResponseEnvelope,
  parseResponseRequest,
  parseWebSocketEnvelope,
  responsePayload,
  usageEmpty,
  webSocketAccountModelUnsupported,
  webSocketErrorCode,
  webSocketErrorMessage,
  webSocketEventHeaders,
  webSocketRateLimited,
  webSocketReplaySafe,
  webSocketResponseVisible,
  webSocketStatus,
} from "../src/protocol.js";
import type { WebSocketEnvelope } from "../src/protocol.js";

const encoder = new TextEncoder();

const envelope = (value: unknown): WebSocketEnvelope => {
  const parsed = parseWebSocketEnvelope(JSON.stringify(value));
  if (parsed === null) {
    throw new Error("expected WebSocket envelope");
  }
  return parsed;
};

const responseEnvelope = (value: unknown) => {
  const parsed = parseResponseEnvelope(value);
  if (parsed === null) {
    throw new Error("expected response envelope");
  }
  return parsed;
};

describe("JSON protocol parsing", () => {
  it("parses byte JSON and rejects malformed input", () => {
    expect(parseJson(encoder.encode('{"model":"gpt"}'))).toEqual({
      model: "gpt",
    });
    expect(() => parseJson(encoder.encode("{"))).toThrow();
  });

  it("extracts response request routing fields", () => {
    expect(
      parseResponseRequest({
        model: "gpt-5.6-sol",
        service_tier: "priority",
      }),
    ).toEqual({ model: "gpt-5.6-sol", serviceTier: "priority" });
    expect(parseResponseRequest(null)).toEqual({
      model: "",
      serviceTier: "",
    });
    expect(parseResponseRequest({ model: 42, service_tier: false })).toEqual({
      model: "",
      serviceTier: "",
    });
  });

  it("classifies complete WebSocket envelopes and rejects invalid ones", () => {
    expect(
      parseWebSocketEnvelope(
        JSON.stringify({
          type: "response.create",
          id: "event",
          response_id: "response",
          generate: false,
          model: "gpt-5.6-sol",
          service_tier: "priority",
          status: 400,
          status_code: 429,
          headers: { "retry-after": 12 },
          error: { code: "top", message: "top message" },
          response: {
            id: "nested",
            model: "gpt-5.6-terra",
            service_tier: "default",
            usage: {
              input_tokens: 20,
              output_tokens: 4,
              input_tokens_details: {
                cached_tokens: 8,
                cache_write_tokens: 3,
              },
            },
            error: { code: "nested", message: "nested message" },
          },
        }),
      ),
    ).toEqual({
      type: "response.create",
      id: "event",
      responseId: "response",
      generate: false,
      model: "gpt-5.6-sol",
      serviceTier: "priority",
      status: 400,
      statusCode: 429,
      headers: { "retry-after": 12 },
      error: { code: "top", message: "top message" },
      response: {
        id: "nested",
        usage: {
          inputTokens: 20,
          outputTokens: 4,
          inputTokensDetails: { cachedTokens: 8, cacheWriteTokens: 3 },
        },
        error: { code: "nested", message: "nested message" },
      },
    });

    expect(parseWebSocketEnvelope("{")).toBeNull();
    expect(parseWebSocketEnvelope("[]")).toBeNull();
    expect(parseWebSocketEnvelope('{"status":1.5}')).toBeNull();
    expect(parseWebSocketEnvelope('{"generate":"yes"}')).toBeNull();
    expect(parseWebSocketEnvelope('{"response":[]}')).toBeNull();
  });

  it("uses null and missing WebSocket fields as Go zero values", () => {
    expect(parseWebSocketEnvelope("{}")).toEqual({
      type: "",
      id: "",
      responseId: "",
      generate: null,
      model: "",
      serviceTier: "",
      status: 0,
      statusCode: 0,
      headers: {},
      error: { code: "", message: "" },
      response: {
        id: "",
        usage: {
          inputTokens: 0,
          outputTokens: 0,
          inputTokensDetails: { cachedTokens: 0, cacheWriteTokens: 0 },
        },
        error: { code: "", message: "" },
      },
    });
  });
});

describe("response envelopes", () => {
  it("selects root payloads for plain responses", () => {
    const parsed = responseEnvelope({
      object: "response",
      id: "resp_plain",
      model: "gpt-5.6-sol",
      service_tier: "priority",
      usage: {
        input_tokens: 12,
        output_tokens: 3,
        input_tokens_details: {
          cached_tokens: 7,
          cache_write_tokens: 2,
        },
      },
    });

    expect(responsePayload(parsed)).toEqual({
      id: "resp_plain",
      usage: {
        inputTokens: 12,
        outputTokens: 3,
        inputTokensDetails: { cachedTokens: 7, cacheWriteTokens: 2 },
      },
    });
  });

  it("selects nested payloads for streamed events", () => {
    const parsed = responseEnvelope({
      type: "response.completed",
      id: "top",
      response: {
        id: "nested",
        usage: { input_tokens_details: { cache_write_tokens: 5 } },
      },
    });

    const payload = responsePayload(parsed);
    expect(payload.id).toBe("nested");
    expect(usageEmpty(payload.usage)).toBe(false);
    expect(usageEmpty(responsePayload(responseEnvelope({})).usage)).toBe(true);
  });
});

describe("unsupported model classification", () => {
  const model = "gpt-5.6-sol";
  const message =
    "The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account.";

  it("requires the exact code, model, and trimmed message", () => {
    expect(
      accountModelUnsupported("invalid_request_error", message, model),
    ).toBe(true);
    expect(
      accountModelUnsupported(
        "invalid_request_error",
        ` \n${message}\t`,
        model,
      ),
    ).toBe(true);
    expect(accountModelUnsupported("other", message, model)).toBe(false);
    expect(accountModelUnsupported("invalid_request_error", message, "")).toBe(
      false,
    );
    expect(
      accountModelUnsupported(
        "invalid_request_error",
        `${message} ` + "extra",
        model,
      ),
    ).toBe(false);
  });

  it("reads WebSocket error fields using independent top-level precedence", () => {
    const event = envelope({
      error: { code: "invalid_request_error" },
      response: { error: { code: "nested", message } },
    });

    expect(webSocketErrorCode(event)).toBe("invalid_request_error");
    expect(webSocketErrorMessage(event)).toBe(message);
    expect(webSocketAccountModelUnsupported(event, model)).toBe(true);
  });
});

describe("WebSocket event classification", () => {
  it("prefers a nonzero status over status_code", () => {
    expect(webSocketStatus(envelope({ status: 400, status_code: 429 }))).toBe(
      400,
    );
    expect(webSocketStatus(envelope({ status_code: 429 }))).toBe(429);
  });

  it("detects rate limits from status or either error code location", () => {
    expect(webSocketRateLimited(envelope({ status: 429 }))).toBe(true);
    expect(
      webSocketRateLimited({
        ...envelope({}),
        error: { code: "usage_limit_reached", message: "" },
      }),
    ).toBe(true);
    expect(
      webSocketRateLimited(
        envelope({ response: { error: { code: "rate_limit_exceeded" } } }),
      ),
    ).toBe(true);
    expect(webSocketRateLimited(envelope({ status: 502 }))).toBe(false);
  });

  it("requires all response identity fields to be empty for safe replay", () => {
    expect(webSocketReplaySafe(envelope({}))).toBe(true);
    expect(webSocketReplaySafe(envelope({ id: "event" }))).toBe(false);
    expect(webSocketReplaySafe(envelope({ response_id: "response" }))).toBe(
      false,
    );
    expect(webSocketReplaySafe(envelope({ response: { id: "nested" } }))).toBe(
      false,
    );
  });

  it("marks semantic response events visible except failed and incomplete", () => {
    expect(
      webSocketResponseVisible(envelope({ type: "response.created" })),
    ).toBe(true);
    expect(
      webSocketResponseVisible(
        envelope({ type: "response.output_text.delta" }),
      ),
    ).toBe(true);
    expect(
      webSocketResponseVisible(envelope({ type: "response.completed" })),
    ).toBe(true);
    expect(
      webSocketResponseVisible(envelope({ type: "response.failed" })),
    ).toBe(false);
    expect(
      webSocketResponseVisible(envelope({ type: "response.incomplete" })),
    ).toBe(false);
    expect(webSocketResponseVisible(envelope({ type: "error" }))).toBe(false);
  });

  it("turns scalar event header values into headers", () => {
    expect(
      Object.fromEntries(
        webSocketEventHeaders({
          "x-string": "value",
          "x-number": 12.5,
          "x-true": true,
          "x-false": false,
          "x-null": null,
          "x-array": ["value"],
          "x-object": { value: true },
        }),
      ),
    ).toEqual({
      "x-false": "false",
      "x-number": "12.5",
      "x-string": "value",
      "x-true": "true",
    });
  });
});
