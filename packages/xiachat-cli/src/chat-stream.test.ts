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

  // opencode 1.19 `run --format json --thinking` 的真实事件形状（实测抓取）：
  // 与另外两家不同，它上报的是整段 part 全文，所以解析结果带 partId + replace，
  // 由调用方换算成增量。
  it("opencode reasoning / text 事件带上 partId 与整段语义", () => {
    expect(parseStreamJsonLine(JSON.stringify({
      type: "reasoning",
      part: { id: "prt_r1", type: "reasoning", text: "先跑 ls 看看" },
    }))).toEqual({ reasoning: "先跑 ls 看看", partId: "prt_r1", replace: true });
    expect(parseStreamJsonLine(JSON.stringify({
      type: "text",
      part: { id: "prt_t1", type: "text", text: "done" },
    }))).toEqual({ text: "done", partId: "prt_t1", replace: true });
    // 非正文/思考的事件不进文本通道（工具走 activity 追踪器）
    expect(parseStreamJsonLine('{"type":"step_start","part":{"type":"step-start"}}')).toBeNull();
    expect(parseStreamJsonLine('{"type":"tool_use","part":{"type":"tool","tool":"bash"}}')).toBeNull();
    expect(parseStreamJsonLine('{"type":"text","part":{"type":"text","text":""}}')).toBeNull();
  });

  it("opencode 最终正文：按 part 去重再拼接（同一 part 重复上报不翻倍）", () => {
    const raw = [
      '{"type":"step_start","part":{"type":"step-start"}}',
      '{"type":"text","part":{"id":"prt_1","type":"text","text":"答"}}',
      '{"type":"text","part":{"id":"prt_1","type":"text","text":"答案"}}',
      '{"type":"text","part":{"id":"prt_2","type":"text","text":"。"}}',
      '{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}',
    ].join("\n");
    expect(extractStreamJsonFinalText(raw)).toBe("答案。");
  });

  it("hermes：text 事件是增量（没有 part），result 里带权威全文", () => {
    expect(parseStreamJsonLine('{"type":"text","text":"上海"}'))
      .toEqual({ text: "上海" });
    const raw = [
      '{"type":"system","subtype":"init","model":"glm-5.3"}',
      '{"type":"text","text":"上海"}',
      '{"type":"text","text":"多云。"}',
      '{"type":"result","exit_code":0,"text":"上海多云。"}',
    ].join("\n");
    expect(extractStreamJsonFinalText(raw)).toBe("上海多云。");
    // 没有 result（被截断/提前退出）时退回增量拼接
    expect(extractStreamJsonFinalText('{"type":"text","text":"a"}\n{"type":"text","text":"b"}'))
      .toBe("ab");
    // 工具事件不进文本通道
    expect(parseStreamJsonLine('{"type":"tool_use","name":"web_search","input":{"query":"天气"}}')).toBeNull();
  });

  it("hermes：reasoning 事件是增量、无 part，与 opencode 的同名事件区分开", () => {
    // hermes 0.21.3 的 reasoning_callback 已经接进 JSONL（本机补丁），形状与 text 一样
    // 是「无 part 的增量」；opencode 的 reasoning 一定带 part 且是整段全文。
    expect(parseStreamJsonLine('{"type":"reasoning","text":"先比较"}'))
      .toEqual({ reasoning: "先比较" });
    expect(parseStreamJsonLine(JSON.stringify({
      type: "reasoning", part: { id: "prt_1", type: "reasoning", text: "整段" },
    }))).toEqual({ reasoning: "整段", partId: "prt_1", replace: true });
    // 空文本不产出（避免把空帧推给前端）
    expect(parseStreamJsonLine('{"type":"reasoning","text":""}')).toBeNull();
  });

  it("思考流可用性标记", () => {
    expect(agentSupportsReasoning("claude")).toBe(true);
    expect(agentSupportsReasoning("codex")).toBe(true);
    expect(agentSupportsReasoning("opencode")).toBe(true);
    // hermes 走 chat -q --format stream-json：text 增量 + tool_use/tool_result，
    // 思考事件由本机 stream_json.py 补丁接上 reasoning_callback 后才有。
    expect(agentSupportsReasoning("hermes")).toBe(true);
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
