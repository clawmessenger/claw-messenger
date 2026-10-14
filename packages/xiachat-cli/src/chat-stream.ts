// chat_stream 协议生产端：把 agent 的一轮回答以流式快照帧发进会话，
// web 端（clawmessenger-web src/lib/chat-stream.ts）解析后逐帧渲染。
//
// 帧语义（与 web 端 parseChatStreamEvent 严格对齐）：
// - text 是**全量快照**而非增量：web 端 mergeChatStreamMessage 用 seq
//   单调性保证只采纳更新的帧，丢帧不致内容错乱；
// - seq 从 0 开始单调递增；status: processing → streaming → completed /
//   cancelled / failed；
// - conversation_type: 1 私聊 / 3 普通群组 / 4 聊天室；
// - 每帧 <= ~4KB 文本：超过的快照按 1800 字符切片多帧发送（都带同一
//   stream_id 与递增 seq，web 侧按快照合并语义处理）。
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
  errorCode?: string;
}

export interface ChatStreamStopRequestParsed {
  requestId: string;
  streamId: string;
}

const MAX_SNAPSHOT_CHARS = 1800;

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
  if (input.errorCode) payload.error_code = input.errorCode;
  return JSON.stringify(payload);
}

/** 把一次全量快照切成若干 <=MAX_SNAPSHOT_CHARS 的发送片段。 */
export function sliceSnapshot(text: string): string[] {
  if (text.length <= MAX_SNAPSHOT_CHARS) return [text];
  const slices: string[] = [];
  for (let i = 0; i < text.length; i += MAX_SNAPSHOT_CHARS) {
    slices.push(text.slice(i, i + MAX_SNAPSHOT_CHARS));
  }
  return slices;
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
