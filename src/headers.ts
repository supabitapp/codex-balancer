const httpRequestHeaders = new Set([
  "accept",
  "cache-control",
  "content-encoding",
  "content-type",
  "openai-beta",
  "openai-organization",
  "openai-project",
  "origin",
  "pragma",
  "session-id",
  "session_id",
  "thread-id",
  "user-agent",
  "x-client-request-id",
  "x-codex-conversation-id",
  "x-codex-session-id",
  "x-codex-turn-state",
  "x-stainless-arch",
  "x-stainless-lang",
  "x-stainless-os",
  "x-stainless-package-version",
  "x-stainless-retry-count",
  "x-stainless-runtime",
  "x-stainless-runtime-version",
  "x-stainless-timeout",
]);

const websocketRequestHeaders = new Set(httpRequestHeaders);

const responseDeniedHeaders = new Set([
  "authorization",
  "chatgpt-account-id",
  "connection",
  "content-length",
  "host",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "set-cookie",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
]);

const responsesWebSocketBeta = "responses_websockets=2026-02-06";

const allowedHeaders = (
  source: Headers,
  allowlist: ReadonlySet<string>,
): Headers => {
  const headers = new Headers();
  for (const [name, value] of source) {
    if (allowlist.has(name.toLowerCase())) {
      headers.append(name, value);
    }
  }
  return headers;
};

export const upstreamHttpHeaders = (
  inbound: Headers,
  accessToken: string,
  accountId: string,
): Headers => {
  const headers = allowedHeaders(inbound, httpRequestHeaders);
  headers.delete("accept-encoding");
  headers.set("authorization", `Bearer ${accessToken}`);
  headers.set("chatgpt-account-id", accountId);
  return headers;
};

const normalizedBeta = (headers: Headers): string => {
  const tokens = (headers.get("openai-beta") ?? "")
    .split(",")
    .map((token) => token.trim())
    .filter(
      (token) =>
        token !== "" && token.toLowerCase() !== "responses=experimental",
    );
  if (
    !tokens.some(
      (token) => token.toLowerCase() === responsesWebSocketBeta.toLowerCase(),
    )
  ) {
    tokens.push(responsesWebSocketBeta);
  }
  return tokens.join(", ");
};

export const upstreamWebSocketHeaders = (
  inbound: Headers,
  accessToken: string,
  accountId: string,
): Headers => {
  const headers = allowedHeaders(inbound, websocketRequestHeaders);
  headers.delete("accept");
  headers.delete("content-type");
  headers.set("authorization", `Bearer ${accessToken}`);
  headers.set("chatgpt-account-id", accountId);
  headers.set("openai-beta", normalizedBeta(headers));
  return headers;
};

export const downstreamHeaders = (upstream: Headers): Headers => {
  const denied = new Set(responseDeniedHeaders);
  for (const token of (upstream.get("connection") ?? "").split(",")) {
    const name = token.trim().toLowerCase();
    if (name !== "") {
      denied.add(name);
    }
  }
  const headers = new Headers();
  for (const [name, value] of upstream) {
    if (!denied.has(name.toLowerCase())) {
      headers.append(name, value);
    }
  }
  return headers;
};

export const downstreamWebSocketHeaders = (upstream: Headers): Headers => {
  const headers = downstreamHeaders(upstream);
  headers.delete("sec-websocket-accept");
  headers.delete("sec-websocket-extensions");
  headers.delete("sec-websocket-key");
  headers.delete("sec-websocket-protocol");
  headers.delete("sec-websocket-version");
  return headers;
};
