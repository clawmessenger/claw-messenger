import { writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ARGV_PROMPT_MAX_CHARS } from "./agents.js";
import { collectProducedArtifacts, snapshotWorkdir, type ProducedArtifact, type WorkdirSnapshot } from "./artifacts.js";
import type { ActivityStep } from "./agent-activity.js";
import type { AnsweredTurn, AnsweredTurnStore } from "./answered-turns.js";
import { decodeCommandContent, encodeCommandResult } from "./protocol.js";
import { buildChatStreamFrame, encodeChatStreamWireFrames, parseChatStopRequest, type ChatStreamStatus, type ChatStreamTimelineEntry } from "./chat-stream.js";
import { buildHtmlCard, extractHtmlFence, htmlReviseFromFilePrompt, htmlRevisePrompt } from "./cards.js";
import { buildAutoContinuationPrompt, classifyTurnError, describeTurnError, TurnIncompleteError } from "./turn-error.js";
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
import { formatUseModelAck, parseUseModelCommand, type UseModelCommand } from "./use-model.js";
import type { UserModelStore } from "./user-models.js";
import {
  behaviorPromptFor,
  decidePrivateChat,
  type PermissionSnapshot,
  type PrivateGateReason,
} from "./agent-permissions.js";

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
  // `force` 来自管理端「重新检测」：跳过缓存新鲜度判定，当轮必须给出新枚举结果。
  loadModelCatalog?: (opts?: { force?: boolean }) => Promise<DiscussionModelCatalog>;
  // 按用户持久化的模型偏好（见 user-models.ts）。用户在私聊里发
  // `/use-model <provider>/<model>` 时写进来的就是它，下一轮该用户的 turn
  // 从它取模型。省略时 `/use-model` 只回一句「本节点不支持」，不会静默吞掉
  // 命令、也不会把命令当提问送进 agent。
  userModels?: UserModelStore;
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
  // 已答轮次台账（见 answered-turns.ts）。服务端按 request_id 重派时，命中
  // 台账就只补帧、不再跑一遍 agent —— 融云重连重放的是**新消息**（新
  // messageUId），run.ts 的 UID 去重拦不住它。省略 -> 退化成旧行为（重跑）。
  answeredTurns?: AnsweredTurnStore;
  // 权限快照读取（supervisor 轮询落盘的那份，见 agent-permissions.ts）。
  // 省略 -> 不做私聊闸门，行为与升级前完全一致（旧版 CLI / 测试）。
  permissionSnapshot?: () => PermissionSnapshot | null;
  // 本节点在**服务端**的 node_id —— 权限快照按它索引。注意不要和上面的
  // `nodeId`（= 本节点的融云 user id，用于流帧）混用，两者不是同一个 id。
  permissionNodeId?: string;
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

/** 一轮 turn 的附加指令（目前只有行为规范）。 */
export interface TurnOptions {
  /**
   * 本轮要注入到**指令层**的行为规范。只有私聊会带：群聊/讨论组/聊天室的行为
   * 准则由服务端拼进 role_prompt，设备端重复注入会白烧上下文还可能被复述。
   */
  behaviorPrompt?: string;
}

export interface TurnRunner {
  runTurn: (prompt: string, model?: string, sessionKey?: string, opts?: TurnOptions) => Promise<string>;
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
    opts?: TurnOptions,
  ) => Promise<string>;
}

// 一轮 your_turn 的会话形态。三种场合共用同一条 IM 命令：//   - group    普通群组（融云 group）闲聊的按序中继，服务端提示词就是
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

/**
 * 服务端**按序派发**的轮次需要流式心跳：服务端每收到一条 relay_ping_<requestId>
 * 就把该轮的 deadline 推到「现在 + 空闲窗口」，心跳在就永不超时。
 *
 * 讨论组与群聊/聊天室中继都按序推进，一个人没说完不该让下一位开跑，所以都要
 * 心跳；私聊是即时应答，没有别人排队等它。
 *
 * ⚠️ 判据**不能**写成 requestId.startsWith("relay_")：闲聊中继的轮次 id 以
 * relay_ 开头，讨论组的轮次 id 是服务端生成的 UUID。按前者判会让讨论组整场
 * 不发心跳，服务端只剩派发时定死的墙钟 —— 设备还在流式产出也被判「响应超时」，
 * 重试、跳过，最后主持人汇总同样超时，整场讨论以 summary_failed 收场
 * （线上 2026-10-22）。
 */
export function turnNeedsHeartbeat(venue: TurnVenue): boolean {
  return venue !== "private";
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
// ⚠️ 实测：500ms（2 条/秒）在同一账号还发卡片/命令回包时仍被全量 23406 拒绝。
// 放宽到 900ms，并把失败重试放进 emit —— 帧是全量快照，少发一帧只影响流畅度，
// 但终帧发不出去前端就会永远停在「正在思考」。
const STREAM_FRAME_INTERVAL_MS = 900;
// 正文增量帧的间隔。上面那个 900ms 是给**全量快照**定的：思考流一帧要重发
// 整段 reasoning（实测 glm-5.2 单轮 4~20k 字），5 帧/秒既会撞融云限频，也会
// 把消息体放大几十倍。正文不是这样：它只在最后几秒密集生成，且这一档帧**不带**
// 思考/过程流（见 sendDelta 的 sideChannels），单帧只有正文本身，几百字节。
// 所以正文可以单独走短窗口 —— 这是「agent 吐什么就实时输出什么」，不是伪流式
// 回放：帧里是真增量，只是把合并粒度从 900ms 收到 200ms（≈5 帧/秒）。
const STREAM_TEXT_FRAME_INTERVAL_MS = 200;
// 终帧补发前的间隔：足够让第一份走完 IM 链路，又不至于让用户多等。
const TERMINAL_RESEND_GAP_MS = 600;
// 流式心跳间隔：比服务端轮次超时（默认 60s）短一个量级，服务端每收到一次
// 心跳就把该轮 deadline 重置成完整超时 —— 只要流还在动就永不超时。
const STREAM_PING_INTERVAL_MS = 15_000;

/** 一个在飞的流式轮次。见 MessageDispatcher.activeStreams。 */
interface ActiveStream {
  controller: AbortController;
  requestMessageId: string;
  conversationType: 1 | 3 | 4;
  conversationId: string;
}

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
  // streamId -> 在飞轮次。除了 AbortController，还要记住这一轮挂在哪会话、
  // 哪个 request_message_id 上：chat_stop 的 stream_id 一旦对不上（帧丢了、
  // web 从占位气泡取到了别的 id），就得靠后两个字段把它找回来 —— 否则停止
  // 静默失效，用户看到的就是「点了停止仍在跑」+「新消息被判『上一个任务还在
  // 执行中』」。条目正好活一轮。
  private readonly activeStreams = new Map<string, ActiveStream>();

  constructor(private readonly deps: DispatcherDeps) {}

  /** 一次性提示的记账：同一原因只记一行，避免每条消息刷屏。 */
  private readonly loggedPrivateGateReasons = new Set<string>();

  /**
   * 私聊闸门 + 该 agent 的行为规范。
   *
   * 快照由 supervisor 的 10s 轮询落盘（agent-permissions.ts 里有 5s 内存缓存 +
   * mtime 失效），这里不做任何网络请求，也不会每条消息都读盘。
   */
  private privateGate(fromUserId: string): {
    allow: boolean;
    reason: PrivateGateReason;
    behaviorPrompt: string;
  } {
    const snapshot = this.deps.permissionSnapshot?.() ?? null;
    const query = {
      fromUserId,
      ...(this.deps.permissionNodeId ? { nodeId: this.deps.permissionNodeId } : {}),
      ...(this.deps.agentName ? { agent: this.deps.agentName } : {}),
    };
    const decision = decidePrivateChat(snapshot, query);
    return {
      allow: decision.allow,
      reason: decision.reason,
      behaviorPrompt: behaviorPromptFor(snapshot, query),
    };
  }

  /**
   * 私聊闸门的日志：四条分支必须一眼可分（任务书 success_criteria #2）。
   *
   * 规则（队长 2026-10-27 裁决 + 审查 S3）：
   *  · `unrestricted`（有快照且 allowPrivate 不是 false）不记 —— 正常路径不刷屏；
   *  · `no-snapshot`（整份快照都没有）**每条 warn** —— fail-open，必须与
   *    「有快照且允许」一眼可分；
   *  · `no-agent-entry`（有快照但没这个节点）**每条 warn** —— 现在是**拒绝**，
   *    不放行，但仍要留痕，否则现场只看到「不回私聊」查不到原因；
   *  · `denied-stranger` **每条 warn** —— 安全事件，宁多勿少；
   *  · `owner` / `unclaimed-fail-open` 每进程只记一次（不会变成刷屏）。
   */
  private logPrivateGate(reason: PrivateGateReason, fromUserId: string): void {
    const node = this.deps.permissionNodeId || this.deps.agentName || "?";
    if (reason === "unrestricted") return;
    if (reason === "no-snapshot") {
      console.warn(
        "im in: 私聊闸门无权限快照，降级放行（fail-open，不是 allowPrivate=true）"
        + ` from=${fromUserId} node=${node} — supervisor 尚未落盘快照，或本机是独立 run 模式`,
      );
      return;
    }
    if (reason === "denied-stranger") {
      console.warn(
        `im in: private chat dropped (allowPrivate=false, denied-stranger) from=${fromUserId} node=${node}`,
      );
      return;
    }
    if (reason === "no-agent-entry") {
      console.warn(
        "im in: private chat dropped (no-agent-entry) — 权限快照里没有本节点；"
        + "有快照就说明服务端已在管权限，查不到不能放行"
        + ` from=${fromUserId} node=${node}`,
      );
      return;
    }
    if (this.loggedPrivateGateReasons.has(reason)) return;
    this.loggedPrivateGateReasons.add(reason);
    if (reason === "unclaimed-fail-open") {
      console.warn(
        "未认领设备，私聊闸门降级放行：allowPrivate=false 但 ownerUserId 为空，"
        + `无法判定谁是主人（from=${fromUserId}）—— 这是唯一允许的 fail-open 点`,
      );
      return;
    }
    if (reason === "owner") {
      console.log(`im in: private chat allowed for device owner (from=${fromUserId})`);
    }
  }

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
      // 私聊闸门（allowPrivate）。
      //
      // 为什么必须在设备端：服务端**没有私聊派发点**，私聊这条 RC:TxtMsg 是 CLI
      // 收到就直接跑 agent 的（本函数下面就是 runStreamedTurn）。服务端 webhook
      // 也收到同一条消息，但那是另一条链路，拦它不影响这一条。
      const gate = this.privateGate(msg.fromUserId);
      this.logPrivateGate(gate.reason, msg.fromUserId);
      if (!gate.allow) {
        // **不回复**「无权限」：那会把闸门变成探测信道（陌生人能靠回包确认这里有
        // 活的 agent），也违背「安静地不理」这个产品语义。只留一行本机日志。
        return;
      }
      // `/use-model` 是 web 切模型发的**文本命令**（设备管理里选模型、圆桌里
      // 给某个角色换模型，都是给目标节点的 AI 账号私发这一条）。必须在进
      // agent 之前截住：以前没有这个分支，命令会变成用户提问，agent 要么回
      // 「没有这个命令」，要么真去执行一个叫 use-model 的任务。
      const useModel = parseUseModelCommand(text);
      if (useModel) {
        await this.handleUseModelCommand(msg.fromUserId, useModel);
        return;
      }
      // 私聊也走流式：web 端 chat_stream 帧先逐帧渲染，完成后 command_result
      // / 卡片兜底。sendStream 缺失（测试）时退回整包回复。
      await this.runStreamedTurn({
        conversationType: 1,
        conversationId: msg.fromUserId,
        requestMessageId: msg.messageUId || `req-${Date.now()}`,
        requesterId: msg.fromUserId,
        prompt: text,
        // per-user 模型：这个人自己选过的就用他的，没选过则跟随节点默认。
        model: this.deps.userModels?.get(msg.fromUserId),
        // 私聊按对端分桶：不给键的话 openclaw 会把私聊也塞进主会话，和群聊、
        // 讨论组共享一串上下文。
        sessionKey: turnSessionKey("private", msg.fromUserId),
        // 行为规范只走指令层（codex 的 AGENTS.md 标记块 / opencode 的
        // instructions 文件），**绝不拼进 text** —— 那会污染用户原话。
        turnOptions: gate.behaviorPrompt ? { behaviorPrompt: gate.behaviorPrompt } : {},
        turns,
        fallback: (out) => this.deliverAgentReply(msg.fromUserId, out),
      });
      return;
    }

    // chat_stop：web 的「停止生成」。取消本轮活动流（runStreamedTurn 的
    // AbortController），并按协议回 chat_stop_result。
    if (msg.objectName === "chat_stop") {
      const stop = parseChatStopRequest(msg.content);
      if (stop) await this.handleChatStop(msg, stop.requestId, stop.streamId, stop.requestMessageId);
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
      const requesterId = typeof params.requester_id === "string" && params.requester_id
        ? params.requester_id
        : msg.fromUserId;
      // 服务端按 request_id 重派（设备重启/重连后必现）：命中台账就只补帧，
      // 绝不再跑一遍 agent。重派在融云里是**一条新消息**（新 messageUId），
      // run.ts 的 UID 去重拦不住它 —— 2026-10-26 现场同一轮 codex 被跑了三次，
      // 正文三份各不相同（276/326/282 字），web 上就是同一角色说了三遍。
      const answeredTurn = this.deps.answeredTurns?.get(cmd.request_id);
      if (answeredTurn) {
        await this.replayAnsweredTurn(cmd.request_id, answeredTurn, replyTo);
        return;
      }
      await this.runStreamedTurn({
        conversationType: convType,
        conversationId: replyTo,
        requestMessageId: cmd.request_id,
        requesterId,
        prompt,
        // 轮次模型：用户自己 `/use-model` 选过的优先 —— 那是运行期的最新意图
        // （web 的圆桌「角色模型」就是给该角色节点私发这条命令，不写服务端
        // 角色配置）；没选过才用服务端按角色下发的 params.model；两者都没有
        // 则交给 run.ts 回落到节点的 --model。
        model: this.deps.userModels?.get(requesterId) ?? params.model,
        // 讨论组/群聊/聊天室各按自己的会话 id 分桶，讨论组里聊出来的共识
        // 不会顺着 agent 的共享会话爬进普通群聊。
        sessionKey: turnSessionKey(venue, replyTo),
        turns,
        // 流式心跳收件人 = 轮次派发者（服务端系统节点私聊下发 your_turn）。
        yourTurnFrom: msg.fromUserId,
        // 服务端对**所有**按序轮次都要心跳续期，不只是闲聊中继（relay_*）：
        // 讨论组的轮次 id 是 UUID，此前心跳条件写死了 startsWith("relay_")，
        // 讨论组于是永远不发心跳，服务端只有派发时定死的墙钟 —— 设备还在流式
        // 产出也会被判「响应超时」，重试后跳过，最后主持人汇总超时 → 讨论失败
        // summary_failed（线上 2026-10-22）。私聊轮次不是按序派发，不需要心跳。
        heartbeat: turnNeedsHeartbeat(venue),
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

  /**
   * 服务端把同一个 request_id 又派了一次：把上一轮已经发出去的东西原样再发
   * 一遍，**不跑 agent**。
   *
   * 为什么两条都要重发：
   *  - 终帧让 web 端的气泡落定。复用原 stream_id + 原 seq，web 侧
   *    mergeChatStreamMessage 对「已是终态 + 同 seq」直接返回原列表，不会多一条
   *    气泡；从没收到过这条消息的新端则能正常渲染出来。
   *  - command_result 才是服务端推进下一位发言人的依据。只补终帧不补它，整场
   *    讨论会干等到轮次超时，用户看到的是「卡住」。
   *
   * 失败都只记日志：这一轮的内容上一轮已经交付过，补发是幂等修复，不该反过来
   * 把这轮弄成失败。
   */
  private async replayAnsweredTurn(
    requestId: string,
    prior: AnsweredTurn,
    replyTo: string,
  ): Promise<void> {
    const sendStream = this.deps.sendStream;
    if (sendStream) {
      const serialized = buildChatStreamFrame({
        streamId: prior.streamId,
        requestMessageId: requestId,
        requesterId: prior.requesterId,
        nodeId: this.deps.nodeId || this.deps.selfUserId || "",
        conversationType: prior.conversationType,
        conversationId: prior.conversationId,
        seq: prior.seq,
        status: "completed",
        text: prior.text,
      });
      const payload = JSON.parse(serialized) as Record<string, unknown>;
      for (const wire of encodeChatStreamWireFrames(payload)) {
        await sendStream(prior.conversationType, prior.conversationId, wire.objectName, wire.content)
          .catch((err: unknown) => {
            console.error(
              "answered turn replay frame failed:",
              err instanceof Error ? err.message : err,
            );
          });
      }
    }
    await this.deps
      .send(replyTo, "command", encodeCommandResult(requestId, "text", { content: prior.text }))
      .catch((err: unknown) => {
        console.error(
          "answered turn replay result failed:",
          err instanceof Error ? err.message : err,
        );
      });
    console.log(
      `im in: replaying answered turn req=${requestId}`
      + ` (answered ${Math.round((Date.now() - prior.at) / 1000)}s ago,`
      + ` text=${prior.text.length}) — agent not re-run`,
    );
  }

  // runStreamedTurn 是所有会话形态共用的流式回合执行器：
  //  1. 先发 status=processing 的 0 号帧（web 立刻出现打字气泡）；
  //  2. runTurnStreamed 可用时逐 chunk 发 status=streaming 全量快照帧；
  //  3. 结束发 completed 终帧；异常发 failed 帧；被停止发 cancelled 帧；
  //  4. 终帧之后仍回调 fallback（command_result / 卡片），保持中继推进与
  //     旧消费方兼容——旧 web 不解析 chat_stream 也照常工作。
  // 帧节流：融云自定义消息限频，**带思考/过程流的帧**间隔至少
  // STREAM_FRAME_INTERVAL_MS（900ms，全量快照太重）；只有正文的增量帧走
  // STREAM_TEXT_FRAME_INTERVAL_MS（200ms），见 sendDelta 里的 textOnly 分支；
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
    /**
     * 传给 agent 的附加指令（目前只有行为规范）。**只有私聊会带** —— 群聊/讨论组
     * 的行为准则由服务端拼进 role_prompt，设备端不重复注入。
     */
    turnOptions?: TurnOptions;
    /** your_turn 轮的派发者（服务端系统节点）：流式心跳的收件人。 */
    yourTurnFrom?: string;
    /**
     * 是否为这一轮发流式心跳。true 用于服务端**按序派发**的轮次（闲聊中继与
     * 讨论组）：服务端按心跳续期该轮 deadline，缺了它长思考轮会被固定墙钟判超时。
     */
    heartbeat?: boolean;
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
      // 终态帧只发一次：内层分支（超时/没答完/取消）已经发过终帧时，外层
      // catch 不能再补一帧把 code/正文顶掉 —— web 端「首个终帧不可变」能挡
      // 一部分，但设备端日志会因此说谎（记成 agent_failed、正文为空）。
      if (status === "completed" || status === "failed" || status === "cancelled") {
        terminalSent = true;
      }
      const isTerminal = status === "completed" || status === "failed" || status === "cancelled";
      // 退避重试：融云 20604/23406 是发送频率限频，等一会儿再发通常就能过。
      //
      // ⚠️ 普通帧只给 1 次短退避：sendDelta 是**同步 await** 的，重试期间
      // agent 的 token 流会被背压住（模型输出/工具执行都变慢），而帧是全量
      // 快照 —— 丢一帧下一帧就带全了，不值得为它拖慢整轮。
      // 终帧不一样：丢了前端永远停在「正在思考…」，所以给足 5 次（最长 ~11s）。
      const delays = isTerminal ? [400, 900, 1800, 3000, 5000] : [200];
      try {
        for (const wire of frame(seq, status, text, reasoning, errorCode, activity, timeline)) {
          for (let attempt = 0; ; attempt += 1) {
            try {
              await sendStream(ctx.conversationType, ctx.conversationId, wire.objectName, wire.content);
              break;
            } catch (sendErr) {
              if (attempt >= delays.length) throw sendErr;
              await new Promise<void>((resolve) => setTimeout(resolve, delays[attempt]!));
            }
          }
        }
      } catch (err) {
        // 单帧失败不打断回答：流帧只是 UI 加速，权威内容走 fallback。
        console.error(
          `stream frame send failed${isTerminal ? " (terminal)" : ""}:`,
          err instanceof Error ? err.message : err,
        );
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
    this.activeStreams.set(ctx.streamId, {
      controller,
      requestMessageId: ctx.requestMessageId,
      conversationType: ctx.conversationType,
      conversationId: ctx.conversationId,
    });
    // 过程流的补偿帧定时器：声明在 try 之外，finally 才能把它撤掉（见文件末尾）。
    let pendingActivityFlush: ReturnType<typeof setTimeout> | null = null;
    // 流式心跳定时器：同理，finally 撤。
    let pingTimer: ReturnType<typeof setInterval> | null = null;
    /** 本轮心跳失败次数（0 = 链路健康），用于收尾时汇总。 */
    let pingFailures = 0;
    /** 帧序号。声明在 try 之外：失败收尾也要发一帧，catch 里得看得见它。 */
    let seq = 0;
    /** 是否已经发过终态帧（completed/failed/cancelled）：防止外层 catch 重复收尾。 */
    let terminalSent = false;
    /** 轮次开始时刻：日志里打 elapsed，用来回答「这一轮到底跑了多久」。 */
    const turnStartedAt = Date.now();
    try {
      // 流式心跳：**所有服务端按序派发的轮次**都从轮次一开始发，不分流式/降级
      // 路径 —— openclaw 这类不流式的运行时全程沉默、只在结束时发一帧，若只挂在
      // 流式分支，它长跑超 60s 照样被服务端轮次超时掐掉（15:36 实测）。
      // 只要轮还没完成，心跳就证明「这位还活着」，服务端持续续期。
      //
      // 判据不是「id 以 relay_ 开头」：讨论组的轮次 id 是服务端生成的 UUID，
      // 旧判据让讨论组整场不发心跳，服务端只有派发时定死的墙钟，于是出现
      // 「设备还在输出 → 响应超时 → 跳过 → 主持人汇总也超时 → summary_failed」。
      if (opts.heartbeat && opts.yourTurnFrom && this.deps.send) {
        const pingRequestId = `relay_ping_${ctx.requestMessageId}`;
        const pingTarget = opts.yourTurnFrom;
        // 心跳失败不能静默：它是服务端唯一的「还在产出」信号，融云限流
        // （20604）或网络抖动都会让长轮悄悄失去续期，症状是「上一位还在
        // 思考、下一位已经开跑」。首次失败与每 8 次失败各留一行，收尾时
        // 再打一条汇总，便于现场判断那条链路断在哪。
        pingFailures = 0;
        pingTimer = setInterval(() => {
          if (controller.signal.aborted) return;
          const beat = () => this.deps.send!(
            pingTarget,
            "command",
            encodeCommandResult(pingRequestId, "text", { content: "" }),
          );
          // 心跳是服务端唯一的「还在产出」信号：单次失败（融云限流/抖动）就
          // 立刻补一次，别让 120s 的空闲窗口被一次丢包吃掉。
          void beat().catch(() => setTimeout(() => {
            void beat().catch((err) => {
              pingFailures += 1;
              if (pingFailures === 1 || pingFailures % 8 === 0) {
                console.error(
                  `relay ping failed (${pingFailures}) req=${ctx.requestMessageId}:`,
                  err instanceof Error ? err.message : err,
                );
              }
            });
          }, 1_000));
        }, STREAM_PING_INTERVAL_MS);
      }
      if (!canStream) {
        // Degraded path (tests, or an agent without streamed stdout): the
        // old single-shot reply is the whole turn — including the old error
        // semantics (single chat: log only; your_turn: error command_result).
        try {
          const out = await opts.turns.runTurn(opts.prompt, opts.model, opts.sessionKey, opts.turnOptions);
          const uploaded = await this.collectArtifacts(artifactSnapshot);
          await opts.fallback(this.ensureVisibleReply(out, uploaded));
          await this.sendArtifactMessages(ctx, uploaded);
        } catch (err) {
          const errMsg = err instanceof Error ? err.message : String(err);
          const incomplete = err instanceof TurnIncompleteError ? err : undefined;
          const errorCode = incomplete ? incomplete.code : classifyTurnError(errMsg);
          // 与流式路径同一原则：中继轮失败也要有日志（原来只在私聊分支打）。
          console.error(
            `turn failed: conv=${ctx.conversationType}/${ctx.conversationId}`
            + ` req=${ctx.requestMessageId}`
            + ` elapsed=${Math.round((Date.now() - turnStartedAt) / 1000)}s`
            + ` code=${errorCode}(${describeTurnError(errorCode)})`
            + ` err=${errMsg}`,
          );
          // 兜底路径整轮没发过任何流帧，失败时同样要给前端一个终态：否则气泡
          // 里连打字气泡都没有，用户只看到一条永远空着的 AI 消息。seq 用 0
          // 即可（这条流不会有别的帧）。老 web 不认 error_code 也能看到
          // 「生成失败」。「没答完」时把已产出的正文一起带走。
          await emit(0, "failed", incomplete?.partialText ?? "", undefined, errorCode);
          if (opts.onError) {
            await opts.onError(errMsg);
            return;
          }
          // Single-chat failure: do not reply; the user can retry.
        }
        return;
      }
      await emit(0, "processing", "");
      seq = 1;
      let snapshot = "";
      // 思考流快照：与正文快照同帧携带（reasoning 字段），同 seq 递增。
      let reasoningSnapshot = "";
      // 过程流快照：agent 本轮的工具调用步骤（联网搜索/执行命令/读写文件…）。
      let activitySnapshot: ActivityStep[] = [];
      // 过程时间线：思考段与工具调用的真实顺序。思考增量接到最后一个思考段
      // （新的一段思考——工具之后又思考——新开条目）；工具步骤只存 id 引用
      // （内容在 activitySnapshot 里），这样同一份信息不会在帧里存两遍。
      // web 端按它渲染「思考卡片 ↔ 工具卡片」交错的流式时间线。
      const timeline: ChatStreamTimelineEntry[] = [];
      const activeReasoningEntry = (): number | null => {
        const last = timeline[timeline.length - 1];
        return last && last[0] === "r" ? timeline.length - 1 : null;
      };
      let lastSent = 0;
      // 节流窗口内到达的过程流更新需要一次补偿帧：长工具阶段正文久久不来，
      // 若完全等下一个 chunk，用户就看不到「正在联网搜索」这一步。
      // sideChannels=false 只发正文：高频正文帧（200ms）省掉每帧重发的思考
      // 快照与时间线。web 端 merge 对这三个字段是 `incoming.X ?? current.X`
      // （chat-stream.ts:253-255），缺字段等于「保留已上屏的那份」，不会清空。
      const emitStreamingFrame = async (sideChannels = true) => {
        const frameSeq = seq;
        seq += 1;
        lastSent = Date.now();
        await emit(
          frameSeq,
          "streaming",
          snapshot,
          sideChannels ? reasoningSnapshot || undefined : undefined,
          undefined,
          sideChannels ? activitySnapshot : undefined,
          sideChannels ? timeline : undefined,
        );
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
        // 只有「纯正文增量」才走短窗口，并且这一帧不带思考/过程流（见上）。
        // 带思考或带步骤的帧仍是全量快照，维持 900ms —— 降它会把消息体放大几十倍。
        const textOnly = delta.length > 0 && !reasoningDelta && !activity;
        const interval = textOnly ? STREAM_TEXT_FRAME_INTERVAL_MS : STREAM_FRAME_INTERVAL_MS;
        // 节流：距上一帧不足间隔时只攒不动，由下个 chunk 或终帧冲刷。
        if (now - lastSent < interval) {
          if (activity && pendingActivityFlush === null) {
            pendingActivityFlush = setTimeout(() => {
              pendingActivityFlush = null;
              void emitStreamingFrame(true);
            }, interval - (now - lastSent));
          }
          return;
        }
        await emitStreamingFrame(!textOnly);
      };
      let finalText = "";
      // 自动补跑：「撞在失败的工具上就没下文」（agent_incomplete + failedStep）
      // 时把失败原因回喂，让 agent 换写法接着答完 —— 用户看到它继续干活，而不是
      // 半句话加一行错误。只补一次：补跑仍失败就按原样发 failed 终帧。
      // （2026-10-20 现场：运维助手把 PowerShell 塞进 git-bash，command not found
      // 之后一声不吭，用户每问一句运维都要手动重发一次。）
      let continuationUsed = false;
      let turnPrompt = opts.prompt;
      for (;;) {
        try {
          finalText = await opts.turns.runTurnStreamed!(
            turnPrompt,
            opts.model,
            sendDelta,
            controller.signal,
            opts.sessionKey,
            opts.turnOptions,
          );
          break;
        } catch (err) {
        if (controller.signal.aborted) {
          await emit(seq, "cancelled", snapshot, reasoningSnapshot || undefined, undefined, activitySnapshot, timeline);
          return;
        }
        const incomplete = err instanceof TurnIncompleteError ? err : undefined;
        if (!continuationUsed && incomplete?.code === "agent_incomplete" && incomplete.failedStep) {
          continuationUsed = true;
          console.warn(
            `auto-continue: conv=${ctx.conversationType}/${ctx.conversationId}`
            + ` req=${ctx.requestMessageId} step=${incomplete.failedStep.label}`
            + (incomplete.failedStep.command ? ` cmd=${incomplete.failedStep.command}` : ""),
          );
          turnPrompt = buildAutoContinuationPrompt(opts.prompt, incomplete);
          continue;
        }
        const errMsg = err instanceof Error ? err.message : String(err);
        const errorCode = incomplete ? incomplete.code : classifyTurnError(errMsg);
        // 「没答完」这一类失败带着已经产出的正文：终帧必须把它带上，否则用户
        // 刚在气泡里看到的半句话会凭空消失，只剩一行错误提示。
        const failedText = incomplete?.partialText ? incomplete.partialText : snapshot;
        // error_code 必须是标识符（无控制字符/无首尾空白/≤256 字节）：web 端
        // parseChatStreamEvent 校验不过就整帧丢弃。以前这里塞的是
        // errMsg.slice(0, 120)，stderr 一带换行整帧就没了 —— 前端于是永远
        // 「正在思考」。分类短码同时解决「帧被丢」和「前端该显示什么」。
        console.error(
          `stream turn failed: conv=${ctx.conversationType}/${ctx.conversationId}`
          + ` req=${ctx.requestMessageId} code=${errorCode}(${describeTurnError(errorCode)})`
          + ` text=${failedText.length} err=${errMsg}`,
        );
        await emit(seq, "failed", failedText, reasoningSnapshot || undefined, errorCode, activitySnapshot, timeline);
        if (opts.onError) {
          await opts.onError(errMsg);
          return;
        }
        throw err;
        }
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
        + ` activity=${activitySnapshot.length} frames=${seq}`
        // 这一轮的真实墙钟耗时。群聊越序排查时先看它：超过服务端空闲窗口
        // （120s）而心跳全绿 → 服务端在等，是 agent 本身慢；耗时正常却越序
        // → 心跳断了（见 relay ping 日志）。
        + ` elapsed=${Math.round((Date.now() - turnStartedAt) / 1000)}s`,
      );
      // 伪流式回放：本轮**几乎没发出正文**时（agent 一次性吐全文），客户端
      // 全程只收到开头那个空帧，于是「转圈几分钟 → 全文一次性蹦出」。
      // 已知会这样的：openclaw（`agent --local --json` 只给一次性信封）、
      // 以及部分 opencode 版本在 `--format json` 下整段缓冲（2026-10-23 实测
      // 70s 内零字节输出）。正文已经在手上，分片回放成 streaming 帧，观感与
      // 真流式一致，也比「白等」诚实。
      //
      // 只补「客户端还没看到的那一段」：帧是全量快照，客户端按 seq 覆盖，
      // 回放比当前快照更短的正文会让气泡里的字倒退。snapshot 是单调增长的，
      // 任何已发出的帧都不超过它，所以从 snapshot.length 开始分片是安全的。
      const replayFrom = snapshot.length;
      if (!controller.signal.aborted
        && visibleText.startsWith(snapshot)
        && visibleText.length - replayFrom >= 80) {
        const total = visibleText.length;
        // 4~24 片；总时长控制在 ~4s 以内（片多则间隔自动缩短）。
        const slices = Math.min(24, Math.max(4, Math.ceil((total - replayFrom) / 120)));
        const size = Math.ceil((total - replayFrom) / slices);
        const gap = Math.min(220, Math.floor(4000 / slices));
        console.log(
          `stream replay: conv=${ctx.conversationType}/${ctx.conversationId}`
          + ` from=${replayFrom} to=${total} frames=${slices}`,
        );
        for (let end = replayFrom; end < total; end += size) {
          if (controller.signal.aborted) break;
          const sliceEnd = Math.min(total, end + size);
          await emit(
            seq,
            "streaming",
            visibleText.slice(0, sliceEnd),
            reasoningSnapshot || undefined,
            undefined,
            activitySnapshot,
            timeline,
          );
          seq += 1;
          if (sliceEnd < total) await new Promise<void>((resolve) => setTimeout(resolve, gap));
        }
      }
      await emit(seq, "completed", visibleText, reasoningSnapshot || undefined, undefined, activitySnapshot, timeline);
      // 终帧补发一次（同 seq、同内容）。它是唯一让气泡收尾的帧：一旦没落地，
      // 前端就一直转圈（还在显示「停止生成」），随后这条兜底整包文本会被
      // 当成第二条回复追加 —— 界面上就是两条一模一样的回答（2026-10-26 现场，
      // 一条终帧实测被切成 16 块，任何一块丢失都会重演）。
      //
      // 重复终帧在两端都是幂等的：web 端 mergeChatStreamMessage 对「已是终态 +
      // 同 seq」直接返回原列表；分块路径按 messageId(=wire_sha) 命中 completed
      // 缓存，判为 replay 后丢弃。所以这里只花一次冗余流量，换「至少送达一次」。
      if (!controller.signal.aborted) {
        await new Promise<void>((resolve) => setTimeout(resolve, TERMINAL_RESEND_GAP_MS));
        await emit(seq, "completed", visibleText, reasoningSnapshot || undefined, undefined, activitySnapshot, timeline);
      }
      await opts.fallback(visibleText);
      // 记账只认**成功收尾**的轮次（终帧与 command_result 都已发出）。中止、
      // 超时、失败的轮次不记账 —— 那种情况服务端重派是合理的，应该真跑一遍。
      this.deps.answeredTurns?.remember(ctx.requestMessageId, {
        at: Date.now(),
        streamId: ctx.streamId,
        seq,
        text: visibleText,
        requesterId: ctx.requesterId,
        conversationType: ctx.conversationType,
        conversationId: ctx.conversationId,
      });
      await this.sendArtifactMessages(ctx, uploaded);
    } catch (err) {
      // 失败必须留痕 + 发终帧。原来这条路径只有 finally：异常冒泡出去，
      // 前端收不到任何终态（「正在思考」永远转圈），设备端也没有一行日志
      // —— 18:13 的现场就是 hermes 的模型 429 每轮静默失败，服务端照常派
      // 下一位，看起来像「上一位卡住、下一位抢话」。
      const errMsg = err instanceof Error ? err.message : String(err);
      const incomplete = err instanceof TurnIncompleteError ? err : undefined;
      const errorCode = incomplete ? incomplete.code : classifyTurnError(errMsg);
      console.error(
        `stream failed: conv=${ctx.conversationType}/${ctx.conversationId}`
        + ` req=${ctx.requestMessageId}`
        + ` elapsed=${Math.round((Date.now() - turnStartedAt) / 1000)}s`
        + ` code=${errorCode}(${describeTurnError(errorCode)})`
        + ` err=${errMsg}`,
      );
      // chat_stop 的收尾由 handleChatStop 负责，别在这里抢发帧；内层分支已经
      // 发过终帧时也不再补发（否则会把「没答完」的短码和半截正文顶成空）。
      if (!controller.signal.aborted && !terminalSent) {
        await emit(seq, "failed", incomplete?.partialText ?? "", undefined, errorCode);
      }
      // 中继轮（your_turn）必须回 command_result：服务端靠它推进下一位，
      // 不回就只能干等超时。重复回包由服务端按 requestID 吞掉，安全。
      if (opts.onError) {
        await opts.onError(errMsg).catch(() => {});
      }
    } finally {
      // 心跳随轮次结束：终帧/取消/失败之后都不再发。
      if (pingTimer !== null) {
        clearInterval(pingTimer);
        pingTimer = null;
      }
      if (pingFailures > 0) {
        console.error(`relay ping summary: ${pingFailures} failure(s) req=${ctx.requestMessageId}`);
      }
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

  // `/use-model`：改「这个人」在本节点上用的模型，并立即回执。
  //
  // 刻意不进 agent、也不占用会话队列：用户点一下模型下拉就在等一个响应，spawn
  // 一个 agent 冷启动好几秒，还会把队列里真正的提问挤到后面。
  private async handleUseModelCommand(fromUserId: string, command: UseModelCommand): Promise<void> {
    const store = this.deps.userModels;
    if (!store) {
      await this.deps.send(
        fromUserId,
        "RC:TxtMsg",
        JSON.stringify({
          content: "本节点不支持按用户切换模型，请在设备管理里调整默认模型。",
        }),
      ).catch(() => { });
      return;
    }

    if (command.kind === "set") store.set(fromUserId, command.route);
    else if (command.kind === "clear") store.set(fromUserId, null);

    const current = store.get(fromUserId);
    const ack = formatUseModelAck(command, current);
    console.log(
      `use-model: user=${fromUserId} action=${command.kind}` +
      `${command.kind === "set" ? ` route=${command.route}` : ""} -> ${current ?? "(node default)"}`,
    );
    await this.deps.send(fromUserId, "RC:TxtMsg", JSON.stringify({ content: ack })).catch(() => { });
  }

  // chat_stop 处理：取消活动流并回执。
  //
  // 三级定位（精确 → 轮次 → 会话）：
  //  1. stream_id 精确命中：正常路径，web 回传的就是我们发出去的那个 id。
  //  2. request_message_id 命中：帧上那个 id 与本地 key 对不上（web 拿到的是
  //     占位气泡、或帧在途丢了），但轮次是同一轮。
  //  3. 同会话唯一在飞轮次：私聊/群聊每会话并发恒为 1（queueDepth 1），所以
  //     「这个会话里只有一轮在跑」是确定的事实，不是猜测。
  // 三级都不中才 reject —— 并且在 reject 之前把「注册了几条」打进日志，
  // 否则又是「点了停止没反应，日志里也看不出为什么」。
  private async handleChatStop(
    msg: InboundIMMessage,
    requestId: string,
    streamId: string,
    requestMessageId: string,
  ): Promise<void> {
    const found = this.locateActiveStream(msg, streamId, requestMessageId);
    if (!found) {
      console.log(
        `chat_stop: rejected no_active_task stream=${streamId}`
        + ` request_message_id=${requestMessageId || "-"}`
        + ` active=${this.activeStreams.size}`,
      );
      await this.sendStopResult(msg, requestId, streamId, "rejected", "no_active_task");
      return;
    }
    const { streamKey, stream, matchedBy } = found;
    const wasAborted = stream.controller.signal.aborted;
    stream.controller.abort();
    this.activeStreams.delete(streamKey);
    console.log(
      `chat_stop: accepted stream=${streamKey} match=${matchedBy}`
      + ` req=${stream.requestMessageId} requester_aborted=${wasAborted}`,
    );
    await this.sendStopResult(msg, requestId, streamId, "accepted", "stop_requested");
  }

  /** 见 handleChatStop 的三级定位。返回命中的 key 与「靠哪一级命中」。 */
  private locateActiveStream(
    msg: InboundIMMessage,
    streamId: string,
    requestMessageId: string,
  ): { streamKey: string; stream: ActiveStream; matchedBy: "stream" | "request" | "conversation" } | undefined {
    const exact = this.activeStreams.get(streamId);
    if (exact) return { streamKey: streamId, stream: exact, matchedBy: "stream" };
    if (requestMessageId) {
      for (const [key, stream] of this.activeStreams) {
        if (stream.requestMessageId === requestMessageId) {
          return { streamKey: key, stream, matchedBy: "request" };
        }
      }
    }
    // 停止请求是从请求者那一侧发来的；私聊轮的 conversationId 就是请求者，
    // 群/聊天室轮则是房间 id（web 不会替别人的房间点停止，这里仍要求同发送者）。
    const sameConversation = [...this.activeStreams].filter(([, stream]) => (
      stream.conversationType === msg.conversationType
      && stream.conversationId === msg.fromUserId
    ));
    if (sameConversation.length === 1) {
      const [key, stream] = sameConversation[0]!;
      return { streamKey: key, stream, matchedBy: "conversation" };
    }
    return undefined;
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
    const force = request.refresh === true;
    console.log(
      `model catalog: request ${request.requestId} from ${msg.fromUserId}${force ? " (refresh)" : ""}`,
    );
    try {
      catalog = (await this.deps.loadModelCatalog?.({ force })) ?? EMPTY_MODEL_CATALOG;
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
      `role recommend: request ${request.requestId} max_roles=${request.maxRoles}`
        + ` model=${request.model ?? "<node default>"} from ${msg.fromUserId}`,
    );
    let reply: string;
    try {
      // model 是主持节点在后台配的那个（服务端只在非空时下发）。它决定这轮用
      // 什么模型出角色 —— 旗舰模型做这件「3 段人设 + 一层 JSON」的短任务要
      // 二三十秒，快模型通常 2-3 秒，链路本身没那么慢。
      const output = await turns.runTurn(buildRoleRecommendPrompt(request), request.model);
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
