// Message protocol shared with the Go server (server/internal/integrations/
// rongcloud/types.go). objectName "command" is a literal, NOT RC:CmdMsg.
// Keep field names byte-compatible: request_id, msg_type, stream_type, turn_id.

export interface CommandContent {
  request_id: string;
  service: string;
  action: string;
  params?: Record<string, unknown>;
}

// Semantic result kind; rides INSIDE payload (payload.kind). The outer
// msg_type is always the literal "command_result" — the Go webhook gate
// (channel.go isCommandResult) routes on that exact string, and the
// server's own sendCommandResult (client.go) emits nothing else.
export type CommandResultKind = "text" | "stream" | "error";

export interface CommandResultContent {
  request_id: string;
  msg_type: "command_result";
  payload: Record<string, unknown>;
}

export interface StreamFrame {
  turn_id: string;
  stream_type: "start" | "chunk" | "end" | "error";
  content: string;
  error?: string;
}

export function encodeCommand(
  requestId: string,
  service: string,
  action: string,
  params?: Record<string, unknown>,
): string {
  return JSON.stringify({ request_id: requestId, service, action, params });
}

export function decodeCommandContent(raw: string): CommandContent | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const c = parsed as Record<string, unknown>;
  if (typeof c.request_id !== "string" || typeof c.action !== "string") {
    return null;
  }
  return {
    request_id: c.request_id,
    service: typeof c.service === "string" ? c.service : "",
    action: c.action,
    params: (c.params as Record<string, unknown>) ?? undefined,
  };
}

export function encodeCommandResult(
  requestId: string,
  kind: CommandResultKind,
  payload: Record<string, unknown>,
): string {
  return JSON.stringify({
    request_id: requestId,
    msg_type: "command_result",
    payload: { kind, ...payload },
  });
}

export function encodeStreamFrame(
  turnId: string,
  streamType: StreamFrame["stream_type"],
  content: string,
  error?: string,
): string {
  return JSON.stringify({
    turn_id: turnId,
    stream_type: streamType,
    content,
    ...(error !== undefined ? { error } : {}),
  });
}
