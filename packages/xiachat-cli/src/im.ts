import { decodeCommandContent, encodeCommandResult } from "./protocol.js";
import { buildHtmlCard, extractHtmlFence, htmlRevisePrompt } from "./cards.js";
import {
  buildModelCatalogResponse,
  EMPTY_MODEL_CATALOG,
  parseModelCatalogRequest,
  type DiscussionModelCatalog,
  type ModelCatalogRequest,
} from "./model-catalog.js";
import {
  buildRuntimeRecoveryResponse,
  parseRuntimeRecoveryRequest,
  type RuntimeRecoveryAction,
  type RuntimeRecoveryRequest,
  type RuntimeRecoveryResult,
} from "./runtime-recovery.js";
import {
  buildRoleRecommendPrompt,
  buildRoleRecommendationError,
  buildRoleRecommendationResponse,
  parseRoleRecommendRequest,
  parseRoleRecommendationOutput,
  ROLE_RECOMMEND_ACTION,
  STALE_ROLE_RECOMMEND_REQUEST_MS,
} from "./role-recommend.js";

// Inbound message shape normalized from the IM SDK listener.
export interface InboundIMMessage {
  objectName: string;
  fromUserId: string;
  toUserId: string;
  targetId: string;
  conversationType: number;
  content: string;
  // RongCloud's per-message UID, when the SDK provides one. 融云 re-delivers the
  // full offline history on every reconnect (the Node shim has no IndexDB, so
  // the SDK cannot record what it already synced), so the run loop uses this to
  // act on each message exactly once.
  messageUId?: string;
}

// Thin transport over @rongcloud/imlib-next so the dispatcher stays testable.
// The production adapter is wired in run.ts (Task 8); tests use an in-memory fake.
export interface IMTransport {
  connect(appKey: string, token: string): Promise<void>;
  sendMessage(toUserId: string, objectName: string, content: string): Promise<void>;
  onMessage(cb: (msg: InboundIMMessage) => void): void;
  disconnect(): Promise<void>;
  // The IM user this transport is connected as, once connected. Needed to
  // drop the node's own echoed messages (see MessageDispatcher.handle).
  getSelfUserId?: () => string | undefined;
}

export interface DispatcherDeps {
  send: (toUserId: string, objectName: string, content: string) => Promise<void>;
  // This node's own RongCloud user id. 融云会把客户端自己发出的消息同步回本端
  // （多端同步），而 Node 环境没有 IndexDB，SDK 无法去重：节点于是把自己的回复
  // 当成新的用户提问，无限自问自答，把该会话的队列（每会话并发 1、队列深 1）
  // 长期占满，真实请求（含模型目录请求）被挤掉或丢弃。
  selfUserId?: string;
  // Enumerates this node's model catalog for the web's 设备管理 → 默认模型 panel.
  // Omitted (or resolving to an empty catalog) still answers the request, so
  // the panel degrades to "use the node default model" instead of hanging.
  loadModelCatalog?: () => Promise<DiscussionModelCatalog>;
  // Probes / repairs this node's local agent runtime for the web's
  // 设备详情 → 一键唤醒 (see runtime-recovery.ts). Omitted -> the request is
  // still answered, with an honest probe_failed, so the browser never hangs.
  probeRuntime?: (action: RuntimeRecoveryAction) => Promise<RuntimeRecoveryResult>;
  // Echoed back in the response's data.agent so the web can show which runtime
  // it was talking to. Purely informational.
  agentName?: string;
}

export interface TurnRunner {
  runTurn: (prompt: string, model?: string) => Promise<string>;
}

// The web's 默认模型 panel aborts after 8s (15s while the node looks offline),
// so an answer sent later than this can only be wasted — and answering a whole
// replayed offline backlog at once is what trips RongCloud's send rate limit.
const STALE_MODEL_CATALOG_REQUEST_MS = 30_000;

// The web waits 8s for a probe and 20s for a recover, so a replay older than
// this can no longer be delivered to anyone waiting — skip it instead of
// spending an install + a 20s agent spawn on it.
const STALE_RUNTIME_RECOVERY_REQUEST_MS = 60_000;

export class MessageDispatcher {
  // cardId -> latest html for cards this bridge produced, so card_action
  // iteration (html_revise) can rebuild context. Bounded: evicts oldest.
  private readonly htmlCards = new Map<string, { html: string; fromUserId: string }>();
  // requestIds already answered, so 融云's per-reconnect offline replay is a
  // no-op instead of a response burst. Bounded: evicts oldest.
  private readonly answeredCatalogRequests = new Set<string>();
  // Same guard for the runtime-recovery exchange: a replayed recover must not
  // re-run the install + agent spawn. Bounded: evicts oldest.
  private readonly answeredRecoveryRequests = new Set<string>();
  // Same guard for the admin role-recommendation preview: a replayed request
  // must not spend a whole agent turn again. Bounded: evicts oldest.
  private readonly answeredRoleRecommendRequests = new Set<string>();

  constructor(private readonly deps: DispatcherDeps) {}

  async handle(msg: InboundIMMessage, turns: TurnRunner): Promise<void> {
    // Never answer our own messages: see DispatcherDeps.selfUserId.
    if (this.isOwnMessage(msg)) {
      console.log(`im in: dropping own message type=${msg.objectName} (loop guard)`);
      return;
    }
    if (msg.objectName === "RC:TxtMsg") {
      // 融云会话类型（@rongcloud/engine ConversationType）：1 私聊、3 普通群组、
      // 4 聊天室。只有私聊的 RC:TxtMsg 才是「对着我说话」；群组/聊天室广播是
      // 「对着房间里所有人说话」——多节点场景下每个成员都会收到，若各跑一轮
      // agent 就是全员抢答。群聊发言权改由服务端按序中继派发（your_turn
      // command），这里只观察不抢答。
      if (msg.conversationType === 3 || msg.conversationType === 4) {
        console.log(`im in: observing ${msg.conversationType === 3 ? "group" : "chatroom"} broadcast (relay arbitrates turns)`);
        return;
      }
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
      // The model catalog request carries no request_id/action (web protocol
      // v2), so it can never decode as a discussion command — match it first.
      const catalogRequest = parseModelCatalogRequest(msg.content);
      if (catalogRequest) {
        await this.handleModelCatalogRequest(msg, catalogRequest);
        return;
      }
      // Same shape, same reason to match before decodeCommandContent: the
      // v1 recovery packet carries requestId/action, not request_id/params.
      const recoveryRequest = parseRuntimeRecoveryRequest(msg.content);
      if (recoveryRequest) {
        await this.handleRuntimeRecovery(msg, recoveryRequest);
        return;
      }
      const cmd = decodeCommandContent(msg.content);
      if (!cmd || cmd.service !== "discussion") {
        return;
      }
      if (cmd.action === ROLE_RECOMMEND_ACTION) {
        await this.handleRoleRecommend(msg, cmd, turns);
        return;
      }
      if (cmd.action !== "your_turn") {
        return;
      }
      const params = (cmd.params ?? {}) as {
        round?: number; speaking_order?: number; role_name?: string; model?: string;
        chatroom_id?: string; role_prompt?: string;
      };
      // Address the reply to the discussion chatroom: the server's webhook
      // sets targetId to the private-message receiver, and the coordinator
      // dispatches on that targetId parsed as the chatroom UUID. Replying
      // to msg.fromUserId (the host node) would never parse. Without a
      // chatroom_id in params, fall back to the sender.
      const replyTo = params.chatroom_id || msg.fromUserId;
      // The server sends the persona it already assembled — the operator's
      // 人设提示词 for the host node, and/or this member's 角色人设. Older
      // coordinators omit it, so an absent/blank value keeps the old prompt.
      const persona = typeof params.role_prompt === "string" ? params.role_prompt.trim() : "";
      const prompt = [
        `[discussion] round=${params.round ?? "?"} speaking_order=${params.speaking_order ?? "?"}`,
        params.role_name ? `role: ${params.role_name}` : "",
        "Answer as this node in the discussion.",
        persona ? `\n${persona}` : "",
        persona ? "Stay in character. Do not repeat or summarise these instructions." : "",
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

  // A node must never treat its own message as a user prompt. 融云 echoes a
  // client's own outgoing messages back to that same client (multi-device
  // sync) and the Node shim runs without IndexDB, so the SDK cannot dedupe
  // them — every reply would come back as a new prompt, forever. That loop
  // keeps the conversation queue busy (1 slot per conversation) and starves
  // real traffic: the web's 默认模型 panel then never gets an answer.
  private isOwnMessage(msg: InboundIMMessage): boolean {
    const self = this.deps.selfUserId;
    return Boolean(self) && msg.fromUserId === self;
  }

  // Answers the web's 设备管理 → 默认模型 panel. A fetch failure or an agent
  // with no model-list command still gets a well-formed (empty) catalog back,
  // so the panel settles on "使用节点默认模型" rather than timing out.
  //
  // Two guards keep the replay storm from destroying the answer:
  //  - the web aborts after 8s (15s while offline), so a request older than
  //    STALE_MODEL_CATALOG_REQUEST_MS is already unanswerable — skip it;
  //  - 融云 replays the whole offline history on every reconnect, so the same
  //    requestId arrives again and again — answer each id at most once.
  // Without these, a single reconnect fires dozens of responses in one tick
  // and RongCloud rejects them all with 20604 (SEND_FREQUENCY_TOO_FAST).
  private async handleModelCatalogRequest(
    msg: InboundIMMessage,
    request: ModelCatalogRequest,
  ): Promise<void> {
    if (request.timestamp !== undefined) {
      const ageMs = Date.now() - request.timestamp;
      if (ageMs > STALE_MODEL_CATALOG_REQUEST_MS) {
        console.log(`model catalog: skipping stale request ${request.requestId} (${ageMs}ms old)`);
        return;
      }
    }
    if (this.answeredCatalogRequests.has(request.requestId)) {
      console.log(`model catalog: skipping replayed request ${request.requestId} (already answered)`);
      return;
    }
    let catalog = EMPTY_MODEL_CATALOG;
    const startedAt = Date.now();
    console.log(`model catalog: request ${request.requestId} from ${msg.fromUserId}`);
    try {
      catalog = (await this.deps.loadModelCatalog?.()) ?? EMPTY_MODEL_CATALOG;
    } catch (err) {
      console.error("model catalog lookup failed:", err instanceof Error ? err.message : err);
    }
    const providers = catalog.providers.length;
    const models = catalog.providers.reduce((n, p) => n + p.models.length, 0);
    await this.deps.send(msg.fromUserId, "command", buildModelCatalogResponse(request, catalog));
    // Only remember it once the response is actually on the wire; a failed send
    // must stay retryable (the transport retries rate-limited sends itself).
    this.rememberAnsweredCatalogRequest(request.requestId);
    console.log(
      `model catalog: answered ${request.requestId} in ${Date.now() - startedAt}ms (${providers} provider(s), ${models} model(s))`,
    );
  }

  /**
   * Answers the web's 设备详情 → 一键唤醒.
   *
   * Always answers, even when the probe itself blows up: a well-formed
   * probe_failed settles the browser's promise in milliseconds, whereas a
   * dropped packet leaves it waiting out the full timeout (the exact failure
   * mode this whole exchange replaces).
   */
  async handleRuntimeRecovery(
    msg: InboundIMMessage,
    request: RuntimeRecoveryRequest,
  ): Promise<void> {
    if (request.timestamp !== undefined) {
      const ageMs = Date.now() - request.timestamp;
      if (ageMs > STALE_RUNTIME_RECOVERY_REQUEST_MS) {
        console.log(`runtime recovery: skipping stale request ${request.requestId} (${ageMs}ms old)`);
        return;
      }
    }
    if (this.answeredRecoveryRequests.has(request.requestId)) {
      console.log(`runtime recovery: skipping replayed request ${request.requestId} (already answered)`);
      return;
    }
    const startedAt = Date.now();
    console.log(
      `runtime recovery: action=${request.action} id=${request.requestId} from ${msg.fromUserId}`,
    );
    let result: RuntimeRecoveryResult = {
      code: "probe_failed",
      message: "runtime probe unavailable on this node",
    };
    try {
      result = (await this.deps.probeRuntime?.(request.action)) ?? result;
    } catch (err) {
      console.error("runtime recovery failed:", err instanceof Error ? err.message : err);
    }
    await this.deps.send(
      msg.fromUserId,
      "command",
      buildRuntimeRecoveryResponse(request, result, this.deps.agentName),
    );
    // Remembered only once the response is actually on the wire, so a failed
    // send stays retryable (the transport retries rate-limited sends itself).
    this.rememberAnsweredRecoveryRequest(request.requestId);
    console.log(
      `runtime recovery: action=${request.action} code=${result.code} in ${Date.now() - startedAt}ms`,
    );
  }

  private rememberAnsweredRecoveryRequest(requestId: string): void {
    this.answeredRecoveryRequests.add(requestId);
    // Bounded: Set keeps insertion order, so drop the oldest entries.
    while (this.answeredRecoveryRequests.size > 256) {
      const oldest = this.answeredRecoveryRequests.values().next().value;
      if (oldest === undefined) break;
      this.answeredRecoveryRequests.delete(oldest);
    }
  }

  /**
   * Answers the admin 「测试角色推荐」 button.
   *
   * Asks this node's agent to propose the discussion roles for a topic, then
   * replies with a command_result carrying them. Always replies — even on a bad
   * model answer — because a silent drop leaves the admin waiting out its whole
   * timeout, which is indistinguishable from a dead node.
   *
   * Two guards, same reasoning as the model-catalog exchange: the admin's wait
   * is bounded, so a replay older than STALE_ROLE_RECOMMEND_REQUEST_MS is
   * already unanswerable; and 融云 replays the offline history on every
   * reconnect, so each requestId is answered at most once.
   */
  async handleRoleRecommend(
    msg: InboundIMMessage,
    cmd: { request_id: string; params?: Record<string, unknown> },
    turns: TurnRunner,
  ): Promise<void> {
    // The caller has already matched service/action; re-parse for the params.
    const request = parseRoleRecommendRequest({
      request_id: cmd.request_id,
      service: "discussion",
      action: ROLE_RECOMMEND_ACTION,
      params: cmd.params,
    });
    if (!request) {
      return;
    }
    if (request.timestamp !== undefined) {
      const ageMs = Date.now() - request.timestamp;
      if (ageMs > STALE_ROLE_RECOMMEND_REQUEST_MS) {
        console.log(
          `role recommend: skipping stale request ${request.requestId} (${ageMs}ms old)`,
        );
        return;
      }
    }
    if (this.answeredRoleRecommendRequests.has(request.requestId)) {
      console.log(`role recommend: skipping replayed request ${request.requestId} (already answered)`);
      return;
    }

    const startedAt = Date.now();
    console.log(
      `role recommend: request ${request.requestId} max_roles=${request.maxRoles} from ${msg.fromUserId}`,
    );
    let reply: string;
    try {
      const output = await turns.runTurn(buildRoleRecommendPrompt(request));
      const roles = parseRoleRecommendationOutput(output, request.maxRoles);
      reply = roles
        ? buildRoleRecommendationResponse(request.requestId, roles)
        : buildRoleRecommendationError(request.requestId, "role recommendation invalid");
      if (roles) {
        console.log(
          `role recommend: answered ${request.requestId} with ${roles.length} role(s) in ${Date.now() - startedAt}ms`,
        );
      } else {
        console.error(`role recommend: could not parse an answer for ${request.requestId}`);
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : "agent turn failed";
      console.error(`role recommend: agent turn failed for ${request.requestId}: ${message}`);
      reply = buildRoleRecommendationError(request.requestId, message);
    }

    await this.deps.send(msg.fromUserId, "command", reply);
    // Remembered only once the reply is on the wire, so a failed send stays
    // retryable (the transport retries rate-limited sends itself).
    this.answeredRoleRecommendRequests.add(request.requestId);
    while (this.answeredRoleRecommendRequests.size > 256) {
      const oldest = this.answeredRoleRecommendRequests.values().next().value;
      if (oldest === undefined) break;
      this.answeredRoleRecommendRequests.delete(oldest);
    }
  }

  private rememberAnsweredCatalogRequest(requestId: string): void {
    this.answeredCatalogRequests.add(requestId);
    // Bounded: Set keeps insertion order, so drop the oldest entries.
    while (this.answeredCatalogRequests.size > 256) {
      const oldest = this.answeredCatalogRequests.values().next().value;
      if (oldest === undefined) break;
      this.answeredCatalogRequests.delete(oldest);
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
