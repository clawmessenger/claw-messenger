import { decodeCommandContent, encodeCommandResult } from "./protocol.js";
import { buildHtmlCard, extractHtmlFence, htmlRevisePrompt } from "./cards.js";

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
  // cardId -> latest html for cards this bridge produced, so card_action
  // iteration (html_revise) can rebuild context. Bounded: evicts oldest.
  private readonly htmlCards = new Map<string, { html: string; fromUserId: string }>();

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
        await this.deliverAgentReply(msg.fromUserId, out);
      } catch (err) {
        // Single-chat failure: do not reply; the user can retry.
        console.error("agent turn failed:", err instanceof Error ? err.message : err);
      }
      return;
    }

    if (msg.objectName === "card_action") {
      await this.handleCardAction(msg, turns);
      return;
    }

    if (msg.objectName === "command") {
      const cmd = decodeCommandContent(msg.content);
      if (!cmd || cmd.service !== "discussion" || cmd.action !== "your_turn") {
        return;
      }
      const params = (cmd.params ?? {}) as {
        round?: number; speaking_order?: number; role_name?: string; model?: string; chatroom_id?: string;
      };
      // Address the reply to the discussion chatroom: the server's webhook
      // sets targetId to the private-message receiver, and the coordinator
      // dispatches on that targetId parsed as the chatroom UUID. Replying
      // to msg.fromUserId (the host node) would never parse. Without a
      // chatroom_id in params, fall back to the sender.
      const replyTo = params.chatroom_id || msg.fromUserId;
      const prompt = [
        `[discussion] round=${params.round ?? "?"} speaking_order=${params.speaking_order ?? "?"}`,
        params.role_name ? `role: ${params.role_name}` : "",
        "Answer as this node in the discussion.",
      ].filter(Boolean).join("\n");
      try {
        const out = await turns.runTurn(prompt, params.model);
        await this.deps.send(
          replyTo,
          "command",
          encodeCommandResult(cmd.request_id, "text", { content: out }),
        );
      } catch (err) {
        await this.deps.send(
          replyTo,
          "command",
          encodeCommandResult(cmd.request_id, "error", {
            error: err instanceof Error ? err.message : "agent turn failed",
          }),
        );
      }
    }
  }

  // Agent replies containing fenced ```html blocks become preview cards;
  // plain text goes out as the usual RC:TxtMsg bubble.
  private async deliverAgentReply(toUserId: string, out: string): Promise<void> {
    const { blocks, rest } = extractHtmlFence(out);
    if (blocks.length === 0) {
      await this.deps.send(toUserId, "RC:TxtMsg", JSON.stringify({ content: out }));
      return;
    }
    const cardId = `card-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const html = blocks.join("\n");
    this.rememberHtmlCard(cardId, { html, fromUserId: toUserId });
    await this.deps.send(
      toUserId,
      "card_message",
      JSON.stringify({ msg_type: "card_message", card: buildHtmlCard({ cardId, markdown: rest, html }) }),
    );
  }

  // card_action from the web (html_revise): re-run the agent with the last
  // html + the user's instruction, then card_update-replace the same card.
  private async handleCardAction(msg: InboundIMMessage, turns: TurnRunner): Promise<void> {
    let action: { cardId?: string; action?: { kind?: string; payload?: Record<string, unknown> } };
    try {
      action = JSON.parse(msg.content);
    } catch {
      return;
    }
    const kind = action.action?.kind;
    if (kind !== "html_revise") return;
    const instruction =
      (typeof action.action?.payload?.inputValue === "string" && action.action.payload.inputValue.trim()) ||
      (typeof action.action?.payload?.instruction === "string" && action.action.payload.instruction.trim()) ||
      "";
    if (!instruction) return;

    const entry = this.findHtmlCard(action.cardId, msg.fromUserId);
    if (!entry) {
      await this.deps.send(msg.fromUserId, "RC:TxtMsg", JSON.stringify({ content: "未找到对应的 HTML 卡片，请重新生成。" }));
      return;
    }

    try {
      const out = await turns.runTurn(htmlRevisePrompt(entry.html, instruction));
      const { blocks, rest } = extractHtmlFence(out);
      if (blocks.length === 0) {
        // Model replied without a fence; surface it as text so nothing is lost.
        await this.deps.send(msg.fromUserId, "RC:TxtMsg", JSON.stringify({ content: out }));
        return;
      }
      const html = blocks.join("\n");
      this.rememberHtmlCard(entry.cardId, { html, fromUserId: entry.fromUserId });
      await this.deps.send(
        entry.fromUserId,
        "card_update",
        JSON.stringify({
          msg_type: "card_update",
          cardId: entry.cardId,
          mode: "replace",
          card: buildHtmlCard({ cardId: entry.cardId, markdown: rest, html }),
        }),
      );
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      await this.deps.send(msg.fromUserId, "RC:TxtMsg", JSON.stringify({ content: `生成失败: ${message}` }));
    }
  }

  private rememberHtmlCard(cardId: string, entry: { html: string; fromUserId: string }): void {
    this.htmlCards.set(cardId, entry);
    // Bound the registry; Map keeps insertion order, drop the oldest.
    while (this.htmlCards.size > 64) {
      const oldest = this.htmlCards.keys().next().value;
      if (oldest === undefined) break;
      this.htmlCards.delete(oldest);
    }
  }

  private findHtmlCard(cardId: string | undefined, fromUserId: string): { cardId: string; html: string; fromUserId: string } | undefined {
    if (cardId && this.htmlCards.has(cardId)) {
      const hit = this.htmlCards.get(cardId)!;
      return { cardId, html: hit.html, fromUserId: hit.fromUserId };
    }
    // Fallback: the most recent card this user owns (Map order = insertion).
    let found: { cardId: string; html: string; fromUserId: string } | undefined;
    for (const [id, entry] of this.htmlCards) {
      if (entry.fromUserId === fromUserId) found = { cardId: id, ...entry };
    }
    return found;
  }
}
