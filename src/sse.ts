import {
  parseJson,
  parseResponseEnvelope,
  responsePayload,
  usageEmpty,
} from "./protocol.js";
import type { ResponseUsage } from "./domain.js";

export const maxResponseEventLine = 1 << 20;

interface ResponseInspectorOptions {
  readonly onResponseId: (id: string) => void | Promise<void>;
  readonly onUsage: (usage: ResponseUsage) => void | Promise<void>;
}

const carriageReturn = 13;
const lineFeed = 10;
const dataPrefix = Uint8Array.of(100, 97, 116, 97, 58);
const emptyBytes = (): Uint8Array => new Uint8Array();

const appendBytes = (first: Uint8Array, second: Uint8Array): Uint8Array => {
  if (first.byteLength === 0) {
    return second.slice();
  }
  if (second.byteLength === 0) {
    return first;
  }
  const joined = new Uint8Array(first.byteLength + second.byteLength);
  joined.set(first);
  joined.set(second, first.byteLength);
  return joined;
};

const nextEnding = (bytes: Uint8Array, start: number): number => {
  for (let index = start; index < bytes.byteLength; index += 1) {
    const value = bytes[index];
    if (value === carriageReturn || value === lineFeed) {
      return index;
    }
  }
  return -1;
};

const isSpace = (value: number): boolean =>
  value === 9 ||
  value === 10 ||
  value === 11 ||
  value === 12 ||
  value === 13 ||
  value === 32;

const trimBytes = (bytes: Uint8Array): Uint8Array => {
  let start = 0;
  let end = bytes.byteLength;
  while (start < end && isSpace(bytes[start] ?? 0)) {
    start += 1;
  }
  while (end > start && isSpace(bytes[end - 1] ?? 0)) {
    end -= 1;
  }
  return bytes.subarray(start, end);
};

const hasDataPrefix = (bytes: Uint8Array): boolean => {
  if (bytes.byteLength < dataPrefix.byteLength) {
    return false;
  }
  for (let index = 0; index < dataPrefix.byteLength; index += 1) {
    if (bytes[index] !== dataPrefix[index]) {
      return false;
    }
  }
  return true;
};

const recordsUsage = (object: string, type: string): boolean =>
  object === "response" ||
  type === "response.completed" ||
  type === "response.failed" ||
  type === "response.incomplete";

export class ResponseStreamInspector {
  readonly #options: ResponseInspectorOptions;
  #line = emptyBytes();
  #event = emptyBytes();
  #afterCarriageReturn = false;
  #discardLine = false;
  #discardEvent = false;
  #usageRecorded = false;

  constructor(options: ResponseInspectorOptions) {
    this.#options = options;
  }

  async write(data: Uint8Array): Promise<void> {
    let offset = 0;
    if (this.#afterCarriageReturn) {
      if (data[0] === lineFeed) {
        offset = 1;
      }
      this.#afterCarriageReturn = false;
    }
    while (offset < data.byteLength) {
      if (this.#discardLine) {
        const endingIndex = nextEnding(data, offset);
        if (endingIndex < 0) {
          return;
        }
        offset = this.#advanceEnding(data, endingIndex);
        this.#discardLine = false;
        continue;
      }
      const endingIndex = nextEnding(data, offset);
      if (endingIndex < 0) {
        const tail = data.subarray(offset);
        if (this.#line.byteLength + tail.byteLength > maxResponseEventLine) {
          this.#line = emptyBytes();
          this.#discardLine = true;
        } else {
          this.#line = appendBytes(this.#line, tail);
        }
        return;
      }
      const segment = data.subarray(offset, endingIndex);
      if (this.#line.byteLength + segment.byteLength > maxResponseEventLine) {
        this.#line = emptyBytes();
        this.#event = emptyBytes();
        this.#discardEvent = true;
      } else {
        const line = appendBytes(this.#line, segment);
        this.#line = emptyBytes();
        await this.#acceptLine(line);
      }
      offset = this.#advanceEnding(data, endingIndex);
    }
  }

  async finish(): Promise<void> {
    if (this.#line.byteLength > 0) {
      await this.#acceptLine(this.#line);
    }
    this.#line = emptyBytes();
    await this.#dispatch();
    this.#discardLine = false;
    this.#discardEvent = false;
  }

  #advanceEnding(data: Uint8Array, endingIndex: number): number {
    const offset = endingIndex + 1;
    if (data[endingIndex] !== carriageReturn) {
      return offset;
    }
    if (offset < data.byteLength && data[offset] === lineFeed) {
      return offset + 1;
    }
    if (offset === data.byteLength) {
      this.#afterCarriageReturn = true;
    }
    return offset;
  }

  async #acceptLine(rawLine: Uint8Array): Promise<void> {
    const line = trimBytes(rawLine);
    if (line.byteLength === 0) {
      if (this.#discardEvent) {
        this.#event = emptyBytes();
        this.#discardEvent = false;
      } else {
        await this.#dispatch();
      }
      return;
    }
    if (this.#discardEvent) {
      return;
    }
    if (!hasDataPrefix(line)) {
      await this.#inspect(line);
      return;
    }
    let data = line.subarray(dataPrefix.byteLength);
    if (data[0] === 32) {
      data = data.subarray(1);
    }
    const separator = this.#event.byteLength === 0 ? 0 : 1;
    if (
      this.#event.byteLength + separator + data.byteLength >
      maxResponseEventLine
    ) {
      this.#event = emptyBytes();
      this.#discardEvent = true;
      return;
    }
    if (separator === 1) {
      this.#event = appendBytes(this.#event, Uint8Array.of(lineFeed));
    }
    this.#event = appendBytes(this.#event, data);
  }

  async #dispatch(): Promise<void> {
    const event = this.#event;
    this.#event = emptyBytes();
    await this.#inspect(event);
  }

  async #inspect(bytes: Uint8Array): Promise<void> {
    const line = trimBytes(bytes);
    if (line.byteLength === 0) {
      return;
    }
    const text = new TextDecoder().decode(line);
    if (text === "[DONE]") {
      return;
    }
    let value: unknown;
    try {
      value = parseJson(line);
    } catch {
      return;
    }
    const envelope = parseResponseEnvelope(value);
    if (envelope === null) {
      return;
    }
    const payload = responsePayload(envelope);
    if (
      !this.#usageRecorded &&
      !usageEmpty(payload.usage) &&
      recordsUsage(envelope.object, envelope.type)
    ) {
      await this.#options.onUsage(payload.usage);
      this.#usageRecorded = true;
    }
    if (
      payload.id !== "" &&
      (envelope.type === "" || envelope.type === "response.created")
    ) {
      await this.#options.onResponseId(payload.id);
    }
  }
}
