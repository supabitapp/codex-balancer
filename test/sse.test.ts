import { describe, expect, it } from "vitest";

import type { ResponseUsage } from "../src/domain.js";
import { maxResponseEventLine, ResponseStreamInspector } from "../src/sse.js";

const encoder = new TextEncoder();

const recorder = () => {
  const ids: string[] = [];
  const usage: ResponseUsage[] = [];
  const inspector = new ResponseStreamInspector({
    onResponseId: (id) => {
      ids.push(id);
    },
    onUsage: (value) => {
      usage.push(value);
    },
  });
  return { ids, inspector, usage };
};

describe("response stream inspection", () => {
  it("parses multiline SSE data across CRLF chunk boundaries", async () => {
    const result = recorder();

    await result.inspector.write(
      encoder.encode('data: {"type":"response.created",\r'),
    );
    await result.inspector.write(
      encoder.encode('\ndata: "response":{"id":"resp_multiline"}}\r\n\r'),
    );
    await result.inspector.write(encoder.encode("\n"));
    await result.inspector.finish();

    expect(result.ids).toEqual(["resp_multiline"]);
  });

  it("accepts lone carriage returns as event endings", async () => {
    const result = recorder();

    await result.inspector.write(
      encoder.encode(
        'data: {"type":"response.created","response":{"id":"resp_cr"}}\r\r',
      ),
    );

    expect(result.ids).toEqual(["resp_cr"]);
  });

  it("inspects a plain JSON response at end of input", async () => {
    const calls: string[] = [];
    const usages: ResponseUsage[] = [];
    const inspector = new ResponseStreamInspector({
      onResponseId: (id) => {
        calls.push(`id:${id}`);
      },
      onUsage: (usage) => {
        calls.push("usage");
        usages.push(usage);
      },
    });

    await inspector.write(
      encoder.encode(
        JSON.stringify({
          id: "resp_plain",
          object: "response",
          usage: {
            input_tokens: 30,
            output_tokens: 4,
            input_tokens_details: {
              cached_tokens: 12,
              cache_write_tokens: 5,
            },
          },
        }),
      ),
    );
    await inspector.finish();

    expect(calls).toEqual(["usage", "id:resp_plain"]);
    expect(usages).toEqual([
      {
        inputTokens: 30,
        outputTokens: 4,
        inputTokensDetails: { cachedTokens: 12, cacheWriteTokens: 5 },
      },
    ]);
  });

  it("records usage without response routing fields", async () => {
    const result = recorder();

    await result.inspector.write(
      encoder.encode(
        'data: {"type":"response.completed","response":{"model":"response-model","service_tier":"response-tier","usage":{"output_tokens":2}}}\n\n',
      ),
    );

    expect(result.usage).toEqual([
      {
        inputTokens: 0,
        inputTokensDetails: { cachedTokens: 0, cacheWriteTokens: 0 },
        outputTokens: 2,
      },
    ]);
  });

  it.each(["response.completed", "response.failed", "response.incomplete"])(
    "records nonempty usage from %s",
    async (type) => {
      const result = recorder();

      await result.inspector.write(
        encoder.encode(
          `data: ${JSON.stringify({
            type,
            response: {
              usage: {
                input_tokens_details: { cache_write_tokens: 7 },
              },
            },
          })}\n\n`,
        ),
      );

      expect(result.usage).toHaveLength(1);
      expect(result.usage[0]?.inputTokensDetails.cacheWriteTokens).toBe(7);
    },
  );

  it("records only the first nonempty terminal usage", async () => {
    const result = recorder();

    await result.inspector.write(
      encoder.encode(
        'data: {"type":"response.completed","response":{"usage":{}}}\n\n' +
          'data: {"type":"response.failed","response":{"usage":{"input_tokens":5}}}\n\n' +
          'data: {"type":"response.incomplete","response":{"usage":{"output_tokens":9}}}\n\n',
      ),
    );

    expect(result.usage).toHaveLength(1);
    expect(result.usage[0]?.inputTokens).toBe(5);
    expect(result.usage[0]?.outputTokens).toBe(0);
  });

  it("ignores done markers, empty events, and malformed JSON", async () => {
    const result = recorder();

    await result.inspector.write(
      encoder.encode("data: [DONE]\n\n\n:data\n\ndata: {\n\n"),
    );
    await result.inspector.finish();

    expect(result.ids).toEqual([]);
    expect(result.usage).toEqual([]);
  });

  it("dispatches a pending SSE event on finish", async () => {
    const result = recorder();

    await result.inspector.write(
      encoder.encode(
        'data: {"type":"response.created","response":{"id":"resp_finish"}}',
      ),
    );
    expect(result.ids).toEqual([]);

    await result.inspector.finish();

    expect(result.ids).toEqual(["resp_finish"]);
  });

  it("drops an oversized line and recovers after its ending", async () => {
    const result = recorder();
    const oversized = new Uint8Array(maxResponseEventLine + 1).fill(120);

    await result.inspector.write(
      oversized.subarray(0, maxResponseEventLine / 2),
    );
    await result.inspector.write(oversized.subarray(maxResponseEventLine / 2));
    await result.inspector.write(encoder.encode("\r"));
    await result.inspector.write(
      encoder.encode(
        '\n\ndata: {"type":"response.created","response":{"id":"resp_after_line"}}\n\n',
      ),
    );
    await result.inspector.finish();

    expect(result.ids).toEqual(["resp_after_line"]);
  });

  it("drops an oversized event and resumes after its blank line", async () => {
    const result = recorder();
    const part = "x".repeat(600_000);

    await result.inspector.write(encoder.encode(`data: ${part}\n`));
    await result.inspector.write(encoder.encode(`data: ${part}\n`));
    await result.inspector.write(
      encoder.encode(
        '\ndata: {"type":"response.created","response":{"id":"resp_after_event"}}\n\n',
      ),
    );
    await result.inspector.finish();

    expect(result.ids).toEqual(["resp_after_event"]);
  });

  it("awaits callback effects before resolving write", async () => {
    let release = (): void => {
      throw new Error("callback not entered");
    };
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    let entered = false;
    let resolved = false;
    const inspector = new ResponseStreamInspector({
      onResponseId: async () => {
        entered = true;
        await gate;
      },
      onUsage: () => undefined,
    });

    const writing = inspector.write(
      encoder.encode(
        'data: {"type":"response.created","response":{"id":"resp_async"}}\n\n',
      ),
    );
    void writing.then(() => {
      resolved = true;
    });
    await Promise.resolve();

    expect(entered).toBe(true);
    expect(resolved).toBe(false);

    release();
    await writing;

    expect(resolved).toBe(true);
  });
});
