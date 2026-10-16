import { createHash } from "node:crypto";

import type { ActivityStep } from "./agent-activity.js";

// chat_stream 协议生产端：把 agent 的一轮回答以流式快照帧发进会话，
// web 端（clawmessenger-web src/lib/chat-stream.ts）解析后逐帧渲染。
//
// 帧语义（与 web 端 parseChatStreamEvent 严格对齐）：
// - text 是**全量快照**而非增量：web 端 mergeChatStreamMessage 用 seq
//   单调性保证只采纳更新的帧，丢帧不致内容错乱；
// - seq 从 0 开始单调递增；status: processing → streaming → completed /
//   cancelled / failed；
// - conversation_type: 1 私聊 / 3 普通群组 / 4 聊天室；
// - reasoning 是可选的**思考流全量快照**（agent 的 thinking/reasoning
//   过程），与 text 同帧携带、同 seq 递增；web 端渲染为可折叠「思考过程」。
// - activity 是可选的**过程流全量快照**（agent 本轮调过的工具步骤：联网搜索、
//   执行命令、读写文件…），同样与 text 同帧同 seq；三者合起来是气泡里的
//   「过程」面板。
// - 单帧序列化后 <= 9000 字节直发（objectName: chat_stream）；超过则按
//   wire 分块协议（chat_stream_chunk，与 web 端 ChatStreamWireReassembler
//   互为生产/消费端）分块发送，web 侧重组回完整帧。旧版「按 1800 字符
//   切片多帧递增 seq」在 web 端按 seq 覆盖合并，只剩最后一片——已废弃。
//
// 发送频率：融云自定义消息限频（20604 SEND_FREQUENCY_TOO_FAST），帧间隔
// 由调用方（sendTurnStreamed）节流，这里不重复等待。

export type ChatStreamStatus = "processing" | "streaming" | "completed" | "cancelled" | "failed";

export interface ChatStreamFrameInput {
  streamId: string;
  requestMessageId: string;
  requesterId: string;
  nodeId: string;
  conversationType: 1 | 3 | 4;
  conversationId: string;
  seq: number;
  status: ChatStreamStatus;
  text: string;
  /** 思考流全量快照（可选）：agent 的 thinking/reasoning 过程文本。 */
  reasoning?: string;
  /**
   * 过程流全量快照（可选）：agent 本轮调用过的工具步骤（联网搜索、执行命令、
   * 读写文件…）。与 text/reasoning 同一套快照语义，web 端渲染成气泡里的
   * 「过程」面板。没有过程可展示时整字段省略。
   */
  activity?: ActivityStep[];
  errorCode?: string;
}

export interface ChatStreamStopRequestParsed {
  requestId: string;
  streamId: string;
}

export function buildChatStreamFrame(input: ChatStreamFrameInput): string {
  const payload: Record<string, unknown> = {
    msg_type: "chat_stream",
    protocol_version: 1,
    stream_id: input.streamId,
    request_message_id: input.requestMessageId,
    requester_id: input.requesterId,
    node_id: input.nodeId,
    conversation_type: input.conversationType,
    conversation_id: input.conversationId,
    seq: input.seq,
    status: input.status,
    text: input.text,
  };
  if (input.reasoning) payload.reasoning = input.reasoning;
  if (input.activity && input.activity.length > 0) payload.activity = input.activity;
  if (input.errorCode) payload.error_code = input.errorCode;
  return JSON.stringify(payload);
}

// --- wire 分块编码（与 web 端 ChatStreamWireReassembler 对齐）------------
// 融云单条自定义消息的安全上限约 9KB。帧（text+reasoning 全量快照）超过
// 9000 字节时切成若干 chat_stream_chunk 块发送；web 端重组器按 sha256 +
// chunkIndex 还原完整帧后再走正常快照合并。块参数与 web fixture 编码器
// 逐字段一致（protocolVersion: 2 / 5700 字节每块）。
const WIRE_DIRECT_BYTE_LIMIT = 9000;
const WIRE_CHUNK_BYTES = 5700;

export interface WireEncodedFrame {
  /** objectName：直发帧用 chat_stream，分块帧用 chat_stream_chunk。 */
  objectName: string;
  content: string;
}

/** 把一帧编码为零或多条可发送的 IM 消息（直发或 wire 分块）。 */
export function encodeChatStreamWireFrames(payload: Record<string, unknown>): WireEncodedFrame[] {
  const serialized = JSON.stringify(payload);
  const bytes = Buffer.from(serialized, "utf8");
  if (bytes.length <= WIRE_DIRECT_BYTE_LIMIT) {
    return [{ objectName: "chat_stream", content: serialized }];
  }
  const sha256 = createHash("sha256").update(bytes).digest("hex");
  const chunkCount = Math.ceil(bytes.length / WIRE_CHUNK_BYTES);
  const frames: WireEncodedFrame[] = [];
  for (let chunkIndex = 0; chunkIndex < chunkCount; chunkIndex += 1) {
    const slice = bytes.subarray(chunkIndex * WIRE_CHUNK_BYTES, (chunkIndex + 1) * WIRE_CHUNK_BYTES);
    frames.push({
      objectName: "chat_stream_chunk",
      content: JSON.stringify({
        msg_type: "chat_stream_chunk",
        protocolVersion: 2,
        sha256,
        messageId: `wire_${sha256}`,
        chunkCount,
        chunkIndex,
        data: slice.toString("base64"),
      }),
    });
  }
  return frames;
}

// chat_stop 解析：web 端发的停止请求（msg_type: chat_stop）。返回 null 表示
// 不是停止请求（透传给其它 handler）。
export function parseChatStopRequest(raw: string): ChatStreamStopRequestParsed | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const c = parsed as Record<string, unknown>;
  if (c.msg_type !== "chat_stop" || typeof c.stream_id !== "string") return null;
  const requestId = typeof c.request_id === "string" ? c.request_id : "";
  return { requestId, streamId: c.stream_id };
}
