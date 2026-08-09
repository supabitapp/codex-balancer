import { Decompress } from "fzstd";

export const maxRawRequestBody = 16 << 20;
export const maxInspectionBody = 32 << 20;

export class RequestBodyError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "RequestBodyError";
    this.status = status;
  }
}

interface InspectedBody {
  readonly inspection: Uint8Array<ArrayBuffer>;
  readonly wire: Uint8Array<ArrayBuffer>;
}

const combine = (
  chunks: readonly Uint8Array<ArrayBuffer>[],
  length: number,
): Uint8Array<ArrayBuffer> => {
  const body = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return body;
};

const bufferedBytes = 1 << 20;

class ByteAccumulator {
  readonly #maximum: number;
  readonly #overflow: () => RequestBodyError;
  #buffer: Uint8Array<ArrayBuffer> | null = null;
  #chunks: Uint8Array<ArrayBuffer>[] = [];
  #length = 0;

  constructor(maximum: number, overflow: () => RequestBodyError) {
    this.#maximum = maximum;
    this.#overflow = overflow;
  }

  push(value: Uint8Array): void {
    const chunk = new Uint8Array(value);
    if (chunk.byteLength > this.#maximum - this.#length) {
      throw this.#overflow();
    }
    if (
      this.#buffer === null &&
      this.#length + chunk.byteLength <= bufferedBytes
    ) {
      this.#chunks.push(chunk);
      this.#length += chunk.byteLength;
      return;
    }
    if (this.#buffer === null) {
      this.#buffer = new Uint8Array(this.#maximum);
      let offset = 0;
      for (const buffered of this.#chunks) {
        this.#buffer.set(buffered, offset);
        offset += buffered.byteLength;
      }
      this.#chunks = [];
    }
    this.#buffer.set(chunk, this.#length);
    this.#length += chunk.byteLength;
  }

  finish(): Uint8Array<ArrayBuffer> {
    return this.#buffer === null
      ? combine(this.#chunks, this.#length)
      : this.#buffer.subarray(0, this.#length);
  }
}

const invalidZstdBody = (): RequestBodyError =>
  new RequestBodyError(400, "invalid request body: decode zstd");

const expandedBodyTooLarge = (): RequestBodyError =>
  new RequestBodyError(400, "invalid request body: expanded body too large");

const requireBytes = (
  body: Uint8Array<ArrayBuffer>,
  offset: number,
  length: number,
): void => {
  if (offset > body.byteLength - length) {
    throw invalidZstdBody();
  }
};

const readUint32 = (body: Uint8Array<ArrayBuffer>, offset: number): number => {
  requireBytes(body, offset, 4);
  return (
    ((body[offset] ?? 0) |
      ((body[offset + 1] ?? 0) << 8) |
      ((body[offset + 2] ?? 0) << 16) |
      ((body[offset + 3] ?? 0) << 24)) >>>
    0
  );
};

const readBoundedInteger = (
  body: Uint8Array<ArrayBuffer>,
  offset: number,
  length: number,
  maximum: number,
): number | undefined => {
  requireBytes(body, offset, length);
  let value = 0;
  for (let index = length - 1; index >= 0; index -= 1) {
    value = value * 256 + (body[offset + index] ?? 0);
    if (value > maximum) {
      return undefined;
    }
  }
  return value;
};

const preflightZstdFrame = (
  body: Uint8Array<ArrayBuffer>,
  offset: number,
): number => {
  requireBytes(body, offset, 5);
  const descriptor = body[offset + 4] ?? 0;
  if ((descriptor & 8) !== 0) {
    throw invalidZstdBody();
  }

  let cursor = offset + 5;
  const singleSegment = (descriptor & 32) !== 0;
  if (!singleSegment) {
    requireBytes(body, cursor, 1);
    const windowDescriptor = body[cursor] ?? 0;
    const windowBase = 2 ** (10 + (windowDescriptor >>> 3));
    const windowSize = windowBase + (windowBase / 8) * (windowDescriptor & 7);
    if (windowSize > maxInspectionBody) {
      throw expandedBodyTooLarge();
    }
    cursor += 1;
  }

  const dictionaryFlag = descriptor & 3;
  const dictionaryLength = dictionaryFlag === 3 ? 4 : dictionaryFlag;
  requireBytes(body, cursor, dictionaryLength);
  cursor += dictionaryLength;

  const contentSizeFlag = descriptor >>> 6;
  const contentSizeLength =
    contentSizeFlag === 0 ? (singleSegment ? 1 : 0) : 1 << contentSizeFlag;
  const contentSizeBias = contentSizeFlag === 1 ? 256 : 0;
  const contentSize = readBoundedInteger(
    body,
    cursor,
    contentSizeLength,
    maxInspectionBody - contentSizeBias,
  );
  if (contentSize === undefined) {
    throw expandedBodyTooLarge();
  }
  cursor += contentSizeLength;

  for (;;) {
    requireBytes(body, cursor, 3);
    const blockHeader =
      (body[cursor] ?? 0) |
      ((body[cursor + 1] ?? 0) << 8) |
      ((body[cursor + 2] ?? 0) << 16);
    cursor += 3;
    const blockType = (blockHeader >>> 1) & 3;
    if (blockType === 3) {
      throw invalidZstdBody();
    }
    const blockSize = blockHeader >>> 3;
    const payloadSize = blockType === 1 ? 1 : blockSize;
    requireBytes(body, cursor, payloadSize);
    cursor += payloadSize;
    if ((blockHeader & 1) !== 0) {
      const checksumSize = (descriptor & 4) === 0 ? 0 : 4;
      requireBytes(body, cursor, checksumSize);
      return cursor + checksumSize;
    }
  }
};

const preflightZstd = (body: Uint8Array<ArrayBuffer>): void => {
  let offset = 0;
  while (offset < body.byteLength) {
    const magic = readUint32(body, offset);
    if (magic === 0xfd2fb528) {
      offset = preflightZstdFrame(body, offset);
    } else if (magic >= 0x184d2a50 && magic <= 0x184d2a5f) {
      const payloadSize = readUint32(body, offset + 4);
      const payloadOffset = offset + 8;
      requireBytes(body, payloadOffset, payloadSize);
      offset = payloadOffset + payloadSize;
    } else {
      throw invalidZstdBody();
    }
  }
};

const readRawBody = async (
  body: ReadableStream<Uint8Array> | null,
): Promise<Uint8Array<ArrayBuffer>> => {
  if (body === null) {
    return new Uint8Array();
  }
  const reader = body.getReader();
  const accumulator = new ByteAccumulator(
    maxRawRequestBody,
    () => new RequestBodyError(413, "request body too large"),
  );
  try {
    for (;;) {
      const result = await reader.read();
      if (result.done) {
        return accumulator.finish();
      }
      accumulator.push(result.value);
    }
  } catch (error) {
    await reader.cancel(error).catch(() => undefined);
    if (error instanceof RequestBodyError) {
      throw error;
    }
    throw new RequestBodyError(400, "could not read request body");
  } finally {
    reader.releaseLock();
  }
};

const decodeInspection = (
  headers: Headers,
  wire: Uint8Array<ArrayBuffer>,
): Uint8Array<ArrayBuffer> => {
  const encoding = (headers.get("content-encoding") ?? "").trim();
  if (encoding === "" || encoding.toLowerCase() === "identity") {
    return wire;
  }
  if (encoding.toLowerCase() !== "zstd") {
    throw new RequestBodyError(
      400,
      `invalid request body: unsupported content encoding ${JSON.stringify(encoding)}`,
    );
  }
  try {
    preflightZstd(wire);
    const accumulator = new ByteAccumulator(
      maxInspectionBody,
      expandedBodyTooLarge,
    );
    const decompressor = new Decompress((chunk) => {
      accumulator.push(chunk);
    });
    decompressor.push(wire, true);
    return accumulator.finish();
  } catch (error) {
    if (error instanceof RequestBodyError) {
      throw error;
    }
    throw invalidZstdBody();
  }
};

export const inspectRequestBody = async (
  request: Request,
): Promise<InspectedBody> => {
  const declared = request.headers.get("content-length");
  if (declared !== null) {
    if (!/^\d+$/u.test(declared)) {
      throw new RequestBodyError(400, "invalid content length");
    }
    if (Number(declared) > maxRawRequestBody) {
      throw new RequestBodyError(413, "request body too large");
    }
  }
  const wire = await readRawBody(request.body);
  return {
    inspection: decodeInspection(request.headers, wire),
    wire,
  };
};
