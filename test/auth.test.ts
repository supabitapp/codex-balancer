import { describe, expect, it } from "vitest";

import { authenticateBearer, parseBearerAuthorization } from "../src/auth.js";

const requestWithAuthorization = (authorization?: string): Request => {
  const headers = new Headers();
  if (authorization !== undefined) {
    headers.set("Authorization", authorization);
  }
  return new Request("https://balancer.example/v1/responses", { headers });
};

describe("bearer authorization parsing", () => {
  it("accepts one strict Bearer token", () => {
    expect(parseBearerAuthorization("Bearer abc-123._~+/==")).toBe(
      "abc-123._~+/==",
    );
  });

  it.each([
    null,
    "",
    "key",
    "bearer key",
    "BEARER key",
    "Bearer",
    "Bearer ",
    "Bearer  key",
    "Bearer\tkey",
    " Bearer key",
    "Bearer key ",
    "Bearer key, Bearer second",
  ])("rejects malformed authorization %j", (value) => {
    expect(parseBearerAuthorization(value)).toBeUndefined();
  });
});

describe("bearer authentication", () => {
  it("accepts the configured key", async () => {
    await expect(
      authenticateBearer(
        requestWithAuthorization("Bearer secret-key"),
        "secret-key",
      ),
    ).resolves.toEqual({ ok: true });
  });

  it.each([undefined, "secret-key", "bearer secret-key", "Bearer wrong-key"])(
    "rejects authorization %j",
    async (authorization) => {
      await expect(
        authenticateBearer(
          requestWithAuthorization(authorization),
          "secret-key",
        ),
      ).resolves.toEqual({
        message: "missing or invalid bearer key",
        ok: false,
        status: 401,
      });
    },
  );

  it.each(["", "   "])("fails closed for configured key %j", async (key) => {
    await expect(
      authenticateBearer(requestWithAuthorization("Bearer secret-key"), key),
    ).resolves.toEqual({
      message: "server bearer key is not configured",
      ok: false,
      status: 500,
    });
  });
});
