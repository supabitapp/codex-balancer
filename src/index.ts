import { authenticateAccess } from "./access.js";
import { authenticateBearer } from "./auth.js";
import type {
  CreateInviteInput,
  InviteInspection,
  ModelEntry,
  OnboardingSnapshot,
  UpdateAccountInput,
} from "./domain.js";
import { randomToken } from "./crypto.js";
import { stateStub, type Env } from "./env.js";
import {
  assetResponse,
  emptyResponse,
  errorResponse,
  HttpError,
  jsonResponse,
  methodNotAllowedResponse,
  readJsonBody,
  redirectResponse,
  requestBodyIsEmpty,
} from "./http.js";
import { proxyHttpResponse } from "./http-proxy.js";
import { asRecord } from "./record.js";
import { modelSlug } from "./routing.js";
import {
  selectedWebSocketProtocolAllowed,
  webSocketHandshakeFailure,
} from "./websocket-handshake.js";
import { proxyWebSocketResponse } from "./websocket-proxy.js";
import { createWorkerPort } from "./worker-port.js";

export { BalancerState } from "./state.js";

const inviteCookieName = "__Host-codex-balancer-invite";
const maximumInviteCookieSeconds = 7 * 24 * 60 * 60;
const maximumClientVersionLength = 256;
const maximumAccountIdLength = 256;
const inviteTokenPattern = /^[A-Za-z0-9_-]{43}$/u;

const keysMatch = (
  value: Record<string, unknown>,
  allowed: ReadonlySet<string>,
): boolean => Object.keys(value).every((key) => allowed.has(key));

const createInviteInput = (value: unknown): CreateInviteInput => {
  const input = asRecord(value);
  if (input === undefined || !keysMatch(input, new Set(["expiresInSeconds"]))) {
    throw new HttpError(400, "invalid invite request");
  }
  if (input.expiresInSeconds === undefined) {
    return {};
  }
  if (
    !Number.isSafeInteger(input.expiresInSeconds) ||
    (input.expiresInSeconds as number) <= 0 ||
    (input.expiresInSeconds as number) > maximumInviteCookieSeconds
  ) {
    throw new HttpError(400, "invalid invite expiry");
  }
  return { expiresInSeconds: input.expiresInSeconds as number };
};

const updateAccountInput = (value: unknown): UpdateAccountInput => {
  const input = asRecord(value);
  if (
    input === undefined ||
    !keysMatch(input, new Set(["paused"])) ||
    Object.keys(input).length !== 1 ||
    typeof input.paused !== "boolean"
  ) {
    throw new HttpError(400, "invalid account update");
  }
  return { paused: input.paused };
};

const bearerFailureResponse = (
  failure: Readonly<{ message: string; status: number }>,
): Response =>
  jsonResponse(
    { error: { message: failure.message, type: "balancer_error" } },
    { status: failure.status },
  );

const authenticateV1 = async (
  request: Request,
  env: Env,
): Promise<Response | undefined> => {
  const result = await authenticateBearer(request, env.BALANCER_KEY);
  return result.ok ? undefined : bearerFailureResponse(result);
};

const healthRoute = async (request: Request, env: Env): Promise<Response> => {
  if (request.method !== "GET") {
    return methodNotAllowedResponse(["GET"]);
  }
  await stateStub(env).health();
  return jsonResponse({ status: "ok", sha: env.GIT_SHA, storage: "ok" });
};

const modelsRoute = async (request: Request, env: Env): Promise<Response> => {
  const failure = await authenticateV1(request, env);
  if (failure !== undefined) {
    return failure;
  }
  if (request.method !== "GET") {
    return methodNotAllowedResponse(["GET"]);
  }
  const url = new URL(request.url);
  const versions = url.searchParams.getAll("client_version");
  if (versions.length > 1) {
    return bearerFailureResponse({
      message: "invalid client version",
      status: 400,
    });
  }
  const version = (versions[0] ?? "").trim();
  if (version.length > maximumClientVersionLength) {
    return bearerFailureResponse({
      message: "invalid client version",
      status: 400,
    });
  }
  const models = await (stateStub(env).models(version) as unknown as Promise<
    readonly ModelEntry[]
  >);
  if (version !== "") {
    return jsonResponse({ models });
  }
  const data = models.flatMap((entry) => {
    const id = modelSlug(entry);
    return id === "" ? [] : [{ id, object: "model", owned_by: "openai" }];
  });
  return jsonResponse({ object: "list", data });
};

const responsesRoute = async (
  request: Request,
  env: Env,
): Promise<Response> => {
  const failure = await authenticateV1(request, env);
  if (failure !== undefined) {
    return failure;
  }
  if (request.method !== "GET" && request.method !== "POST") {
    return methodNotAllowedResponse(["GET", "POST"]);
  }
  const stub = stateStub(env);
  const options = {
    port: createWorkerPort(stub),
    upstreamBaseUrl: env.UPSTREAM_BASE_URL,
  };
  return request.method === "POST"
    ? proxyHttpResponse(request, options)
    : proxyWebSocketResponse(request, options);
};

const dashboardSocketRequest = (request: Request): Request => {
  const headers = new Headers();
  for (const name of [
    "Connection",
    "Sec-WebSocket-Key",
    "Sec-WebSocket-Protocol",
    "Sec-WebSocket-Version",
    "Upgrade",
  ]) {
    const value = request.headers.get(name);
    if (value !== null) {
      headers.set(name, value);
    }
  }
  return new Request(request, { headers });
};

const dashboardSocketRoute = async (
  request: Request,
  env: Env,
): Promise<Response> => {
  const failure = webSocketHandshakeFailure(request);
  if (failure !== null) {
    const response = errorResponse(failure.status, failure.message);
    for (const [name, value] of new Headers(failure.headers)) {
      response.headers.set(name, value);
    }
    return response;
  }
  const response = await stateStub(env).fetch(dashboardSocketRequest(request));
  const safeHeaders = new Set(["sec-websocket-protocol"]);
  const unsafeHeader = [...response.headers].some(
    ([name]) => !safeHeaders.has(name.toLowerCase()),
  );
  if (
    response.status !== 101 ||
    !response.webSocket ||
    unsafeHeader ||
    !selectedWebSocketProtocolAllowed(request, response)
  ) {
    await Promise.allSettled([
      Promise.resolve().then(() =>
        response.webSocket?.close(1008, "invalid handshake"),
      ),
      response.body?.cancel() ?? Promise.resolve(),
    ]);
    return errorResponse(503, "dashboard stream unavailable");
  }
  return response;
};

const inviteToken = (value: string | null): string | undefined =>
  value !== null && inviteTokenPattern.test(value) ? value : undefined;

interface OnboardingCredentials {
  readonly inviteToken: string;
  readonly sessionToken: string;
}

const onboardingCredentials = (
  request: Request,
): OnboardingCredentials | undefined => {
  const header = request.headers.get("Cookie");
  if (header === null || header.length > 8192) {
    return undefined;
  }
  const values = header
    .split(";")
    .map((part) => part.trim())
    .filter((part) => part.startsWith(`${inviteCookieName}=`))
    .map((part) => part.slice(inviteCookieName.length + 1));
  if (values.length !== 1) {
    return undefined;
  }
  const [rawInviteToken, rawSessionToken, extra] = (values[0] ?? "").split(".");
  const parsedInviteToken = inviteToken(rawInviteToken ?? null);
  const sessionToken = inviteToken(rawSessionToken ?? null);
  return parsedInviteToken === undefined ||
    sessionToken === undefined ||
    extra !== undefined
    ? undefined
    : { inviteToken: parsedInviteToken, sessionToken };
};

const expiredInviteCookie = `${inviteCookieName}=; Path=/; Max-Age=0; Secure; HttpOnly; SameSite=Lax`;

const inviteCookie = (
  token: string,
  sessionToken: string,
  inspection: InviteInspection,
): string => {
  const expiresAt = Date.parse(inspection.expiresAt);
  const remaining = Number.isFinite(expiresAt)
    ? Math.ceil((expiresAt - Date.now()) / 1000)
    : 0;
  const maximumAge = Math.max(
    0,
    Math.min(maximumInviteCookieSeconds, remaining),
  );
  return `${inviteCookieName}=${token}.${sessionToken}; Path=/; Max-Age=${String(maximumAge)}; Secure; HttpOnly; SameSite=Lax`;
};

const onboardingResponse = (snapshot: OnboardingSnapshot): Response => {
  const response = jsonResponse(snapshot);
  if (snapshot.status === "complete" || snapshot.status === "expired") {
    response.headers.set("Set-Cookie", expiredInviteCookie);
  }
  return response;
};

const accountsPageRoute = async (
  request: Request,
  env: Env,
): Promise<Response> => {
  const url = new URL(request.url);
  const invites = url.searchParams.getAll("invite");
  if (invites.length === 0) {
    if (request.method !== "GET" && request.method !== "HEAD") {
      return methodNotAllowedResponse(["GET", "HEAD"]);
    }
    return assetResponse(request, env.ASSETS, "/accounts.html");
  }
  if (request.method !== "GET") {
    return methodNotAllowedResponse(["GET"]);
  }
  if (invites.length !== 1) {
    return errorResponse(400, "invalid invite");
  }
  const token = inviteToken(invites[0] ?? null);
  if (token === undefined) {
    return errorResponse(400, "invalid invite");
  }
  const inspection = await stateStub(env).inspectInvite(token);
  if (inspection === null) {
    return errorResponse(404, "invite is invalid or expired");
  }
  const sessionToken = randomToken();
  return redirectResponse(
    "/accounts",
    inviteCookie(token, sessionToken, inspection),
  );
};

const accountStatusRoute = async (
  request: Request,
  env: Env,
): Promise<Response> => {
  if (request.method !== "GET") {
    return methodNotAllowedResponse(["GET"]);
  }
  const credentials = onboardingCredentials(request);
  if (credentials === undefined) {
    return onboardingResponse({ status: "expired" });
  }
  return onboardingResponse(
    await stateStub(env).onboardingStatus(
      credentials.inviteToken,
      credentials.sessionToken,
    ),
  );
};

const accountDeviceRoute = async (
  request: Request,
  env: Env,
): Promise<Response> => {
  if (request.method !== "POST") {
    return methodNotAllowedResponse(["POST"]);
  }
  const credentials = onboardingCredentials(request);
  if (credentials === undefined) {
    return onboardingResponse({ status: "expired" });
  }
  if (!(await requestBodyIsEmpty(request))) {
    return errorResponse(400, "request body must be empty");
  }
  return onboardingResponse(
    await stateStub(env).startDeviceLogin(
      credentials.inviteToken,
      credentials.sessionToken,
    ),
  );
};

const accountIdFromPath = (pathname: string): string | undefined => {
  const encoded = pathname.slice("/admin/accounts/".length);
  if (encoded === "" || encoded.includes("/")) {
    return undefined;
  }
  try {
    const accountId = decodeURIComponent(encoded);
    return accountId !== "" &&
      accountId.length <= maximumAccountIdLength &&
      !accountId.includes("/")
      ? accountId
      : undefined;
  } catch {
    return undefined;
  }
};

const adminApiRoute = async (
  request: Request,
  env: Env,
  pathname: string,
): Promise<Response> => {
  const stub = stateStub(env);
  if (pathname === "/admin/state") {
    return request.method === "GET"
      ? jsonResponse(await stub.adminState())
      : methodNotAllowedResponse(["GET"]);
  }
  if (pathname === "/admin/invites") {
    if (request.method !== "POST") {
      return methodNotAllowedResponse(["POST"]);
    }
    const input = createInviteInput(await readJsonBody(request));
    return jsonResponse(
      await stub.createInvite(new URL(request.url).origin, input),
    );
  }
  if (pathname.startsWith("/admin/accounts/")) {
    const accountId = accountIdFromPath(pathname);
    if (accountId === undefined) {
      return errorResponse(404, "not found");
    }
    if (request.method === "PATCH") {
      const input = updateAccountInput(await readJsonBody(request));
      await stub.updateAccount(accountId, input);
      return emptyResponse();
    }
    if (request.method === "DELETE") {
      await stub.deleteAccount(accountId);
      return emptyResponse();
    }
    return methodNotAllowedResponse(["PATCH", "DELETE"]);
  }
  return errorResponse(404, "not found");
};

const adminRoute = async (
  request: Request,
  env: Env,
  pathname: string,
): Promise<Response> => {
  const authentication = await authenticateAccess(request, env);
  if (!authentication.ok) {
    return errorResponse(authentication.status, authentication.message);
  }
  if (pathname === "/admin") {
    return request.method === "GET" || request.method === "HEAD"
      ? assetResponse(request, env.ASSETS, "/admin.html")
      : methodNotAllowedResponse(["GET", "HEAD"]);
  }
  if (pathname === "/admin.js") {
    return request.method === "GET" || request.method === "HEAD"
      ? assetResponse(request, env.ASSETS, pathname)
      : methodNotAllowedResponse(["GET", "HEAD"]);
  }
  if (pathname === "/admin.html") {
    return errorResponse(404, "not found");
  }
  return adminApiRoute(request, env, pathname);
};

const staticRoute = async (
  request: Request,
  env: Env,
  pathname: string,
): Promise<Response> => {
  if (request.method !== "GET" && request.method !== "HEAD") {
    return methodNotAllowedResponse(["GET", "HEAD"]);
  }
  return assetResponse(request, env.ASSETS, pathname);
};

export const handleRequest = async (
  request: Request,
  env: Env,
): Promise<Response> => {
  try {
    const pathname = new URL(request.url).pathname;
    if (pathname === "/healthz") {
      return await healthRoute(request, env);
    }
    if (pathname === "/v1/models") {
      return await modelsRoute(request, env);
    }
    if (pathname === "/v1/responses") {
      return await responsesRoute(request, env);
    }
    if (pathname === "/stats") {
      if (request.method !== "GET") {
        return methodNotAllowedResponse(["GET"]);
      }
      return jsonResponse(await stateStub(env).dashboard());
    }
    if (pathname === "/dashboard/ws") {
      return await dashboardSocketRoute(request, env);
    }
    if (pathname === "/accounts") {
      return await accountsPageRoute(request, env);
    }
    if (pathname === "/accounts/status") {
      return await accountStatusRoute(request, env);
    }
    if (pathname === "/accounts/device") {
      return await accountDeviceRoute(request, env);
    }
    if (
      pathname === "/admin" ||
      pathname === "/admin.html" ||
      pathname === "/admin.js" ||
      pathname.startsWith("/admin/")
    ) {
      return await adminRoute(request, env, pathname);
    }
    if (pathname === "/") {
      return request.method === "GET" || request.method === "HEAD"
        ? redirectResponse("/dashboard")
        : methodNotAllowedResponse(["GET", "HEAD"]);
    }
    if (pathname === "/dashboard") {
      return await staticRoute(request, env, "/dashboard.html");
    }
    if (pathname === "/dashboard.html") {
      return request.method === "GET" || request.method === "HEAD"
        ? redirectResponse("/dashboard")
        : methodNotAllowedResponse(["GET", "HEAD"]);
    }
    if (pathname === "/accounts.html") {
      return errorResponse(404, "not found");
    }
    if (
      pathname === "/app.css" ||
      pathname === "/accounts.js" ||
      pathname === "/dashboard.js" ||
      pathname === "/shared.js"
    ) {
      return await staticRoute(request, env, pathname);
    }
    return errorResponse(404, "not found");
  } catch (error) {
    return error instanceof HttpError
      ? errorResponse(error.status, error.message)
      : errorResponse(500, "internal server error");
  }
};

const worker: ExportedHandler<Env> = {
  fetch(request, env) {
    return handleRequest(request, env);
  },
};

export default worker;
