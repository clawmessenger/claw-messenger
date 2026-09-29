import type { BaseMessage, IAReceivedMessage } from "@rongcloud/imlib-next";
import type { IMTransport, InboundIMMessage } from "./im.js";
import { MessageDispatcher } from "./im.js";
import { runAgentTurn } from "./agents.js";
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

export interface StartRunLoopOpts {
  creds: StoredCredentials;
  api: XiachatApi;
  transport: IMTransport;
  agentExecPath: string;
  model?: string;
  stdout: NodeJS.WriteStream;
  turnTimeoutMs?: number;
}

export async function startRunLoop(opts: StartRunLoopOpts): Promise<void> {
  const { appKey, token } = await ensureConnection(opts);
  await opts.transport.connect(appKey, token);

  const dispatcher = new MessageDispatcher({
    send: (toUserId, objectName, content) => opts.transport.sendMessage(toUserId, objectName, content),
  });
  // onBusy only receives the conversation key; remember the latest peer per
  // conversation so the busy hint can be addressed (private chat only).
  const busyPeer = new Map<string, string>();
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
        .sendMessage(peer, "RC:TxtMsg", JSON.stringify({ content: "正在思考中，请稍候…" }))
        .catch(() => {});
    },
  });

  opts.transport.onMessage((msg) => {
    const key = conversationKeyOf(msg);
    busyPeer.set(key, msg.fromUserId);
    queue.enqueue(key, async () => {
      try {
        await dispatcher.handle(msg, {
          runTurn: (prompt, model) =>
            runAgentTurn({
              execPath: opts.agentExecPath,
              prompt,
              model: opts.model ?? model,
              timeoutMs: opts.turnTimeoutMs ?? 120_000,
            }),
        });
      } catch (err) {
        // Carried fix from Task 7 review: dispatcher.handle rejections
        // (including error-path send failures that escape) must never crash
        // the process from inside the queue's task.
        console.error("dispatch failed:", err instanceof Error ? err.message : err);
      }
    });
  });

  opts.stdout.write("xiachat run: connected and dispatching\n");
  await new Promise<void>(() => {}); // run until process exit
}

async function ensureConnection(opts: StartRunLoopOpts): Promise<{ appKey: string; token: string }> {
  const appKey = opts.creds.appKey ?? (await opts.api.getConfig()).appKey;
  let token = opts.creds.token;
  if (!token) {
    // The refreshed token is used for this session only; startRunLoop has no
    // keystore access to persist it (run `xiachat login` to store one).
    const refreshed = await opts.api.refreshToken(opts.creds.nodeId);
    token = refreshed.token;
  }
  return { appKey, token };
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

export async function createImlibTransport(): Promise<IMTransport> {
  const imlib = await import("@rongcloud/imlib-next");
  const listeners: Array<(msg: InboundIMMessage) => void> = [];
  let commandMessage: CommandMessageCtor | undefined;

  const toBaseMessage = (objectName: string, content: string): BaseMessage<never> => {
    if (objectName === "RC:TxtMsg") {
      // Dispatcher hands us the full text-message content JSON.
      return new imlib.TextMessage(JSON.parse(content) as { content: string }) as BaseMessage<never>;
    }
    if (objectName === "command" && commandMessage) {
      return new commandMessage(JSON.parse(content) as Record<string, unknown>) as BaseMessage<never>;
    }
    throw new Error(`unsupported objectName: ${objectName}`);
  };

  return {
    async connect(appKey: string, token: string): Promise<void> {
      imlib.init({ appkey: appKey });
      // Custom message type for the "command" objectName shared with the Go
      // server (protocol.ts); must be registered before sending.
      commandMessage = imlib.registerMessageType<Record<string, unknown>>("command", true, true);
      const res = await imlib.connect(token);
      if (!res.isOk) {
        throw new Error(`imlib connect failed: ${res.code} ${res.msg}`);
      }
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
            });
          }
        }
      });
    },
    async sendMessage(toUserId: string, objectName: string, content: string): Promise<void> {
      const conversation = { conversationType: imlib.ConversationType.PRIVATE, targetId: toUserId };
      const res = await imlib.sendMessage(conversation, toBaseMessage(objectName, content));
      if (!res.isOk) {
        throw new Error(`imlib sendMessage failed: ${res.code} ${res.msg}`);
      }
    },
    onMessage(cb): void {
      listeners.push(cb);
    },
    async disconnect(): Promise<void> {
      await imlib.disconnect();
    },
  };
}
