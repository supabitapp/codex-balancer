export const maximumJsonBodyBytes = 16 << 10;

export class HttpError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "HttpError";
    this.status = status;
  }
}

const documentPolicy =
  "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'";
const dataPolicy =
  "default-src 'none'; base-uri 'none'; frame-ancestors 'none'";

const secureHeaders = (headers: Headers, policy: string): Headers => {
  headers.set("Cache-Control", "no-store");
  headers.set("Content-Security-Policy", policy);
  headers.set("Cross-Origin-Resource-Policy", "same-origin");
  headers.set("Referrer-Policy", "no-referrer");
  headers.set("X-Content-Type-Options", "nosniff");
  headers.set("X-Frame-Options", "DENY");
  return headers;
};

export const jsonResponse = (
  value: unknown,
  init: ResponseInit = {},
): Response => {
  const headers = secureHeaders(new Headers(init.headers), dataPolicy);
  headers.set("Content-Type", "application/json; charset=utf-8");
  return new Response(JSON.stringify(value), { ...init, headers });
};

export const errorResponse = (status: number, message: string): Response =>
  jsonResponse({ error: message }, { status });

export const emptyResponse = (status = 204): Response =>
  new Response(null, {
    headers: secureHeaders(new Headers(), dataPolicy),
    status,
  });

export const methodNotAllowedResponse = (
  allowed: readonly string[],
): Response => {
  const response = errorResponse(405, "method not allowed");
  response.headers.set("Allow", allowed.join(", "));
  return response;
};

export const redirectResponse = (
  location: string,
  cookie?: string,
): Response => {
  const headers = secureHeaders(
    new Headers({ Location: location }),
    dataPolicy,
  );
  if (cookie !== undefined) {
    headers.set("Set-Cookie", cookie);
  }
  return new Response(null, { headers, status: 303 });
};

const readLimitedBody = async (
  request: Request,
  maximumBytes: number,
): Promise<Uint8Array<ArrayBuffer>> => {
  const declared = request.headers.get("Content-Length");
  if (declared !== null) {
    if (!/^\d+$/u.test(declared)) {
      throw new HttpError(400, "invalid content length");
    }
    if (Number(declared) > maximumBytes) {
      throw new HttpError(413, "request body too large");
    }
  }
  if (request.body === null) {
    return new Uint8Array();
  }
  const stream = request.body as ReadableStream<Uint8Array>;
  const reader = stream.getReader();
  const chunks: Uint8Array<ArrayBuffer>[] = [];
  let size = 0;
  try {
    for (;;) {
      const result = await reader.read();
      if (result.done) {
        break;
      }
      const chunk = new Uint8Array(result.value);
      size += chunk.byteLength;
      if (size > maximumBytes) {
        await reader.cancel();
        throw new HttpError(413, "request body too large");
      }
      chunks.push(chunk);
    }
  } catch (error) {
    if (error instanceof HttpError) {
      throw error;
    }
    throw new HttpError(400, "could not read request body");
  } finally {
    reader.releaseLock();
  }
  const combined = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    combined.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return combined;
};

export const requestBodyIsEmpty = async (
  request: Request,
): Promise<boolean> => {
  try {
    await readLimitedBody(request, 0);
    return true;
  } catch (error) {
    if (error instanceof HttpError && error.status === 413) {
      return false;
    }
    throw error;
  }
};

const jsonContentType = (value: string | null): boolean => {
  const mediaType = (value ?? "").split(";", 1)[0]?.trim().toLowerCase();
  return (
    mediaType === "application/json" || mediaType?.endsWith("+json") === true
  );
};

export const readJsonBody = async (
  request: Request,
  maximumBytes = maximumJsonBodyBytes,
): Promise<unknown> => {
  if (!jsonContentType(request.headers.get("Content-Type"))) {
    throw new HttpError(415, "content type must be application/json");
  }
  const body = await readLimitedBody(request, maximumBytes);
  try {
    return JSON.parse(
      new TextDecoder("utf-8", { fatal: true, ignoreBOM: false }).decode(body),
    ) as unknown;
  } catch {
    throw new HttpError(400, "invalid JSON body");
  }
};

export const assetResponse = async (
  request: Request,
  assets: Fetcher,
  pathname: string,
): Promise<Response> => {
  const url = new URL(request.url);
  url.pathname = pathname;
  url.search = "";
  const assetRequest = new Request(url, {
    headers: { Accept: request.headers.get("Accept") ?? "*/*" },
    method: request.method,
  });
  const asset = await assets.fetch(assetRequest);
  const headers = new Headers();
  const contentType = asset.headers.get("Content-Type");
  if (contentType !== null) {
    headers.set("Content-Type", contentType);
  }
  const policy =
    contentType?.toLowerCase().includes("text/html") === true
      ? documentPolicy
      : dataPolicy;
  secureHeaders(headers, policy);
  return new Response(request.method === "HEAD" ? null : asset.body, {
    headers,
    status: asset.status,
    statusText: asset.statusText,
  });
};
