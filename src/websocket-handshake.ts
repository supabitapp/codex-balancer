interface WebSocketHandshakeFailure {
  readonly headers: HeadersInit;
  readonly message: string;
  readonly status: 400 | 405 | 426;
}

const validWebSocketKey = (value: string | null): boolean => {
  if (value === null) {
    return false;
  }
  try {
    return atob(value).length === 16;
  } catch {
    return false;
  }
};

export const webSocketHandshakeFailure = (
  request: Request,
): WebSocketHandshakeFailure | null => {
  if (request.method !== "GET") {
    return {
      headers: { Allow: "GET" },
      message: "method not allowed",
      status: 405,
    };
  }
  const connection = (request.headers.get("Connection") ?? "")
    .split(",")
    .some((token) => token.trim().toLowerCase() === "upgrade");
  if (
    request.headers.get("Upgrade")?.toLowerCase() !== "websocket" ||
    !connection
  ) {
    return {
      headers: { Connection: "Upgrade", Upgrade: "websocket" },
      message: "websocket upgrade required",
      status: 426,
    };
  }
  if (
    request.headers.get("Sec-WebSocket-Version") !== "13" ||
    !validWebSocketKey(request.headers.get("Sec-WebSocket-Key"))
  ) {
    return {
      headers: {},
      message: "invalid websocket handshake",
      status: 400,
    };
  }
  return null;
};

const webSocketProtocol = /^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/u;

export const selectedWebSocketProtocolAllowed = (
  request: Request,
  response: Response,
): boolean => {
  const selected = response.headers.get("Sec-WebSocket-Protocol");
  if (selected === null) {
    return true;
  }
  if (!webSocketProtocol.test(selected)) {
    return false;
  }
  return (request.headers.get("Sec-WebSocket-Protocol") ?? "")
    .split(",")
    .map((protocol) => protocol.trim())
    .includes(selected);
};
