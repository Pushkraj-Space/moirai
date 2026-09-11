import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { ClaudeCodeCodec, MoiraiError, Registry, SimpleCodec, toText } from "../dist/index.js";

const fixturePath = "../../testdata/native/claude_code.jsonl";

const source = JSON.stringify({
  id: "session",
  timestamp: "2026-01-01T00:00:00Z",
  messages: [
    { role: "user", content: "repair the parser" },
    { role: "assistant", content: [{ type: "thinking", text: "inspect first" }, { type: "tool_use", id: "call-1", name: "Read", input: { file_path: "parser.go" } }], model: "claude-sonnet", usage: { input_tokens: 10, output_tokens: 20 } },
    { role: "user", content: [{ type: "tool_result", tool_use_id: "call-1", content: "package parser", is_error: true }] },
    { role: "assistant", content: "Parser repaired and tests pass.", stop_reason: "end_turn" },
  ],
});

function fixture() { return new SimpleCodec().parse(source).transcript; }

test("claude code codec parses the native fixture", async () => {
  const codec = new ClaudeCodeCodec();
  const { transcript, warnings } = codec.parse(await readFile(fixturePath, "utf8"), { sourceId: "native-fixture" });
  assert.equal(transcript.meta.id, "aaaaaaaa-1111-4222-8333-444444444444");
  assert.equal(transcript.meta.cwd, "/tmp/e2e.dot_proj");
  assert.equal(transcript.meta.title, "Synthetic native Claude fixture");
  assert.equal(transcript.meta.model, "claude-sonnet");
  // user text, assistant tool call, tool result, assistant text — sidechain and snapshots omitted
  assert.equal(transcript.messages.length, 4);
  assert.equal(transcript.messages[0].content[0].text, "native fixture");
  assert.equal(transcript.messages[0].content[1].type, "image");
  assert.equal(transcript.messages[0].content[1].source.data, "aGVsbG8=");
  assert.equal(transcript.messages[1].content[0].type, "thinking");
  assert.equal(transcript.messages[1].content[0].encrypted, "synthetic-ciphertext");
  const call = transcript.messages[1].content[1];
  assert.equal(call.type, "tool_use");
  assert.equal(call.id, "tool-1");
  assert.deepEqual(call.input, { file_path: "README.md" });
  const result = transcript.messages[2].content[0];
  assert.equal(result.type, "tool_result");
  assert.equal(result.tool_use_id, "tool-1");
  assert.deepEqual(result.content, [{ type: "text", text: "synthetic file contents" }]);
  assert.equal(transcript.messages[3].content[0].text, "done");
  assert.ok(warnings.some((warning) => warning.code === "native_record_omitted" && warning.message.includes("file-history-snapshot")));
  assert.ok(warnings.some((warning) => warning.code === "native_record_omitted" && warning.message.includes("sidechain")));
  // encrypted thinking parses to a timestamped message; stop_reason survives parse
  assert.equal(transcript.messages[0].timestamp, "2026-01-01T00:00:00Z");
  assert.equal(transcript.messages[1].stop_reason, "tool_use");
  assert.equal(transcript.messages[3].stop_reason, "end_turn");
  // rendering the native fixture emits a redacted_thinking block and never the "encrypted" key
  const rendered = codec.render(transcript).data;
  assert.ok(rendered.includes("redacted_thinking"));
  assert.ok(!rendered.includes('"encrypted"'));
  const reparsed = codec.parse(rendered).transcript;
  assert.equal(reparsed.messages[1].content[0].type, "thinking");
  assert.equal(reparsed.messages[1].content[0].encrypted, "synthetic-ciphertext");
  assert.equal(reparsed.messages[1].stop_reason, "tool_use");
  const plain = toText(transcript, { maxBytes: 1 << 20 });
  assert.ok(plain.includes("native fixture"));
  assert.ok(!plain.includes("SIDECHAIN MUST NOT LEAK"));
});

test("claude code codec round-trips canonical transcripts", () => {
  const codec = new ClaudeCodeCodec();
  const rendered = codec.render(fixture()).data;
  const reparsed = codec.parse(rendered).transcript;
  assert.equal(reparsed.messages.length, 4);
  assert.equal(reparsed.messages[0].content[0].text, "repair the parser");
  const call = reparsed.messages[1].content[1];
  assert.equal(call.type, "tool_use");
  assert.equal(call.id, "call-1");
  assert.deepEqual(call.input, { file_path: "parser.go" });
  assert.equal(reparsed.messages[1].model, "claude-sonnet");
  assert.deepEqual(reparsed.messages[1].usage, { input_tokens: 10, output_tokens: 20 });
  assert.equal(reparsed.messages[2].content[0].tool_use_id, "call-1");
  assert.equal(reparsed.messages[2].content[0].is_error, true);
  assert.equal(reparsed.messages[3].content[0].text, "Parser repaired and tests pass.");
  const second = codec.parse(codec.render(reparsed).data).transcript;
  assert.equal(second.messages.length, 4);
  assert.equal(second.messages[3].content[0].text, "Parser repaired and tests pass.");
  // rendered output is line-delimited JSON records with Claude Code envelope fields
  const records = rendered.trim().split("\n").map((line) => JSON.parse(line));
  assert.ok(records.every((record) => record.sessionId === "session" && typeof record.uuid === "string"));
  assert.equal(records[0].type, "user");
  assert.equal(records[1].type, "assistant");
  assert.equal(records[1].message.model, "claude-sonnet");
  // unsigned canonical thinking renders as visible [Reasoning] text, matching Go
  assert.ok(records[1].message.content.some((block) => block.type === "text" && block.text.startsWith("[Reasoning]")));
  assert.ok(records[1].message.content.some((block) => block.type === "tool_use"));
  assert.ok(records.every((record) => record.parentUuid === null || typeof record.parentUuid === "string"));
});

test("claude code codec omits invalid records and bookkeeping with warnings", () => {
  const codec = new ClaudeCodeCodec();
  const lines = [
    JSON.stringify({ type: "user", isMeta: true, sessionId: "s", uuid: "u1", message: { role: "user", content: "hello" } }),
    JSON.stringify({ type: "assistant", sessionId: "s", uuid: "a1", message: { role: "assistant", content: [{ type: "text", text: "hi" }] } }),
    JSON.stringify({ type: "queue" }),
    "{not json",
    JSON.stringify({ type: "user", sessionId: "s", uuid: "u2", message: { role: "user", content: [] } }),
  ].join("\n");
  const { transcript, warnings } = codec.parse(lines);
  assert.equal(transcript.messages.length, 2);
  assert.equal(transcript.messages[0].content[0].text, "hello");
  assert.equal(transcript.messages[1].content[0].text, "hi");
  assert.ok(warnings.some((warning) => warning.code === "native_fields_omitted"));
  assert.ok(warnings.some((warning) => warning.code === "native_record_omitted" && warning.message.includes("queue")));
  assert.ok(warnings.some((warning) => warning.code === "invalid_json"));
  // hello/hi transcript round-trips through render+parse unchanged (omissions do not affect content)
  const rendered = codec.render(transcript).data;
  const reparsed = codec.parse(rendered).transcript;
  assert.equal(reparsed.messages[0].content[0].text, "hello");
});

test("claude code codec rejects transcripts without conversational records and enforces limits", () => {
  const codec = new ClaudeCodeCodec();
  const empty = [JSON.stringify({ type: "file-history-snapshot" }), JSON.stringify({ type: "user", isSidechain: true, message: { role: "user", content: "hidden" } })].join("\n");
  assert.throws(() => codec.parse(empty), (error) => error instanceof MoiraiError && error.code === "invalid_transcript");
  const deep = `${JSON.stringify({ type: "user", sessionId: "s", message: { role: "user", content: "ok" } })}\n{"type":"user","message":{"role":"user","content":[[[[[[[[[[[[[[[["deep"]]]]]]]]]]]]]]]]}}\n`;
  assert.throws(() => codec.parse(deep, { limits: { maxInputBytes: 1 << 20, maxMessages: 100, maxBlocks: 100, maxTextBytes: 1 << 20, maxInlineMediaBytes: 1 << 20, maxMetadataBytes: 1 << 20, maxNestingDepth: 8 } }), (error) => error instanceof MoiraiError && error.code === "limit_exceeded");
});

test("convert reports artifact, unknown, and extension loss when rendering claude code", () => {
  const registry = new Registry([new SimpleCodec(), new ClaudeCodeCodec()]);
  const simple = JSON.stringify({
    id: "lossy",
    timestamp: "2026-01-01T00:00:00Z",
    extra: { vendor: "keep me" },
    messages: [
      { role: "user", content: [{ type: "text", text: "keep this" }], extra: { note: "mine" } },
      { role: "user", content: [{ type: "artifact", artifact: { name: "report.md", source: { type: "text", text: "body" } } }] },
      { role: "user", content: [{ type: "unknown", data: { anything: true } }] },
    ],
  });
  const { data, warnings } = registry.convert(simple, "simple", "claude_code");
  // representable content survives; artifact and unknown blocks are dropped from data
  assert.ok(data.includes("keep this"));
  assert.ok(!data.includes("report.md"));
  assert.ok(!data.includes("anything"));
  const artifactWarning = warnings.find((warning) => warning.code === "unsupported_block" && warning.path === "messages[1].content[0]");
  assert.ok(artifactWarning, `missing artifact warning in ${JSON.stringify(warnings)}`);
  assert.equal(artifactWarning.message, "claude_code cannot represent artifact content; block omitted");
  const unknownWarning = warnings.find((warning) => warning.code === "unsupported_block" && warning.path === "messages[2].content[0]");
  assert.ok(unknownWarning, `missing unknown warning in ${JSON.stringify(warnings)}`);
  assert.equal(unknownWarning.message, "claude_code cannot represent unknown content; block omitted");
  assert.ok(warnings.some((warning) => warning.code === "extension_omitted" && warning.message === "claude_code cannot represent canonical extension data; extension omitted"));
  assert.ok(warnings.some((warning) => warning.code === "extension_omitted" && warning.path === "messages[0].extra" && warning.message === "claude_code cannot represent message extension data; extension omitted"));

  // artifact-ONLY message: the block is dropped and the loss is still reported
  const artifactOnly = JSON.stringify({
    id: "artifact-only",
    timestamp: "2026-01-01T00:00:00Z",
    messages: [{ role: "user", content: [{ type: "artifact", artifact: { name: "only.md" } }] }],
  });
  const onlyResult = registry.convert(artifactOnly, "simple", "claude_code");
  assert.ok(!onlyResult.data.includes("only.md"));
  const onlyWarning = onlyResult.warnings.find((warning) => warning.code === "unsupported_block" && warning.path === "messages[0].content[0]");
  assert.ok(onlyWarning, `missing artifact-only warning in ${JSON.stringify(onlyResult.warnings)}`);
  assert.equal(onlyWarning.message, "claude_code cannot represent artifact content; block omitted");

  // role mismatches are reported too: thinking/tool_use on user, tool_result on assistant
  const misplaced = new SimpleCodec().parse(JSON.stringify({
    id: "misplaced", timestamp: "2026-01-01T00:00:00Z",
    messages: [
      { role: "user", content: [{ type: "thinking", text: "why" }, { type: "tool_use", id: "t1", name: "Read", input: {} }] },
      { role: "assistant", content: [{ type: "tool_result", tool_use_id: "t1", content: "x" }] },
    ],
  })).transcript;
  const misplacedWarnings = new ClaudeCodeCodec().render(misplaced).warnings;
  assert.deepEqual(misplacedWarnings.map((warning) => warning.path), ["messages[0].content[0]", "messages[0].content[1]", "messages[1].content[0]"]);
});

test("claude code timestamps preserve RFC3339 nanosecond precision", () => {
  const codec = new ClaudeCodeCodec();
  const nano = "2026-01-01T00:00:00.123456789Z";
  const line = JSON.stringify({ type: "user", sessionId: "s", uuid: "u1", timestamp: nano, message: { role: "user", content: "hello" } });
  const { transcript } = codec.parse(line);
  assert.equal(transcript.messages[0].timestamp, nano);
  assert.equal(transcript.meta.timestamp, nano);
  // render + re-parse preserves the full nanosecond fraction
  const roundTrip = codec.parse(codec.render(transcript).data).transcript;
  assert.equal(roundTrip.messages[0].timestamp, nano);
  assert.equal(roundTrip.meta.timestamp, nano);
  // a millisecond timestamp that Date could represent exactly is still returned unchanged
  const milli = "2026-01-01T00:00:00.123Z";
  const milliParsed = codec.parse(JSON.stringify({ type: "user", sessionId: "s", uuid: "u2", timestamp: milli, message: { role: "user", content: "hi" } }));
  assert.equal(milliParsed.transcript.messages[0].timestamp, milli);
});
