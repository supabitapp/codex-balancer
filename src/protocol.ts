import type { ResponseUsage } from "./domain.js";
import { isRecord } from "./record.js";

interface ResponseRequest {
  readonly model: string;
  readonly serviceTier: string;
}

interface ResponsePayload {
  readonly id: string;
  readonly usage: ResponseUsage;
}

interface ResponseEnvelope extends ResponsePayload {
  readonly object: string;
  readonly type: string;
  readonly response: ResponsePayload;
}

interface ProtocolError {
  readonly code: string;
  readonly message: string;
}

export interface WebSocketEnvelope {
  readonly type: string;
  readonly id: string;
  readonly responseId: string;
  readonly generate: boolean | null;
  readonly model: string;
  readonly serviceTier: string;
  readonly status: number;
  readonly statusCode: number;
  readonly headers: Readonly<Record<string, unknown>>;
  readonly error: ProtocolError;
  readonly response: ResponsePayload &
    Readonly<{
      error: ProtocolError;
    }>;
}

const emptyUsage = (): ResponseUsage => ({
  inputTokensDetails: {
    cachedTokens: 0,
    cacheWriteTokens: 0,
  },
  inputTokens: 0,
  outputTokens: 0,
});

const emptyError = (): ProtocolError => ({ code: "", message: "" });

const stringField = (
  value: Readonly<Record<string, unknown>>,
  name: string,
): string | null => {
  const field = value[name];
  return field === undefined || field === null
    ? ""
    : typeof field === "string"
      ? field
      : null;
};

const integerField = (
  value: Readonly<Record<string, unknown>>,
  name: string,
): number | null => {
  const field = value[name];
  return field === undefined || field === null
    ? 0
    : typeof field === "number" && Number.isSafeInteger(field)
      ? field
      : null;
};

const parseUsage = (value: unknown): ResponseUsage | null => {
  if (value === undefined || value === null) {
    return emptyUsage();
  }
  if (!isRecord(value)) {
    return null;
  }
  const inputTokens = integerField(value, "input_tokens");
  const outputTokens = integerField(value, "output_tokens");
  if (inputTokens === null || outputTokens === null) {
    return null;
  }
  const detailsValue = value.input_tokens_details;
  if (
    detailsValue !== undefined &&
    detailsValue !== null &&
    !isRecord(detailsValue)
  ) {
    return null;
  }
  const details = isRecord(detailsValue) ? detailsValue : {};
  const cachedTokens = integerField(details, "cached_tokens");
  const cacheWriteTokens = integerField(details, "cache_write_tokens");
  if (cachedTokens === null || cacheWriteTokens === null) {
    return null;
  }
  return {
    inputTokensDetails: { cachedTokens, cacheWriteTokens },
    inputTokens,
    outputTokens,
  };
};

const parsePayload = (value: unknown): ResponsePayload | null => {
  if (value === undefined || value === null) {
    return { id: "", usage: emptyUsage() };
  }
  if (!isRecord(value)) {
    return null;
  }
  const id = stringField(value, "id");
  const usage = parseUsage(value.usage);
  if (id === null || usage === null) {
    return null;
  }
  return { id, usage };
};

const parseError = (value: unknown): ProtocolError | null => {
  if (value === undefined || value === null) {
    return emptyError();
  }
  if (!isRecord(value)) {
    return null;
  }
  const code = stringField(value, "code");
  const message = stringField(value, "message");
  return code === null || message === null ? null : { code, message };
};

export const parseJson = (bytes: Uint8Array): unknown =>
  JSON.parse(new TextDecoder().decode(bytes)) as unknown;

export const parseResponseRequest = (payload: unknown): ResponseRequest => {
  if (!isRecord(payload)) {
    return { model: "", serviceTier: "" };
  }
  return {
    model: typeof payload.model === "string" ? payload.model : "",
    serviceTier:
      typeof payload.service_tier === "string" ? payload.service_tier : "",
  };
};

export const parseResponseEnvelope = (
  payload: unknown,
): ResponseEnvelope | null => {
  if (!isRecord(payload)) {
    return null;
  }
  const object = stringField(payload, "object");
  const type = stringField(payload, "type");
  const topLevel = parsePayload(payload);
  const nested = parsePayload(payload.response);
  if (
    object === null ||
    type === null ||
    topLevel === null ||
    nested === null
  ) {
    return null;
  }
  return { ...topLevel, object, response: nested, type };
};

export const responsePayload = (envelope: ResponseEnvelope): ResponsePayload =>
  envelope.object === "response"
    ? {
        id: envelope.id,
        usage: envelope.usage,
      }
    : envelope.response;

export const usageEmpty = (usage: ResponseUsage): boolean =>
  usage.inputTokens === 0 &&
  usage.outputTokens === 0 &&
  usage.inputTokensDetails.cachedTokens === 0 &&
  usage.inputTokensDetails.cacheWriteTokens === 0;

export const parseWebSocketEnvelope = (
  text: string,
): WebSocketEnvelope | null => {
  let payload: unknown;
  try {
    payload = JSON.parse(text) as unknown;
  } catch {
    return null;
  }
  if (!isRecord(payload)) {
    return null;
  }
  const type = stringField(payload, "type");
  const id = stringField(payload, "id");
  const responseId = stringField(payload, "response_id");
  const model = stringField(payload, "model");
  const serviceTier = stringField(payload, "service_tier");
  const status = integerField(payload, "status");
  const statusCode = integerField(payload, "status_code");
  const generateValue = payload.generate;
  const generate =
    generateValue === undefined || generateValue === null
      ? null
      : typeof generateValue === "boolean"
        ? generateValue
        : undefined;
  const headersValue = payload.headers;
  const headers =
    headersValue === undefined || headersValue === null
      ? {}
      : isRecord(headersValue)
        ? headersValue
        : null;
  const error = parseError(payload.error);
  const responseValue = payload.response;
  const nestedPayload = parsePayload(responseValue);
  const nestedError = parseError(
    isRecord(responseValue) ? responseValue.error : undefined,
  );
  if (
    type === null ||
    id === null ||
    responseId === null ||
    model === null ||
    serviceTier === null ||
    status === null ||
    statusCode === null ||
    generate === undefined ||
    headers === null ||
    error === null ||
    nestedPayload === null ||
    nestedError === null
  ) {
    return null;
  }
  return {
    error,
    generate,
    headers,
    id,
    model,
    response: { ...nestedPayload, error: nestedError },
    responseId,
    serviceTier,
    status,
    statusCode,
    type,
  };
};

export const accountModelUnsupported = (
  code: string,
  message: string,
  model: string,
): boolean =>
  code === "invalid_request_error" &&
  model !== "" &&
  message.trim() ===
    `The '${model}' model is not supported when using Codex with a ChatGPT account.`;

export const webSocketStatus = (event: WebSocketEnvelope): number =>
  event.status !== 0 ? event.status : event.statusCode;

export const webSocketErrorCode = (event: WebSocketEnvelope): string =>
  event.error.code !== "" ? event.error.code : event.response.error.code;

export const webSocketErrorMessage = (event: WebSocketEnvelope): string =>
  event.error.message !== ""
    ? event.error.message
    : event.response.error.message;

export const webSocketAccountModelUnsupported = (
  event: WebSocketEnvelope,
  model: string,
): boolean =>
  accountModelUnsupported(
    webSocketErrorCode(event),
    webSocketErrorMessage(event),
    model,
  );

export const webSocketRateLimited = (event: WebSocketEnvelope): boolean => {
  const code = webSocketErrorCode(event);
  return (
    webSocketStatus(event) === 429 ||
    code === "rate_limit_exceeded" ||
    code === "usage_limit_reached"
  );
};

export const webSocketReplaySafe = (event: WebSocketEnvelope): boolean =>
  event.id === "" && event.responseId === "" && event.response.id === "";

export const webSocketResponseVisible = (event: WebSocketEnvelope): boolean =>
  event.type.startsWith("response.") &&
  event.type !== "response.failed" &&
  event.type !== "response.incomplete";

export const webSocketEventHeaders = (
  values: Readonly<Record<string, unknown>>,
): Headers => {
  const headers = new Headers();
  for (const [name, value] of Object.entries(values)) {
    if (
      typeof value !== "string" &&
      typeof value !== "number" &&
      typeof value !== "boolean"
    ) {
      continue;
    }
    try {
      headers.set(name, String(value));
    } catch {
      continue;
    }
  }
  return headers;
};
