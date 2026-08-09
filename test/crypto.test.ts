import { describe, expect, it } from "vitest";

import {
  decodeJwtPayload,
  decryptSecret,
  encryptSecret,
  hashToken,
  randomToken,
  secureEqual,
} from "../src/crypto.js";

describe("encrypted secrets", () => {
  it("round trips with bound associated data", async () => {
    const key = randomToken();
    const encrypted = await encryptSecret(
      "refresh-token",
      key,
      "account:refresh",
    );

    expect(encrypted).not.toContain("refresh-token");
    await expect(
      decryptSecret(encrypted, key, "account:refresh"),
    ).resolves.toBe("refresh-token");
    await expect(
      decryptSecret(encrypted, key, "other:refresh"),
    ).rejects.toThrow();
  });

  it("rejects malformed keys and ciphertext", async () => {
    await expect(encryptSecret("token", "short", "account")).rejects.toThrow(
      "TOKEN_ENCRYPTION_KEY must contain 32 bytes",
    );
    await expect(
      decryptSecret("v2.invalid.value", randomToken(), "account"),
    ).rejects.toThrow("invalid encrypted secret");
  });
});

describe("opaque identifiers", () => {
  it("hashes tokens without exposing them", async () => {
    expect(await hashToken("invite-token")).toMatch(/^[\w-]{43}$/u);
    expect(await hashToken("invite-token")).toBe(
      await hashToken("invite-token"),
    );
    expect(await hashToken("other-token")).not.toBe(
      await hashToken("invite-token"),
    );
  });

  it("creates independent capability tokens", () => {
    const first = randomToken();
    const second = randomToken();

    expect(first).toMatch(/^[\w-]{43}$/u);
    expect(second).not.toBe(first);
    expect(() => randomToken(8)).toThrow(
      "token length must be at least 16 bytes",
    );
  });
});

describe("JWT payloads", () => {
  it("decodes valid payloads and rejects malformed tokens", () => {
    const payload = btoa(JSON.stringify({ email: "friend@example.com" }))
      .replaceAll("+", "-")
      .replaceAll("/", "_")
      .replace(/=+$/u, "");

    expect(decodeJwtPayload(`header.${payload}.signature`)).toEqual({
      email: "friend@example.com",
    });
    expect(decodeJwtPayload("invalid")).toBeUndefined();
  });
});

describe("secure equality", () => {
  it("compares bearer values", async () => {
    await expect(secureEqual("key", "key")).resolves.toBe(true);
    await expect(secureEqual("key", "wrong")).resolves.toBe(false);
    await expect(secureEqual("", "key")).resolves.toBe(false);
  });
});
