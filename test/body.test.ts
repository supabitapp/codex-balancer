import { describe, expect, it } from "vitest";

import {
  inspectRequestBody,
  maxInspectionBody,
  maxRawRequestBody,
} from "../src/body.js";

const decoder = new TextDecoder();
const jsonBody = '{"model":"gpt","input":[]}';
const encodedJsonBody = Uint8Array.from(
  atob("KLUv/QRY0QAAeyJtb2RlbCI6ImdwdCIsImlucHV0IjpbXX2bhj5z"),
  (character) => character.charCodeAt(0),
);

const makeOversizedZstdFrame = (declareSize: boolean): Uint8Array => {
  const blockSize = 128 << 10;
  const blockCount = maxInspectionBody / blockSize + 1;
  const expandedSize = blockSize * blockCount;
  const header = declareSize
    ? [
        0x28,
        0xb5,
        0x2f,
        0xfd,
        0xa0,
        expandedSize,
        expandedSize >>> 8,
        expandedSize >>> 16,
        expandedSize >>> 24,
      ]
    : [0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x38];
  const frame = new Uint8Array(header.length + blockCount * 4);
  frame.set(header);
  let offset = header.length;
  for (let index = 0; index < blockCount; index += 1) {
    const lastBlock = index === blockCount - 1 ? 1 : 0;
    const blockHeader = (blockSize << 3) | 2 | lastBlock;
    frame[offset] = blockHeader;
    frame[offset + 1] = blockHeader >>> 8;
    frame[offset + 2] = blockHeader >>> 16;
    frame[offset + 3] = 0x61;
    offset += 4;
  }
  return frame;
};

describe("request body inspection", () => {
  it("accepts identity bodies without copying their meaning", async () => {
    const inspected = await inspectRequestBody(
      new Request("https://balancer.test/v1/responses", {
        body: jsonBody,
        headers: { "content-encoding": "identity" },
        method: "POST",
      }),
    );

    expect(decoder.decode(inspected.inspection)).toBe(jsonBody);
    expect(decoder.decode(inspected.wire)).toBe(jsonBody);
  });

  it("decodes mixed-case zstd for inspection and keeps exact wire bytes", async () => {
    const inspected = await inspectRequestBody(
      new Request("https://balancer.test/v1/responses", {
        body: encodedJsonBody,
        headers: { "content-encoding": " ZsTd " },
        method: "POST",
      }),
    );

    expect(decoder.decode(inspected.inspection)).toBe(jsonBody);
    expect(inspected.wire).toEqual(encodedJsonBody);
  });

  it("decodes concatenated zstd frames with skippable data", async () => {
    const skippable = Uint8Array.of(
      0x50,
      0x2a,
      0x4d,
      0x18,
      3,
      0,
      0,
      0,
      1,
      2,
      3,
    );
    const encoded = new Uint8Array(
      encodedJsonBody.byteLength * 2 + skippable.byteLength,
    );
    encoded.set(encodedJsonBody);
    encoded.set(skippable, encodedJsonBody.byteLength);
    encoded.set(
      encodedJsonBody,
      encodedJsonBody.byteLength + skippable.byteLength,
    );

    const inspected = await inspectRequestBody(
      new Request("https://balancer.test/v1/responses", {
        body: encoded,
        headers: { "content-encoding": "zstd" },
        method: "POST",
      }),
    );

    expect(decoder.decode(inspected.inspection)).toBe(jsonBody.repeat(2));
    expect(inspected.wire).toEqual(encoded);
  });

  it("rejects malformed, truncated, and unsupported encodings", async () => {
    const invalidZstdBodies = [
      "not-zstd",
      Uint8Array.of(0x28, 0xb5, 0x2f, 0xfd),
      Uint8Array.of(0x28, 0xb5, 0x2f, 0xfd, 0x20, 1, 9, 0, 0),
      Uint8Array.of(0x50, 0x2a, 0x4d, 0x18, 1, 0, 0, 0),
    ];
    for (const body of invalidZstdBodies) {
      await expect(
        inspectRequestBody(
          new Request("https://balancer.test/v1/responses", {
            body,
            headers: { "content-encoding": "zstd" },
            method: "POST",
          }),
        ),
      ).rejects.toMatchObject({
        message: "invalid request body: decode zstd",
        status: 400,
      });
    }
    await expect(
      inspectRequestBody(
        new Request("https://balancer.test/v1/responses", {
          body: "body",
          headers: { "content-encoding": "gzip" },
          method: "POST",
        }),
      ),
    ).rejects.toMatchObject({ status: 400 });
  });

  it("rejects a small zstd frame with a declared expanded size over the limit", async () => {
    const encoded = makeOversizedZstdFrame(true);

    await expect(
      inspectRequestBody(
        new Request("https://balancer.test/v1/responses", {
          body: encoded,
          headers: { "content-encoding": "zstd" },
          method: "POST",
        }),
      ),
    ).rejects.toMatchObject({
      message: "invalid request body: expanded body too large",
      status: 400,
    });
    expect(encoded.byteLength).toBeLessThan(2 << 10);
  });

  it("rejects streamed zstd output over the limit without a declared size", async () => {
    const encoded = makeOversizedZstdFrame(false);

    await expect(
      inspectRequestBody(
        new Request("https://balancer.test/v1/responses", {
          body: encoded,
          headers: { "content-encoding": "zstd" },
          method: "POST",
        }),
      ),
    ).rejects.toMatchObject({
      message: "invalid request body: expanded body too large",
      status: 400,
    });
  });

  it("rejects a declared raw body over the limit", async () => {
    const request = new Request("https://balancer.test/v1/responses", {
      body: "body",
      headers: { "content-length": String(maxRawRequestBody + 1) },
      method: "POST",
    });

    await expect(inspectRequestBody(request)).rejects.toMatchObject({
      status: 413,
    });
  });

  it("rejects an invalid content length", async () => {
    const request = new Request("https://balancer.test/v1/responses", {
      body: "body",
      headers: { "content-length": "four" },
      method: "POST",
    });

    await expect(inspectRequestBody(request)).rejects.toMatchObject({
      message: "invalid content length",
      status: 400,
    });
  });
});
