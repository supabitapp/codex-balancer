import { describe, expect, it } from "vitest";

import {
  assetResponse,
  HttpError,
  jsonResponse,
  maximumJsonBodyBytes,
  methodNotAllowedResponse,
  readJsonBody,
  redirectResponse,
} from "../src/http.js";

const expectHttpError = async (
  promise: Promise<unknown>,
  status: number,
): Promise<void> => {
  try {
    await promise;
    throw new Error("expected an HTTP error");
  } catch (error) {
    expect(error).toBeInstanceOf(HttpError);
    expect((error as HttpError).status).toBe(status);
  }
};

describe("safe HTTP responses", () => {
  it("marks JSON as private data and sets browser guards", async () => {
    const response = jsonResponse({ ok: true });

    expect(await response.json()).toEqual({ ok: true });
    expect(response.headers.get("cache-control")).toBe("no-store");
    expect(response.headers.get("content-type")).toBe(
      "application/json; charset=utf-8",
    );
    expect(response.headers.get("content-security-policy")).toContain(
      "default-src 'none'",
    );
    expect(response.headers.get("referrer-policy")).toBe("no-referrer");
    expect(response.headers.get("x-content-type-options")).toBe("nosniff");
  });

  it("sets Allow on method failures", async () => {
    const response = methodNotAllowedResponse(["GET", "HEAD"]);

    expect(response.status).toBe(405);
    expect(response.headers.get("allow")).toBe("GET, HEAD");
    expect(await response.json()).toEqual({ error: "method not allowed" });
  });

  it("creates a clean private redirect", () => {
    const response = redirectResponse(
      "/accounts",
      "__Host-invite=token; Path=/; Secure; HttpOnly; SameSite=Lax",
    );

    expect(response.status).toBe(303);
    expect(response.headers.get("location")).toBe("/accounts");
    expect(response.headers.get("cache-control")).toBe("no-store");
    expect(response.headers.get("referrer-policy")).toBe("no-referrer");
    expect(response.headers.get("set-cookie")).toContain("HttpOnly");
  });
});

describe("bounded JSON input", () => {
  it("accepts one JSON value", async () => {
    const request = new Request("https://balancer.test/admin/invites", {
      body: '{"expiresInSeconds":900}',
      headers: { "content-type": "application/json; charset=utf-8" },
      method: "POST",
    });

    await expect(readJsonBody(request)).resolves.toEqual({
      expiresInSeconds: 900,
    });
  });

  it("rejects wrong media types, malformed JSON, and declared excess", async () => {
    await expectHttpError(
      readJsonBody(
        new Request("https://balancer.test/admin/invites", {
          body: "{}",
          headers: { "content-type": "text/plain" },
          method: "POST",
        }),
      ),
      415,
    );
    await expectHttpError(
      readJsonBody(
        new Request("https://balancer.test/admin/invites", {
          body: "{",
          headers: { "content-type": "application/json" },
          method: "POST",
        }),
      ),
      400,
    );
    await expectHttpError(
      readJsonBody(
        new Request("https://balancer.test/admin/invites", {
          body: "{}",
          headers: {
            "content-length": String(maximumJsonBodyBytes + 1),
            "content-type": "application/json",
          },
          method: "POST",
        }),
      ),
      413,
    );
    await expectHttpError(
      readJsonBody(
        new Request("https://balancer.test/admin/invites", {
          body: "x".repeat(maximumJsonBodyBytes + 1),
          headers: {
            "content-length": "0",
            "content-type": "application/json",
          },
          method: "POST",
        }),
      ),
      413,
    );
  });
});

describe("asset responses", () => {
  it("maps canonical HTML without forwarding secrets or asset headers", async () => {
    let fetched: Request | undefined;
    const assets = {
      fetch(request: Request): Promise<Response> {
        fetched = request;
        return Promise.resolve(
          new Response("<main>dashboard</main>", {
            headers: {
              "content-type": "text/html; charset=utf-8",
              "set-cookie": "secret=leak",
              "x-private": "secret",
            },
          }),
        );
      },
    } as Fetcher;
    const response = await assetResponse(
      new Request("https://balancer.test/dashboard?secret=query", {
        headers: {
          authorization: "Bearer secret",
          cookie: "secret=value",
        },
      }),
      assets,
      "/dashboard.html",
    );

    expect(fetched?.url).toBe("https://balancer.test/dashboard.html");
    expect(fetched?.headers.get("authorization")).toBeNull();
    expect(fetched?.headers.get("cookie")).toBeNull();
    expect(response.headers.get("set-cookie")).toBeNull();
    expect(response.headers.get("x-private")).toBeNull();
    expect(response.headers.get("content-security-policy")).toContain(
      "connect-src 'self'",
    );
    expect(await response.text()).toBe("<main>dashboard</main>");
  });
});
