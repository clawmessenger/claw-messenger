import type { BaseMessage, IAReceivedMessage } from "@rongcloud/imlib-next";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
import type { IMTransport, InboundIMMessage, TurnRunner } from "./im.js";
import { MessageDispatcher } from "./im.js";
import { runAgentTurn, runAgentCommand } from "./agents.js";
import { createModelCatalogLoader, parseModelCatalogRequest } from "./model-catalog.js";
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
}

export class SessionQueue {
  private readonly active = new Map<string, number>();
  private readonly waiting = new Map<string, PendingTask[]>();
  private readonly idleResolvers: Array<() => void> = [];
  private totalPending = 0;

  constructor(private readonly opts: SessionQueueOpts) {}

  enqueue(conversationKey: string, task: () => Promise<void>): void {
    const activeCount = this.active.get(conversationKey) ?? 0;
    const waiting = this.waiting.get(conversationKey) ?? [];
    if (activeCount >= this.opts.concurrencyPerConversation) {
      const depth = this.opts.queueDepth ?? Number.MAX_SAFE_INTEGER;
      if (waiting.length >= depth) {
        this.opts.onBusy?.(conversationKey);
        return;
      }
      waiting.push({ key: conversationKey, run: task });
      this.waiting.set(conversationKey, waiting);
      return;
    }
    this.start(conversationKey, task);
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

// Bounded LRU-ish map (the run loop's busyPeer tracker): Map preserves
// insertion order, so on insert past the cap the oldest entry is evicted;
// a re-set refreshes recency via delete+set.
export interface BoundedMap {
  get(key: string): string | undefined;
  set(key: string, value: string): void;
  readonly size: number;
}

export function createBoundedMap(maxEntries: number): BoundedMap {
  const entries = new Map<string, string>();
  return {
    get: (key) => entries.get(key),
    set(key, value) {
      entries.delete(key);
      entries.set(key, value);
      if (entries.size > maxEntries) {
        const oldest = entries.keys().next().value;
        if (oldest !== undefined) entries.delete(oldest);
      }
    },
    get size() {
      return entries.size;
    },
  };
}

export interface ConnectTransportOpts {
  creds: StoredCredentials;
  api: XiachatApi;
  transport: IMTransport;
}

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
}

export async function startRunLoop(opts: StartRunLoopOpts): Promise<void> {
  await connectTransport(opts);

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

  const dispatcher = new MessageDispatcher({
    send: (toUserId, objectName, content) => opts.transport.sendMessage(toUserId, objectName, content),
    loadModelCatalog: () => catalogLoader.load(),
    selfUserId: opts.transport.getSelfUserId?.(),
  });
  const turns: TurnRunner = {
    runTurn: (prompt, model) =>
      runAgentTurn({
        execPath: opts.agentExecPath,
        agentName: opts.agentName,
        prompt,
        model: opts.model ?? model,
        timeoutMs: opts.turnTimeoutMs ?? 120_000,
        cwd: opts.cwd,
      }),
  };
  // onBusy only receives the conversation key; remember the latest peer per
  // conversation so the busy hint can be addressed (private chat only).
  // Bounded so a hostile/long-lived peer stream cannot grow it forever.
  const busyPeer = createBoundedMap(128);
  const queue = new SessionQueue({
    concurrencyPerConversation: 1,
    queueDepth: 1,
    onBusy: (key) => {
      const [convType] = key.split(":", 2);
      if (convType !== "1") return;
      const peer = busyPeer.get(key);
      if (!peer) return;
      // Single chat busy hint; discussion turns never overflow (coordinator
      // serializes turns itself).
      void opts.transport
        .sendMessage(peer, "RC:TxtMsg", JSON.stringify({ content: "正在思考中…" }))
        .catch(() => {});
    },
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
    busyPeer.set(key, msg.fromUserId);
    // 模型目录请求是廉价的协议查询，web 侧只等 8s。若走会话队列，一次长 agent
    // 回合（最长 120s）会把它拖过超时，或因为队列只有 1 个等待位而被直接丢弃。
    if (msg.objectName === "command" && parseModelCatalogRequest(msg.content)) {
      void dispatcher.handle(msg, turns).catch((err: unknown) => {
        console.error("dispatch failed:", err instanceof Error ? err.message : err);
      });
      return;
    }
    queue.enqueue(key, async () => {
      try {
        await dispatcher.handle(msg, turns);
      } catch (err) {
        // Carried fix from Task 7 review: dispatcher.handle rejections
        // (including error-path send failures that escape) must never crash
        // the process from inside the queue's task.
        console.error("dispatch failed:", err instanceof Error ? err.message : err);
      }
    });
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

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export async function createImlibTransport(): Promise<IMTransport> {
  // imlib-next is browser-built; give it the minimal Node globals it
  // touches (window/localStorage/XHR) before importing.
  const { installBrowserShim } = await import("./browser-shim.js");
  installBrowserShim();
  const imlib = await import("@rongcloud/imlib-next");
  const listeners: Array<(msg: InboundIMMessage) => void> = [];
  const customMessages = new Map<string, CommandMessageCtor>();
  // The user id this transport is logged in as, captured at connect time so
  // the run loop can drop RongCloud's echoes of our own sent messages.
  let selfUserId: string | undefined;

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
      for (const [name, persisted, counted] of [
        ["command", true, true],
        ["card_message", true, true],
        ["card_update", true, true],
        ["card_action", false, false],
      ] as const) {
        customMessages.set(name, imlib.registerMessageType<Record<string, unknown>>(name, persisted, counted));
      }
      const res = await imlib.connect(token);
      if (!res.isOk) {
        throw new Error(`imlib connect failed: ${res.code} ${res.msg}`);
      }
      selfUserId = res.data?.userId || imlib.getCurrentUserId() || undefined;
      imlib.addEventListener(imlib.Events.MESSAGES, (evt: { messages: IAReceivedMessage[] }) => {
        for (const m of evt.messages) {
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
      for (let attempt = 1; ; attempt += 1) {
        const res = await imlib.sendMessage(conversation, message);
        if (res.isOk) return;
        if (res.code !== SEND_FREQUENCY_TOO_FAST || attempt >= SEND_RETRY_ATTEMPTS) {
          throw new Error(`imlib sendMessage failed: ${res.code} ${res.msg}`);
        }
        // Back off and try again: 250ms, 500ms, 1s.
        await sleep(SEND_RETRY_BASE_MS * 2 ** (attempt - 1));
      }
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
