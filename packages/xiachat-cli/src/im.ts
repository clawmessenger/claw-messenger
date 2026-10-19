import { writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ARGV_PROMPT_MAX_CHARS } from "./agents.js";
import { collectProducedArtifacts, snapshotWorkdir, type ProducedArtifact, type WorkdirSnapshot } from "./artifacts.js";
import type { ActivityStep } from "./agent-activity.js";
import { decodeCommandContent, encodeCommandResult } from "./protocol.js";
import { buildChatStreamFrame, encodeChatStreamWireFrames, parseChatStopRequest, type ChatStreamStatus, type ChatStreamTimelineEntry } from "./chat-stream.js";
import { buildHtmlCard, extractHtmlFence, htmlReviseFromFilePrompt, htmlRevisePrompt } from "./cards.js";
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

// 已读回执的「该不该回、回给谁、走哪条通道」判定。
//
// 融云不替客户端回执：web 端发单聊/群聊消息时带 needReceipt，人类气泡要等
// 收件端回调回执接口才会从「未读」翻成「已读」。一个只收不回的 AI 节点，会让
// 每次提问都永远停在「未读」——这正是本次要修的问题。
//
// 抽成纯函数是为了能脱离 imlib 单测（imlib 只在浏览器/Node 运行时可用）。
// 返回 null 表示不回执。
export type ReadReceiptPlan =
  | { channel: "private"; targetId: string; messageUId: string; sentTime: number }
  | { channel: "group"; conversationType: 3; targetId: string; messageUId: string };

export interface ReadReceiptCandidate {
  messageUId?: string;
  /** 发送方是否要求回执（web 端单聊/群聊发送时置 true）。 */
  needReceipt?: boolean;
  /** 融云 MessageDirection：1=己方发送，2=接收。 */
  messageDirection?: number;
  senderUserId?: string;
  targetId?: string;
  conversationType?: number;
  sentTime?: number;
}

export function planReadReceipt(
  msg: ReadReceiptCandidate,
  selfUserId?: string,
): ReadReceiptPlan | null {
  // 没有 needReceipt 就不回。融云只在单聊/群聊允许该标记（聊天室本就没有
  // 已读能力），而 AI 节点之间的 relay / chat_stream 帧也不带它——顺势把
  // 群内「每个节点对彼此的每一帧都回一次回执」的 N² 回执风暴挡在门外。
  if (!msg.messageUId || msg.needReceipt !== true) return null;
  // 多端同步会把本端发出的消息回推；给自己的消息回执没有意义。
  if (msg.messageDirection === 1) return null;
  if (selfUserId && msg.senderUserId === selfUserId) return null;
  if (!msg.senderUserId || !msg.targetId) return null;
  if (msg.conversationType === 1) {
    return {
      channel: "private",
      targetId: msg.senderUserId,
      messageUId: msg.messageUId,
      sentTime: Number(msg.sentTime || 0),
    };
  }
  if (msg.conversationType === 3) {
    return {
      channel: "group",
      conversationType: 3,
      targetId: msg.targetId,
      messageUId: msg.messageUId,
    };
  }
  return null;
}

// Thin transport over @rongcloud/imlib-next so the dispatcher stays testable.
// The production adapter is wired in run.ts (Task 8); tests use an in-memory fake.
export interface IMTransport {
  connect(appKey: string, token: string): Promise<void>;
  sendMessage(toUserId: string, objectName: string, content: string): Promise<void>;
  // Sends into an arbitrary conversation (1 private / 3 group / 4 chatroom).
  // chat_stream frames for group/chatroom relay turns ride this; private
  // streams keep using sendMessage (conversationId === peer user id).
  sendMessageTo?: (
    conversationType: 1 | 3 | 4,
    targetId: string,
    objectName: string,
    content: string,
  ) => Promise<void>;
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
  // Streams a chat_stream frame into a conversation (1 private / 3 group /
  // 4 chatroom). The web renders these incrementally (chat-stream.ts there);
  // when omitted the dispatcher degrades to the old single-shot reply.
  sendStream?: (
    conversationType: 1 | 3 | 4,
    conversationId: string,
    objectName: string,
    content: string,
  ) => Promise<void>;
  // The RongCloud user id of THIS node (stream frames carry node_id = self).
  // Falls back to selfUserId when omitted.
  nodeId?: string;
  // Directory for the html_revise scratch file (see writeReviseSource). Wired
  // by run.ts to the agent's working directory so the file lands inside the
  // workspace the agent may read; falls back to the OS temp dir when omitted.
  reviseSourceDir?: string;
  // agent 的工作目录。设了它才会做「本轮产出了哪些文件」的采集（见
  // artifacts.ts）：一轮开始前拍快照、结束后取差集。省略即不采集。
  artifactDir?: string;
  // 把采集到的文件送上服务端，返回可被 web 引用的地址。省略、抛错或返回
  // undefined 都只意味着「这个文件没上去」，绝不影响正常回合。
  uploadArtifact?: (artifact: ProducedArtifact) => Promise<UploadedArtifact | undefined>;
}

/** 一个已经落到服务端的产出文件。 */
export interface UploadedArtifact {
  /** 站点相对的 API 路径（/api/claw/files/<key>），由 web 端补自己的前缀。 */
  url: string;
  name: string;
  size: number;
  /** MIME，web 据此决定用 iframe / 图片 / 下载哪种方式打开。 */
  type: string;
}

export interface TurnRunner {
  runTurn: (prompt: string, model?: string, sessionKey?: string) => Promise<string>;
  // Streaming variant: same contract as runTurn plus per-chunk deltas of the
  // answer as the agent produces them. Optional: dispatchers fall back to
  // runTurn when absent (tests / agents without stdout streaming).
  runTurnStreamed?: (
    prompt: string,
    model: string | undefined,
    // 三个参数都是「最新全量的一部分」：正文增量、思考流增量、过程流快照。
    // 分开传而不是合成一个对象，是为了让调用方（run.ts 的 onChunk /
    // onReasoningChunk / onActivity）各自只需管自己那一摊。
    onDelta: (delta: string, reasoningDelta?: string, activity?: ActivityStep[]) => void,
    signal?: AbortSignal,
    sessionKey?: string,
  ) => Promise<string>;
}

// 一轮 your_turn 的会话形态。三种场合共用同一条 IM 命令：
//   - group    普通群组（融云 group）闲聊的按序中继，服务端提示词就是
//              「这是普通群聊的一次自由对话…」；
//   - chatroom 聊天室（讨论组的非讨论态）闲聊的按序中继，提示词同构；
//   - discussion v3 圆桌讨论的正式轮次，role_prompt 是角色人设。
// 三者的提示词必须分开：旧版本一律盖上「[discussion] … Answer as this node
// in the discussion.」抬头，agent 于是在普通群聊里也当自己是圆桌成员等主持
// 人发题，把群里的问题答成「等待主题发布」。
export type TurnVenue = "private" | "group" | "chatroom" | "discussion";

// 服务端闲聊中继的提示词恒以这个抬头开头（server chatroom_relay.go 的
// buildPrompt：普通群聊与聊天室用同一段格式，"[group chat] <scope>：…"）。
// v3 圆桌讨论的 role_prompt 是操作者的 人设提示词 + 角色人设，不带它。
// conversation_type 区分不开中继与讨论（都是 4），只能看提示词本身。
const RELAY_PROMPT_PREFIX = "[group chat] ";

export function resolveTurnVenue(
  conversationType: number | undefined,
  rolePrompt: string,
): TurnVenue {
  const isRelayPrompt = rolePrompt.trimStart().startsWith(RELAY_PROMPT_PREFIX);
  if (conversationType === 1) return "private";
  if (conversationType === 3) return "group";
  if (conversationType === 4) return isRelayPrompt ? "chatroom" : "discussion";
  // 缺 conversation_type 的老协调器：只剩提示词可判。
  return isRelayPrompt ? "chatroom" : "discussion";
}

// 一轮发言绑定的 agent 会话键：同一形态 + 同一会话 id 才共享上下文。
// 不带键时（openclaw）所有形态挤在同一个主会话里，讨论组的共识会被群聊继承。
export function turnSessionKey(venue: TurnVenue, conversationId: string): string {
  return `cm-${venue}-${conversationId}`;
}

// html_revise 要把上一版 HTML 交给 agent。内嵌进提示词最省事，但页面一大就超过
// agents.ts 的 argv 上限，整轮退化到 stdin —— codex/opencode/hermes 对 stdin
// 一律拒绝或挂起，只能空转到 120s 超时，用户看到「生成失败: agent turn timeout
// after 120000ms」。所以超限时改成落盘：提示词只留路径，agent 自己去读。
// 落在 reviseSourceDir（run.ts 接的是 agent 的工作目录，保证在它可读范围内），
// 缺省退到系统临时目录。用完即删 —— 交给调用方 cleanup()。
export function writeReviseSource(
  dir: string | undefined,
  cardId: string,
  html: string,
): { path: string; cleanup: () => void } {
  // cardId 目前只可能是本进程生成的 `card-<ts>-<rand>`，但仍按不可信输入处理：
  // 它要参与文件名，绝不能让 `..` / 分隔符把文件写出目标目录。
  const safeId = cardId.replace(/[^A-Za-z0-9_-]/g, "_");
  const path = join(dir ?? tmpdir(), `cm-revise-${safeId}.html`);
  writeFileSync(path, html, "utf8");
  return {
    path,
    cleanup: () => {
      try {
        rmSync(path, { force: true });
      } catch {
        // 删不掉就算了：临时文件残留无害，不能因此打断这一轮。
      }
    },
  };
}

// The web's 默认模型 panel aborts after 8s (15s while the node looks offline),
// so an answer sent later than this can only be wasted — and answering a whole
// replayed offline backlog at once is what trips RongCloud's send rate limit.
const STALE_MODEL_CATALOG_REQUEST_MS = 30_000;

// The web waits 8s for a probe and 20s for a recover, so a replay older than
// this can no longer be delivered to anyone waiting — skip it instead of
// spending an install + a 20s agent spawn on it.
const STALE_RUNTIME_RECOVERY_REQUEST_MS = 60_000;

// chat_stream 帧的最小发送间隔：融云自定义消息有每秒条数限频（20604
// SEND_FREQUENCY_TOO_FAST），逐 token 发帧必然触发；快照帧按此间隔节流，
// 攒下的内容由下一帧或终帧补全（快照语义使丢帧无害）。
const STREAM_FRAME_INTERVAL_MS = 500;

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
  // streamId -> AbortController for turns in flight, so chat_stop can cancel
  // the active one. Entries live exactly for the duration of a streamed turn.
  private readonly activeStreams = new Map<string, AbortController>();

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
      //
      // fail-closed：判定条件是「不是私聊就观察」，而不是「是群/聊天室才观察」。
      // 0.2.5 及更早的二进制不看 conversationType，任何 RC:TxtMsg 都当私聊提问
      // 并私发回答，于是群聊/讨论组里的广播被当成私聊问题，回答被私发给提问者
      // ——房间里的话串进私聊（消息串联）。未知/缺失/异常类型（0、undefined、
      // 字符串）同样一律按广播处理，宁可漏答也不串会话。
      if (msg.conversationType !== 1) {
        const kind = msg.conversationType === 3 ? "group"
          : msg.conversationType === 4 ? "chatroom"
          : `unknown(${String(msg.conversationType)})`;
        console.log(`im in: observing ${kind} broadcast (only private messages are prompts)`);
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
      // 私聊也走流式：web 端 chat_stream 帧先逐帧渲染，完成后 command_result
      // / 卡片兜底。sendStream 缺失（测试）时退回整包回复。
      await this.runStreamedTurn({
        conversationType: 1,
        conversationId: msg.fromUserId,
        requestMessageId: msg.messageUId || `req-${Date.now()}`,
        requesterId: msg.fromUserId,
        prompt: text,
        model: undefined,
        // 私聊按对端分桶：不给键的话 openclaw 会把私聊也塞进主会话，和群聊、
        // 讨论组共享一串上下文。
        sessionKey: turnSessionKey("private", msg.fromUserId),
        turns,
        fallback: (out) => this.deliverAgentReply(msg.fromUserId, out),
      });
      return;
    }

    // chat_stop：web 的「停止生成」。取消本轮活动流（runStreamedTurn 的
    // AbortController），并按协议回 chat_stop_result。
    if (msg.objectName === "chat_stop") {
      const stop = parseChatStopRequest(msg.content);
      if (stop) await this.handleChatStop(msg, stop.requestId, stop.streamId);
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
        conversation_type?: number; requester_id?: string;
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
      const venue = resolveTurnVenue(params.conversation_type, persona);
      const isRelayChat = venue === "group" || venue === "chatroom";
      // 闲聊中继（群聊/聊天室）：服务端的提示词本身就是完整指令（「[group chat]
      // 这是普通群聊的一次自由对话。用户刚在群里发言：<原话>…请以「X」的身份
      // 回应用户」），原样喂给 agent 即可。**不要**再补 round / speaking_order /
      // 「Answer as this node in the discussion.」这类圆桌词汇 —— 旧版对群聊也
      // 盖这层抬头，agent 于是当自己是圆桌成员等主持人发题，把群聊问题答成
      // 「等待主题发布」。普通群聊就是闲聊，没有轮次、没有主题、没有主持。
      const prompt = isRelayChat
        ? (persona
          || `[${venue === "group" ? "group chat" : "chatroom"}] 用户刚在${venue === "group" ? "群里" : "聊天室"}发言，请直接回应用户。`)
        : [
          `[discussion] round=${params.round ?? "?"} speaking_order=${params.speaking_order ?? "?"}`,
          params.role_name ? `role: ${params.role_name}` : "",
          "Answer as this node in the discussion.",
          persona ? `\n${persona}` : "",
          persona ? "Stay in character. Do not repeat or summarise these instructions." : "",
        ].filter(Boolean).join("\n");
      // 群/聊天室闲聊中继轮次：回答同时以 chat_stream 流帧广播进房间，
      // 完成帧之后仍回 command_result（中继据此推进下一位发言人）。
      const hasConvType = params.conversation_type === 3 || params.conversation_type === 4
        || params.conversation_type === 1;
      const convType: 1 | 3 | 4 = !hasConvType ? 1
        : params.conversation_type === 3 ? 3
        : params.conversation_type === 4 ? 4 : 1;
      await this.runStreamedTurn({
        conversationType: convType,
        conversationId: replyTo,
        requestMessageId: cmd.request_id,
        requesterId: typeof params.requester_id === "string" ? params.requester_id : msg.fromUserId,
        prompt,
        model: params.model,
        // 讨论组/群聊/聊天室各按自己的会话 id 分桶，讨论组里聊出来的共识
        // 不会顺着 agent 的共享会话爬进普通群聊。
        sessionKey: turnSessionKey(venue, replyTo),
        turns,
        fallback: async (out) => {
          await this.deps.send(
            replyTo,
            "command",
            encodeCommandResult(cmd.request_id, "text", { content: out }),
          );
        },
        onError: async (errMsg) => {
          await this.deps.send(
            replyTo,
            "command",
            encodeCommandResult(cmd.request_id, "error", {
              error: errMsg,
            }),
          );
        },
      });
    }
  }

  // runStreamedTurn 是所有会话形态共用的流式回合执行器：
  //  1. 先发 status=processing 的 0 号帧（web 立刻出现打字气泡）；
  //  2. runTurnStreamed 可用时逐 chunk 发 status=streaming 全量快照帧；
  //  3. 结束发 completed 终帧；异常发 failed 帧；被停止发 cancelled 帧；
  //  4. 终帧之后仍回调 fallback（command_result / 卡片），保持中继推进与
  //     旧消费方兼容——旧 web 不解析 chat_stream 也照常工作。
  // 帧节流：融云自定义消息限频，帧间隔至少 STREAM_FRAME_INTERVAL_MS；
  // 帧超限时由 encodeChatStreamWireFrames 自动切 wire 分块。
  private async runStreamedTurn(opts: {
    conversationType: 1 | 3 | 4;
    conversationId: string;
    requestMessageId: string;
    requesterId: string;
    prompt: string;
    model: string | undefined;
    turns: TurnRunner;
    /** 见 turnSessionKey：把这一轮绑到本会话独立的 agent 会话上。 */
    sessionKey?: string;
    fallback: (out: string) => Promise<void>;
    onError?: (errMsg: string) => Promise<void>;
  }): Promise<void> {
    const ctx = {
      streamId: `stream-${opts.requestMessageId}-${Math.random().toString(36).slice(2, 8)}`,
      requestMessageId: opts.requestMessageId,
      requesterId: opts.requesterId,
      conversationType: opts.conversationType,
      conversationId: opts.conversationId,
    };
    const frame = (
      seq: number,
      status: ChatStreamStatus,
      text: string,
      reasoning?: string,
      errorCode?: string,
      activity?: ActivityStep[],
      timeline?: ChatStreamTimelineEntry[],
    ) => {
      const serialized = buildChatStreamFrame({
        streamId: ctx.streamId,
        requestMessageId: ctx.requestMessageId,
        requesterId: ctx.requesterId,
        nodeId: this.deps.nodeId || this.deps.selfUserId || "",
        conversationType: ctx.conversationType,
        conversationId: ctx.conversationId,
        seq,
        status,
        text,
        ...(reasoning ? { reasoning } : {}),
        ...(activity && activity.length > 0 ? { activity } : {}),
        ...(timeline && timeline.length > 0 ? { timeline } : {}),
        ...(errorCode ? { errorCode } : {}),
      });
      // 融云单条消息 ~9KB 上限：超限帧切 wire 分块（web 端重组器还原）。
      // 帧是全量快照（text+reasoning 同帧），不再按 1800 字符切片多帧。
      const payload = JSON.parse(serialized) as Record<string, unknown>;
      return encodeChatStreamWireFrames(payload);
    };
    const emit = async (
      seq: number,
      status: ChatStreamStatus,
      text: string,
      reasoning?: string,
      errorCode?: string,
      activity?: ActivityStep[],
      timeline?: ChatStreamTimelineEntry[],
    ) => {
      const sendStream = this.deps.sendStream;
      if (!sendStream) return;
      try {
        for (const wire of frame(seq, status, text, reasoning, errorCode, activity, timeline)) {
          await sendStream(ctx.conversationType, ctx.conversationId, wire.objectName, wire.content);
        }
      } catch (err) {
        // 单帧失败不打断回答：流帧只是 UI 加速，权威内容走 fallback。
        console.error("stream frame send failed:", err instanceof Error ? err.message : err);
      }
    };
    const canStream = Boolean(this.deps.sendStream) && Boolean(opts.turns.runTurnStreamed);
    // 产物采集的第一张快照：必须在 agent 真正开跑之前拍，否则「本轮新建的文件」
    // 会被算成「本来就有的文件」。目录很大时这一步也要花时间，所以只在真的
    // 配了 artifactDir + uploadArtifact 时才拍。
    const artifactSnapshot = this.deps.artifactDir && this.deps.uploadArtifact
      ? snapshotWorkdir(this.deps.artifactDir)
      : undefined;
    // Active-stream registry keyed by streamId for chat_stop cancellation.
    const controller = new AbortController();
    this.activeStreams.set(ctx.streamId, controller);
    // 过程流的补偿帧定时器：声明在 try 之外，finally 才能把它撤掉（见文件末尾）。
    let pendingActivityFlush: ReturnType<typeof setTimeout> | null = null;
    try {
      if (!canStream) {
        // Degraded path (tests, or an agent without streamed stdout): the
        // old single-shot reply is the whole turn — including the old error
        // semantics (single chat: log only; your_turn: error command_result).
        try {
          const out = await opts.turns.runTurn(opts.prompt, opts.model, opts.sessionKey);
          const uploaded = await this.collectArtifacts(artifactSnapshot);
          await opts.fallback(this.ensureVisibleReply(out, uploaded));
          await this.sendArtifactMessages(ctx, uploaded);
        } catch (err) {
          const errMsg = err instanceof Error ? err.message : String(err);
          if (opts.onError) {
            await opts.onError(errMsg);
            return;
          }
          // Single-chat failure: do not reply; the user can retry.
          console.error("agent turn failed:", errMsg);
        }
        return;
      }
      await emit(0, "processing", "");
      let seq = 1;
      let snapshot = "";
      // 思考流快照：与正文快照同帧携带（reasoning 字段），同 seq 递增。
      let reasoningSnapshot = "";
      // 过程流快照：agent 本轮的工具调用步骤（联网搜索/执行命令/读写文件…）。
      let activitySnapshot: ActivityStep[] = [];
      // 过程时间线：思考段与工具调用的真实顺序。思考增量接到最后一个思考段
      // （新的一段思考——工具之后又思考——新开条目）；工具步骤只存 id 引用
      // （内容在 activitySnapshot 里），这样同一份信息不会在帧里存两遍。
      // web 端按它渲染「思考卡片 ↔ 工具卡片」交错的流式时间线。
      let timeline: ChatStreamTimelineEntry[] = [];
      const activeReasoningEntry = (): number | null => {
        const last = timeline[timeline.length - 1];
        return last && last[0] === "r" ? timeline.length - 1 : null;
      };
      let lastSent = 0;
      // 节流窗口内到达的过程流更新需要一次补偿帧：长工具阶段正文久久不来，
      // 若完全等下一个 chunk，用户就看不到「正在联网搜索」这一步。
      const emitStreamingFrame = async () => {
        const frameSeq = seq;
        seq += 1;
        lastSent = Date.now();
        await emit(frameSeq, "streaming", snapshot, reasoningSnapshot || undefined, undefined, activitySnapshot, timeline);
      };
      const sendDelta = async (delta: string, reasoningDelta?: string, activity?: ActivityStep[]) => {
        if (controller.signal.aborted) return;
        snapshot += delta;
        if (reasoningDelta) {
          reasoningSnapshot += reasoningDelta;
          const at = activeReasoningEntry();
          if (at !== null) timeline[at]![1] += reasoningDelta;
          else timeline.push(["r", reasoningDelta]);
        }
        if (activity) {
          // 工具步骤按 activity 快照里的顺序补进时间线：新步骤（id 没出现过）
          // 追加 ["s", id]；已有的只更新快照，时间线不动。claude/opencode 的
          // 工具在思考之后出现，顺序天然正确。
          const seen = new Set(timeline.filter((e): e is ["s", string] => e[0] === "s").map((e) => e[1]));
          for (const step of activity) {
            if (!seen.has(step.id)) timeline.push(["s", step.id]);
          }
          activitySnapshot = activity;
        }
        const now = Date.now();
        // 节流：距上一帧不足间隔时只攒不动，由下个 chunk 或终帧冲刷。
        if (now - lastSent < STREAM_FRAME_INTERVAL_MS) {
          if (activity && pendingActivityFlush === null) {
            pendingActivityFlush = setTimeout(() => {
              pendingActivityFlush = null;
              void emitStreamingFrame();
            }, STREAM_FRAME_INTERVAL_MS - (now - lastSent));
          }
          return;
        }
        await emitStreamingFrame();
      };
      let finalText = "";
      try {
        finalText = await opts.turns.runTurnStreamed!(
          opts.prompt,
          opts.model,
          sendDelta,
          controller.signal,
          opts.sessionKey,
        );
      } catch (err) {
        if (controller.signal.aborted) {
          await emit(seq, "cancelled", snapshot, reasoningSnapshot || undefined, undefined, activitySnapshot, timeline);
          return;
        }
        const errMsg = err instanceof Error ? err.message : String(err);
        await emit(seq, "failed", snapshot, reasoningSnapshot || undefined, errMsg.slice(0, 120), activitySnapshot, timeline);
        if (opts.onError) {
          await opts.onError(errMsg);
          return;
        }
        throw err;
      }
      // 已被 chat_stop 中止：不再发任何终帧（cancelled 帧只在 agent 轮
      // 因 abort 抛错时由 catch 分支发出；这里 turn 已正常返回，沉默即止）。
      if (controller.signal.aborted) {
        return;
      }
      // 上传本轮产出的文件，拿到 web 可引用的地址。必须在发终帧之前做：
      // web 端把「流式终帧」与「随后广播的兜底整包文本」按内容相等去重
      // （lib/ai-message-lifecycle.ts 的 streamEchoIndex），两者正文一旦不同，
      // 群里就会出现一对重复气泡。
      const uploaded = await this.collectArtifacts(artifactSnapshot);
      // 终帧用完整全文（agent 的最终 stdout 可能比 chunk 累积更权威）。
      const visibleText = this.ensureVisibleReply(finalText || snapshot, uploaded);
      // 每轮一行诊断（不含正文）：排查「某个会话看不到思考/过程」时先看这里
      // —— 设备端唯一一处把三路快照的长度写进日志的地方。reasoning=0 说明
      // 这一轮的 agent 根本没吐思考文本（或它的结构化输出没开），不是 web 丢帧。
      console.log(
        `stream: conv=${ctx.conversationType}/${ctx.conversationId} req=${ctx.requestMessageId}`
        + ` text=${visibleText.length} reasoning=${reasoningSnapshot.length}`
        + ` activity=${activitySnapshot.length} frames=${seq}`,
      );
      await emit(seq, "completed", visibleText, reasoningSnapshot || undefined, undefined, activitySnapshot, timeline);
      await opts.fallback(visibleText);
      await this.sendArtifactMessages(ctx, uploaded);
    } finally {
      // 补偿帧还在等就别再发了：这一轮已经收尾，晚到的帧只会顶掉终帧。
      if (pendingActivityFlush !== null) {
        clearTimeout(pendingActivityFlush);
        pendingActivityFlush = null;
      }
      this.activeStreams.delete(ctx.streamId);
    }
  }

  /**
   * 采集本轮 agent 产出的文件并上传。任何一步失败都只是「这一轮没有附件」，
   * 绝不能把正常回复带崩 —— 采集是锦上添花，不是回复的必经环节。
   */
  private async collectArtifacts(snapshot: WorkdirSnapshot | undefined): Promise<UploadedArtifact[]> {
    const dir = this.deps.artifactDir;
    const upload = this.deps.uploadArtifact;
    if (!dir || !snapshot || !upload) return [];
    let produced: ProducedArtifact[];
    try {
      produced = collectProducedArtifacts(dir, snapshot);
    } catch (err) {
      console.error("artifact scan failed:", err instanceof Error ? err.message : err);
      return [];
    }
    const uploaded: UploadedArtifact[] = [];
    for (const artifact of produced) {
      try {
        const result = await upload(artifact);
        if (result) uploaded.push(result);
      } catch (err) {
        console.error(
          `artifact upload failed (${artifact.name}):`,
          err instanceof Error ? err.message : err,
        );
      }
    }
    if (uploaded.length > 0) {
      console.log(`artifacts: uploaded ${uploaded.length} file(s) for this turn`);
    }
    return uploaded;
  }

  /** 把产出文件作为可点击的气泡发进本轮会话（私聊/群/聊天室/讨论组同一条路）。 */
  private async sendArtifactMessages(
    ctx: { conversationType: 1 | 3 | 4; conversationId: string },
    uploaded: UploadedArtifact[],
  ): Promise<void> {
    const sendStream = this.deps.sendStream;
    if (!sendStream || uploaded.length === 0) return;
    for (const file of uploaded) {
      const payload = JSON.stringify({
        msg_type: "file_message",
        protocol_version: 1,
        node_id: this.deps.nodeId || this.deps.selfUserId || "",
        file,
      });
      try {
        await sendStream(ctx.conversationType, ctx.conversationId, "file_message", payload);
      } catch (err) {
        console.error("artifact message send failed:", err instanceof Error ? err.message : err);
      }
    }
  }

  /**
   * 正文兜底：agent 写盘不打印是常态（尤其被要求「生成个文件」时），此时
   * stdout 为空。空正文在中继那边会被当成「该成员没有产出」，在私聊里则表现
   * 为一个永远填不满的气泡 —— 两种都是用户眼里的「没有任何回复」。所以这里
   * 用「已生成哪些文件」或一句明确的说明把空正文补上。
   *
   * 注意只补空正文：agent 用 ```html 代码块作答时正文非空，得原样交给
   * deliverAgentReply 走卡片，不能被这里改写。
   */
  private ensureVisibleReply(text: string, uploaded: UploadedArtifact[]): string {
    if (text.trim()) return text;
    if (uploaded.length > 0) {
      return `已生成 ${uploaded.length} 个文件：${uploaded.map((file) => file.name).join("、")}`;
    }
    return "（本轮没有产出内容）";
  }

  // chat_stop 处理：取消活动流并回执。
  private async handleChatStop(msg: InboundIMMessage, requestId: string, streamId: string): Promise<void> {
    const controller = this.activeStreams.get(streamId);
    if (!controller) {
      await this.sendStopResult(msg, requestId, streamId, "rejected", "no_active_task");
      return;
    }
    controller.abort();
    this.activeStreams.delete(streamId);
    await this.sendStopResult(msg, requestId, streamId, "accepted", "stop_requested");
  }

  private async sendStopResult(
    msg: InboundIMMessage,
    requestId: string,
    streamId: string,
    status: "accepted" | "rejected",
    code: string,
  ): Promise<void> {
    const frame = JSON.stringify({
      msg_type: "chat_stop_result",
      protocol_version: 1,
      request_id: requestId,
      stream_id: streamId,
      node_id: this.deps.nodeId || this.deps.selfUserId || "",
      status,
      code,
    });
    await this.deps.send(msg.fromUserId, "chat_stop_result", frame).catch(() => {});
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

    let source: { path: string; cleanup: () => void } | undefined;
    try {
      // 上一版 HTML 内嵌进提示词，长页面会超过 argv 上限并退化到 stdin
      // （见 writeReviseSource）：超限时改成落盘 + 只给路径。
      const inlinePrompt = htmlRevisePrompt(entry.html, instruction);
      let prompt = inlinePrompt;
      if (inlinePrompt.length > ARGV_PROMPT_MAX_CHARS) {
        try {
          source = writeReviseSource(this.deps.reviseSourceDir, entry.cardId, entry.html);
          prompt = htmlReviseFromFilePrompt(source.path, instruction, entry.html.length);
        } catch {
          // 落盘失败（目录只读等）：退回内嵌，行为与修前一致，不额外报错。
          prompt = inlinePrompt;
          source = undefined;
        }
      }
      // 与私聊/群聊轮次一致地绑会话：不带 sessionKey 时 openclaw 会把 revise 轮
      // 塞进所有会话共享的默认主会话，和别的会话串味。
      const out = await turns.runTurn(prompt, undefined, turnSessionKey("private", msg.fromUserId));
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
    } finally {
      source?.cleanup();
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
