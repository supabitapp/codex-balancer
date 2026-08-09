import { extractRequestAffinity } from "./affinity.js";
import { inspectRequestBody, RequestBodyError } from "./body.js";
import type { AccountGrant, SelectAccountInput } from "./domain.js";
import { downstreamHeaders, upstreamHttpHeaders } from "./headers.js";
import { accountModelUnsupported, parseResponseRequest } from "./protocol.js";
import { isRecord } from "./record.js";
import { ResponseStreamInspector } from "./sse.js";
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

const maxAttempts = 3;
const maxUpstreamErrorBody = 64 << 10;

type Fetcher = (
  input: RequestInfo | URL,
  init?: RequestInit,
) => Promise<Response>;

export interface HttpProxyOptions {
  readonly fetcher?: Fetcher;
  readonly now?: () => number;
  readonly port: TransportPort;
  readonly random?: () => number;
  readonly sleep?: (delayMs: number, signal: AbortSignal) => Promise<void>;
  readonly upstreamBaseUrl: string;
}

interface SentResponse {
  readonly response: Response;
  readonly sentAtMs: number;
}

interface PrimedBody {
  readonly first: Uint8Array<ArrayBuffer>;
  readonly reader: ReadableStreamDefaultReader<Uint8Array>;
}

export { upstreamRetryBackoffMs } from "./transport-port.js";

const upstreamUrl = (base: string): string =>
  `${base.replace(/\/+$/u, "")}/responses`;

const errorResponse = (status: number, message: string): Response =>
  Response.json({ error: { message, type: "balancer_error" } }, { status });

const byteBody = (response: Response): ReadableStream<Uint8Array> | null =>
  response.body as ReadableStream<Uint8Array> | null;

const selectionError = (
  failure: "ambiguous" | "conflict" | "no_account" | "owner_unavailable",
  exhausted: boolean,
): Response => {
  switch (failure) {
    case "conflict":
      return errorResponse(502, "account-owned affinity sources conflict");
    case "ambiguous":
      return errorResponse(503, "account-owned affinity is ambiguous");
    case "owner_unavailable":
      return errorResponse(503, "account-owned affinity is unavailable");
    case "no_account":
      return errorResponse(
        503,
        exhausted ? "every account failed this turn" : "no account available",
      );
  }
};

const discard = async (response: Response): Promise<void> => {
  try {
    await response.body?.cancel();
  } catch {
    return;
  }
};

const readLimitedText = async (
  response: Response,
  limit: number,
): Promise<string> => {
  const body = byteBody(response);
  if (body === null) {
    return "";
  }
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let length = 0;
  try {
    while (length < limit) {
      const result = await reader.read();
      if (result.done) {
        break;
      }
      const remaining = limit - length;
      const chunk = result.value.subarray(0, remaining);
      chunks.push(chunk);
      length += chunk.byteLength;
      if (result.value.byteLength > remaining) {
        break;
      }
    }
  } finally {
    await reader.cancel();
    reader.releaseLock();
  }
  const bytes = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder().decode(bytes);
};

const unsupportedModelResponse = async (
  response: Response,
  model: string,
): Promise<boolean> => {
  if (response.status !== 400 || model === "") {
    return false;
  }
  let value: unknown;
  try {
    value = JSON.parse(
      await readLimitedText(response.clone(), maxUpstreamErrorBody),
    ) as unknown;
  } catch {
    return false;
  }
  if (!isRecord(value)) {
    return false;
  }
  const error = value.error;
  if (!isRecord(error)) {
    return false;
  }
  const code = error.code;
  const message = error.message;
  return accountModelUnsupported(
    typeof code === "string" ? code : "",
    typeof message === "string" ? message : "",
    model,
  );
};

const send = async (
  request: Request,
  wire: Uint8Array<ArrayBuffer>,
  grant: AccountGrant,
  options: HttpProxyOptions,
): Promise<SentResponse> => {
  const fetcher = options.fetcher ?? fetch;
  const now = options.now ?? Date.now;
  const random = options.random ?? Math.random;
  const sleep = options.sleep ?? sleepWithAbort;
  let retry = 0;
  for (;;) {
    const sentAtMs = now();
    const response = await fetcher(upstreamUrl(options.upstreamBaseUrl), {
      body: wire,
      headers: upstreamHttpHeaders(
        request.headers,
        grant.accessToken,
        grant.accountId,
      ),
      method: "POST",
      signal: upstreamRequestSignal(request.signal),
    });
    if (response.status < 500 || retry === maxUpstreamRetries) {
      return { response, sentAtMs };
    }
    await discard(response);
    retry += 1;
    await sleep(upstreamRetryDelayMs(retry, random), request.signal);
  }
};

const prime = async (response: Response): Promise<PrimedBody> => {
  const body = byteBody(response);
  if (body === null) {
    throw new Error("upstream response ended before its body");
  }
  const reader = body.getReader();
  try {
    for (;;) {
      const result = await reader.read();
      if (result.done) {
        throw new Error("upstream response ended before its body");
      }
      if (result.value.byteLength > 0) {
        return { first: new Uint8Array(result.value), reader };
      }
    }
  } catch (error) {
    await reader.cancel();
    reader.releaseLock();
    throw error;
  }
};

const relayBody = (
  source: Response,
  primed: PrimedBody | null,
  inspector: ResponseStreamInspector,
  answered: () => Promise<void>,
): ReadableStream<Uint8Array> | null => {
  if (source.body === null && primed === null) {
    void inspector.finish();
    return null;
  }
  const reader = primed?.reader ?? byteBody(source)?.getReader();
  if (reader === undefined) {
    void inspector.finish();
    return null;
  }
  let first = true;
  let prefix = primed?.first ?? null;
  return new ReadableStream<Uint8Array>({
    async cancel(reason) {
      await reader.cancel(reason);
      reader.releaseLock();
    },
    async pull(controller) {
      try {
        const chunk = prefix;
        prefix = null;
        const result =
          chunk === null
            ? await reader.read()
            : { done: false as const, value: chunk };
        if (result.done) {
          await inspector.finish();
          reader.releaseLock();
          controller.close();
          return;
        }
        await inspector.write(result.value);
        controller.enqueue(result.value);
        if (first) {
          first = false;
          await answered();
        }
      } catch (error) {
        await inspector.finish();
        await reader.cancel(error);
        reader.releaseLock();
        controller.error(error);
      }
    },
  });
};

const failure = (
  grant: AccountGrant,
  attempt: number,
  kind: AccountFailure["kind"],
  headers: Headers,
): AccountFailure => ({
  accountId: grant.accountId,
  attempt,
  failedOver: false,
  headers: observedHeaders(headers),
  kind,
});

export const proxyHttpResponse = async (
  request: Request,
  options: HttpProxyOptions,
): Promise<Response> => {
  let body;
  try {
    body = await inspectRequestBody(request);
  } catch (error) {
    if (error instanceof RequestBodyError) {
      return errorResponse(error.status, error.message);
    }
    throw error;
  }

  let payload: unknown;
  try {
    payload =
      body.inspection.byteLength === 0
        ? undefined
        : (JSON.parse(new TextDecoder().decode(body.inspection)) as unknown);
  } catch (error) {
    return errorResponse(400, `invalid affinity: ${String(error)}`);
  }
  let affinity;
  try {
    affinity = extractRequestAffinity(request.headers, payload);
  } catch (error) {
    return errorResponse(400, `invalid affinity: ${String(error)}`);
  }
  const responseRequest = parseResponseRequest(payload);
  const excluded = new Set<string>();
  const reauthed = new Set<string>();
  let modelRetried = false;
  let pendingFailure: AccountFailure | null = null;
  let preservedResponse: Response | null = null;
  let attempt = 0;

  while (attempt < maxAttempts) {
    const selection: SelectAccountInput = {
      affinity,
      attempt,
      model: responseRequest.model,
      requiredAccountId: null,
      serviceTier: responseRequest.serviceTier,
      skipAccountIds: [...excluded],
      transport: "http",
    };
    const selected = await options.port.selectAccount(selection);
    if (!selected.ok) {
      if (preservedResponse !== null) {
        return new Response(preservedResponse.body, {
          headers: downstreamHeaders(preservedResponse.headers),
          status: preservedResponse.status,
          statusText: preservedResponse.statusText,
        });
      }
      if (pendingFailure !== null) {
        await options.port.recordFailure(pendingFailure);
      }
      return selectionError(selected.failure, false);
    }
    if (pendingFailure !== null) {
      await options.port.recordFailure({
        ...pendingFailure,
        failedOver: pendingFailure.accountId !== selected.grant.accountId,
      });
    }
    if (preservedResponse !== null) {
      await discard(preservedResponse);
      preservedResponse = null;
    }
    let grant = selected.grant;
    let sent: SentResponse;
    try {
      sent = await send(request, body.wire, grant, options);
    } catch (error) {
      if (request.signal.aborted) {
        throw error;
      }
      excluded.add(grant.accountId);
      pendingFailure = failure(grant, attempt, "unreachable", new Headers());
      attempt += 1;
      continue;
    }

    let response = sent.response;
    if (response.status === 401 && !reauthed.has(grant.accountId)) {
      await discard(response);
      reauthed.add(grant.accountId);
      const refreshed = await options.port.refreshAccount(
        grant.accountId,
        grant.accessToken,
      );
      if (refreshed.ok) {
        grant = { ...grant, accessToken: refreshed.accessToken };
        try {
          sent = await send(request, body.wire, grant, options);
          response = sent.response;
        } catch (error) {
          if (request.signal.aborted) {
            throw error;
          }
          excluded.add(grant.accountId);
          pendingFailure = failure(
            grant,
            attempt,
            "unreachable",
            new Headers(),
          );
          attempt += 1;
          continue;
        }
      } else {
        excluded.add(grant.accountId);
        pendingFailure = failure(grant, attempt, "unauthorized", new Headers());
        attempt += 1;
        continue;
      }
    }

    const unsupportedModel = await unsupportedModelResponse(
      response,
      responseRequest.model,
    );
    const retryable =
      response.status === 429 ||
      response.status === 401 ||
      response.status >= 500 ||
      unsupportedModel;
    if (retryable) {
      const kind =
        response.status === 429
          ? "rate_limited"
          : response.status === 401
            ? "unauthorized"
            : unsupportedModel
              ? "model_unsupported"
              : "server_failure";
      if (grant.resolution.hard || (unsupportedModel && modelRetried)) {
        if (!unsupportedModel) {
          await options.port.recordFailure(
            failure(grant, attempt, kind, response.headers),
          );
        }
        return new Response(response.body, {
          headers: downstreamHeaders(response.headers),
          status: response.status,
          statusText: response.statusText,
        });
      }
      excluded.add(grant.accountId);
      pendingFailure = failure(grant, attempt, kind, response.headers);
      attempt += 1;
      if (unsupportedModel) {
        modelRetried = true;
        preservedResponse = response;
      } else {
        await discard(response);
      }
      continue;
    }

    let primed: PrimedBody | null = null;
    if (response.status >= 200 && response.status < 300) {
      try {
        primed = await prime(response);
      } catch {
        excluded.add(grant.accountId);
        pendingFailure = failure(
          grant,
          attempt,
          "empty_response",
          response.headers,
        );
        attempt += 1;
        if (grant.resolution.hard) {
          await options.port.recordFailure(pendingFailure);
          return errorResponse(502, "upstream response ended before its body");
        }
        continue;
      }
    }

    const headers = observedHeaders(response.headers);
    const rawTurnState = response.headers.get("x-codex-turn-state")?.trim();
    const turnState = rawTurnState === "" ? null : (rawTurnState ?? null);
    await ignoreFailure(async () => {
      await options.port.recordRoute({
        accountId: grant.accountId,
        bindings: grant.resolution.bindings,
        counted: true,
        headers,
        transport: "http",
        turnState,
      });
    });
    const inspector = new ResponseStreamInspector({
      onResponseId: async (responseId) => {
        try {
          await options.port.claimResponseId(grant.accountId, responseId);
        } catch {
          return;
        }
      },
      onUsage: async (usage) => {
        try {
          await options.port.recordUsage(usage);
        } catch {
          return;
        }
      },
    });
    return new Response(
      relayBody(response, primed, inspector, async () => {
        const now = options.now ?? Date.now;
        try {
          await options.port.answered(now() - sent.sentAtMs);
        } catch {
          return;
        }
      }),
      {
        headers: downstreamHeaders(response.headers),
        status: response.status,
        statusText: response.statusText,
      },
    );
  }

  if (pendingFailure !== null) {
    await options.port.recordFailure(pendingFailure);
  }
  return errorResponse(503, "every account failed this turn");
};
