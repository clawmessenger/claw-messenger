/**
 * opencode 服务端模式 —— 让「agent 吐什么就输出什么」成为事实。
 *
 * 背景（2026-10-26 实测，同一台设备、同一个模型）：
 *   · `opencode run --format json --thinking` 只在 part **完成**时上报一次
 *     「整段全文」，一轮 800 字长文只产出 4 行事件（step_start / reasoning /
 *     text / step_finish），正文与思考各出现一次。设备端拿到的就是 1 帧正文，
 *     客户端于是「转圈两分钟 → 全文一次性蹦出」。
 *   · `opencode serve` + `GET /event`（SSE）走的是真增量：`message.part.delta`
 *     一条一个 token 片段。同一 prompt 实测 12,620 条、平均 1.7 字/片、
 *     从 12.4s 持续到 113.3s（101 秒匀速），最大间隔 503ms。
 *
 * delta 事件的形状：
 *   {sessionID, messageID, partID, field:"text", delta:"长江"}
 * `field` **恒为 "text"**（正文与思考都是 text），二者只能靠 `partID` 对应的
 * `part.type` 区分。所以这里维护 partID → type 映射；映射未就绪时先把 delta
 * 暂存（见 PENDING_MAX_*），避免把思考串进正文。
 *
 * 本文件只做协议层（HTTP/SSE），不 spawn 进程 —— 进程生命周期留在 agents.ts，
 * 好让 killProcessTree / buildSpawnInvocation 这两处平台差异只有一份实现。
 */

import { Agent as HttpAgent, request as httpRequest, type IncomingMessage } from "node:http";

import { createActivityTracker, type ActivityStep, type ActivityTracker } from "./agent-activity.js";

// 单条 SSE 帧里 data 的最大长度：超长的多半是工具输出回显，不需要整段解析。
const MAX_EVENT_LINE = 1_000_000;
// 未识别 partID 的 delta 暂存上限：到这个量级还没等到 part.updated，就按正文
// 放行（fail-open：宁可把思考混进正文，也不能把已经生成的内容吞掉）。
const PENDING_MAX_DELTAS = 80;
const PENDING_MAX_MS = 2_000;
// POST /message 返回后仍可能有几帧 delta 在路上（实测末尾差 ~7ms），
// 收尾前给它一个窗口，别让最后几个字留在 SSE 缓冲区里。
const IDLE_GRACE_MS = 800;
// 收尾时至少要等的时长：idle 可能比 POST 的响应先到，此时不能立刻拆连接。
const SETTLE_MIN_MS = 120;

/** 从 `opencode serve` 的 stdout 里抓监听地址。 */
export function parseServeListeningUrl(line: string): string | null {
  // 实测行：`opencode server listening on http://127.0.0.1:49882`
  // Windows 上可能带 ANSI 颜色码，所以匹配到空白或 ESC 为止。
  // eslint-disable-next-line no-control-regex -- 要停下来的就是 ESC 本身
  const match = /listening on (https?:\/\/[^\s\u001b]+)/i.exec(line);
  if (!match) return null;
  return match[1].replace(/[),.;]+$/, "");
}

/** `provider/model` → 请求体的 model 字段。解析失败返回 null（用配置默认）。 */
export function toRequestModel(
  route: string | undefined,
): { providerID: string; modelID: string } | null {
  if (!route) return null;
  const slash = route.indexOf("/");
  if (slash <= 0 || slash === route.length - 1) return null;
  const providerID = route.slice(0, slash);
  const modelID = route.slice(slash + 1);
  // provider 与 model 都只允许 opencode 认识的字符集；带空格/引号的多半是
  // 用户在界面上填的自由文本，直接交给配置默认更安全。
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(providerID)) return null;
  if (!/^[A-Za-z0-9][A-Za-z0-9._:-]*$/.test(modelID)) return null;
  return { providerID, modelID };
}

export type PartKind = "text" | "reasoning" | "tool" | "other";

export interface ServeEventSink {
  onText?: (delta: string) => void;
  onReasoning?: (delta: string) => void;
  onActivity?: (steps: ActivityStep[]) => void;
}

export interface ServeEventState {
  sessionID: string;
  /** partID → part 类型。delta 只有 field，没有 type，全靠这张表分流。 */
  partTypes: Map<string, PartKind>;
  /** 未等到 part.updated 的 delta：按 partID 暂存，映射就绪后按序放行。 */
  pending: Map<string, { chunks: string[]; firstAt: number }>;
  text: string;
  reasoning: string;
  sawDelta: boolean;
  deltaCount: number;
  idle: boolean;
  /** 服务端上报的会话错误（session.error）。 */
  error: string | null;
  /** 最近一个工具步骤的 id 与「其后是否还有正文」，用于半句收尾判据。 */
  lastToolId: string | null;
  textAfterLastTool: boolean;
  activity: ActivityTracker | null;
  /** 工具 part 没有 callID 时用它造一个稳定 id。 */
  toolSeq: number;
}

export function createServeEventState(sessionID: string, withActivity: boolean): ServeEventState {
  return {
    sessionID,
    partTypes: new Map(),
    pending: new Map(),
    text: "",
    reasoning: "",
    sawDelta: false,
    deltaCount: 0,
    idle: false,
    error: null,
    lastToolId: null,
    textAfterLastTool: false,
    activity: withActivity ? createActivityTracker() : null,
    toolSeq: 0,
  };
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : undefined;
}

function partKindOf(partType: string | undefined): PartKind {
  if (partType === "text") return "text";
  if (partType === "reasoning") return "reasoning";
  if (partType === "tool") return "tool";
  return "other";
}

function emitDelta(state: ServeEventState, sink: ServeEventSink, kind: PartKind, delta: string): void {
  if (!delta) return;
  state.sawDelta = true;
  state.deltaCount += 1;
  if (kind === "reasoning") {
    state.reasoning += delta;
    sink.onReasoning?.(delta);
    return;
  }
  // tool 与 other 的 delta 不进正文（工具进度走 activity 快照）。
  if (kind !== "text") return;
  state.text += delta;
  state.textAfterLastTool = true;
  sink.onText?.(delta);
}

function flushPending(state: ServeEventState, sink: ServeEventSink, partID: string): void {
  const bucket = state.pending.get(partID);
  if (!bucket) return;
  state.pending.delete(partID);
  const kind = state.partTypes.get(partID) ?? "text";
  for (const chunk of bucket.chunks) emitDelta(state, sink, kind, chunk);
}

/** 暂存超时/超量的兜底：把还没认领的 delta 一律按正文放行。 */
function flushStalePending(state: ServeEventState, sink: ServeEventSink, now: number): void {
  if (state.pending.size === 0) return;
  for (const [partID, bucket] of [...state.pending]) {
    if (bucket.chunks.length < PENDING_MAX_DELTAS && now - bucket.firstAt < PENDING_MAX_MS) continue;
    if (!state.partTypes.has(partID)) state.partTypes.set(partID, "text");
    flushPending(state, sink, partID);
  }
}

/**
 * 消化一条 SSE 事件。只改 state、只调 sink，便于单测。
 *
 * 关心五种事件：message.part.updated（建 partID 映射 + 工具步骤）、
 * message.part.delta（真增量）、session.idle（收尾）、session.error（失败）、
 * 其余（session.status / message.updated / plugin.added / heartbeat）忽略。
 */
export function applyServeEvent(
  state: ServeEventState,
  eventType: string,
  payload: unknown,
  sink: ServeEventSink,
  now: number = Date.now(),
): void {
  const root = asRecord(payload);
  const props = asRecord(root?.properties) ?? root;
  if (!props) return;
  // 只要本会话的事件：serve 是全机的，可能同时有别的 session 在跑。
  const sid = typeof props.sessionID === "string" ? props.sessionID : undefined;
  if (sid && state.sessionID && sid !== state.sessionID) return;

  flushStalePending(state, sink, now);

  if (eventType === "message.part.delta") {
    const partID = typeof props.partID === "string" ? props.partID : "";
    const delta = typeof props.delta === "string" ? props.delta : "";
    if (!partID || !delta) return;
    const kind = state.partTypes.get(partID);
    if (kind) {
      emitDelta(state, sink, kind, delta);
      return;
    }
    const bucket = state.pending.get(partID) ?? { chunks: [], firstAt: now };
    bucket.chunks.push(delta);
    state.pending.set(partID, bucket);
    return;
  }

  if (eventType === "message.part.updated") {
    const part = asRecord(props.part);
    if (!part) return;
    const partID = typeof part.id === "string" ? part.id : "";
    const partType = typeof part.type === "string" ? part.type : "";
    const kind = partKindOf(partType);
    if (partID && partType) {
      state.partTypes.set(partID, kind);
      flushPending(state, sink, partID);
    }
    // 工具步骤：改写成 run 模式同款形状后交给 tracker，复用同一套分类逻辑
    // （agent-activity.ts 认的是 {type:"tool_use", part:{type:"tool",…}}）。
    if (kind === "tool" && state.activity) {
      state.toolSeq += 1;
      const callID = typeof part.callID === "string" && part.callID
        ? part.callID
        : `${typeof part.tool === "string" ? part.tool : "tool"}-${state.toolSeq}`;
      const line = JSON.stringify({ type: "tool_use", part: { ...part, callID, id: callID } });
      if (state.activity.push(line)) {
        const steps = state.activity.snapshot();
        sink.onActivity?.(steps);
        const newest = steps[steps.length - 1];
        if (newest && newest.id !== state.lastToolId) {
          state.lastToolId = newest.id;
          state.textAfterLastTool = false;
        }
      }
    }
    return;
  }

  if (eventType === "session.idle") {
    state.idle = true;
    return;
  }

  if (eventType === "session.error") {
    const err = asRecord(props.error);
    state.error = typeof err?.message === "string"
      ? err.message
      : typeof props.message === "string" ? props.message : "opencode session error";
  }
}

// --- HTTP / SSE -----------------------------------------------------------

interface HttpJsonResult {
  status: number;
  body: string;
}

function requestJson(
  base: string,
  method: string,
  path: string,
  body: unknown,
  agent: HttpAgent,
  signal?: AbortSignal,
): Promise<HttpJsonResult> {
  return new Promise((resolve, reject) => {
    const url = new URL(path, base);
    const data = body === undefined ? null : JSON.stringify(body);
    const req = httpRequest(
      {
        hostname: url.hostname,
        port: url.port,
        path: url.pathname + url.search,
        method,
        agent,
        headers: data
          ? {
            "content-type": "application/json",
            "content-length": Buffer.byteLength(data),
            accept: "application/json",
          }
          : { accept: "application/json" },
      },
      (res: IncomingMessage) => {
        let text = "";
        res.setEncoding("utf8");
        res.on("data", (chunk: string) => {
          if (text.length < MAX_EVENT_LINE) text += chunk;
        });
        res.on("end", () => resolve({ status: res.statusCode ?? 0, body: text }));
      },
    );
    req.on("error", reject);
    if (signal) {
      const onAbort = () => req.destroy(new Error("aborted"));
      signal.addEventListener("abort", onAbort, { once: true });
      req.on("close", () => signal.removeEventListener("abort", onAbort));
    }
    if (data) req.write(data);
    req.end();
  });
}

export interface RunServerSessionOpts {
  /** `http://127.0.0.1:<port>`（来自 serve 的 stdout）。 */
  baseUrl: string;
  prompt: string;
  model?: string;
  timeoutMs: number;
  signal?: AbortSignal;
  onText?: (delta: string) => void;
  onReasoning?: (delta: string) => void;
  onActivity?: (steps: ActivityStep[]) => void;
  /** 正文长度硬上限（字符）：超了就中止并以 output_truncated 失败。 */
  maxTextChars?: number;
}

export interface RunServerSessionResult {
  /** 权威正文：优先取 POST /message 的返回，其次取 delta 累计。 */
  text: string;
  sawDelta: boolean;
  deltaCount: number;
  reasoningChars: number;
  idleSeen: boolean;
  /** 最后一个工具步骤失败、其后又没正文 —— 这一轮只有半句话（与 run 模式同判据）。 */
  endedOnFailedTool: boolean;
  failedStep: ActivityStep | null;
}

export class OpenCodeServerError extends Error {
  constructor(message: string, readonly truncated = false) {
    super(message);
    this.name = "OpenCodeServerError";
  }
}

function extractReplyText(body: unknown): string | undefined {
  const root = asRecord(body);
  const parts = root?.parts;
  if (!Array.isArray(parts)) return undefined;
  const texts: string[] = [];
  for (const item of parts) {
    const part = asRecord(item);
    if (!part) continue;
    if (part.type === "text" && typeof part.text === "string" && part.text) texts.push(part.text);
  }
  const joined = texts.join("").trim();
  return joined.length > 0 ? joined : undefined;
}

/**
 * 在已启动的 serve 上跑完一轮：建会话 → 订阅 SSE → 发消息 → 收集增量。
 * 调用方负责起停进程；这里只保证「收完就断开 SSE」。
 */
export async function runServerSession(opts: RunServerSessionOpts): Promise<RunServerSessionResult> {
  const agent = new HttpAgent({ keepAlive: true, maxSockets: 8 });
  const abort = new AbortController();
  let timedOut = false;
  const onOuterAbort = () => abort.abort();
  opts.signal?.addEventListener("abort", onOuterAbort, { once: true });

  const state = createServeEventState("", opts.onActivity !== undefined);
  const sink: ServeEventSink = {
    onText: opts.onText,
    onReasoning: opts.onReasoning,
    onActivity: opts.onActivity,
  };

  let sseReq: ReturnType<typeof httpRequest> | null = null;
  let timer: NodeJS.Timeout | undefined;
  let overflow = false;

  const cleanup = () => {
    if (timer) clearTimeout(timer);
    opts.signal?.removeEventListener("abort", onOuterAbort);
    if (sseReq) {
      try {
        sseReq.destroy();
      } catch {
        // 已经关掉了。
      }
    }
    agent.destroy();
  };

  try {
    // 1) 建会话。title 只用于 opencode 自己的会话列表，不影响语义。
    const created = await requestJson(
      opts.baseUrl, "POST", "/session", { title: "clawmessenger" }, agent, abort.signal,
    );
    if (created.status !== 200) {
      throw new OpenCodeServerError(`POST /session -> HTTP ${created.status}: ${created.body.slice(0, 200)}`);
    }
    let sessionID = "";
    try {
      sessionID = String((JSON.parse(created.body) as { id?: unknown }).id ?? "");
    } catch {
      // 落到下面的空 id 报错。
    }
    if (!sessionID) throw new OpenCodeServerError("POST /session returned no id");
    state.sessionID = sessionID;

    // 2) 订阅事件流。必须在发消息之前建立，否则最早几个 delta 会漏掉。
    await new Promise<void>((resolve, reject) => {
      const url = new URL("/event", opts.baseUrl);
      sseReq = httpRequest(
        {
          hostname: url.hostname,
          port: url.port,
          path: url.pathname,
          method: "GET",
          agent,
          headers: { accept: "text/event-stream" },
        },
        (res) => {
          if ((res.statusCode ?? 0) !== 200) {
            reject(new OpenCodeServerError(`GET /event -> HTTP ${res.statusCode}`));
            return;
          }
          res.setEncoding("utf8");
          let buf = "";
          res.on("data", (chunk: string) => {
            buf += chunk;
            // SSE 以空行分帧；\r\n 与 \n 都要吃。
            let idx: number;
            while ((idx = buf.search(/\r?\n\r?\n/)) >= 0) {
              const raw = buf.slice(0, idx);
              buf = buf.slice(idx + (buf[idx] === "\r" ? 4 : 2));
              let eventType = "";
              const dataLines: string[] = [];
              for (const line of raw.split(/\r?\n/)) {
                if (line.startsWith("event:")) eventType = line.slice(6).trim();
                else if (line.startsWith("data:")) dataLines.push(line.slice(5).trim());
                // 其它字段（id:/retry:）与心跳注释（`:heartbeat`）忽略。
              }
              if (dataLines.length === 0) continue;
              let payload: unknown;
              try {
                payload = JSON.parse(dataLines.join("\n"));
              } catch {
                continue;
              }
              const type = eventType
                || (typeof (payload as { type?: unknown })?.type === "string"
                  ? String((payload as { type?: unknown }).type)
                  : "");
              applyServeEvent(state, type, payload, sink);
              if (!overflow
                && opts.maxTextChars !== undefined
                && state.text.length > opts.maxTextChars) {
                overflow = true;
                abort.abort();
              }
            }
          });
          resolve();
        },
      );
      sseReq.on("error", (err) => reject(err));
      sseReq.end();
    });

    // 3) 发消息。这个请求要一直挂到本轮结束（实测 800 字长文 113 秒）。
    const model = toRequestModel(opts.model);
    timer = setTimeout(() => {
      timedOut = true;
      abort.abort();
    }, opts.timeoutMs);
    const replied = await requestJson(
      opts.baseUrl,
      "POST",
      `/session/${encodeURIComponent(sessionID)}/message`,
      { parts: [{ type: "text", text: opts.prompt }], ...(model ? { model } : {}) },
      agent,
      abort.signal,
    );

    if (overflow) {
      throw new OpenCodeServerError(`opencode reply exceeded ${opts.maxTextChars} chars`, true);
    }
    if (replied.status !== 200) {
      // 服务端在 body 里给出原因（模型 401 / 上下文超限等），保留前 400 字符。
      throw new OpenCodeServerError(
        `POST /session/${sessionID}/message -> HTTP ${replied.status}: ${replied.body.slice(0, 400)}`,
      );
    }
    if (state.error) throw new OpenCodeServerError(`opencode session error: ${state.error}`);

    // 4) 收尾窗口：POST 返回后仍有几帧 delta 在路上（实测末尾差 ~7ms），
    //    让最后几个字也走完 onText。至少要等一小会儿 —— idle 有可能比 POST
    //    的响应先到，那就不能因为「已经 idle」立刻拆掉连接。
    const settleStart = Date.now();
    const graceDeadline = settleStart + IDLE_GRACE_MS;
    while (Date.now() - settleStart < SETTLE_MIN_MS || (!state.idle && Date.now() < graceDeadline)) {
      await new Promise((r) => setTimeout(r, 20));
    }

    // 溢出可能发生在 POST 返回**之后**（delta 还在管道里），所以这里要再看一眼：
    // 前面的检查只覆盖「还没等 POST 就溢出了」那种情形。
    if (overflow) {
      throw new OpenCodeServerError(`opencode reply exceeded ${opts.maxTextChars} chars`, true);
    }

    let authoritative: string | undefined;
    try {
      authoritative = extractReplyText(JSON.parse(replied.body));
    } catch {
      authoritative = undefined;
    }

    // delta 累计与 POST 返回不一致时以 POST 为准（它是最终的完整消息）。
    const finalText = authoritative ?? (state.text.trim().length > 0 ? state.text : "");

    const steps = state.activity?.snapshot() ?? [];
    const lastStep = steps[steps.length - 1];
    const endedOnFailedTool = Boolean(lastStep && lastStep.status === "failed" && !state.textAfterLastTool);

    return {
      text: finalText,
      sawDelta: state.sawDelta,
      deltaCount: state.deltaCount,
      reasoningChars: state.reasoning.length,
      idleSeen: state.idle,
      endedOnFailedTool,
      failedStep: endedOnFailedTool && lastStep ? lastStep : null,
    };
  } catch (err) {
    if (timedOut) throw new Error(`agent turn timeout after ${opts.timeoutMs}ms`);
    if (opts.signal?.aborted) throw new Error("agent turn aborted");
    throw err;
  } finally {
    cleanup();
  }
}

/** 中止一轮：让 opencode 自己取消生成（比直接杀进程干净，会话状态不留半截）。 */
export async function abortServerSession(baseUrl: string, sessionID: string): Promise<void> {
  if (!sessionID) return;
  const agent = new HttpAgent({ keepAlive: false });
  try {
    await requestJson(baseUrl, "POST", `/session/${encodeURIComponent(sessionID)}/abort`, undefined, agent);
  } catch {
    // 进程可能已经没了；中止的语义是「尽量」，失败不该盖住真正的原因。
  } finally {
    agent.destroy();
  }
}

/** 供 agents.ts 判定「这条错误是不是 serve 路径自己放弃、该退回 run 模式」。 */
export function isServeStartupFailure(err: unknown): boolean {
  const message = err instanceof Error ? err.message : String(err);
  // 只认「serve 根本没起来」这一类：没打监听地址、进程退出、端口连不上、
  // 端点 404（旧版没有 serve）。**不认**超时与模型报错 —— 那些说明 serve
  // 已经在跑了，退回 run 只会把同一次失败再放大一遍。
  return /opencode serve (?:did not report its listening URL|exited with code)|ECONNREFUSED|GET \/event -> HTTP|POST \/session -> HTTP 404/
    .test(message);
}
