import type { BaseMessage, IAReceivedMessage } from "@rongcloud/imlib-next";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
import type { IMTransport, InboundIMMessage, TurnRunner } from "./im.js";
import { MessageDispatcher, planReadReceipt } from "./im.js";
import { runAgentTurn, runAgentCommand } from "./agents.js";
import { buildChatStreamFrame } from "./chat-stream.js";
import { createModelCatalogLoader, parseModelCatalogRequest } from "./model-catalog.js";
import {
  createRuntimeProber,
  createSerialGate,
  parseRuntimeRecoveryRequest,
  type RuntimeRecoveryAction,
} from "./runtime-recovery.js";
import type { StoredCredentials } from "./keystore.js";
import type { XiachatApi } from "./api.js";

// Per-conversation FIFO. Same conversation serializes (agent busy -> queue,
// overflow -> onBusy); different conversations run in parallel. Cross-session
// concurrency safety is the agent platform's responsibility (spec 2.3.5).
export interface SessionQueueOpts {
  concurrencyPerConversation: number;
  queueDepth?: number;
  onBusy?: (conversationKey: string) => void;
}

interface PendingTask {
  key: string;
  run: () => Promise<void>;
  /**
   * 任务还在排队就被撤掉（chat_stop 清队列）时的回调：给这条从未开跑的
   * 消息补一个终态帧，否则 web 端的占位气泡会永远「正在思考」。
   */
  onDropped?: () => void;
}

export class SessionQueue {
  private readonly active = new Map<string, number>();
  private readonly waiting = new Map<string, PendingTask[]>();
  private readonly idleResolvers: Array<() => void> = [];
  private totalPending = 0;

  constructor(private readonly opts: SessionQueueOpts) {}

  /**
   * 返回 false 表示队列满（onBusy 已触发），这条任务被丢弃 —— 调用方需要
   * 据此给用户一个明确的终态（见 startRunLoop 的 agent_busy 失败帧），
   * 否则 web 端的「正在思考」占位气泡会永远转圈。
   */
  enqueue(conversationKey: string, task: () => Promise<void>, onDropped?: () => void): boolean {
    const activeCount = this.active.get(conversationKey) ?? 0;
    const waiting = this.waiting.get(conversationKey) ?? [];
    if (activeCount >= this.opts.concurrencyPerConversation) {
      const depth = this.opts.queueDepth ?? Number.MAX_SAFE_INTEGER;
      if (waiting.length >= depth) {
        this.opts.onBusy?.(conversationKey);
        return false;
      }
      waiting.push({ key: conversationKey, run: task, ...(onDropped ? { onDropped } : {}) });
      this.waiting.set(conversationKey, waiting);
      return true;
    }
    this.start(conversationKey, task);
    return true;
  }

  /**
   * 撤掉该会话所有还在排队的任务（chat_stop 用）：被撤任务逐个触发
   * onDropped 补终态帧。返回撤掉的数量。正在执行的任务不归这里管 ——
   * 它由 MessageDispatcher 的 activeStreams abort 掉。
   */
  dropWaiting(conversationKey: string): number {
    const waiting = this.waiting.get(conversationKey);
    if (!waiting || waiting.length === 0) return 0;
    this.waiting.delete(conversationKey);
    for (const task of waiting) {
      try {
        task.onDropped?.();
      } catch (err) {
        console.error("dropped-task notice failed:", err instanceof Error ? err.message : err);
      }
    }
    // 被撤的任务永远不会跑 finish，检查点必须在这里补 —— 否则 idle() 卡住。
    this.checkIdle();
    return waiting.length;
  }

  private start(conversationKey: string, task: () => Promise<void>): void {
    this.totalPending++;
    this.active.set(conversationKey, (this.active.get(conversationKey) ?? 0) + 1);
    let promise: Promise<void>;
    try {
      promise = task();
    } catch (err) {
      // A synchronously throwing task must not escape into the message
      // listener; treat it like a rejected task and keep draining.
      console.error("queued task failed:", err instanceof Error ? err.message : err);
      this.finish(conversationKey);
      return;
    }
    promise
      .catch((err: unknown) => {
        console.error("queued task failed:", err instanceof Error ? err.message : err);
      })
      .finally(() => {
        this.finish(conversationKey);
      });
  }

  private finish(conversationKey: string): void {
    this.totalPending--;
    const count = (this.active.get(conversationKey) ?? 1) - 1;
    if (count > 0) this.active.set(conversationKey, count);
    else this.active.delete(conversationKey);
    const waiting = this.waiting.get(conversationKey);
    const next = waiting?.shift();
    if (waiting && waiting.length === 0) this.waiting.delete(conversationKey);
    if (next) {
      this.start(conversationKey, next.run);
    } else {
      this.checkIdle();
    }
  }

  // Idle only when nothing is active, nothing is pending, and no waiting
  // queue holds a task; called after every completion so a momentary
  // totalPending === 0 never resolves idle while work is still queued.
  private checkIdle(): void {
    if (this.totalPending !== 0 || this.active.size !== 0 || this.waiting.size !== 0) {
      return;
    }
    this.idleResolvers.splice(0).forEach((r) => r());
  }

  async idle(): Promise<void> {
    if (this.totalPending === 0 && this.active.size === 0 && this.waiting.size === 0) {
      return;
    }
    await new Promise<void>((resolve) => this.idleResolvers.push(resolve));
  }
}

export function conversationKeyOf(msg: InboundIMMessage): string {
  return `${msg.conversationType}:${msg.targetId}`;
}

// RongCloud re-delivers the entire offline history on every reconnect and the
// Node shim has no IndexDB for the SDK to record what it already synced. Acting
// on each replay again re-runs agent turns for messages answered long ago — a
// send storm RongCloud then rejects with 20604 (SEND_FREQUENCY_TOO_FAST), which
// is what leaves the web's 默认模型 panel without an answer. This keeps the run
// loop to exactly one dispatch per message UID.
export interface InboundReplayDedupe {
  // true when this UID has already been dispatched. A missing UID is never a
  // duplicate, so an SDK that omits it degrades to the old behaviour.
  isDuplicate(messageUId: string | undefined): boolean;
  // Persist immediately instead of waiting for the debounce (flush on shutdown).
  flush(): void;
  readonly size: number;
}

export interface InboundReplayDedupeOpts {
  // Where to keep the UID set between runs. Omitted -> pure in-memory (tests).
  path?: string;
  maxEntries?: number;
  // Debounce for disk writes; each reconnect can add hundreds of UIDs.
  flushDelayMs?: number;
}

export function createInboundReplayDedupe(
  opts: InboundReplayDedupeOpts | number = {},
): InboundReplayDedupe {
  const { path, maxEntries = 2000, flushDelayMs = 1_000 } =
    typeof opts === "number" ? { maxEntries: opts } : opts;
  const seen = new Set<string>(readProcessedUids(path));
  // Insertion order is the eviction order, so the oldest UIDs go first.
  const evict = (): void => {
    while (seen.size > maxEntries) {
      const oldest = seen.values().next().value;
      if (oldest === undefined) break;
      seen.delete(oldest);
    }
  };
  let timer: ReturnType<typeof setTimeout> | undefined;
  const flush = (): void => {
    if (timer) {
      clearTimeout(timer);
      timer = undefined;
    }
    if (!path) return;
    try {
      mkdirSync(dirname(path), { recursive: true });
      writeFileSync(path, JSON.stringify([...seen]), { mode: 0o600 });
    } catch (err) {
      // Best effort: losing the marker only costs a re-dispatch next connect.
      console.error("processed-uids write failed:", err instanceof Error ? err.message : err);
    }
  };
  return {
    isDuplicate(messageUId) {
      if (!messageUId) return false;
      if (seen.has(messageUId)) return true;
      seen.add(messageUId);
      evict();
      if (path && !timer) timer = setTimeout(flush, flushDelayMs);
      return false;
    },
    flush,
    get size() {
      return seen.size;
    },
  };
}

function readProcessedUids(path: string | undefined): string[] {
  if (!path) return [];
  try {
    const parsed: unknown = JSON.parse(readFileSync(path, "utf8"));
    return Array.isArray(parsed) ? parsed.filter((v): v is string => typeof v === "string") : [];
  } catch {
    // Missing or corrupt: start clean rather than refuse to run.
    return [];
  }
}

/**
 * 队列满丢弃一条私聊消息时，给发送方回一帧规范的 chat_stream failed 终帧
 * （error_code=agent_busy）。
 *
 * 为什么不用纯文本：旧实现的「正在思考中…」会被 web 端当成普通 AI 消息，
 * 原位替换掉用户提问时预埋的「正在思考」占位气泡；等排队的回合开始流式，
 * 又追加一条新气泡 —— 一条提问留下两个气泡，而且文案有歧义（读起来像
 * 「这条正在处理」，实际是被丢弃了）。failed 终帧走 web 的占位替换 +
 * 失败文案路径（chatStreamFailureText），一个气泡、一句明白话。
 *
 * 信封字段与 web 端 validStreamDelivery 的私聊校验逐条对齐：
 * node_id 必须等于发送账号（selfUserId），conversation_id/requester_id
 * 都是发消息的那个用户。selfUserId 缺失时无法通过校验，静默放弃（下一条
 * 正常回帧仍会顶掉占位气泡）。
 */
export function sendBusyDropNotice(
  transport: IMTransport,
  msg: InboundIMMessage,
  selfUserId: string | undefined,
): void {
  // 只有私聊有「占位气泡等替换」的问题；群/聊天室由协调者串行，不会溢出。
  if (msg.conversationType !== 1 || !selfUserId) return;
  const requestMessageId = msg.messageUId || `req-${Date.now()}`;
  const frame = buildChatStreamFrame({
    streamId: `stream-${requestMessageId}-busy`,
    requestMessageId,
    requesterId: msg.fromUserId,
    nodeId: selfUserId,
    conversationType: 1,
    conversationId: msg.fromUserId,
    seq: 0,
    status: "failed",
    text: "",
    errorCode: "agent_busy",
  });
  void transport.sendMessage(msg.fromUserId, "chat_stream", frame).catch(() => {});
}

/**
 * 「停止生成」把还在排队的消息撤掉时，给那条消息补的终态帧：cancelled。
 * 这条消息从未开跑、没有任何流帧，web 端只有一个等替换的占位气泡 ——
 * 不补这一帧它就永远「正在思考」，看起来像停止没生效（2026-10-20 现场：
 * 用户连发两条「上线」后点停止，第一条被中止，第二条照常排队开跑，
 * 用户看到的就是「停止无效、消息重复」）。
 */
export function sendStopDropNotice(
  transport: IMTransport,
  msg: InboundIMMessage,
  selfUserId: string | undefined,
): void {
  // 同 sendBusyDropNotice：只有私聊有占位气泡；群/聊天室没有排队等待位。
  if (msg.conversationType !== 1 || !selfUserId) return;
  const requestMessageId = msg.messageUId || `req-${Date.now()}`;
  const frame = buildChatStreamFrame({
    streamId: `stream-${requestMessageId}-stopped`,
    requestMessageId,
    requesterId: msg.fromUserId,
    nodeId: selfUserId,
    conversationType: 1,
    conversationId: msg.fromUserId,
    seq: 0,
    status: "cancelled",
    text: "",
  });
  void transport.sendMessage(msg.fromUserId, "chat_stream", frame).catch(() => {});
}

export interface ConnectTransportOpts {
  creds: StoredCredentials;
  api: XiachatApi;
  transport: IMTransport;
}

// 单轮 agent 的默认上限。120s 对深思考的运行时不够：hermes 的长思考轮实测
// 会跑过 3 分钟（reasoning 上万字），被掐断时服务端收到的是一条失败回包，
// 于是下一位立刻接棒 —— 用户看到的就是「上一位还在思考，下一位已经开口」。
// 取 300s 与服务端中继保底（relayTurnTimeoutFloor）同一量级，避免两端任意
// 一侧先放弃；真正的兜底仍由服务端超时负责。
export const DEFAULT_TURN_TIMEOUT_MS = 300_000;

export interface StartRunLoopOpts extends ConnectTransportOpts {
  agentExecPath: string;
  // Selects the per-agent invocation strategy (argv vs stdin); see
  // agents.ts buildAgentArgs.
  agentName?: string;
  model?: string;
  stdout: NodeJS.WriteStream;
  turnTimeoutMs?: number;
  // Working directory for agent turns; see RunAgentTurnOpts.cwd.
  cwd?: string;
  // Where to persist the processed-message markers (see
  // createInboundReplayDedupe). Omitted -> in-memory only.
  processedUidsPath?: string;
  // Idempotent local repair for action="recover" (main.ts passes
  // ensureOpencodeInstalled for opencode nodes). Omitted -> recover can only
  // diagnose, never install.
  ensureRuntime?: () => Promise<void>;
}

// Two recovers closer together than this would re-run the (installing)
// ensureRuntime and pile up agent spawns; queued requests still get answered,
// they just wait their turn.
const RUNTIME_RECOVERY_MIN_INTERVAL_MS = 5_000;

export async function startRunLoop(opts: StartRunLoopOpts): Promise<void> {
  await connectTransport(opts);

  // 产出文件上传的「凭据缺失」告警只打一次（见 uploadArtifact）。
  let artifactUploadWarned = false;

  // Catalogue the bound agent's models up front: the web's 设备管理 → 默认模型
  // panel aborts after 8s, and `openclaw models list` alone takes ~10s cold.
  // Warm-up runs in the background so it never delays message dispatch.
  const catalogLoader = createModelCatalogLoader({
    agentName: opts.agentName,
    runCommand: (argv, timeoutMs, maxOutputBytes) =>
      runAgentCommand({
        execPath: opts.agentExecPath,
        argv,
        timeoutMs,
        maxOutputBytes,
        cwd: opts.cwd,
      }),
    logger: (message) => console.error(message),
  });
  catalogLoader.warm();

  // 「一键唤醒」: the probe runs locally on this device and the answer rides
  // back over the same browser<->node "command" channel the model catalog uses.
  // Serialized with a 5s floor so a burst of wake requests cannot stack up
  // agent spawns; a successful recover also warms the catalog, which is the
  // exact query the user's next action would otherwise ask for cold.
  const prober = createRuntimeProber({
    agentName: opts.agentName,
    execPath: opts.agentExecPath,
    runCommand: (argv, timeoutMs, maxOutputBytes) =>
      runAgentCommand({
        execPath: opts.agentExecPath,
        argv,
        timeoutMs,
        maxOutputBytes,
        cwd: opts.cwd,
      }),
    ensureRuntime: opts.ensureRuntime,
    logger: (message) => console.error(message),
  });
  const recoveryGate = createSerialGate({ minIntervalMs: RUNTIME_RECOVERY_MIN_INTERVAL_MS });
  const probeRuntime = (action: RuntimeRecoveryAction) =>
    recoveryGate(async () => {
      const result = await prober.probe(action);
      if (action === "recover" && result.code === "ready") catalogLoader.warm();
      return result;
    });

  const dispatcher = new MessageDispatcher({
    send: (toUserId, objectName, content) => opts.transport.sendMessage(toUserId, objectName, content),
    loadModelCatalog: () => catalogLoader.load(),
    probeRuntime,
    agentName: opts.agentName,
    selfUserId: opts.transport.getSelfUserId?.(),
    nodeId: opts.transport.getSelfUserId?.(),
    // html_revise 的上一版 HTML 落盘目录：放 agent 的工作目录里，读它不需要
    // 越出工作区（见 im.ts writeReviseSource）。
    reviseSourceDir: opts.cwd,
    // agent 的工作目录：产物采集（artifacts.ts）在这里找本轮新写出的文件。
    artifactDir: opts.cwd,
    // 把产出文件送上服务端。设备凭据是这里的门槛：standalone `run` 模式
    // （没走配对/注册换出的 credentialId+deviceSecret）拿不到它，只能放弃
    // 上传 —— 回复本身照发，只是少一个文件气泡。这种情况只提示一次，免得
    // 每个回合都刷一行日志。
    uploadArtifact: async (artifact) => {
      const { nodeId, credentialId, deviceSecret } = opts.creds;
      if (!nodeId || !credentialId || !deviceSecret) {
        if (!artifactUploadWarned) {
          artifactUploadWarned = true;
          console.error(
            "artifact upload disabled: this node has no device credential (pair/register first); agent-produced files cannot be attached",
          );
        }
        return undefined;
      }
      const data = new Uint8Array(readFileSync(artifact.path));
      const result = await opts.api.uploadNodeFile(nodeId, credentialId, deviceSecret, {
        data,
        name: artifact.name,
      });
      return {
        url: result.url,
        name: result.filename || artifact.name,
        size: result.size,
        type: result.type || "application/octet-stream",
      };
    },
    // chat_stream 帧按会话类型路由：私聊走 PRIVATE 会话，群组/聊天室走
    // 对应会话号（由 runStreamedTurn 的 ctx.conversationType 决定）。
    sendStream: (conversationType, conversationId, objectName, content) =>
      opts.transport.sendMessageTo
        ? opts.transport.sendMessageTo(conversationType, conversationId, objectName, content)
        : Promise.reject(new Error("transport lacks sendMessageTo")),
  });
  const turns: TurnRunner = {
    runTurn: (prompt, model, sessionKey) =>
      runAgentTurn({
        execPath: opts.agentExecPath,
        agentName: opts.agentName,
        prompt,
        model: opts.model ?? model,
        timeoutMs: opts.turnTimeoutMs ?? DEFAULT_TURN_TIMEOUT_MS,
        cwd: opts.cwd,
        sessionKey,
      }),
    // Streaming variant: the agent's stdout is decoded chunk by chunk
    // (agents.ts onChunk, UTF-8 boundary safe); the dispatcher turns those
    // deltas into chat_stream snapshot frames. signal lets chat_stop kill
    // the agent process instead of just muting the frames.
    runTurnStreamed: (prompt, model, onDelta, signal, sessionKey) =>
      runAgentTurn({
        execPath: opts.agentExecPath,
        agentName: opts.agentName,
        prompt,
        model: opts.model ?? model,
        timeoutMs: opts.turnTimeoutMs ?? DEFAULT_TURN_TIMEOUT_MS,
        cwd: opts.cwd,
        onChunk: onDelta,
        // 思考流增量直接并入 onDelta 第二参：runStreamedTurn 维护
        // reasoningSnapshot 并与正文同帧发出。
        onReasoningChunk: (reasoning) => onDelta("", reasoning),
        // 过程流（工具调用步骤）走第三参：全量快照，runStreamedTurn 存最新的
        // 那份并与正文同帧发出。非 stream-json 型 agent 一次都不回调。
        onActivity: (steps) => onDelta("", undefined, steps),
        signal,
        sessionKey,
      }),
  };
  const queue = new SessionQueue({
    concurrencyPerConversation: 1,
    queueDepth: 1,
  });

  const selfUserId = opts.transport.getSelfUserId?.();
  const replayDedupe = createInboundReplayDedupe({ path: opts.processedUidsPath });
  if (opts.processedUidsPath) {
    // The write is debounced, so a clean shutdown must not drop the last batch.
    process.once("exit", () => replayDedupe.flush());
    const flushAndExit = (): void => {
      replayDedupe.flush();
      process.exit(0);
    };
    process.once("SIGINT", flushAndExit);
    process.once("SIGTERM", flushAndExit);
  }
  opts.transport.onMessage((msg) => {
    // IM observability: every inbound message is logged so smoke runs can
    // tell "message never arrived" from "dispatch failed".
    console.log(`im in: type=${msg.objectName} from=${msg.fromUserId} conv=${msg.conversationType}`);
    // 融云会把本端自己发出的消息同步回本端。若把它当用户提问，节点就会对自己
    // 的每条回复再回复一次，形成无限自问自答，并把该会话队列长期占满，真实请求
    // （包括模型目录请求）会被饿死或直接丢弃。
    if (selfUserId && msg.fromUserId === selfUserId) {
      console.log("im in: dropping own message (loop guard)");
      return;
    }
    if (replayDedupe.isDuplicate(msg.messageUId)) {
      console.log(`im in: dropping replayed message uid=${msg.messageUId}`);
      return;
    }
    const key = conversationKeyOf(msg);
    // 模型目录请求是廉价的协议查询，web 侧只等 8s。若走会话队列，一次长 agent
    // 回合（最长 120s）会把它拖过超时，或因为队列只有 1 个等待位而被直接丢弃。
    if (msg.objectName === "command" && parseModelCatalogRequest(msg.content)) {
      void dispatcher.handle(msg, turns).catch((err: unknown) => {
        console.error("dispatch failed:", err instanceof Error ? err.message : err);
      });
      return;
    }
    // Same reason as the catalog above, and one more: a recover can spend up to
    // 20s in an agent spawn, which would both blow the web's window and starve
    // the conversation queue (depth 1) it was waiting in.
    if (msg.objectName === "command") {
      const recoveryRequest = parseRuntimeRecoveryRequest(msg.content);
      if (recoveryRequest) {
        void dispatcher.handleRuntimeRecovery(msg, recoveryRequest).catch((err: unknown) => {
          console.error("runtime recovery failed:", err instanceof Error ? err.message : err);
        });
        return;
      }
    }
    // chat_stop 是即时动作，必须绕过会话队列：排在进行中的轮次后面意味着
    // 等轮次跑完才处理停止 —— 用户看到的就是「停止无效」（2026-10-20 现场）。
    // 同时把同会话还在排队的消息一起撤掉：停止的意图是「这个话题到此为止」，
    // 排队的旧消息再开跑就是用户眼里的「消息重复」。
    if (msg.objectName === "chat_stop") {
      const dropped = queue.dropWaiting(key);
      if (dropped > 0) console.log(`chat_stop: dropped ${dropped} queued message(s) for ${key}`);
      void dispatcher.handle(msg, turns).catch((err: unknown) => {
        console.error("dispatch failed:", err instanceof Error ? err.message : err);
      });
      return;
    }
    const accepted = queue.enqueue(
      key,
      async () => {
        try {
          await dispatcher.handle(msg, turns);
        } catch (err) {
          // Carried fix from Task 7 review: dispatcher.handle rejections
          // (including error-path send failures that escape) must never crash
          // the process from inside the queue's task.
          console.error("dispatch failed:", err instanceof Error ? err.message : err);
        }
      },
      // 这条消息若在排队时被 chat_stop 撤掉，给它的占位气泡补 cancelled 终帧。
      () => sendStopDropNotice(opts.transport, msg, selfUserId),
    );
    if (!accepted) {
      // 队列满（1 在执行 + 1 排队），这条消息被丢弃。必须给用户一个明确的
      // 终态：旧实现回纯文本「正在思考中…」，web 端会拿它原位替换占位气泡，
      // 之后排队的回合开始流式时又追加一条新气泡 —— 一条提问两个气泡，而且
      // 文案读起来像「正在处理你这条」，实际上消息根本没进队列。改成规范的
      // chat_stream failed 终帧（error_code=agent_busy）：web 端把占位气泡
      // 原位翻成「上一个任务还在执行」的明确失败文案，不会再有第二个气泡。
      sendBusyDropNotice(opts.transport, msg, selfUserId);
    }
  });

  opts.stdout.write("clawmessenger run: connected and dispatching\n");
  await new Promise<void>(() => {}); // run until process exit
}

// Resolve appKey + token and connect. Token-expiry auto-reconnect (spec
// 9.1): when the first connect fails, refresh the token once and retry
// with the fresh token — a single retry, no loops. The refreshed token is
// used for this session only; startRunLoop has no keystore access to
// persist it (run `clawmessenger login` to store one).
export async function connectTransport(opts: ConnectTransportOpts): Promise<void> {
  const appKey = opts.creds.appKey ?? (await opts.api.getConfig()).appKey;
  let token = opts.creds.token;
  if (!token) {
    const refreshed = await opts.api.refreshToken(opts.creds.nodeId);
    token = refreshed.token;
  }
  try {
    await opts.transport.connect(appKey, token);
  } catch (err) {
    console.error(
      "connect failed, refreshing token once and retrying:",
      err instanceof Error ? err.message : err,
    );
    const refreshed = await opts.api.refreshToken(opts.creds.nodeId);
    await opts.transport.connect(appKey, refreshed.token);
  }
}

// Production transport over @rongcloud/imlib-next 5.46.1. The imlib API
// mapping (init/connect/addEventListener(Events.MESSAGES)/sendMessage/
// disconnect) follows the installed package's index.d.ts: received messages
// expose messageType (mapped to objectName), senderUserId, numeric
// conversationType, and an already-parsed content object; sendMessage takes
// a conversation identifier plus a BaseMessage instance. This wiring is
// compiled and type-checked here but only truly exercised in the M4 smoke
// test (spec 7.2); unit tests inject fakes.
type CommandMessageCtor = new (content: Record<string, unknown>) => BaseMessage<Record<string, unknown>>;

// RongCloud rejects a send with 20604 (SEND_FREQUENCY_TOO_FAST) when the client
// bursts past its per-user rate limit. Reconnects replay the whole offline
// backlog at once, so this is a normal transient — a dropped response here is
// what leaves the web's 默认模型 panel hanging. Retry with backoff instead.
const SEND_FREQUENCY_TOO_FAST = 20604;
const SEND_RETRY_ATTEMPTS = 4;
const SEND_RETRY_BASE_MS = 250;

// 全连接串行发送队列：融云限速是「整个连接每秒 ≤5 条」，不分消息类型。
// 流帧每 500ms 一帧、一帧超 9KB 又切多个 wire 分块，突发就是限速的数倍 ——
// 事后退避重试追不上持续突发。所有发送（流帧/分块/command_result/文本）都
// 过同一个最小间隔队列，从源头不触发 20604；退避重试保留作最后防线。
// 间隔取 220ms（≈4.5 条/秒），给限速窗口留余量。
const SEND_QUEUE_INTERVAL_MS = 220;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * 串行发送节流器：同一进程内所有融云发送共享一个队列，相邻两条间隔不小于
 * SEND_QUEUE_INTERVAL_MS。回调抛错原样上抛（调用方决定重试/放弃），队列
 * 不因单条失败卡死。
 */
export class SendThrottle {
  private queue: Promise<void> = Promise.resolve();
  private lastStart = 0;

  run<T>(task: () => Promise<T>): Promise<T> {
    const result = this.queue.then(async () => {
      const wait = this.lastStart + SEND_QUEUE_INTERVAL_MS - Date.now();
      if (wait > 0) await sleep(wait);
      this.lastStart = Date.now();
      return task();
    });
    // 队列链不吞错也不因错断：失败的结果照样返回给调用方，链本身继续。
    this.queue = result.then(() => undefined, () => undefined);
    return result;
  }
}

/**
 * 设备端注册的融云自定义消息（objectName → [持久化, 计未读]）。与 web 端
 * src/services/im.ts 的同名表同源 —— 缺一条，那类消息就被融云当未知类型拒发
 * （unsupported objectName）。0.2.29 漏了 chat_stream_chunk：超过 9KB 的流帧
 * 全部被拒，长思考/长正文轮次在 web 上永远停在流式中，直到兜底整包文本落地
 * 才补上 —— 0.2.30 修。
 */
export const CUSTOM_MESSAGE_TYPES: ReadonlyArray<readonly [string, boolean, boolean]> = [
  ["command", true, true],
  ["card_message", true, true],
  ["card_update", true, true],
  ["card_action", false, false],
  // chat_stream: persisted but uncounted — same registration the web
  // uses, so the frames store as history yet never badge unread.
  ["chat_stream", true, false],
  // >9KB 帧的 wire 分块（web 端 ChatStreamWireReassembler 重组）。
  ["chat_stream_chunk", true, false],
  ["chat_stop", false, false],
  ["chat_stop_result", false, false],
  // file_message: agent 本轮产出的文件（HTML/报告/表格…）。持久化并计数，
  // 不注册这条，融云会把它当未知类型丢掉，用户永远看不到文件气泡。
  ["file_message", true, true],
];

export async function createImlibTransport(): Promise<IMTransport> {
  // imlib-next is browser-built; give it the minimal Node globals it
  // touches (window/localStorage/XHR) before importing.
  const { installBrowserShim } = await import("./browser-shim.js");
  installBrowserShim();
  const imlib = await import("@rongcloud/imlib-next");
  const listeners: Array<(msg: InboundIMMessage) => void> = [];
  const customMessages = new Map<string, CommandMessageCtor>();
  // 本连接共用的串行发送队列（每连接一个实例，见 SendThrottle 注释）。
  const sendThrottle = new SendThrottle();
  // The user id this transport is logged in as, captured at connect time so
  // the run loop can drop RongCloud's echoes of our own sent messages.
  let selfUserId: string | undefined;

  // 已读回执：判定见 im.ts planReadReceipt，这里只负责把它翻成 imlib 调用。
  // 单聊用 sendReadReceiptMessage（发送方收 READ_RECEIPT_RECEIVED 推送），
  // 群聊用 sendReadReceiptResponseV5（发送方靠查询已读数看到）。
  // 尽力而为：回执失败绝不能影响消息处理本身。
  const acknowledgeReceipt = async (m: IAReceivedMessage): Promise<void> => {
    const plan = planReadReceipt(m, selfUserId);
    if (!plan) return;
    try {
      if (plan.channel === "private") {
        const res = await imlib.sendReadReceiptMessage(plan.targetId, plan.messageUId, plan.sentTime);
        if (!res.isOk) console.warn(`read receipt (private) failed: ${res.code} ${res.msg}`);
        return;
      }
      const res = await imlib.sendReadReceiptResponseV5(
        { conversationType: plan.conversationType, targetId: plan.targetId },
        [plan.messageUId],
      );
      if (!res.isOk) console.warn(`read receipt (group) failed: ${res.code} ${res.msg}`);
    } catch (err) {
      console.warn("read receipt failed:", err instanceof Error ? err.message : err);
    }
  };

  const toBaseMessage = (objectName: string, content: string): BaseMessage<never> => {
    if (objectName === "RC:TxtMsg") {
      // Dispatcher hands us the full text-message content JSON.
      return new imlib.TextMessage(JSON.parse(content) as { content: string }) as BaseMessage<never>;
    }
    const custom = customMessages.get(objectName);
    if (custom) {
      return new custom(JSON.parse(content) as Record<string, unknown>) as BaseMessage<never>;
    }
    throw new Error(`unsupported objectName: ${objectName}`);
  };

  return {
    async connect(appKey: string, token: string): Promise<void> {
      imlib.init({ appkey: appKey });
      // Custom message types shared with the web client / Go server
      // (protocol.ts, web im.ts CUSTOM_MESSAGE_TYPES); registered before
      // sending. card_message/card_update are persisted; card_action is
      // transient (fire-and-forget interaction).
      for (const [name, persisted, counted] of CUSTOM_MESSAGE_TYPES) {
        customMessages.set(name, imlib.registerMessageType<Record<string, unknown>>(name, persisted, counted));
      }
      const res = await imlib.connect(token);
      if (!res.isOk) {
        throw new Error(`imlib connect failed: ${res.code} ${res.msg}`);
      }
      selfUserId = res.data?.userId || imlib.getCurrentUserId() || undefined;
      imlib.addEventListener(imlib.Events.MESSAGES, (evt: { messages: IAReceivedMessage[] }) => {
        for (const m of evt.messages) {
          // 收到即回执（不阻塞分发）：人类气泡的「已读」靠这一步。
          void acknowledgeReceipt(m);
          const raw = m.content as unknown;
          const content = typeof raw === "string" ? raw : JSON.stringify(raw ?? {});
          for (const cb of listeners) {
            cb({
              objectName: m.messageType,
              fromUserId: m.senderUserId,
              toUserId: "",
              targetId: m.targetId,
              conversationType: m.conversationType,
              content,
              ...(m.messageUId ? { messageUId: m.messageUId } : {}),
            });
          }
        }
      });
    },
    async sendMessage(toUserId: string, objectName: string, content: string): Promise<void> {
      const conversation = { conversationType: imlib.ConversationType.PRIVATE, targetId: toUserId };
      const message = toBaseMessage(objectName, content);
      return sendThrottle.run(async () => {
        for (let attempt = 1; ; attempt += 1) {
          const res = await imlib.sendMessage(conversation, message);
          if (res.isOk) return;
          if (res.code !== SEND_FREQUENCY_TOO_FAST || attempt >= SEND_RETRY_ATTEMPTS) {
            throw new Error(`imlib sendMessage failed: ${res.code} ${res.msg}`);
          }
          // Back off and try again: 250ms, 500ms, 1s.
          await sleep(SEND_RETRY_BASE_MS * 2 ** (attempt - 1));
        }
      });
    },
    // Sends into private (1) / group (3) / chatroom (4) conversations.
    // chat_stream frames for relay turns address the room itself.
    async sendMessageTo(conversationType: 1 | 3 | 4, targetId: string, objectName: string, content: string): Promise<void> {
      const conversation = {
        conversationType:
          conversationType === 3 ? imlib.ConversationType.GROUP
          : conversationType === 4 ? imlib.ConversationType.CHATROOM
          : imlib.ConversationType.PRIVATE,
        targetId,
      };
      const message = toBaseMessage(objectName, content);
      return sendThrottle.run(async () => {
        for (let attempt = 1; ; attempt += 1) {
          const res = await imlib.sendMessage(conversation, message);
          if (res.isOk) return;
          if (res.code !== SEND_FREQUENCY_TOO_FAST || attempt >= SEND_RETRY_ATTEMPTS) {
            throw new Error(`imlib sendMessage failed: ${res.code} ${res.msg}`);
          }
          await sleep(SEND_RETRY_BASE_MS * 2 ** (attempt - 1));
        }
      });
    },
    onMessage(cb): void {
      listeners.push(cb);
    },
    getSelfUserId(): string | undefined {
      return selfUserId || imlib.getCurrentUserId() || undefined;
    },
    async disconnect(): Promise<void> {
      await imlib.disconnect();
    },
  };
}
