// Run by TestHTTPResponsesPinnedSDK with the checkout's pinned, patched adapter.
// No OpenCode process, auth store, plugins, discovery or external inference.
import assert from "node:assert/strict";
import { createOpenAI } from "@ai-sdk/openai";
import { generateObject, generateText, streamText, tool } from "ai";
import { z } from "zod";

const baseURL = process.env.CODEX_BALANCER_TEST_URL;
assert.equal(new URL(baseURL).hostname, "127.0.0.1");
const requests = [];
const openai = createOpenAI({
  baseURL,
  apiKey: process.env.CODEX_BALANCER_TEST_KEY,
  fetch: async (url, init) => {
    assert.equal(String(url), `${baseURL}/responses`);
    assert.equal(init.method, "POST");
    requests.push(JSON.parse(init.body));
    return fetch(url, init);
  },
});
const model = openai.responses("gpt-6-astra");
const headers = { "session-id": "sdk-session", "x-session-affinity": "sdk-session" };
const tools = { lookup: tool({ inputSchema: z.object({ value: z.string() }) }) };
const first = streamText({
  model, headers, tools, maxRetries: 0,
  system: "ordinary instructions",
  messages: [{ role: "user", content: "ORDINARY_TURN" }],
  providerOptions: { openai: { store: false, forceReasoning: true, reasoningSummary: "auto", serviceTier: "priority" } },
});
const parts = [];
for await (const part of first.fullStream) {
  assert.notEqual(part.type, "error", JSON.stringify(part));
  parts.push(part);
}
assert(parts.some((p) => p.type === "text-delta" && p.text === "hello"));
assert(parts.some((p) => p.type === "reasoning-delta" && p.text === "thinking"));
assert(parts.some((p) => p.type === "tool-call" && p.toolCallId === "call-sdk"), JSON.stringify(parts));
assert.equal((await first.usage).totalTokens, 14);
const history = (await first.response).messages;
const second = await generateText({
  model, headers, tools, maxRetries: 0,
  system: "ordinary instructions",
  messages: [
    { role: "user", content: "ORDINARY_TURN" },
    ...history,
    { role: "tool", content: [{ type: "tool-result", toolCallId: "call-sdk", toolName: "lookup", output: { type: "text", value: "TOOL_RESULT" } }] },
  ],
  providerOptions: { openai: { store: false, forceReasoning: true } },
});
assert.equal(second.text, "second answer");
// Mirrors an auxiliary path that bypasses chat.params: no instructions/store
// provider options, and a generation knob that the HTTP adapter must handle.
const title = await generateObject({
  model, headers, maxRetries: 0, temperature: 0.3, maxOutputTokens: 128,
  system: "Generate a title",
  prompt: "TITLE_AUXILIARY",
  schema: z.object({ title: z.string() }),
});
assert.deepEqual(title.object, { title: "Local title" });
assert.equal(requests.length, 3);
assert.equal(requests[0].stream, true);
assert.equal(requests[0].service_tier, "priority"); // checkout's SDK patch
assert.notEqual(requests[1].stream, true);
assert.notEqual(requests[2].stream, true);
assert.equal(requests[2].max_output_tokens, 128);
assert.equal(requests[2].temperature, undefined); // SDK removes it for known reasoning models.
assert.equal(requests[2].instructions, undefined);
assert(requests[2].input.some((item) => ["system", "developer"].includes(item.role)));
assert(requests[1].input.some((item) => item.type === "function_call_output" && item.output === "TOOL_RESULT"));
assert(requests[1].input.some((item) => item.type === "reasoning" && item.encrypted_content === "encrypted-sdk"));
for (const prompt of ["ERROR_BEFORE", "ERROR_AFTER"]) {
  const failure = streamText({ model, headers, prompt, maxRetries: 0, onError() {} });
  let sawError = false;
  for await (const part of failure.fullStream) {
    if (part.type === "error") sawError = true;
    if (part.type === "finish") assert.notEqual(part.finishReason, "stop");
  }
  assert(sawError, `${prompt} was silently treated as success`);
}
assert.equal(requests.length, 5);
console.log("Pinned AI SDK: streaming text/reasoning/tool call, full-history JSON tool result, auxiliary structured JSON, pre/post-stream errors passed");
