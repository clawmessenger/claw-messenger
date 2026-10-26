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
      .toEqual({ requestId: "r1", streamId: "s1", requestMessageId: "" });
    expect(parseChatStopRequest(JSON.stringify({ msg_type: "other" }))).toBeNull();
    expect(parseChatStopRequest("not json")).toBeNull();
  });

  it("带上 request_message_id 时透传，供设备端按轮次回找", () => {
    // stream_id 对不上是停止失效的主因（见 handleChatStop 的三级定位）：
    // 有轮次 id 就能退一步命中，老 web 不带这个字段时缺省空串。
    expect(parseChatStopRequest(JSON.stringify({
      msg_type: "chat_stop", stream_id: "s1", request_id: "r1", request_message_id: "D1ER-XYZ",
    }))).toEqual({ requestId: "r1", streamId: "s1", requestMessageId: "D1ER-XYZ" });
    expect(parseChatStopRequest(JSON.stringify({
      msg_type: "chat_stop", stream_id: "s1", request_message_id: 42,
    }))).toEqual({ requestId: "", streamId: "s1", requestMessageId: "" });
  });
});

// ---- 帧内步骤详情瘦身：终帧块数从 16 降到 3~4 ----
//
// 2026-10-26 现场：codex 一轮 13 个工具步骤，每步带 inputDetail(≤2000)/outputDetail(≤1500)，
// 终帧序列化 90 KB → 16 个 wire 块 → 16 条融云消息。任何一块没落地，前端就收不到
// 终帧（气泡一直转圈），随后兜底整包文本被当成第二条回复 → 界面上两条一样的回答。
describe("chat_stream 帧内 detail 瘦身", () => {
  const heavySteps = Array.from({ length: 13 }, (_, i) => ({
    id: `step_${i}`,
    kind: "command" as const,
    label: "执行命令",
    detail: "d".repeat(120),
    inputDetail: "i".repeat(2000),
    outputDetail: "o".repeat(1500),
    status: "done" as const,
    startedAt: 1_700_000_000_000,
    endedAt: 1_700_000_001_000,
  }));

  it("单步 inputDetail/outputDetail 在帧里被截到 160 字（detail 摘要不动）", () => {
    const frame = JSON.parse(buildChatStreamFrame({ ...baseFrame, activity: heavySteps }));
    expect(frame.activity).toHaveLength(13);
    for (const step of frame.activity) {
      expect(step.inputDetail.length).toBe(160);
      expect(step.inputDetail.endsWith("…")).toBe(true);
      expect(step.outputDetail.length).toBe(160);
      expect(step.detail).toBe("d".repeat(120));
      // id/kind/label/status/时间戳原样保留（web 端要按它渲染与算耗时）
      expect(step.kind).toBe("command");
      expect(step.startedAt).toBe(1_700_000_000_000);
    }
  });

  it("重终帧（13 步 + 6366 字思考）块数从 16 降到个位数", () => {
    const reasoning = "想".repeat(6366);
    const frame = buildChatStreamFrame({
      ...baseFrame,
      seq: 35,
      status: "completed",
      text: "长".repeat(848),
      reasoning,
      activity: heavySteps,
      timeline: [["r", reasoning] as ["r", string], ...heavySteps.map((s) => ["s", s.id] as ["s", string])],
    });
    const bytes = Buffer.byteLength(frame, "utf8");
    const chunks = encodeChatStreamWireFrames(JSON.parse(frame) as Record<string, unknown>);
    // 未瘦身时实测 90,482 字节 / 16 块；瘦身后 ~49KB / 9 块。
    // 剩下的体量主要是「思考全文 ×2」（reasoning 与 timeline 各存一份），
    // 那属于协议结构问题，不在这一刀里动。
    expect(bytes).toBeLessThan(50_000);
    expect(chunks.length).toBeLessThanOrEqual(9);
  });

  it("没有详情字段时不凭空造出 undefined（帧体与原样一致）", () => {
    const plain = [{ id: "t1", kind: "search" as const, label: "联网搜索", status: "running" as const }];
    const frame = JSON.parse(buildChatStreamFrame({ ...baseFrame, activity: plain }));
    expect(frame.activity).toEqual(plain);
    expect("inputDetail" in frame.activity[0]).toBe(false);
  });
});
