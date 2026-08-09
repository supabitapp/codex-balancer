import {
  exportJWK,
  generateKeyPair,
  SignJWT,
  type CryptoKey,
  type JWK,
} from "jose";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { authenticateAccess, type AccessConfig } from "../src/access.js";

const config: AccessConfig = {
  ACCESS_AUD: "access-audience",
  ACCESS_TEAM_DOMAIN: "team.cloudflareaccess.com",
};

let privateKey: CryptoKey;
let publicJwk: JWK;

beforeAll(async () => {
  const pair = await generateKeyPair("RS256", { extractable: true });
  privateKey = pair.privateKey;
  publicJwk = {
    ...(await exportJWK(pair.publicKey)),
    alg: "RS256",
    kid: "access-key",
    use: "sig",
  };
});

afterEach(() => {
  vi.restoreAllMocks();
});

const assertionRequest = (assertion?: string): Request => {
  const headers = new Headers();
  if (assertion !== undefined) {
    headers.set("Cf-Access-Jwt-Assertion", assertion);
  }
  return new Request("https://balancer.example/admin", { headers });
};

const signAssertion = async (
  overrides: {
    audience?: string;
    expiresAt?: number;
    issuer?: string;
    subject?: string;
  } = {},
): Promise<string> =>
  new SignJWT({ email: "friend@example.com" })
    .setProtectedHeader({ alg: "RS256", kid: "access-key" })
    .setIssuer(overrides.issuer ?? "https://team.cloudflareaccess.com")
    .setAudience(overrides.audience ?? "access-audience")
    .setSubject(overrides.subject ?? "user-123")
    .setIssuedAt()
    .setExpirationTime(overrides.expiresAt ?? "5m")
    .sign(privateKey);

const mockCertificates = () =>
  vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
    const requestedURL =
      input instanceof Request
        ? input.url
        : input instanceof URL
          ? input.href
          : input;
    expect(requestedURL).toBe(
      "https://team.cloudflareaccess.com/cdn-cgi/access/certs",
    );
    return Promise.resolve(Response.json({ keys: [publicJwk] }));
  });

describe("Cloudflare Access authentication", () => {
  it("verifies the assertion and caches the remote key set", async () => {
    const fetchCertificates = mockCertificates();
    const assertion = await signAssertion();

    await expect(
      authenticateAccess(assertionRequest(assertion), config),
    ).resolves.toEqual({
      identity: {
        email: "friend@example.com",
        subject: "user-123",
      },
      ok: true,
    });
    await expect(
      authenticateAccess(assertionRequest(assertion), config),
    ).resolves.toMatchObject({ ok: true });
    expect(fetchCertificates).toHaveBeenCalledTimes(1);
  });

  it.each([
    { ACCESS_AUD: "", ACCESS_TEAM_DOMAIN: "team.cloudflareaccess.com" },
    { ACCESS_AUD: "access-audience", ACCESS_TEAM_DOMAIN: "" },
    {
      ACCESS_AUD: "access-audience",
      ACCESS_TEAM_DOMAIN: "https://team.cloudflareaccess.com",
    },
    {
      ACCESS_AUD: "access-audience",
      ACCESS_TEAM_DOMAIN: "team.cloudflareaccess.com/path",
    },
  ])("fails closed for invalid configuration %#", async (invalidConfig) => {
    await expect(
      authenticateAccess(assertionRequest("assertion"), invalidConfig),
    ).resolves.toEqual({
      message: "Cloudflare Access is not configured",
      ok: false,
      status: 500,
    });
  });

  it("rejects a missing assertion without fetching keys", async () => {
    const fetchCertificates = mockCertificates();

    await expect(
      authenticateAccess(assertionRequest(), config),
    ).resolves.toEqual({
      message: "missing or invalid Cloudflare Access assertion",
      ok: false,
      status: 401,
    });
    expect(fetchCertificates).not.toHaveBeenCalled();
  });

  it.each([
    { audience: "wrong-audience" },
    { issuer: "https://other.cloudflareaccess.com" },
    { expiresAt: 1 },
    { subject: "" },
  ])("rejects invalid verified claims %#", async (overrides) => {
    mockCertificates();
    const assertion = await signAssertion(overrides);

    await expect(
      authenticateAccess(assertionRequest(assertion), config),
    ).resolves.toEqual({
      message: "missing or invalid Cloudflare Access assertion",
      ok: false,
      status: 401,
    });
  });
});
