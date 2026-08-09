import { createRemoteJWKSet, jwtVerify, type RemoteJWKSet } from "jose";

import type { AuthenticationFailure } from "./auth.js";
import type { Env } from "./env.js";

const maximumCachedKeySets = 8;
const keySets = new Map<string, RemoteJWKSet>();

export type AccessConfig = Pick<Env, "ACCESS_AUD" | "ACCESS_TEAM_DOMAIN">;

export interface AccessIdentity {
  readonly email?: string;
  readonly subject: string;
}

export type AccessAuthResult =
  | AuthenticationFailure
  | { readonly identity: AccessIdentity; readonly ok: true };

interface AccessSettings {
  audience: string;
  issuer: string;
  jwks: URL;
}

const accessSettings = (config: AccessConfig): AccessSettings | undefined => {
  const audience = config.ACCESS_AUD.trim();
  const domain = config.ACCESS_TEAM_DOMAIN.trim().toLowerCase();
  if (audience === "" || domain === "" || domain.includes(":")) {
    return undefined;
  }

  let issuer: URL;
  try {
    issuer = new URL(`https://${domain}`);
  } catch {
    return undefined;
  }
  if (
    issuer.hostname !== domain ||
    issuer.host !== domain ||
    issuer.pathname !== "/" ||
    issuer.search !== "" ||
    issuer.hash !== ""
  ) {
    return undefined;
  }

  return {
    audience,
    issuer: issuer.origin,
    jwks: new URL("/cdn-cgi/access/certs", issuer),
  };
};

const keySetFor = (url: URL): RemoteJWKSet => {
  const cacheKey = url.href;
  const cached = keySets.get(cacheKey);
  if (cached !== undefined) {
    keySets.delete(cacheKey);
    keySets.set(cacheKey, cached);
    return cached;
  }

  if (keySets.size === maximumCachedKeySets) {
    const oldest = keySets.keys().next().value;
    if (oldest !== undefined) {
      keySets.delete(oldest);
    }
  }
  const created = createRemoteJWKSet(url, {
    cacheMaxAge: 10 * 60 * 1000,
    cooldownDuration: 30 * 1000,
    timeoutDuration: 5 * 1000,
  });
  keySets.set(cacheKey, created);
  return created;
};

export const authenticateAccess = async (
  request: Request,
  config: AccessConfig,
): Promise<AccessAuthResult> => {
  const settings = accessSettings(config);
  if (settings === undefined) {
    return {
      message: "Cloudflare Access is not configured",
      ok: false,
      status: 500,
    };
  }

  const assertion = request.headers.get("Cf-Access-Jwt-Assertion");
  if (assertion === null || assertion === "") {
    return {
      message: "missing or invalid Cloudflare Access assertion",
      ok: false,
      status: 401,
    };
  }

  try {
    const { payload } = await jwtVerify(assertion, keySetFor(settings.jwks), {
      algorithms: ["RS256"],
      audience: settings.audience,
      clockTolerance: 5,
      issuer: settings.issuer,
      requiredClaims: ["exp", "iat", "sub"],
    });
    if (typeof payload.sub !== "string" || payload.sub === "") {
      throw new Error("Access assertion has no subject");
    }
    const identity: AccessIdentity = { subject: payload.sub };
    if (typeof payload.email === "string" && payload.email !== "") {
      return {
        identity: { ...identity, email: payload.email },
        ok: true,
      };
    }
    return { identity, ok: true };
  } catch {
    return {
      message: "missing or invalid Cloudflare Access assertion",
      ok: false,
      status: 401,
    };
  }
};
