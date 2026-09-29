import { decodeCommandContent, encodeCommandResult } from "./protocol.js";

// Inbound message shape normalized from the IM SDK listener.
export interface InboundIMMessage {
  objectName: string;
  fromUserId: string;
  toUserId: string;
  targetId: string;
  conversationType: number;
  content: string;
}

// Thin transport over @rongcloud/imlib-next so the dispatcher stays testable.
// The production adapter is wired in run.ts (Task 8); tests use an in-memory fake.
export interface IMTransport {
  connect(appKey: string, token: string): Promise<void>;
  sendMessage(toUserId: string, objectName: string, content: string): Promise<void>;
  onMessage(cb: (msg: InboundIMMessage) => void): void;
  disconnect(): Promise<void>;
}

export interface DispatcherDeps {
  send: (toUserId: string, objectName: string, content: string) => Promise<void>;
}

export interface TurnRunner {
  runTurn: (prompt: string, model?: string) => Promise<string>;
}

export class MessageDispatcher {
  constructor(private readonly deps: DispatcherDeps) {}

  async handle(msg: InboundIMMessage, turns: TurnRunner): Promise<void> {
    if (msg.objectName === "RC:TxtMsg") {
      let text = "";
      try {
        const parsed = JSON.parse(msg.content) as { content?: unknown };
        text = typeof parsed.content === "string" ? parsed.content : "";
      } catch {
        return;
      }
      if (!text) return;
      try {
        const out = await turns.runTurn(text);
        await this.deps.send(
          msg.fromUserId,
          "RC:TxtMsg",
          JSON.stringify({ content: out }),
        );
      } catch (err) {
        // Single-chat failure: do not reply; the user can retry.
        console.error("agent turn failed:", err instanceof Error ? err.message : err);
      }
      return;
    }

    if (msg.objectName === "command") {
      const cmd = decodeCommandContent(msg.content);
      if (!cmd || cmd.service !== "discussion" || cmd.action !== "your_turn") {
        return;
      }
      const params = (cmd.params ?? {}) as {
        round?: number; speaking_order?: number; role_name?: string; model?: string;
      };
      const prompt = [
        `[discussion] round=${params.round ?? "?"} speaking_order=${params.speaking_order ?? "?"}`,
        params.role_name ? `role: ${params.role_name}` : "",
        "Answer as this node in the discussion.",
      ].filter(Boolean).join("\n");
      try {
        const out = await turns.runTurn(prompt, params.model);
        await this.deps.send(
          msg.fromUserId,
          "command",
          encodeCommandResult(cmd.request_id, "text", { content: out }),
        );
      } catch (err) {
        await this.deps.send(
          msg.fromUserId,
          "command",
          encodeCommandResult(cmd.request_id, "error", {
            error: err instanceof Error ? err.message : "agent turn failed",
          }),
        );
      }
    }
  }
}
