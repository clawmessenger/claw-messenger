/**
 * opencode 2.x 的 `/api/*` 会话协议（真流式）。
 *
 * 为什么单独一个文件：2.x 的 `serve` 不再是 1.x 那个「纯 HTTP 会话 API」。实测
 * 2.1.4（2026-10-27，Windows）：
 *   · `POST /session` → 405。`/session`、`/event` 这些根路径现在是 Web UI 的
 *     catch-all，GET 一律回 index.html（200 + HTML），所以 1.x 客户端不但建不了
 *     会话，连 SSE 都会读到一坨 HTML。
 *   · 真 API 全部挪到 `/api/*`，并且要鉴权：**HTTP Basic，用户名固定 `opencode`，
 *     密码是 serve 启动时打在 stdout 的那串**（`server password <43 位>`），
 *     也可以用 `OPENCODE_PASSWORD` 环境变量告知客户端。只带 Bearer 是 401。
 *   · 事件流 `GET /api/event` 的帧形如 `data: {"type":...,"data":{...}}`，
 *     没有 SSE 的 `event:` 字段，类型在 JSON 里 —— 与 1.x 的 `event:` + `data:`
 *     双字段写法不同，所以 1.x 的解析器读不出类型。
 *
 * 事件词表（实测两轮对话：纯文本一轮 + shell/read 各一次、含一次工具失败）：
 *   session.text.delta        data.delta 是**纯字符串片段**（不需要 partID 映射）
 *   session.reasoning.delta   同上，思考流
 *   session.tool.*            **工具调用**，过程流（activity）的唯一数据源：
 *     input.started  {id, name}                工具名在这里，且只在这里
 *     input.delta    {id, delta}               入参流式片段（未必出现）
 *     input.ended    {id, text}                入参**整段 JSON 字符串**
 *     called         {id, input, executed}     入参已解析成对象
 *     progress       {id, metadata}            执行中（如 shell 已起、未退出）
 *     success        {id, content:[{type,text}], metadata:{status,exit,truncated}}
 *     failed         {id, error:{type,message}}
 *   session.step.*            **一次模型调用**的开始/结束（不是工具！）：
 *     started {agent, model, assistantMessageID, started}
 *     streamed / ended {finish, cost, tokens{input,output,reasoning,cache}} / failed
 *   session.execution.started / succeeded / failed / interrupted
 *   session.error             会话级错误
 *   message.part.updated      整段 part 快照（兜底取权威正文）
 *
 * 这比 1.x 简单：1.x 的正文与思考共用一个 `message.part.delta`，只能靠 partID
 * 反查 part.type 区分；2.x 直接分成两种事件，`delta` 就是字符串。
 *
 * 注意 1.x 的经验在这里会误导人：1.x 的「step_finish」是 run 模式的收尾，而 2.x 的
 * `session.step.*` 是**模型调用**级别的（带 finish/cost/tokens），工具执行完全不在
 * 里面。所以过程流必须接 `session.tool.*`，`session.step.*` 只用于失败原因兜底。
 */

import { Agent as HttpAgent, request as httpRequest, type IncomingMessage } from "node:http";

import {
  classifyTool,
  createActivityTracker,
  type ActivityStep,
  type ActivityTracker,
} from "./agent-activity.js";

/** 单条 SSE 帧里 data 的最大长度：超长的多半是工具输出回显。 */
const MAX_EVENT_LINE = 1_000_000;
/** 收到执行结束帧后，再等这么久把在途的尾巴 delta 收干净。 */
const SETTLE_GRACE_MS = 300;
/**
 * 中止请求的上限。`interrupt` 只挂在 `finally` 的 `void` 上（不影响本轮返回），
 * 但它的 socket 若一直不关就会拖住事件循环、拖慢进程退出 —— 3s 足够本地
 * serve 应答，超时即毁掉 socket，语义本来就是「尽量」。
 */
const INTERRUPT_TIMEOUT_MS = 3_000;

export interface RunV2SessionOpts {
  /** `http://127.0.0.1:<port>`（来自 serve 的 stdout）。 */
  baseUrl: string;
  /** serve 启动时打印的 server password；Basic 鉴权的密码。 */
  password: string;
  prompt: string;
  /** `provider/model` 或 `provider/model#variant`。 */
  model?: string;
  timeoutMs: number;
  signal?: AbortSignal;
  onText?: (delta: string) => void;
  onReasoning?: (delta: string) => void;
  /** 过程流：每次工具步骤变化给一份**全量**快照（与 1.x / run 模式同语义）。 */
  onActivity?: (steps: ActivityStep[]) => void;
  /** 正文长度硬上限（字符）：超了就中止并以 output_truncated 失败。 */
  maxTextChars?: number;
}

export interface RunV2SessionResult {
  text: string;
  sawDelta: boolean;
  deltaCount: number;
  reasoningChars: number;
  /** 最后一个工具步骤失败、其后又没正文 —— 这一轮只有半句话（与 1.x 同判据）。 */
  endedOnFailedTool: boolean;
  failedStep: ActivityStep | null;
}

export class OpenCodeV2Error extends Error {
  constructor(message: string, readonly truncated = false) {
    super(message);
    this.name = "OpenCodeV2Error";
  }
}

/** 2.x 的鉴权头：Basic + 固定用户名 opencode。 */
export function v2AuthHeader(password: string): string {
  return `Basic ${Buffer.from(`opencode:${password}`, "utf8").toString("base64")}`;
}

/**
 * `provider/model[#variant]` → 2.x 的 Model.Ref `{providerID, id, variant}`。
 * 2.x 没有 `--variant`，思考等级骑在模型串的 `#` 后面（服务端同样按这个约定处理）。
 */
export function toV2ModelRef(route: string | undefined): {
  providerID: string;
  id: string;
  variant?: string;
} | null {
  if (!route) return null;
  const hash = route.indexOf("#");
  const base = hash >= 0 ? route.slice(0, hash) : route;
  const variant = hash >= 0 ? route.slice(hash + 1) : "";
  const slash = base.indexOf("/");
  if (slash <= 0 || slash === base.length - 1) return null;
  return {
    providerID: base.slice(0, slash),
    id: base.slice(slash + 1),
    ...(variant ? { variant } : {}),
  };
}

interface V2JsonResult {
  status: number;
  body: string;
}

function v2Request(
  base: string,
  method: string,
  path: string,
  password: string,
  body: unknown,
  agent: HttpAgent,
  signal?: AbortSignal,
  timeoutMs?: number,
): Promise<V2JsonResult> {
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
        // 只给「不该长挂」的请求（interrupt）设上界；正文/建会话一律不设，
        // 它们的长短由 opts.timeoutMs 统一管。
        ...(timeoutMs ? { timeout: timeoutMs } : {}),
        headers: {
          authorization: v2AuthHeader(password),
          accept: "application/json",
          ...(data
            ? {
              "content-type": "application/json",
              "content-length": Buffer.byteLength(data),
            }
            : {}),
        },
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
    if (timeoutMs) {
      // 不带监听器时 socket 超时不会自己做任何事（Node 只 emit），必须销毁。
      req.on("timeout", () => req.destroy(new Error(`request timeout after ${timeoutMs}ms`)));
    }
    if (signal) {
      const onAbort = () => req.destroy(new Error("aborted"));
      signal.addEventListener("abort", onAbort, { once: true });
      req.on("close", () => signal.removeEventListener("abort", onAbort));
      // 已经中止过的 signal 不会补发事件：不主动收一次，这个请求就永远悬着
      // （超时正好落在两次请求之间的窗口里）。
      if (signal.aborted) onAbort();
    }
    if (data) req.write(data);
    req.end();
  });
}

function record(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : null;
}

function nested(value: unknown, path: string[]): unknown {
  let cursor: unknown = value;
  for (const key of path) {
    const rec = record(cursor);
    if (!rec) return undefined;
    cursor = rec[key];
  }
  return cursor;
}

function errorMessage(payload: unknown): string {
  const message = nested(payload, ["data", "error", "message"]);
  if (typeof message === "string" && message.trim()) return message.trim();
  const fallback = nested(payload, ["data", "message"]);
  return typeof fallback === "string" ? fallback.trim() : "";
}

/**
 * 会结束本轮、或决定本轮成败的事件。这几类必须能确认是**本会话**的：`/api/event`
 * 是全机的，一帧不带 `sessionID` 的 `execution.succeeded` 会把本轮提前判成成功
 * （半句话当完整回答），一帧无关的 `session.error` 会把本轮判成失败。
 * 正文/过程类事件不走这个判据 —— `message.part.updated` 的 sessionID 挂在
 * `data.part` 上，收紧会误杀兜底正文。
 */
function isTurnTerminalEvent(type: string): boolean {
  return (
    type.startsWith("session.execution.")
    || type === "session.error"
    || type === "session.step.failed"
  );
}

/** `session.tool.success` 的 content（`[{type:"text",text}]`）→ 纯文本结果。 */
function toolOutputText(content: unknown): string | undefined {
  if (!Array.isArray(content)) return undefined;
  const parts: string[] = [];
  for (const item of content) {
    const rec = record(item);
    const text = typeof rec?.text === "string" ? rec.text : "";
    if (text) parts.push(text);
  }
  const joined = parts.join("\n").trim();
  return joined || undefined;
}

/**
 * 在已启动的 2.x serve 上跑完一轮：建会话 → 订阅 /api/event → 投 prompt →
 * 收到 execution 终态为止。进程的起停仍由调用方负责。
 */
export async function runV2Session(opts: RunV2SessionOpts): Promise<RunV2SessionResult> {
  const agent = new HttpAgent({ keepAlive: true, maxSockets: 8 });
  const abort = new AbortController();
  let timedOut = false;
  let overflow = false;
  let sessionID = "";
  const onOuterAbort = () => abort.abort();
  opts.signal?.addEventListener("abort", onOuterAbort, { once: true });

  let sseReq: ReturnType<typeof httpRequest> | null = null;
  let timer: NodeJS.Timeout | undefined;

  let text = "";
  let reasoningChars = 0;
  let deltaCount = 0;
  let sawDelta = false;
  /** part 快照兜底：deltas 一条都没有时用它给正文。 */
  let partText = "";
  let done = false;
  let failure: string | null = null;
  /**
   * SSE 流在对端被关掉时置位（serve 崩溃 / 被杀 / 端口被重置）。此时 promise 早就
   * resolve 了，reject 是空操作，只能靠标志位收场（M2）。
   */
  let streamClosed = false;
  /** 断流原因（只用于错误措辞：要让人看出是 serve 没了，不是超时）。 */
  let streamCloseReason = "";
  /** `session.step.failed` 的原因：只在「没有任何正文」时用来解释空回答。 */
  let stepFailure = "";

  // --- 过程流（activity）--------------------------------------------------
  // 2.x 把一次工具调用拆成好几个事件（input.started/ended、called、progress、
  // success/failed），而 tracker 认的是 run 模式那条
  // `{type:"tool_use", part:{type:"tool", tool, callID, state}}`（1.x 的 serve 分支
  // 也是把 part 改写成这个形状再喂进去）。这里沿用同一座桥，分类/摘要/截断就都
  // 只有一份实现。
  const activity: ActivityTracker | undefined = opts.onActivity ? createActivityTracker() : undefined;
  /** callID → 工具名。只有 `input.started` 带 name，其余事件都没有。 */
  const toolNames = new Map<string, string>();
  /**
   * 轮内工具序号（从 1 起）。**刻意不复用 tracker 的快照**：tracker 到
   * ACTIVITY_MAX_STEPS(20) 就不再收新步骤，若拿「快照最后一步」判「半句话」，
   * 第 21 个工具起失败就永远判不出来（用户拿到半句话被当成完整回答）。
   */
  let toolSeq = 0;
  const toolSeqs = new Map<string, number>();
  /** 最近一次进入 failed 的工具序号与其 callID（用于 endedOnFailedTool）。 */
  let lastFailedSeq = 0;
  let lastFailedCallID = "";
  /** 该失败步骤在快照里已被截断时的替身（只用于把 label/命令带给上层）。 */
  let failedStepFallback: ActivityStep | null = null;
  /** 最后一个工具之后有没有再出正文：判断「半句话」用（与 1.x 同口径）。 */
  let textAfterLastTool = false;

  const emitActivity = () => {
    if (!activity) return;
    opts.onActivity?.(activity.snapshot());
  };

  /** 首次见到某个 callID 时给它一个序号；新工具出现即意味着上一段正文已收尾。 */
  const noteTool = (callID: string): number => {
    const seen = toolSeqs.get(callID);
    if (seen !== undefined) return seen;
    toolSeq += 1;
    toolSeqs.set(callID, toolSeq);
    textAfterLastTool = false;
    return toolSeq;
  };

  const pushTool = (callID: string, tool: string, state: Record<string, unknown>) => {
    if (!activity || !callID) return;
    const line = JSON.stringify({ type: "tool_use", part: { type: "tool", tool, callID, state } });
    if (activity.push(line)) emitActivity();
  };

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

  /** 计时起点：`opts.timeoutMs` 覆盖「建会话 + 建 SSE + prompt + 收尾」全过程。 */
  const startedAt = Date.now();

  try {
    // 超时从**第一件事**就开始计。原实现把 timer 挂在 SSE 建连成功之后，于是
    // `POST /api/session` / `GET /api/event` 的响应头迟迟不来时整轮没有任何时间
    // 上界 —— 上层没有看门狗，表现是设备端永久「正在思考」（M1）。
    timer = setTimeout(() => {
      timedOut = true;
      abort.abort();
    }, opts.timeoutMs);

    const model = toV2ModelRef(opts.model);
    const created = await v2Request(
      opts.baseUrl,
      "POST",
      "/api/session",
      opts.password,
      { title: "clawmessenger", ...(model ? { model } : {}) },
      agent,
      abort.signal,
    );
    if (created.status !== 200) {
      throw new OpenCodeV2Error(`POST /api/session -> HTTP ${created.status}: ${created.body.slice(0, 200)}`);
    }
    try {
      sessionID = String(nested(JSON.parse(created.body), ["data", "id"]) ?? "");
    } catch {
      // 落到下面的空 id 报错。
    }
    if (!sessionID) throw new OpenCodeV2Error("POST /api/session returned no id");

    // 事件流必须早于 prompt 建立：2.x 的第一条 delta 可能在 prompt 返回前就到。
    await new Promise<void>((resolve, reject) => {
      const url = new URL("/api/event", opts.baseUrl);
      let settled = false;
      const settle = (fn: () => void) => {
        if (settled) return;
        settled = true;
        abort.signal.removeEventListener("abort", onConnectAbort);
        fn();
      };
      /**
       * 建连阶段也必须能被超时打断（M1 的另一半）：响应头迟迟不来时，这个
       * promise 只能靠 destroy 才能落地 —— 光有 `sseReq.on("error")` 不够，
       * 没有任何东西会在超时时去动这个 req。
       */
      const onConnectAbort = () => {
        try {
          sseReq?.destroy(new Error("aborted"));
        } catch {
          // 已经关掉了。
        }
      };
      abort.signal.addEventListener("abort", onConnectAbort, { once: true });
      sseReq = httpRequest(
        {
          hostname: url.hostname,
          port: url.port,
          path: url.pathname,
          method: "GET",
          agent,
          headers: { accept: "text/event-stream", authorization: v2AuthHeader(opts.password) },
        },
        (res) => {
          if ((res.statusCode ?? 0) !== 200) {
            settle(() => reject(new OpenCodeV2Error(`GET /api/event -> HTTP ${res.statusCode}`)));
            return;
          }
          res.setEncoding("utf8");
          /**
           * 对端关掉这条长连接 = serve 挂了/被杀了。promise 早已 resolve（reject
           * 是空操作），只能置标志位让主循环提前收场：否则要空转到 opts.timeoutMs
           * 到点，而且错误会归因成「超时」而不是「serve 没了」（M2）。
           *
           * `done` 守卫是必须的：正常收尾时 cleanup() 里的 destroy() 同样会触发
           * close，没有这道守卫就会把成功的一轮误判成断流。
           */
          const markStreamClosed = (reason: string) => {
            if (done || streamClosed) return;
            streamClosed = true;
            streamCloseReason = reason;
            abort.abort();
          };
          res.on("error", (err) => markStreamClosed(err instanceof Error ? err.message : String(err)));
          res.on("close", () => markStreamClosed("connection closed"));
          let buf = "";
          res.on("data", (chunk: string) => {
            buf += chunk;
            let idx: number;
            while ((idx = buf.search(/\r?\n\r?\n/)) >= 0) {
              const raw = buf.slice(0, idx);
              buf = buf.slice(idx + (buf[idx] === "\r" ? 4 : 2));
              const dataLines: string[] = [];
              for (const line of raw.split(/\r?\n/)) {
                // 2.x 没有 `event:` 字段，类型在 JSON 的 type 上；`: heartbeat` 是注释。
                if (line.startsWith("data:")) dataLines.push(line.slice(5).trim());
              }
              if (dataLines.length === 0) continue;
              let payload: Record<string, unknown> | null;
              try {
                payload = record(JSON.parse(dataLines.join("\n")));
              } catch {
                continue;
              }
              if (!payload) continue;
              const type = typeof payload.type === "string" ? payload.type : "";
              const data = record(payload.data);
              // 只处理本会话：/api/event 是全机的，可能同时有别的会话在跑。
              const eventSession = typeof data?.sessionID === "string" ? data.sessionID : "";
              if (eventSession && eventSession !== sessionID) continue;
              // 终态/失败类必须能确认是本会话的：缺 sessionID 就丢掉。正文/过程类
              // 保持宽松（`message.part.updated` 的 sessionID 挂在 data.part 上，
              // 收紧会误杀兜底正文）。
              if (!eventSession && isTurnTerminalEvent(type)) continue;

              if (type === "session.text.delta") {
                const delta = typeof data?.delta === "string" ? data.delta : "";
                if (!delta) continue;
                sawDelta = true;
                deltaCount += 1;
                text += delta;
                opts.onText?.(delta);
                // 工具之后的正文：这一轮有收尾，不能算「半句话」。
                if (toolSeq > 0) textAfterLastTool = true;
                if (!overflow && opts.maxTextChars !== undefined && text.length > opts.maxTextChars) {
                  overflow = true;
                  abort.abort();
                }
              } else if (type === "session.reasoning.delta") {
                const delta = typeof data?.delta === "string" ? data.delta : "";
                if (!delta) continue;
                reasoningChars += delta.length;
                opts.onReasoning?.(delta);
              } else if (type === "session.tool.input.started") {
                // 工具名在这里，且只在这里出现。
                const callID = typeof data?.id === "string" ? data.id : "";
                const name = typeof data?.name === "string" && data.name ? data.name : "tool";
                if (!callID) continue;
                toolNames.set(callID, name);
                noteTool(callID);
                pushTool(callID, name, { status: "running" });
              } else if (type === "session.tool.input.ended" || type === "session.tool.called") {
                // called 给的是已解析的对象；input.ended 给的是**整段 JSON 字符串**。
                const callID = typeof data?.id === "string" ? data.id : "";
                if (!callID) continue;
                let input: unknown = data?.input;
                if (input === undefined && typeof data?.text === "string") {
                  try {
                    input = JSON.parse(data.text);
                  } catch {
                    input = data.text;
                  }
                }
                noteTool(callID);
                pushTool(callID, toolNames.get(callID) ?? "tool", { status: "running", input });
              } else if (type === "session.tool.progress") {
                // 执行中（shell 已起、还没退出）。不改状态：终态由 success/failed
                // 决定，重复上报 running 只会让 upsert 白算一遍。
                const callID = typeof data?.id === "string" ? data.id : "";
                if (callID) noteTool(callID);
                continue;
              } else if (type === "session.tool.success") {
                const callID = typeof data?.id === "string" ? data.id : "";
                if (!callID) continue;
                const seq = noteTool(callID);
                // 同一个 callID 失败后又成功（重试）：失败不再算「收尾在半句话上」。
                if (seq === lastFailedSeq) lastFailedSeq = 0;
                pushTool(callID, toolNames.get(callID) ?? "tool", {
                  status: "completed",
                  output: toolOutputText(data?.content),
                });
              } else if (type === "session.tool.failed") {
                const callID = typeof data?.id === "string" ? data.id : "";
                if (!callID) continue;
                const name = toolNames.get(callID) ?? "tool";
                const reason = errorMessage(payload) || "tool failed";
                // 失败序号自己记（不读 tracker 快照）：快照到 20 步就截断，靠它判
                // 「半句话」会把工具密集轮次的失败收尾漏掉（m4）。
                lastFailedSeq = noteTool(callID);
                lastFailedCallID = callID;
                // 失败原因在 data.error.message（实测 read 找不到文件就是这个形状）。
                pushTool(callID, name, { status: "error", output: reason });
                // 快照里可能已经没有这一步（被 ACTIVITY_MAX_STEPS 截断），留个替身
                // 好把 label / 失败原因交给上层。
                const cls = classifyTool(name);
                failedStepFallback = {
                  id: callID,
                  kind: cls.kind,
                  label: cls.label,
                  status: "failed",
                  outputDetail: reason.slice(0, 200),
                  endedAt: Date.now(),
                };
              } else if (type === "session.step.failed") {
                // 模型调用失败（**不是**工具失败）：工具步骤各自收过尾了，这里只
                // 留原因。不立刻 done —— opencode 之后还会发 execution.* 终态，
                // 而且它可能重试；抢跑会把重试中的一轮误判成失败。
                stepFailure = errorMessage(payload) || "opencode session.step.failed";
                if (activity?.finish()) emitActivity();
              } else if (type === "message.part.updated") {
                // 兜底用：part.type === "text" 时取整段。
                const part = record(data?.part);
                if (part?.type === "text" && typeof part.text === "string") partText = part.text;
              } else if (type === "session.execution.succeeded" || type === "session.execution.interrupted") {
                // 收尾：悬着的 running 步骤就地转 done，别让用户一直看到「执行中」。
                if (activity?.finish()) emitActivity();
                done = true;
              } else if (type === "session.execution.failed" || type === "session.error") {
                failure = errorMessage(payload) || `opencode ${type}`;
                if (activity?.finish()) emitActivity();
                done = true;
              }
            }
          });
          settle(() => resolve());
        },
      );
      sseReq.on("error", (err) => settle(() => reject(err)));
      sseReq.end();
      // 极小窗口：timer 在建会话之后、这一行之前到点。abort 已经发生过，监听器
      // 不会补发 —— 不主动收一次，建连就会永远等下去（M1 的边角）。
      if (abort.signal.aborted) onConnectAbort();
    });

    // prompt 是异步的：实测 200 立刻返回（返回体是 inbox 条目），真正的工作在
    // /api/event 上。所以这里只校验请求被接受，然后等执行终态。
    const prompted = await v2Request(
      opts.baseUrl,
      "POST",
      `/api/session/${encodeURIComponent(sessionID)}/prompt`,
      opts.password,
      { text: opts.prompt },
      agent,
      abort.signal,
    );
    if (overflow) {
      throw new OpenCodeV2Error(`opencode reply exceeded ${opts.maxTextChars} chars`, true);
    }
    if (prompted.status !== 200) {
      throw new OpenCodeV2Error(
        `POST /api/session/${sessionID}/prompt -> HTTP ${prompted.status}: ${prompted.body.slice(0, 400)}`,
      );
    }

    // 双保险：deadline 与上面的 timer 同一起点、同一时长，哪怕 timer 被什么
    // 东西清掉了（或被 fake timer 吃掉）这里仍会收场。
    const deadline = startedAt + opts.timeoutMs;
    while (!done) {
      if (abort.signal.aborted) break;
      if (streamClosed) break;
      if (Date.now() > deadline) {
        timedOut = true;
        abort.abort();
        break;
      }
      await new Promise((r) => setTimeout(r, 20));
    }

    // 终态帧之后可能还有几帧尾巴在管道里。
    if (done && !overflow) {
      await new Promise((r) => setTimeout(r, SETTLE_GRACE_MS));
    }

    if (overflow) {
      throw new OpenCodeV2Error(`opencode reply exceeded ${opts.maxTextChars} chars`, true);
    }
    /**
     * 主循环还有另一个出口：`abort.signal.aborted`（timer 到点与外部中止都从这里
     * 出去，因为 timer 的处理器本身也是 `abort.abort()`）。那个出口**不抛**，所以
     * 不算这里一道就会把半句话按成功交上去 —— `catch` 里的 timedOut/aborted 判断
     * 根本走不到，因为压根没有异常。顺序与 `catch` 里保持一致。
     */
    if (!done && timedOut) throw new Error(`agent turn timeout after ${opts.timeoutMs}ms`);
    if (!done && opts.signal?.aborted) throw new Error("agent turn aborted");
    // 断流：serve 崩了/被杀，工作没有真正结束。措辞要与「超时」区分开 ——
    // 上层据此才知道该看 serve 的死活，而不是「模型太慢」。
    if (streamClosed && !done) {
      throw new OpenCodeV2Error(
        `opencode event stream closed unexpectedly before the turn finished`
          + ` (${streamCloseReason || "serve exited or was killed"})`,
      );
    }
    if (failure) throw new OpenCodeV2Error(`opencode session failed: ${failure}`);

    const finalText = text.trim().length > 0 ? text : partText;
    // 一个字都没有、但某次模型调用失败过：把原因抛出来，别把「空回答」当成
    // 正常结果交给上层（设备端看到的就是「提示失败、消息是空的」）。
    if (!finalText.trim() && stepFailure) {
      throw new OpenCodeV2Error(`opencode produced no text: ${stepFailure}`);
    }

    // 「半句话」判据用自己记的工具序号，不读快照末步（快照到 20 步会截断）。
    const steps = activity?.snapshot() ?? [];
    const endedOnFailedTool = lastFailedSeq > 0 && lastFailedSeq === toolSeq && !textAfterLastTool;
    const failedStep = endedOnFailedTool
      ? steps.find((step) => step.id === lastFailedCallID) ?? failedStepFallback
      : null;

    return {
      text: finalText,
      sawDelta,
      deltaCount,
      reasoningChars,
      endedOnFailedTool,
      failedStep,
    };
  } catch (err) {
    // 溢出要排在超时/中止之前：溢出自己也会 abort()，否则会被吞成裸 `aborted`
    // （classifyTurnError 认不出，用户看到「生成失败」而不是「超出上限」）。
    if (overflow) {
      throw new OpenCodeV2Error(`opencode reply exceeded ${opts.maxTextChars} chars`, true);
    }
    if (timedOut) throw new Error(`agent turn timeout after ${opts.timeoutMs}ms`);
    if (opts.signal?.aborted) throw new Error("agent turn aborted");
    // 断流也会 abort() 在途请求，让它以 Error("aborted") 冒到这里。
    if (streamClosed && !done) {
      throw new OpenCodeV2Error(
        `opencode event stream closed unexpectedly before the turn finished`
          + ` (${streamCloseReason || "serve exited or was killed"})`,
      );
    }
    throw err;
  } finally {
    // 没等到终态就收场（超时 / 溢出 / 外部中止）：显式打断会话。2.x 的执行跑在
    // serve 里，光断 SSE 不会让它停下；调用方随后会杀掉整棵进程树，这里是先礼。
    // `.catch(() => {})` 是显式的：这里是 `void` 调用，将来内部若去掉 catch，
    // 不该变成一个没人处理的 unhandled rejection。
    if (!done && sessionID) void interruptV2Session(opts.baseUrl, sessionID, opts.password).catch(() => {});
    cleanup();
  }
}

/**
 * 中止一轮。2.x 的 `run` / 会话都是瘦客户端，杀进程停不住后台执行，必须显式
 * 调 `/api/session/{id}/interrupt`（1.x 叫 `abort`）。
 */
export async function interruptV2Session(
  baseUrl: string,
  sessionID: string,
  password: string,
): Promise<void> {
  if (!sessionID) return;
  const agent = new HttpAgent({ keepAlive: false });
  try {
    await v2Request(
      baseUrl,
      "POST",
      `/api/session/${encodeURIComponent(sessionID)}/interrupt`,
      password,
      undefined,
      agent,
      undefined,
      // 上界：调用点在 finally 里是 `void` 的，socket 若不关就会拖住事件循环。
      INTERRUPT_TIMEOUT_MS,
    );
  } catch {
    // 进程可能已经没了；中止的语义是「尽量」，失败不该盖住真正的原因。
  } finally {
    agent.destroy();
  }
}
