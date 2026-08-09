import { extractRequestAffinity, turnStateAffinity } from "./affinity.js";
import type {
  AccountGrant,
  AccountId,
  AffinityFailure,
  AffinityRef,
  AffinityResolution,
  RequestAffinity,
  RouteFailureKind,
  SelectAccountInput,
} from "./domain.js";
import {
  downstreamWebSocketHeaders,
  upstreamWebSocketHeaders,
} from "./headers.js";
import {
  parseWebSocketEnvelope,
  usageEmpty,
  webSocketAccountModelUnsupported,
  webSocketEventHeaders,
  webSocketRateLimited,
  webSocketReplaySafe,
  webSocketResponseVisible,
  webSocketStatus,
  type WebSocketEnvelope,
} from "./protocol.js";
import {
  ignoreFailure,
  maxUpstreamRetries,
  observedHeaders,
  sleepWithAbort,
  upstreamRetryDelayMs,
  upstreamRequestSignal,
  type AccountFailure,
  type TransportPort,
} from "./transport-port.js";
import { webSocketHandshakeFailure } from "./websocket-handshake.js";

const maxAttempts = 3;

export const maxWebSocketFrameBytes = 32 << 20;

type WebSocketFetcher = (
  input: RequestInfo | URL,
  init?: RequestInit,
) => Promise<Response>;

interface WebSocketPairValue {
  readonly 0: WebSocket;
  readonly 1: WebSocket;
}

export interface WebSocketProxyOptions {
  readonly fetcher?: WebSocketFetcher;
  readonly now?: () => number;
  readonly pairFactory?: () => WebSocketPairValue;
  readonly port: TransportPort;
  readonly random?: () => number;
  readonly sleep?: (delayMs: number, signal: AbortSignal) => Promise<void>;
  readonly upgradeResponse?: (
    webSocket: WebSocket,
    headers: Headers,
  ) => Response;
  readonly upstreamBaseUrl: string;
}

interface Connection {
  readonly grant: AccountGrant;
  readonly handshakeTurnState: AffinityRef | null;
  readonly response: Response;
  readonly socket: WebSocket;
}

type ConnectResult =
  | Readonly<{ connection: Connection; ok: true }>
  | Readonly<{
      message: string;
      ok: false;
      response: Response | null;
    }>;

interface DialFailure {
  readonly kind: RouteFailureKind;
  readonly ok: false;
  readonly response: Response | null;
}

type DialResult =
  | Readonly<{
      ok: true;
      response: Response;
      socket: WebSocket;
    }>
  | DialFailure;

interface WebSocketTurn {
  readonly affinity: RequestAffinity;
  readonly counted: boolean;
  created: boolean;
  readonly excluded: Set<AccountId>;
  readonly model: string;
  modelRetried: boolean;
  readonly reauthed: Set<AccountId>;
  readonly resolution: AffinityResolution;
  sentAtMs: number;
  readonly serviceTier: string;
  visible: boolean;
  readonly wire: string;
}

const upstreamUrl = (base: string): string =>
  `${base.replace(/\/+$/u, "")}/responses`;

const discardResponse = async (response: Response): Promise<void> => {
  try {
    await response.body?.cancel();
  } catch {
    return;
  }
};

const affinityFailure = (
  failure: AffinityFailure | "no_account",
): Readonly<{ message: string; status: number }> => {
  switch (failure) {
    case "conflict":
      return {
        message: "account-owned affinity sources conflict",
        status: 502,
      };
    case "ambiguous":
      return {
        message: "account-owned affinity is ambiguous",
        status: 503,
      };
    case "owner_unavailable":
    case "no_account":
      return {
        message: "account-owned affinity is unavailable",
        status: 503,
      };
  }
};

const errorResponse = (status: number, message: string): Response =>
  Response.json({ error: { message, type: "balancer_error" } }, { status });

const failedResponse = (response: Response): Response =>
  new Response(response.body, {
    headers: downstreamWebSocketHeaders(response.headers),
    status: response.status,
    statusText: response.statusText,
  });

const dialGrant = async (
  request: Request,
  grant: AccountGrant,
  options: WebSocketProxyOptions,
): Promise<DialResult> => {
  const fetcher = options.fetcher ?? fetch;
  const random = options.random ?? Math.random;
  const sleep = options.sleep ?? sleepWithAbort;
  let retry = 0;
  for (;;) {
    let response: Response;
    try {
      const headers = upstreamWebSocketHeaders(
        request.headers,
        grant.accessToken,
        grant.accountId,
      );
      headers.set("upgrade", "websocket");
      response = await fetcher(upstreamUrl(options.upstreamBaseUrl), {
        headers,
        method: "GET",
        signal: upstreamRequestSignal(request.signal),
      });
    } catch (error) {
      if (request.signal.aborted) {
        throw error;
      }
      return {
        kind: "unreachable",
        ok: false,
        response: null,
      };
    }
    const socket = response.webSocket;
    const selectedProtocol = response.headers.get("Sec-WebSocket-Protocol");
    const selectedExtensions = response.headers.get("Sec-WebSocket-Extensions");
    if (
      response.status === 101 &&
      socket &&
      selectedProtocol === null &&
      selectedExtensions === null
    ) {
      return { ok: true, response, socket };
    }
    if (response.status === 101 && socket) {
      await ignoreFailure(() => {
        socket.accept();
        socket.close(1002, "invalid handshake");
      });
    }
    if (response.status >= 500 && retry < maxUpstreamRetries) {
      await discardResponse(response);
      retry += 1;
      await sleep(upstreamRetryDelayMs(retry, random), request.signal);
      continue;
    }
    const kind =
      response.status === 101
        ? "invalid_handshake"
        : response.status === 401
          ? "unauthorized"
          : response.status === 429
            ? "rate_limited"
            : response.status >= 500
              ? "server_failure"
              : "invalid_handshake";
    return { kind, ok: false, response };
  }
};

const accountFailure = (
  grant: AccountGrant,
  attempt: number,
  dial: DialFailure,
): AccountFailure => ({
  accountId: grant.accountId,
  attempt,
  failedOver: false,
  headers: observedHeaders(dial.response?.headers ?? new Headers()),
  kind: dial.kind,
});

const connect = async (
  request: Request,
  affinity: RequestAffinity,
  model: string,
  serviceTier: string,
  excluded: Set<AccountId>,
  options: WebSocketProxyOptions,
  firstGrant: AccountGrant | null,
): Promise<ConnectResult> => {
  let attempt = 0;
  let initial = firstGrant;
  let pendingFailure: AccountFailure | null = null;
  const reauthed = new Set<AccountId>();
  while (attempt < maxAttempts) {
    let grant: AccountGrant;
    if (initial !== null) {
      grant = initial;
      initial = null;
    } else {
      const input: SelectAccountInput = {
        affinity,
        attempt,
        model,
        requiredAccountId: null,
        serviceTier,
        skipAccountIds: [...excluded],
        transport: "websocket",
      };
      const selected = await options.port.selectAccount(input);
      if (!selected.ok) {
        if (pendingFailure !== null) {
          await options.port.recordFailure(pendingFailure);
        }
        const description = affinityFailure(selected.failure);
        return { message: description.message, ok: false, response: null };
      }
      grant = selected.grant;
    }

    if (pendingFailure !== null) {
      await options.port.recordFailure({
        ...pendingFailure,
        failedOver: pendingFailure.accountId !== grant.accountId,
      });
    }

    let dial = await dialGrant(request, grant, options);
    if (
      !dial.ok &&
      dial.response?.status === 401 &&
      !reauthed.has(grant.accountId)
    ) {
      await discardResponse(dial.response);
      reauthed.add(grant.accountId);
      const refreshed = await options.port.refreshAccount(
        grant.accountId,
        grant.accessToken,
      );
      if (refreshed.ok) {
        grant = { ...grant, accessToken: refreshed.accessToken };
        dial = await dialGrant(request, grant, options);
      } else {
        excluded.add(grant.accountId);
        pendingFailure = accountFailure(grant, attempt, dial);
        attempt += 1;
        continue;
      }
    }

    if (dial.ok) {
      await ignoreFailure(async () => {
        await options.port.observeAccount({
          accountId: grant.accountId,
          headers: observedHeaders(dial.response.headers),
        });
      });
      return {
        connection: {
          grant,
          handshakeTurnState: turnStateAffinity(dial.response.headers),
          response: dial.response,
          socket: dial.socket,
        },
        ok: true,
      };
    }

    const retryable =
      dial.response === null ||
      dial.response.status === 101 ||
      dial.response.status === 401 ||
      dial.response.status === 429 ||
      dial.response.status >= 500;
    if (!retryable) {
      return {
        message: "upstream websocket rejected the connection",
        ok: false,
        response: dial.response,
      };
    }
    if (
      grant.resolution.hard &&
      dial.response !== null &&
      dial.response.status !== 101
    ) {
      await options.port.recordFailure(accountFailure(grant, attempt, dial));
      return {
        message: "upstream websocket rejected the connection",
        ok: false,
        response: dial.response,
      };
    }
    if (dial.response !== null) {
      await discardResponse(dial.response);
    }
    excluded.add(grant.accountId);
    pendingFailure = accountFailure(grant, attempt, dial);
    attempt += 1;
  }
  if (pendingFailure !== null) {
    await options.port.recordFailure(pendingFailure);
  }
  return {
    message: "every account failed this websocket",
    ok: false,
    response: null,
  };
};

const utf8ByteLengthOverLimit = (value: string, limit: number): boolean => {
  let length = 0;
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (code <= 0x7f) {
      length += 1;
    } else if (code <= 0x7ff) {
      length += 2;
    } else if (
      code >= 0xd800 &&
      code <= 0xdbff &&
      index + 1 < value.length &&
      value.charCodeAt(index + 1) >= 0xdc00 &&
      value.charCodeAt(index + 1) <= 0xdfff
    ) {
      length += 4;
      index += 1;
    } else {
      length += 3;
    }
    if (length > limit) {
      return true;
    }
  }
  return false;
};

const frameTooLarge = (value: unknown): boolean => {
  if (typeof value === "string") {
    return utf8ByteLengthOverLimit(value, maxWebSocketFrameBytes);
  }
  if (value instanceof ArrayBuffer) {
    return value.byteLength > maxWebSocketFrameBytes;
  }
  return ArrayBuffer.isView(value)
    ? value.byteLength > maxWebSocketFrameBytes
    : true;
};

const closeSocket = (
  socket: WebSocket,
  code?: number,
  reason?: string,
): void => {
  try {
    if (code === undefined) {
      socket.close();
    } else {
      socket.close(code, reason);
    }
  } catch {
    return;
  }
};

const sendSocket = (socket: WebSocket, value: unknown): boolean => {
  if (
    typeof value !== "string" &&
    !(value instanceof ArrayBuffer) &&
    !ArrayBuffer.isView(value)
  ) {
    return false;
  }
  socket.send(value);
  return true;
};

const uniqueBindings = (
  bindings: readonly AffinityRef[],
  additional: AffinityRef | null,
): readonly AffinityRef[] => {
  if (additional === null) {
    return bindings;
  }
  if (
    bindings.some(
      (binding) =>
        binding.kind === additional.kind && binding.value === additional.value,
    )
  ) {
    return bindings;
  }
  return [...bindings, additional];
};

class WebSocketRelay {
  readonly #downstream: WebSocket;
  readonly #options: WebSocketProxyOptions;
  readonly #request: Request;
  #current: Connection;
  #generation = 1;
  #queue = Promise.resolve();
  #stopped = false;
  #turns: WebSocketTurn[] = [];

  constructor(
    request: Request,
    downstream: WebSocket,
    current: Connection,
    options: WebSocketProxyOptions,
  ) {
    this.#current = current;
    this.#downstream = downstream;
    this.#options = options;
    this.#request = request;
  }

  start(): void {
    this.#listenDownstream();
    this.#listenUpstream(this.#current.socket, this.#generation);
    this.#request.signal.addEventListener(
      "abort",
      () => {
        this.#enqueue(async () => {
          await this.#stop();
        });
      },
      { once: true },
    );
  }

  #enqueue(work: () => Promise<void>): void {
    this.#queue = this.#queue.then(work).catch(async () => {
      await this.#stop();
    });
  }

  #listenDownstream(): void {
    this.#downstream.addEventListener("message", (event) => {
      this.#enqueue(async () => {
        await this.#downstreamMessage(event.data);
      });
    });
    this.#downstream.addEventListener("close", () => {
      this.#enqueue(async () => {
        await this.#stop();
      });
    });
    this.#downstream.addEventListener("error", () => {
      this.#enqueue(async () => {
        await this.#stop();
      });
    });
  }

  #listenUpstream(socket: WebSocket, generation: number): void {
    socket.addEventListener("message", (event) => {
      this.#enqueue(async () => {
        if (generation === this.#generation) {
          await this.#upstreamMessage(event.data);
        }
      });
    });
    socket.addEventListener("close", () => {
      this.#enqueue(async () => {
        if (generation === this.#generation) {
          await this.#upstreamClosed();
        }
      });
    });
    socket.addEventListener("error", () => {
      this.#enqueue(async () => {
        if (generation === this.#generation) {
          await this.#upstreamClosed();
        }
      });
    });
  }

  async #stop(code?: number, reason?: string): Promise<void> {
    if (this.#stopped) {
      return;
    }
    this.#stopped = true;
    closeSocket(this.#downstream, code, reason);
    closeSocket(this.#current.socket, code, reason);
    await ignoreFailure(async () => {
      await this.#options.port.websocketClosed(this.#current.grant.accountId);
    });
  }

  async #switchConnection(next: Connection): Promise<void> {
    next.socket.accept({ allowHalfOpen: true });
    const previous = this.#current;
    this.#generation += 1;
    this.#current = next;
    this.#listenUpstream(next.socket, this.#generation);
    closeSocket(previous.socket);
    await ignoreFailure(async () => {
      await this.#options.port.websocketClosed(previous.grant.accountId);
    });
    await ignoreFailure(async () => {
      await this.#options.port.websocketOpened(next.grant.accountId);
    });
  }

  async #downstreamMessage(value: unknown): Promise<void> {
    if (this.#stopped) {
      return;
    }
    if (frameTooLarge(value)) {
      await this.#stop(1009, "message too big");
      return;
    }
    if (typeof value !== "string") {
      if (!sendSocket(this.#current.socket, value)) {
        await this.#stop(1003, "unsupported message");
      }
      return;
    }
    const event = parseWebSocketEnvelope(value);
    if (event?.type !== "response.create") {
      this.#current.socket.send(value);
      return;
    }
    let payload: unknown;
    try {
      payload = JSON.parse(value) as unknown;
    } catch {
      this.#current.socket.send(value);
      return;
    }
    let affinity: RequestAffinity;
    try {
      affinity = extractRequestAffinity(this.#request.headers, payload);
    } catch (error) {
      this.#writeAffinityError(400, `invalid affinity: ${String(error)}`);
      return;
    }
    const selection: SelectAccountInput = {
      affinity,
      attempt: 0,
      model: event.model,
      requiredAccountId:
        this.#turns.length === 0 ? null : this.#current.grant.accountId,
      serviceTier: event.serviceTier,
      skipAccountIds: [],
      transport: "websocket",
    };
    const selected = await this.#options.port.selectAccount(selection);
    if (!selected.ok) {
      const description = affinityFailure(selected.failure);
      this.#writeAffinityError(description.status, description.message);
      return;
    }
    let resolution = selected.grant.resolution;
    if (selected.grant.accountId !== this.#current.grant.accountId) {
      if (this.#turns.length > 0) {
        const description = affinityFailure("owner_unavailable");
        this.#writeAffinityError(description.status, description.message);
        return;
      }
      const connected = await connect(
        this.#request,
        affinity,
        event.model,
        event.serviceTier,
        new Set<AccountId>(),
        this.#options,
        selected.grant,
      );
      if (!connected.ok) {
        if (connected.response !== null) {
          await discardResponse(connected.response);
        }
        const description = affinityFailure("owner_unavailable");
        this.#writeAffinityError(description.status, description.message);
        return;
      }
      await this.#switchConnection(connected.connection);
      resolution = connected.connection.grant.resolution;
    }
    this.#current.socket.send(value);
    this.#turns.push({
      affinity,
      counted: event.generate !== false,
      created: false,
      excluded: new Set<AccountId>(),
      model: event.model,
      modelRetried: false,
      reauthed: new Set<AccountId>(),
      resolution,
      sentAtMs: (this.#options.now ?? Date.now)(),
      serviceTier: event.serviceTier,
      visible: false,
      wire: value,
    });
  }

  async #upstreamMessage(value: unknown): Promise<void> {
    if (this.#stopped) {
      return;
    }
    if (frameTooLarge(value)) {
      await this.#stop(1009, "message too big");
      return;
    }
    if (typeof value === "string") {
      const event = parseWebSocketEnvelope(value);
      if (event !== null && (await this.#inspectUpstreamEvent(event))) {
        return;
      }
    }
    if (!sendSocket(this.#downstream, value)) {
      await this.#stop(1003, "unsupported message");
    }
  }

  async #inspectUpstreamEvent(event: WebSocketEnvelope): Promise<boolean> {
    const eventHeaders = webSocketEventHeaders(event.headers);
    const observed = observedHeaders(eventHeaders);
    if (Object.keys(observed).length > 0) {
      await ignoreFailure(async () => {
        await this.#options.port.observeAccount({
          accountId: this.#current.grant.accountId,
          headers: observed,
        });
      });
    }
    const first = this.#turns[0];
    if (first !== undefined && webSocketResponseVisible(event)) {
      first.visible = true;
    }
    const previsible =
      this.#turns.length === 1 &&
      first !== undefined &&
      !first.created &&
      !first.visible;
    const identitySafe = webSocketReplaySafe(event);
    if (
      first !== undefined &&
      webSocketStatus(event) === 401 &&
      previsible &&
      identitySafe
    ) {
      const accountId = this.#current.grant.accountId;
      if (!first.reauthed.has(accountId)) {
        first.reauthed.add(accountId);
        const refreshed = await this.#options.port.refreshAccount(
          accountId,
          this.#current.grant.accessToken,
        );
        if (
          refreshed.ok &&
          (await this.#redialCurrent(first, refreshed.accessToken))
        ) {
          return true;
        }
      }
      if (
        !first.resolution.hard &&
        (await this.#replay(first, "unauthorized", observed))
      ) {
        return true;
      }
      await this.#recordEventFailure("unauthorized", observed, false);
    } else if (first !== undefined && webSocketStatus(event) === 401) {
      await this.#recordEventFailure("unauthorized", observed, false);
    }

    const rateLimited = webSocketRateLimited(event);
    let retryKind: RouteFailureKind | null = null;
    if (rateLimited) {
      retryKind = "rate_limited";
    } else if (webSocketStatus(event) >= 500) {
      retryKind = "server_failure";
    } else if (
      first !== undefined &&
      !first.modelRetried &&
      webSocketAccountModelUnsupported(event, first.model)
    ) {
      retryKind = "model_unsupported";
    }
    if (first !== undefined && retryKind !== null) {
      if (
        previsible &&
        !first.resolution.hard &&
        (identitySafe || rateLimited)
      ) {
        if (retryKind === "model_unsupported") {
          first.modelRetried = true;
        }
        if (await this.#replay(first, retryKind, observed)) {
          return true;
        }
      }
      if (retryKind !== "model_unsupported") {
        await this.#recordEventFailure(retryKind, observed, false);
      }
    }

    if (event.type === "response.created") {
      await this.#created(event, eventHeaders, observed);
    } else if (
      event.type === "error" ||
      event.type === "response.completed" ||
      event.type === "response.failed" ||
      event.type === "response.incomplete"
    ) {
      await this.#terminal(event);
    }
    return false;
  }

  async #redialCurrent(
    turn: WebSocketTurn,
    accessToken: string,
  ): Promise<boolean> {
    const grant = { ...this.#current.grant, accessToken };
    const dial = await dialGrant(this.#request, grant, this.#options);
    if (!dial.ok) {
      if (dial.response !== null) {
        await discardResponse(dial.response);
      }
      return false;
    }
    await ignoreFailure(async () => {
      await this.#options.port.observeAccount({
        accountId: grant.accountId,
        headers: observedHeaders(dial.response.headers),
      });
    });
    await this.#switchConnection({
      grant,
      handshakeTurnState: turnStateAffinity(dial.response.headers),
      response: dial.response,
      socket: dial.socket,
    });
    this.#current.socket.send(turn.wire);
    turn.sentAtMs = (this.#options.now ?? Date.now)();
    return true;
  }

  async #replay(
    turn: WebSocketTurn,
    kind: RouteFailureKind,
    headers: ReturnType<typeof observedHeaders>,
  ): Promise<boolean> {
    const failedAccountId = this.#current.grant.accountId;
    turn.excluded.add(failedAccountId);
    const connected = await connect(
      this.#request,
      turn.affinity,
      turn.model,
      turn.serviceTier,
      turn.excluded,
      this.#options,
      null,
    );
    if (!connected.ok) {
      if (connected.response !== null) {
        await discardResponse(connected.response);
      }
      return false;
    }
    if (connected.connection.grant.accountId === failedAccountId) {
      closeSocket(connected.connection.socket);
      return false;
    }
    await this.#switchConnection(connected.connection);
    this.#current.socket.send(turn.wire);
    turn.sentAtMs = (this.#options.now ?? Date.now)();
    await this.#recordEventFailure(kind, headers, true, failedAccountId);
    return true;
  }

  async #recordEventFailure(
    kind: RouteFailureKind,
    headers: ReturnType<typeof observedHeaders>,
    failedOver: boolean,
    accountId = this.#current.grant.accountId,
  ): Promise<void> {
    await ignoreFailure(async () => {
      await this.#options.port.recordFailure({
        accountId,
        attempt: 0,
        failedOver,
        headers,
        kind,
      });
    });
  }

  async #created(
    event: WebSocketEnvelope,
    eventHeaders: Headers,
    headers: ReturnType<typeof observedHeaders>,
  ): Promise<void> {
    const turn = this.#turns.find((candidate) => !candidate.created);
    if (turn === undefined) {
      return;
    }
    turn.created = true;
    const eventTurnState = turnStateAffinity(eventHeaders);
    const bindings = uniqueBindings(
      turn.resolution.bindings,
      this.#current.handshakeTurnState,
    );
    await ignoreFailure(async () => {
      await this.#options.port.recordRoute({
        accountId: this.#current.grant.accountId,
        bindings,
        counted: turn.counted,
        headers,
        transport: "websocket",
        turnState:
          eventTurnState?.value ??
          this.#current.handshakeTurnState?.value ??
          null,
      });
    });
    if (event.response.id !== "") {
      await ignoreFailure(async () => {
        await this.#options.port.claimResponseId(
          this.#current.grant.accountId,
          event.response.id,
        );
      });
    }
    if (turn.counted) {
      const now = this.#options.now ?? Date.now;
      await ignoreFailure(async () => {
        await this.#options.port.answered(now() - turn.sentAtMs);
      });
    }
  }

  async #terminal(event: WebSocketEnvelope): Promise<void> {
    const turn = this.#turns.shift();
    if (
      turn === undefined ||
      !turn.counted ||
      event.type === "error" ||
      usageEmpty(event.response.usage)
    ) {
      return;
    }
    await ignoreFailure(async () => {
      await this.#options.port.recordUsage(event.response.usage);
    });
  }

  async #upstreamClosed(): Promise<void> {
    const turn = this.#turns[0];
    if (
      this.#turns.length === 1 &&
      turn !== undefined &&
      !turn.created &&
      !turn.visible &&
      !turn.resolution.hard
    ) {
      if (await this.#replay(turn, "disconnected", {})) {
        return;
      }
      await this.#recordEventFailure("disconnected", {}, false);
    }
    await this.#stop();
  }

  #writeAffinityError(status: number, message: string): void {
    this.#downstream.send(
      JSON.stringify({
        error: { code: "affinity_error", message },
        status,
        type: "error",
      }),
    );
  }
}

const defaultPairFactory = (): WebSocketPairValue => new WebSocketPair();

const defaultUpgradeResponse = (
  webSocket: WebSocket,
  headers: Headers,
): Response => new Response(null, { headers, status: 101, webSocket });

export const proxyWebSocketResponse = async (
  request: Request,
  options: WebSocketProxyOptions,
): Promise<Response> => {
  const failure = webSocketHandshakeFailure(request);
  if (failure !== null) {
    return new Response(`${failure.message}\n`, {
      headers: failure.headers,
      status: failure.status,
    });
  }
  let affinity: RequestAffinity;
  try {
    affinity = extractRequestAffinity(request.headers, undefined);
  } catch (error) {
    return errorResponse(400, `invalid affinity: ${String(error)}`);
  }
  const connected = await connect(
    request,
    affinity,
    "",
    "",
    new Set<AccountId>(),
    options,
    null,
  );
  if (!connected.ok) {
    return connected.response === null
      ? errorResponse(503, connected.message)
      : failedResponse(connected.response);
  }
  const pair = (options.pairFactory ?? defaultPairFactory)();
  const client = pair[0];
  const server = pair[1];
  try {
    server.accept({ allowHalfOpen: true });
    connected.connection.socket.accept({ allowHalfOpen: true });
    await ignoreFailure(async () => {
      await options.port.websocketOpened(connected.connection.grant.accountId);
    });
    const relay = new WebSocketRelay(
      request,
      server,
      connected.connection,
      options,
    );
    relay.start();
    const responseFactory = options.upgradeResponse ?? defaultUpgradeResponse;
    return responseFactory(
      client,
      downstreamWebSocketHeaders(connected.connection.response.headers),
    );
  } catch (error) {
    closeSocket(server);
    closeSocket(client);
    closeSocket(connected.connection.socket);
    await ignoreFailure(async () => {
      await options.port.websocketClosed(connected.connection.grant.accountId);
    });
    throw error;
  }
};
