import { describe, expect, it } from "vitest";
import { buildChatStreamFrame, encodeChatStreamWireFrames, parseChatStopRequest } from "./chat-stream.js";
import { agentSupportsReasoning, extractStreamJsonFinalText, parseStreamJsonLine } from "./agents.js";

const baseFrame = {
  streamId: "stream-1", requestMessageId: "req-1", requesterId: "user-1", nodeId: "node-1",
  conversationType: 1 as const, conversationId: "conv-1", seq: 1, status: "streaming" as const, text: "你好",
};

describe("chat_stream 帧编码", () => {
  it("reasoning 非空时进帧、空时省略", () => {
    const withReasoning = JSON.parse(buildChatStreamFrame({ ...baseFrame, reasoning: "让我想想…" }));
    expect(withReasoning.reasoning).toBe("让我想想…");
    const without = JSON.parse(buildChatStreamFrame(baseFrame));
    expect("reasoning" in without).toBe(false);
  });

  it("activity 非空时进帧、空数组时省略", () => {
    const steps = [{ id: "t1", kind: "search" as const, label: "联网搜索", detail: "虾说", status: "done" as const }];
    const withActivity = JSON.parse(buildChatStreamFrame({ ...baseFrame, activity: steps }));
    expect(withActivity.activity).toEqual(steps);
    const empty = JSON.parse(buildChatStreamFrame({ ...baseFrame, activity: [] }));
    expect("activity" in empty).toBe(false);
    const without = JSON.parse(buildChatStreamFrame(baseFrame));
    expect("activity" in without).toBe(false);
  });

  it("error_code 正确从 errorCode 输出", () => {
    const frame = JSON.parse(buildChatStreamFrame({ ...baseFrame, status: "failed", errorCode: "boom" }));
    expect(frame.error_code).toBe("boom");
  });

  it("小帧直发 chat_stream；大帧切 chat_stream_chunk 分块", () => {
    const small = encodeChatStreamWireFrames({ ...JSON.parse(buildChatStreamFrame(baseFrame)) });
    expect(small).toHaveLength(1);
    expect(small[0].objectName).toBe("chat_stream");

    const bigPayload = { ...JSON.parse(buildChatStreamFrame({ ...baseFrame, text: "长".repeat(5000), reasoning: "思".repeat(3000) })) };
    const chunks = encodeChatStreamWireFrames(bigPayload);
    expect(chunks.length).toBeGreaterThan(1);
    expect(chunks.every(c => c.objectName === "chat_stream_chunk")).toBe(true);

    // 重组验证：块参数与 web fixture 编码器一致，能拼回原帧
    const parsed = chunks.map(c => JSON.parse(c.content));
    expect(new Set(parsed.map(c => c.sha256)).size).toBe(1);
    expect(parsed.map(c => c.chunkIndex)).toEqual(parsed.map((_, i) => i));
    const bytes = Buffer.concat(parsed.map(c => Buffer.from(c.data, "base64")));
    expect(JSON.parse(bytes.toString("utf8"))).toEqual(bigPayload);
  });
});

describe("stream-json 解析器", () => {
  it("claude thinking_delta / text_delta 分流", () => {
    expect(parseStreamJsonLine(JSON.stringify({
      type: "stream_event", event: { type: "content_block_delta", delta: { type: "thinking_delta", thinking: "思考中" } },
    }))).toEqual({ reasoning: "思考中" });
    expect(parseStreamJsonLine(JSON.stringify({
      type: "stream_event", event: { type: "content_block_delta", delta: { type: "text_delta", text: "正文" } },
    }))).toEqual({ text: "正文" });
  });

  it("codex item.completed 的 reasoning / agent_message 分流，噪声行丢弃", () => {
    expect(parseStreamJsonLine(JSON.stringify({
      type: "item.completed", item: { type: "reasoning", text: "推理" },
    }))).toEqual({ reasoning: "推理" });
    expect(parseStreamJsonLine(JSON.stringify({
      type: "item.completed", item: { type: "agent_message", text: "回答" },
    }))).toEqual({ text: "回答" });
    expect(parseStreamJsonLine("WARNING: failed to clean up stale arg0 temp dirs")).toBeNull();
    expect(parseStreamJsonLine("2026-10-15T03:35:24.714256Z ERROR rmcp::transport::worker: worker quit")).toBeNull();
    expect(parseStreamJsonLine('{"type":"turn.completed","usage":{"output_tokens":33}}')).toBeNull();
    expect(parseStreamJsonLine('{"type":"item.started","item":{"type":"agent_message"}}')).toBeNull();
  });

  it("最终正文提取：claude result 优先、codex 聚合 agent_message", () => {
    const claude = [
      '{"type":"assistant","message":{"content":[{"type":"text","text":"部分"}]}}',
      '{"type":"result","result":"最终全文"}',
    ].join("\n");
    expect(extractStreamJsonFinalText(claude)).toBe("最终全文");
    const codex = [
      '{"type":"item.completed","item":{"type":"reasoning","text":"思考"}}',
      '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"你好"}}',
      '{"type":"turn.completed","usage":{"output_tokens":33}}',
    ].join("\n");
    expect(extractStreamJsonFinalText(codex)).toBe("你好");
    expect(extractStreamJsonFinalText("完全不是 JSON")).toBeUndefined();
  });

  it("思考流可用性标记", () => {
    expect(agentSupportsReasoning("claude")).toBe(true);
    expect(agentSupportsReasoning("codex")).toBe(true);
    expect(agentSupportsReasoning("openclaw")).toBe(false);
    expect(agentSupportsReasoning(undefined)).toBe(false);
  });
});

describe("chat_stop 解析（回归）", () => {
  it("解析停止请求并透传非停止消息", () => {
    expect(parseChatStopRequest(JSON.stringify({ msg_type: "chat_stop", stream_id: "s1", request_id: "r1" })))
      .toEqual({ requestId: "r1", streamId: "s1" });
    expect(parseChatStopRequest(JSON.stringify({ msg_type: "other" }))).toBeNull();
    expect(parseChatStopRequest("not json")).toBeNull();
  });
});
