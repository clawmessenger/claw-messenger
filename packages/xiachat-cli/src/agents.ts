import { spawn } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { access, constants } from "node:fs/promises";
import { homedir } from "node:os";
import { delimiter, dirname, join } from "node:path";
import { StringDecoder } from "node:string_decoder";

import {
  type ActivityStep,
  classifyTool,
  createActivityTracker,
} from "./agent-activity.js";
import { ensureCodexBehaviorPrompt, ensureCodexOutputStyle } from "./agent-instructions.js";
import {
  fsGateExempt,
  fsRestriction,
  loadPermissionSnapshot,
  opencodeBehaviorPath,
  sanitizeAgentId,
  writeOpencodeBehaviorFile,
  type PermissionSnapshot,
} from "./agent-permissions.js";
import {
  buildDeviceOpencodeConfig,
  discoverOpencodeConfigPath,
  readAgentConfig,
  type AgentConfig,
} from "./model-providers.js";
import {
  isServeStartupFailure,
  parseServeListeningUrl,
  parseServePassword,
  runServerSession,
} from "./opencode-server.js";
import { TurnIncompleteError } from "./turn-error.js";

// Mirrors knownAgentCLIs in server/internal/integrations/rongcloud/
// discussion_bridge.go — keep both lists in sync.
export const KNOWN_AGENT_CLIS: readonly string[] = [
  "claude", "codex", "opencode", "codebuddy", "codearts", "deveco",
  "openclaw", "hermes", "pi", "omp", "cursor-agent", "kimi",
  "reasonix", "dsh", "kiro-cli", "agy", "qodercli", "qoderclicn",
  "traecli", "grok", "qwen", "qwenpaw", "mcode", "dim", "zeroclaw",
];

export interface AgentInfo {
  name: string;
  path: string;
}

export interface RunAgentTurnOpts {
  execPath: string;
  // Agent CLI name (as in KNOWN_AGENT_CLIS); selects the invocation
  // strategy. When omitted, the prompt always goes via stdin.
  agentName?: string;
  prompt: string;
  model?: string;
  timeoutMs?: number;
  // 原始缓冲上限（只影响「留不留原始 stdout」，不影响解析）。
  maxOutputBytes?: number;
  // 真止损：读到这个量级就掐进程并以 output_truncated 失败收尾。
  // 默认 HARD_OUTPUT_BYTES；测试用它把阈值调到可复现的规模。
  hardOutputBytes?: number;
  // Working directory for the agent CLI process. The ops agent sets this to
  // its workdir so `opencode run` picks up the ops AGENTS.md system prompt.
  cwd?: string;
  onChunk?: (chunk: string) => void;
  // 思考流增量（可选）：claude/codex 的 stream-json 模式下，agent 的
  // thinking/reasoning 过程文本与正文分道回调。不支持思考流的 agent
  // 不调用它，调用方天然降级为纯正文流。
  onReasoningChunk?: (chunk: string) => void;
  // 过程流（可选）：agent 本轮的工具调用步骤。回调的是**全量快照**而非增量，
  // 与 text/reasoning 同语义 —— 调用方只管存最新的那份。仅 stream-json 型
  // agent（claude/codex）会产生，其余 agent 一次都不回调。
  onActivity?: (steps: ActivityStep[]) => void;
  signal?: AbortSignal;
  // 会话隔离键（可选）。多个会话形态共用同一个 agent 二进制，而 agent CLI
  // 默认把它们全塞进自己的「主会话」：openclaw 的默认键就是 agent:main:main，
  // 于是讨论组、普通群聊、私聊共享一串上下文。讨论组里达成过的共识（例如
  // 「没有主题就回『等待主题发布』占位」）会被后续群聊继承，群聊于是答非
  // 所问。支持显式会话键的 agent（当前只有 openclaw 的 --session-key）按
  // 「会话形态 + 会话 id」分桶；不支持的 agent 忽略此参数。
  sessionKey?: string;
  /**
   * 本轮要注入的**行为规范**（仅私聊非空）。
   *
   * 群聊/讨论组/聊天室的行为准则由服务端拼进 role_prompt，设备端不能再注入一遍；
   * 私聊没有服务端注入点，所以只有私聊轮次会带上它。落在**指令层**（codex 的
   * AGENTS.md 标记块 / opencode 的 instructions 文件），绝不拼进用户消息 ——
   * 见 agent-instructions.ts 顶部的设计说明。
   */
  behaviorPrompt?: string;
  /**
   * 内部用：本轮是否处于「文件读写禁止」限制态。由 runAgentTurn 从权限快照算出，
   * 递归回退（disableServerMode）时沿用同一个值。
   */
  fsRestricted?: boolean;
  // 内部用：serve 模式起不来时退回 run --format json，递归进来时置 true，
  // 避免二次分流。外部调用方不该传。
  disableServerMode?: boolean;
}

// Agents known to reject or hang on stdin-mode invocation get an argv
// strategy: the prompt rides as a CLI argument instead of stdin.
//   claude:   print mode (-p), non-interactive
//   codex:    `exec` subcommand (stdin-mode invocation is rejected)
//   opencode: `run` subcommand (stdin-mode invocation hangs)
//   openclaw: `agent --local --json -m` one-shot message; --json emits a
//             clean result envelope (plain stdout interleaves ANSI-colored
//             log lines with the answer — see parseOpenclawJsonOutput)
//   hermes:   `-z` one-shot prompt (stdin-mode unsupported)
// Keep in sync with agentArgvArgs in server discussion_bridge.go.
export interface AgentArgvContext {
  /** 本轮处于「文件读写禁止」限制态（见 agent-permissions.ts fsRestriction）。 */
  fsRestricted?: boolean;
}
type AgentArgvStrategy = (
  prompt: string,
  model?: string,
  sessionKey?: string,
  ctx?: AgentArgvContext,
) => string[];

// The web stores a model as a `provider/model` route. Most agent CLIs take
// that verbatim, but two split it:
//   codex  — `-m` takes a bare slug; the provider comes from config, so the
//            route's provider is applied via `-c model_provider=<id>`.
//            (`-m quukk_direct/glm-5.2` fails with model_not_found.)
//   hermes — `-m <slug> --provider <id>`.
// Verified against codex 0.x and hermes with the 127.0.0.1:49830 gateway.
const MODEL_ROUTE_RE = /^([A-Za-z0-9][A-Za-z0-9._-]{0,127})\/([A-Za-z0-9][A-Za-z0-9._:-]{0,127})$/;

export function splitModelRoute(route: string): { provider: string; model: string } | null {
  const match = MODEL_ROUTE_RE.exec(route);
  return match ? { provider: match[1], model: match[2] } : null;
}

// `agentArgvStrategies` 在本文件里定义于 buildModelArgs **之后**。它是 const，
// 直接写 `agentArgvStrategies[name]` 在可读性上像 TDZ 陷阱（实际调用发生在模块
// 初始化完成后，是安全的），所以统一经这个惰性函数判断：谁在前谁在后都不影响。
function agentHasArgvStrategy(name: string): boolean {
  return agentArgvStrategies[name] !== undefined;
}

/** Renders the model-selection flags for one agent. */
export function buildModelArgs(name: string, model: string | undefined): string[] {
  // codebuddy 的 `--model` 只认它自己的模型 id（`custom-local:<m>` 或云端托管
  // 模型）；传裸模型名会直接要求登录，传自定义网关的 slug 则无意义 —— 它的
  // 模型选择必须走 `CODEBUDDY_MODEL` 环境变量（见 agentRuntimeEnv）。这里只留
  // `--port 0`：不给它固定端口，多个 agent 实例并发时会撞 EADDRINUSE。
  if (name === "codebuddy") return ["--port", "0"];
  // kimi 的 `-m` 只认自己的模型别名（`kimi-code/kimi-for-coding` 这类），
  // 我们的 `provider/model` 路由会被它当成未知模型拒绝。模型选择交给它自己的
  // 配置（`default_model`），这里不注入。
  if (name === "kimi") return [];
  if (!model) return [];
  const route = splitModelRoute(model);
  // qwen 走 OpenAI 兼容网关，`--model` 只接受上游的裸模型名（`quukk/glm-5.2`
  // 会被它当成未知模型名发给上游）。
  //
  // 它必须留在下面那道「无策略不注入」的守卫**之前**：qwen 在设备端没有 argv
  // 策略（提示词走 stdin），但 `--model` 是它确实支持的参数，守卫先行会把
  // stdin 模式下选好的模型静默丢掉。
  if (name === "qwen") {
    return route ? ["--model", route.model] : ["--model", model];
  }
  // 没有 argv 策略的 agent 会退化成「裸进程名」（buildAgentArgs 返回 []），给它
  // 追加 `--model` 只会让不认这个参数的 CLI 更早退出（opencode 未知参数就是
  // usage dump + exit 1）。模型选择对这些 agent 本来就无效，不要注入。
  if (!agentHasArgvStrategy(name)) return [];
  if (name === "codex") {
    return route
      ? ["-c", `model_provider=${route.provider}`, "-m", route.model]
      : ["-m", model];
  }
  if (name === "hermes") {
    return route ? ["-m", route.model, "--provider", route.provider] : ["-m", model];
  }
  return ["--model", model];
}

// Subprocess environment overrides per agent. Returns the full env to hand to
// spawn() (the caller's env already merged in), or `null` when the agent needs
// no change and should simply inherit.
//
// codebuddy is the only one today: its CLI dropped ANTHROPIC_BASE_URL support
// and only honours the self-hosted-gateway switches `CODEBUDDY_BASE_URL` /
// `CODEBUDDY_AUTH_TOKEN` / `CODEBUDDY_MODEL` (OpenAI-compatible
// `/v1/chat/completions`, verified against quukk). Without these it falls back
// to the WorkBuddy-hosted channel, which answers `Authentication required`
// or 429 weekly-quota once the account limit is hit.
export function agentRuntimeEnv(
  agentName: string | undefined,
  model: string | undefined,
  opts: { env?: NodeJS.ProcessEnv; config?: AgentConfig | null } = {},
): NodeJS.ProcessEnv | null {
  if (agentName !== "codebuddy") return null;
  const config = opts.config === undefined ? readAgentConfig("codebuddy") : opts.config;
  const endpoint = config?.endpoints[0];
  if (!endpoint) return null;
  const baseEnv = opts.env ?? process.env;
  const route = model ? splitModelRoute(model) : null;
  const chosen = route?.model
    ?? (config?.defaultModel ? splitModelRoute(config.defaultModel)?.model : undefined)
    ?? model;
  const env: NodeJS.ProcessEnv = {
    ...baseEnv,
    CODEBUDDY_BASE_URL: endpoint.baseUrl,
    ...(endpoint.apiKey ? { CODEBUDDY_AUTH_TOKEN: endpoint.apiKey } : {}),
    ...(chosen ? { CODEBUDDY_MODEL: chosen } : {}),
  };
  return env;
}

// 结构化事件流可用性：这些 agent 都能以「逐行 JSON 事件」模式运行，正文增量
// 与工具步骤可被解析（见 parseStreamJsonLine / agent-activity.ts）。claude /
// codex / opencode 原生上报 thinking/reasoning；hermes 上游的 stream_json.py
// 只把 text 与 tool_use/tool_result 写进协议（-q 静默模式还会把 reasoning
// 回调置空），需要跑 scripts/patch-hermes-reasoning.py 把思考接进来 ——
// 没打补丁时它只贡献「逐字正文 + 工具过程」。
// 其余 agent（stdin 模式、或只吐一次性 JSON 信封如 openclaw）不在此列；
// openclaw 的信封里没有 reasoning payload（实测 2026.9.2），只有 toolSummary。
const STREAM_JSON_AGENTS = new Set(["claude", "codex", "opencode", "hermes"]);

export function agentSupportsReasoning(name: string | undefined): boolean {
  return name !== undefined && STREAM_JSON_AGENTS.has(name);
}

const agentArgvStrategies: Readonly<Record<string, AgentArgvStrategy>> = {
  // kimi — `-p/--prompt` 是官方唯一「不打开 TUI」的非交互入口（kimi Command
  // 参考原文："Run a single prompt non-interactively and stream the Assistant
  // output to stdout. This mode does not open the TUI"）。
  //
  // ⚠️ 这条策略是必须的，不是优化：过去 kimi 没有策略 → buildAgentArgs 返回 []
  // → 实际 spawn 的是**裸 `kimi`**（交互式 TUI）。TUI 在 Windows 上拿不到
  // console 时直接退出，stderr 只有一句
  // `No Windows console found. Are you running cmd.exe?`（生产事故实证，
  // 设备端由 VBS 自启、没有 console，所以 100% 复现）。
  //
  // `--output-format` 只能与 `-p` 同用，取值 text / stream-json。这里先用
  // text：stdout 即正文（thinking 与工具过程走 stderr），不必为此新增一个
  // JSONL 解析器——opencode 的教训就是「拿到 JSON 却解析不了」等于没输出。
  //
  // 不注入 --model：kimi 的 `-m` 只认它自己的模型别名（如
  // kimi-code/kimi-for-coding），我们这里的 `provider/model` 路由会被它拒绝。
  kimi: (prompt) => ["-p", prompt, "--output-format", "text"],
  claude: (prompt, model) => [
    "-p", prompt,
    // stream-json：逐行 JSON 事件流（thinking_delta / text_delta 可解析，
    // 同时是思考流数据源）。--verbose 是 stream-json 的前置要求。
    "--output-format", "stream-json", "--verbose",
    ...buildModelArgs("claude", model),
  ],
  codex: (prompt, model, _sessionKey, ctx) => [
    "exec", prompt,
    // --json：JSONL 事件流（reasoning.item_start/delta 等可解析）。实测
    // codex 0.144：未信任目录需 --skip-git-repo-check；stdin 模式会挂起
    // 等输入，必须保持 argv 模式（runAgentTurn argvMode 下 stdin 已 end）。
    "--json", "--skip-git-repo-check",
    // 文件读写禁止：codex 的 `-s/--sandbox read-only` 是**官方**沙箱策略
    // （0.144.6 的 `codex exec --help` 实测：read-only / workspace-write /
    // danger-full-access）。read-only 挡写入与联网，但**仍允许读取** ——
    // 这是 codex 侧能做到的最强限制，做不到「连读都不许」，报告里如实写了。
    ...(ctx?.fsRestricted ? ["--sandbox", "read-only"] : []),
    ...buildModelArgs("codex", model),
  ],
  // --format json --thinking：opencode 1.19 起支持逐行 JSON 事件（reasoning /
  // tool_use / text / step_start / step_finish），是思考流与过程流的数据源。
  // 不传这两个参数时它只输出「格式化好的纯文本」，思考与工具步骤全都拿不到
  // ——私聊里「AI 答了但看不到它想了什么、调了什么」就是这么来的。
  // ⚠️ `--format` 是 1.19 才有的参数；更老的 opencode 收到未知参数会直接退出。
  opencode: (prompt, model) => [
    "run", prompt,
    "--format", "json", "--thinking",
    ...buildModelArgs("opencode", model),
  ],
  // --session-key 把这一轮绑到「会话形态 + 会话 id」的独立会话上。不带它时
  // openclaw 一律走默认主会话（agent:main:main），群聊会被讨论组的上下文
  // 带跑（见 RunAgentTurnOpts.sessionKey）。
  openclaw: (prompt, model, sessionKey) => [
    "agent", "--local", "--json", "-m", prompt,
    ...(sessionKey ? ["--session-key", sessionKey] : []),
    ...buildModelArgs("openclaw", model),
  ],
  // hermes chat -q … --format stream-json：1.0+ 的 one-shot 结构化事件流
  // （system/init → text 增量 / tool_use / tool_result → result）。旧的
  // `-z <prompt>` 只有「最后一坨正文」，工具过程与逐字上屏都拿不到，群聊里
  // 就只能等整包落地。--oneshot 保证非 TTY 下也是「答完即退」。
  hermes: (prompt, model) => [
    "chat", "-q", prompt, "--oneshot", "--format", "stream-json",
    ...buildModelArgs("hermes", model),
  ],
};

// Windows CreateProcess caps the whole command line near 32k chars (and
// .cmd wrappers shrink that further), so very long prompts cannot ride as
// argv — past this cap every agent falls back to stdin mode regardless of
// strategy.
//
// Exported because the html_revise path (im.ts) has to decide between
// inlining the previous page and handing over a file path: it must use
// exactly this threshold, or the prompt it builds still lands on the stdin
// fallback that codex/opencode/hermes reject or hang on.
export const ARGV_PROMPT_MAX_CHARS = 8000;

// Returns the argv for an agent turn; an empty array means stdin mode
// (unknown agent, or a prompt too long for argv).
export function buildAgentArgs(
  name: string,
  prompt: string,
  model?: string,
  sessionKey?: string,
  ctx?: AgentArgvContext,
): string[] {
  const strategy = agentArgvStrategies[name];
  if (!strategy || prompt.length > ARGV_PROMPT_MAX_CHARS) return [];
  return strategy(prompt, model, sessionKey, ctx);
}

const DEFAULT_TIMEOUT_MS = 120_000;
const DEFAULT_MAX_OUTPUT_BYTES = 65_536;
// stream-json（claude/codex/opencode/hermes）的事件流含元数据行与思考流，同回答
// 体积约为纯文本 2-3 倍；opencode 还是「整段 part 全文」重复上报，一次带工具的
// 长回合轻松几十万字节。
//
// ⚠️ 这个上限现在只是**原始缓冲**的上限，不再是「这一轮到此为止」：超出后我们
// 停止攒原始字节，但**继续解析**事件流（正文照样上屏，最终正文由解析累积给出）。
// 过去在超限处直接 return，工具输出一洪峰，后半轮（含最终回答）就整段丢了，
// 前端却拿到一个 completed 终帧 —— 用户看到的就是「话说一半就结束了」。
const STREAM_JSON_MAX_OUTPUT_BYTES = 512 * 1024;
// openclaw 这类「一次性 JSON 信封」必须整份留存才能解析，没有流式正文可退，
// 所以给足额度（以前的 64KB 连一份带思考 payload 的正常回答都装不下）。
const ENVELOPE_MAX_OUTPUT_BYTES = 2 * 1024 * 1024;
// 真止损：读到这个量级说明 agent 在喷日志/输出结果，这一轮不可能再有用，
// 直接掐进程并以 output_truncated 失败收尾（绝不假装答完）。
const HARD_OUTPUT_BYTES = 16 * 1024 * 1024;

function isWindowsScript(execPath: string): boolean {
  return process.platform === "win32" && /\.(cmd|bat)$/i.test(execPath);
}

// npm 生成的 .cmd/.bat shim 只是个转发器：openclaw.cmd → node openclaw.mjs，
// opencode.cmd → node_modules/opencode-ai/bin/opencode.exe。
//
// 为什么要穿透它：Node ≥18.20/20.12 拒绝直接 spawn .cmd（CVE-2024-27980），
// 于是命令行必须经 cmd.exe，而 cmd.exe 把命令里的**换行当命令分隔符**，还会
// 解释 & | ^ % 等元字符。2026-10-14 实测（openclaw 的 transcript_events 里存的
// 提示词只剩第一行）后果是：
//   · 多行提示词只到第一行就断 —— 群聊/讨论的 your_turn 因此只带着
//     `[discussion] round=1 speaking_order=1` 这一行进了 agent，用户的问题、
//     角色人设全部丢失，agent 只能回「等待主题发布」；
//   · 断点之后的参数一起丢 —— 包括 --session-key、--model，于是会话隔离失效；
//   · 用户正文里的 & 会被 cmd 当成命令分隔符。
// 所以能解析出真实目标时直接 spawn 它，shell 完全不经手。
export function resolveWindowsShim(
  execPath: string,
): { file: string; prefixArgs: string[] } | null {
  const cached = shimTargetCache.get(execPath);
  if (cached !== undefined) return cached;
  const resolved = parseWindowsShim(execPath);
  shimTargetCache.set(execPath, resolved);
  return resolved;
}

const shimTargetCache = new Map<string, { file: string; prefixArgs: string[] } | null>();

function parseWindowsShim(execPath: string): { file: string; prefixArgs: string[] } | null {
  let text: string;
  try {
    text = readFileSync(execPath, "utf8");
  } catch {
    return null;
  }
  const dir = dirname(execPath);
  // shim 末行一定是 `"<目标>" %*`。取**最后一个**带可执行扩展名的引号路径，
  // 前面探测用的 "%dp0%\node.exe" 与 "%_prog%" 因此不会被误取。
  const matches = [...text.matchAll(/"([^"\r\n]+\.(?:exe|mjs|cjs|js))"/gi)];
  const raw = matches.at(-1)?.[1];
  if (!raw) return null;
  const target = raw.replace(/%~?dp0%?\\?/gi, dir.endsWith("\\") ? dir : `${dir}\\`);
  if (!existsSync(target)) return null;
  if (/\.exe$/i.test(target)) return { file: target, prefixArgs: [] };
  // 脚本目标要用 node 跑：优先 shim 同目录的 node.exe（nvm 的布局），否则交给
  // PATH 上的 node。**不能用 process.execPath** —— 那是本 CLI 自己的 SEA 二进制。
  const nodeExe = join(dir, "node.exe");
  return { file: existsSync(nodeExe) ? nodeExe : "node", prefixArgs: [target] };
}

// 兜底（shim 解析不出来时仍走 cmd.exe）：把参数里的换行抹掉 —— cmd 会把换行
// 当命令分隔符，一条多行提示词只到第一行就断。
function sanitizeForCmdArgs(args: string[]): string[] {
  return args.map((arg) => arg.replace(/\r?\n/g, "⏎"));
}

// Node >= 18.20/20.12 refuses to spawn .cmd/.bat directly (CVE-2024-27980),
// so those go through cmd.exe like a shell would — unless the shim's real
// target can be resolved, which we prefer for the reasons above. Everything
// else spawns directly, POSIX included.
export function buildSpawnInvocation(
  execPath: string,
  args: string[],
): { file: string; args: string[] } {
  if (isWindowsScript(execPath)) {
    const real = resolveWindowsShim(execPath);
    if (real) return { file: real.file, args: [...real.prefixArgs, ...args] };
    return { file: "cmd.exe", args: ["/d", "/s", "/c", execPath, ...sanitizeForCmdArgs(args)] };
  }
  return { file: execPath, args };
}

// On Windows, child.kill() only terminates the immediate wrapper process;
// the agent lives in a descendant. taskkill /T tears down the whole tree.
function killProcessTree(child: { pid?: number; kill: () => void }): void {
  if (process.platform === "win32" && child.pid !== undefined) {
    try {
      spawn("taskkill", ["/pid", String(child.pid), "/T", "/F"], { windowsHide: true });
      return;
    } catch {
      // fall through to child.kill()
    }
  }
  child.kill();
}

// Drops trailing incomplete UTF-8 sequences (lead byte without its
// continuation bytes) so a byte-cap cut never decodes to U+FFFD. Exported
// for direct unit testing (pure function).
export function trimIncompleteUtf8Tail(buf: Buffer): Buffer {
  // Scan back over trailing continuation bytes (max 3) to find the last
  // sequence's lead byte; cut before that lead if the sequence is
  // incomplete within the buffer.
  const end = buf.length;
  for (let i = 1; i <= Math.min(4, end); i++) {
    const byte = buf[end - i];
    if ((byte & 0x80) === 0) {
      // ASCII: the sequence before it is complete; nothing to trim.
      return buf;
    }
    if ((byte & 0xe0) === 0xc0 || (byte & 0xf0) === 0xe0 || (byte & 0xf8) === 0xf0) {
      // Lead byte i-1 bytes from the end; incomplete → cut before the lead.
      const need = utf8SequenceLength(byte);
      return i < need ? buf.subarray(0, end - i) : buf;
    }
    // Continuation byte (0b10xxxxxx): keep walking back.
  }
  // Walked past 4 bytes with no lead byte: not valid UTF-8; drop the
  // trailing continuation run.
  return buf.subarray(0, Math.max(0, end - 4));
}

function utf8SequenceLength(lead: number): number {
  if ((lead & 0xe0) === 0xc0) return 2;
  if ((lead & 0xf0) === 0xe0) return 3;
  if ((lead & 0xf8) === 0xf0) return 4;
  return 1;
}

// --- openclaw output extraction -------------------------------------------
// `openclaw agent --local` without --json interleaves ANSI-colored log
// lines ([model-fetch] start …, [agent] run … ended with stopReason=stop)
// with the answer on stdout. With --json (preferred) it emits one clean
// envelope instead. These helpers turn either shape into the plain reply.

// ANSI CSI/SGR escape sequences (colors, cursor moves) — openclaw colors
// its log lines even when piped.
// eslint-disable-next-line no-control-regex -- 这里匹配的就是 ESC/BEL 控制符本身
const ANSI_ESCAPE_RE = /\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))/g;

export function stripAnsiCodes(text: string): string {
  return text.replace(ANSI_ESCAPE_RE, "");
}

// Log lines look like `[model-fetch] start …` / `[agents/agent-command] …`
// — a leading bracket tag of word chars, dashes and slashes, NOT followed
// by `(` (markdown links `[label](url)` must survive).
const OPENCLAW_LOG_LINE_RE = /^\[[A-Za-z0-9_\-/]+\](?!\()/;

export function normalizeOpenclawOutput(raw: string): string {
  const lines = stripAnsiCodes(raw).split("\n");
  const kept = lines.filter((line) => !OPENCLAW_LOG_LINE_RE.test(line.trimStart()));
  return kept.join("\n").replace(/\n{3,}/g, "\n\n").trim();
}

// Parses the `openclaw agent --json` result envelope. Returns the reply
// text, or undefined when the output is not a recognizable envelope.
export function parseOpenclawJsonOutput(raw: string): string | undefined {
  const text = stripAnsiCodes(raw).trim();
  if (!text.startsWith("{")) return undefined;
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (typeof parsed !== "object" || parsed === null) return undefined;
  const envelope = parsed as {
    payloads?: unknown;
    meta?: { finalAssistantVisibleText?: unknown } | null;
  };
  if (Array.isArray(envelope.payloads)) {
    const texts = envelope.payloads
      // 思考（isReasoning）与错误（isError）payload 不是正文：混进气泡就是
      // 把模型的内心独白当成回答。它们走 extractOpenclawEnvelopeDetails。
      .filter((p) => !(typeof p === "object" && p !== null
        && ((p as { isReasoning?: unknown }).isReasoning === true
          || (p as { isError?: unknown }).isError === true)))
      .map((p) => (typeof p === "object" && p !== null ? (p as { text?: unknown }).text : undefined))
      .filter((t): t is string => typeof t === "string" && t.length > 0);
    if (texts.length > 0) return texts.join("\n");
  }
  const visible = envelope.meta?.finalAssistantVisibleText;
  if (typeof visible === "string" && visible.length > 0) return visible;
  return undefined;
}

/**
 * openclaw 信封里的「这一轮到底有没有正常收尾」。
 *
 * 只看信封自己给出的权威字段，不做文案猜测：
 *   - meta.aborted === true            → 运行时把这轮掐了；
 *   - completion.finishReason（缺省取 stopReason）为 length / max_tokens /
 *     content_filter / error / tool_error → 回答被截断或被上游判废。
 * 这两种情况下正文通常只剩「我这就去查…：」这半句，绝不能当完整回答上屏。
 */
export interface OpenclawTurnHealth {
  incomplete: boolean;
  detail?: string;
}

const INCOMPLETE_FINISH_REASONS: ReadonlySet<string> = new Set([
  "length", "max_tokens", "max_output_tokens", "content_filter", "error", "tool_error",
]);

export function openclawTurnHealth(raw: string): OpenclawTurnHealth {
  const text = stripAnsiCodes(raw).trim();
  if (!text.startsWith("{")) return { incomplete: false };
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { incomplete: false };
  }
  if (typeof parsed !== "object" || parsed === null) return { incomplete: false };
  const meta = (parsed as { meta?: unknown }).meta;
  if (typeof meta !== "object" || meta === null) return { incomplete: false };
  const m = meta as {
    aborted?: unknown;
    stopReason?: unknown;
    completion?: { stopReason?: unknown; finishReason?: unknown } | null;
  };
  if (m.aborted === true) return { incomplete: true, detail: "aborted" };
  const finish = m.completion?.finishReason
    ?? m.completion?.stopReason
    ?? m.stopReason;
  if (typeof finish === "string" && INCOMPLETE_FINISH_REASONS.has(finish.trim().toLowerCase())) {
    return { incomplete: true, detail: `finishReason=${finish}` };
  }
  return { incomplete: false };
}

// Extracts the clean reply from an openclaw agent turn: the --json
// envelope when present, otherwise the ANSI/log-stripped plain stdout.
// Falls back to the raw trimmed text when both extractions come up empty
// so a turn never yields a blank reply.
export function extractOpenclawReply(raw: string): string {  const fromJson = parseOpenclawJsonOutput(raw);
  if (fromJson !== undefined && fromJson.trim().length > 0) return fromJson.trim();
  const normalized = normalizeOpenclawOutput(raw);
  if (normalized.length > 0) return normalized;
  return raw.trim();
}

// 有些 agent 的 stdout 是「一次性 JSON 信封」，而不是逐段正文：openclaw 的
// `agent --local --json` 把输入提示词、最终回复、执行 trace 打包成一个 JSON
// 一次吐出。这类 agent 绝不能参与增量回调 —— 原始增量是整份信封（含提示词
// 全文），web 的打字气泡里会出现一整坨 JSON；而终帧与兜底广播走的是
// extractOpenclawReply 提取后的正文，两者内容不等，web 按内容相等去重的
// streamEchoIndex（ai-message-lifecycle.ts）就失效，聊天里于是出现一对
// 「内容重复」的气泡（一条 JSON、一条正文）。
// 它们只发「处理中 → 完成」两端：终帧本身携带完整正文，气泡照样填满。
export function emitsJsonEnvelope(agentName: string | undefined): boolean {
  return agentName === "openclaw";
}

/**
 * openclaw 的 `agent --local --json` 信封里，除了最终正文还带着本条回复的
 * payloads（isReasoning 的那些就是模型的思考）与 toolSummary（工具调用汇总）。
 *
 * 信封是**一次性**的，所以这里拿到的思考不是逐字流，而是在一轮结束时整段补上
 * —— 群聊/讨论组里 openclaw 节点因此至少能展开「它想了什么、调了什么工具」，
 * 而不是只有一个光秃秃的结论。字段缺失时一律返回空对象（老版本信封）。
 */
export interface OpenclawEnvelopeDetails {
  reasoning?: string;
  tools?: { name: string; calls: number; failures: number }[];
}

export function extractOpenclawEnvelopeDetails(raw: string): OpenclawEnvelopeDetails {
  const text = stripAnsiCodes(raw).trim();
  if (!text.startsWith("{")) return {};
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return {};
  }
  if (typeof parsed !== "object" || parsed === null) return {};
  const envelope = parsed as {
    payloads?: unknown;
    toolSummary?: unknown;
  };
  const details: OpenclawEnvelopeDetails = {};
  if (Array.isArray(envelope.payloads)) {
    const reasoning = envelope.payloads
      .map((payload) => (typeof payload === "object" && payload !== null
        ? payload as { isReasoning?: unknown; text?: unknown } : undefined))
      .filter((payload): payload is { isReasoning?: unknown; text?: unknown } =>
        payload?.isReasoning === true && typeof payload.text === "string")
      .map((payload) => (payload.text as string).trim())
      .filter((chunk) => chunk.length > 0);
    if (reasoning.length > 0) details.reasoning = reasoning.join("\n\n");
  }
  const summary = envelope.toolSummary;
  if (summary && typeof summary === "object" && !Array.isArray(summary)) {
    const rec = summary as { tools?: unknown; calls?: unknown; failures?: unknown };
    const names = Array.isArray(rec.tools)
      ? rec.tools.filter((name): name is string => typeof name === "string" && name.trim().length > 0)
      : [];
    const totalCalls = typeof rec.calls === "number" && rec.calls > 0 ? rec.calls : names.length;
    const failures = typeof rec.failures === "number" && rec.failures > 0 ? rec.failures : 0;
    if (names.length > 0) {
      // 信封只给「工具名 + 总次数」，没有逐次调用 → 每种工具一条汇总步骤，
      // 次数作为摘要。宁可粒度粗一点也不编造不存在的调用顺序；多种工具时
      // 总次数无法摊到具体某个工具上，就按 1 次展示、丢失败计数。
      const single = names.length === 1;
      details.tools = names.map((name) => ({
        name,
        calls: single ? totalCalls : 1,
        failures: single ? failures : 0,
      }));
    }
  }
  return details;
}

export async function discoverAgents(opts: { extraPath?: string }): Promise<AgentInfo[]> {
  // When extraPath is provided (even empty) we scan ONLY there (tests and
  // explicit overrides must not pick up a real claude on the dev machine);
  // when absent we scan the real PATH.
  const dirs = opts.extraPath !== undefined
    ? opts.extraPath.split(delimiter).filter(Boolean)
    : (process.env.PATH ?? "").split(delimiter).filter(Boolean);

  const found: AgentInfo[] = [];
  for (const name of KNOWN_AGENT_CLIS) {
    for (const dir of dirs) {
      const candidates = process.platform === "win32"
        ? [join(dir, `${name}.exe`), join(dir, `${name}.cmd`), join(dir, `${name}.bat`)]
        : [join(dir, name)];
      for (const candidate of candidates) {
        try {
          await access(candidate, constants.X_OK);
          found.push({ name, path: candidate });
          break;
        } catch {
          // keep scanning
        }
      }
      if (found.some((f) => f.name === name)) break;
    }
  }
  return found;
}

export interface RunAgentCommandOpts {
  execPath: string;
  argv: string[];
  timeoutMs?: number;
  maxOutputBytes?: number;
  cwd?: string;
}

const DEFAULT_COMMAND_TIMEOUT_MS = 30_000;
const DEFAULT_COMMAND_MAX_OUTPUT_BYTES = 256 * 1024;

// Runs a short, non-interactive agent subcommand (e.g. `opencode models`) and
// returns stdout. Shares runAgentTurn's spawn/argv handling but sends no stdin
// and never applies the prompt-argv strategy.
export function runAgentCommand(opts: RunAgentCommandOpts): Promise<string> {
  const timeoutMs = opts.timeoutMs ?? DEFAULT_COMMAND_TIMEOUT_MS;
  const maxBytes = opts.maxOutputBytes ?? DEFAULT_COMMAND_MAX_OUTPUT_BYTES;

  return new Promise<string>((resolve, reject) => {
    const { file, args } = buildSpawnInvocation(opts.execPath, opts.argv);
    const child = spawn(file, args, {
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
      cwd: opts.cwd,
    });

    const chunks: Buffer[] = [];
    let totalBytes = 0;
    let stderr = "";
    let settled = false;

    const settle = (fn: () => void) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      fn();
    };

    const timer = setTimeout(() => {
      settle(() => {
        killProcessTree(child);
        reject(new Error(`agent command timeout after ${timeoutMs}ms`));
      });
    }, timeoutMs);

    child.stdout.on("data", (buf: Buffer) => {
      if (totalBytes >= maxBytes) return;
      const room = maxBytes - totalBytes;
      const slice = buf.length > room ? trimIncompleteUtf8Tail(buf.subarray(0, room)) : buf;
      chunks.push(slice);
      totalBytes += slice.length;
    });
    child.stderr.on("data", (buf: Buffer) => {
      stderr = (stderr + buf.toString("utf8")).slice(-2000);
    });
    child.on("error", (err) => {
      settle(() => reject(err));
    });
    child.on("close", (code) => {
      settle(() => {
        const stdout = Buffer.concat(chunks).toString("utf8");
        if (code === 0) {
          resolve(stdout);
        } else {
          reject(new Error(`agent command exited with code ${code}: ${stderr}`));
        }
      });
    });
  });
}

// --- stream-json / JSONL 解析（claude & codex 思考流数据源）----------------
// 两个 agent 都以「每行一个 JSON 事件」输出，思考流与正文分道。解析器是
// 纯函数：一行进，{reasoning?, text?, result?} 出。非 JSON 行（codex 的
// WARNING: / rmcp ERROR 行）返回 null 丢弃。
//
// claude stream-json（--output-format stream-json --verbose）：
//   {"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"…"}}}
//   {"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"…"}}}
//   {"type":"assistant","message":{"content":[{"type":"text","text":"全文"}]}}
//   {"type":"result","result":"全文"}
//
// codex exec --json（0.144 实测）：
//   {"type":"item.completed","item":{"type":"reasoning","text":"…"}}
//   {"type":"item.completed","item":{"type":"agent_message","text":"…"}}
//   {"type":"item.started","item":{"type":"agent_message"}}
//   {"type":"turn.completed","usage":{…}}
//
// opencode run --format json --thinking（1.19 实测）：
//   {"type":"step_start","part":{"type":"step-start"}}
//   {"type":"reasoning","part":{"id":"prt_…","type":"reasoning","text":"…"}}
//   {"type":"tool_use","part":{"type":"tool","tool":"bash","callID":"call_…","state":{…}}}
//   {"type":"text","part":{"id":"prt_…","type":"text","text":"…"}}
//   {"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}
// 与 claude/codex 的关键差异：它上报的是**整段 part 的全文**（同一 part 可能被
// 反复上报），不是增量。所以解析结果带 replace/partId，由调用方按 part 去重后
// 只发新增后缀，否则正文会成倍重复。
export interface StreamJsonDelta {
  reasoning?: string;
  text?: string;
  /** 整段全文语义下，这条内容属于哪个 part（opencode 用；其余 agent 不带）。 */
  partId?: string;
  /** true = 本次给出的是该 part 的完整文本，调用方需自行换算增量。 */
  replace?: boolean;
}

export function parseStreamJsonLine(line: string): StreamJsonDelta | null {
  const trimmed = line.trim();
  if (!trimmed.startsWith("{")) return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const event = parsed as Record<string, unknown>;

  // --- claude ---
  if (event.type === "stream_event") {
    const inner = event.event as Record<string, unknown> | undefined;
    const delta = inner?.delta as Record<string, unknown> | undefined;
    if (!delta) return null;
    if (delta.type === "thinking_delta" && typeof delta.thinking === "string") {
      return { reasoning: delta.thinking };
    }
    if (delta.type === "text_delta" && typeof delta.text === "string") {
      return { text: delta.text };
    }
    return null;
  }

  // --- codex ---
  if (event.type === "item.completed") {
    const item = event.item as Record<string, unknown> | undefined;
    if (!item) return null;
    if (item.type === "reasoning" && typeof item.text === "string") {
      return { reasoning: item.text };
    }
    if (item.type === "agent_message" && typeof item.text === "string") {
      return { text: item.text };
    }
    return null;
  }

  // --- hermes ---
  // 形状：{"type":"text","text":"…"}（增量，无 part）/ {"type":"tool_use",…} /
  // {"type":"reasoning","text":"…"}（增量，0.21.3 起补齐：TTY 的 [thinking] 框
  // 走同一个 reasoning_callback，之前静默模式把它置空所以思考整段丢失）。
  // opencode 的事件一定带 part，所以「有 text 没 part」只可能是 hermes。
  if (event.type === "text" && event.part === undefined && typeof event.text === "string") {
    return { text: event.text };
  }
  if (event.type === "reasoning" && event.part === undefined
    && typeof event.text === "string" && event.text) {
    return { reasoning: event.text };
  }

  // --- opencode ---
  if (event.type === "reasoning" || event.type === "text") {
    const part = event.part as Record<string, unknown> | undefined;
    if (!part || typeof part.text !== "string" || !part.text) return null;
    // 只认与事件同名的 part 类型：`reasoning` 事件里偶尔会混进别的 part。
    const partType = typeof part.type === "string" ? part.type : "";
    const id = typeof part.id === "string" ? part.id : undefined;
    if (event.type === "reasoning") {
      if (partType && partType !== "reasoning") return null;
      return { reasoning: part.text, partId: id, replace: true };
    }
    if (partType && partType !== "text") return null;
    return { text: part.text, partId: id, replace: true };
  }
  return null;
}

// 从 stream-json 全量 stdout 提取最终正文（claude 优先 result.result / 最长
// assistant 文本；codex 聚合 agent_message；opencode 按 part 顺序拼 text part）。
// 空时返回 undefined 走兜底。
export function extractStreamJsonFinalText(raw: string): string | undefined {
  const texts: string[] = [];
  let claudeResult: string | undefined;
  // hermes：{"type":"text","text":…} 是**增量**（没有 part），逐条追加即可；
  // 末尾的 {"type":"result","text":…} 是同一次回答的权威全文，优先用它。
  const hermesDeltas: string[] = [];
  let hermesResult: string | undefined;
  // opencode 的正文按 part 分片上报，同一 part 可能重复出现 → 以 id 去重、保序。
  const opencodeTexts = new Map<string, string>();
  for (const line of raw.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed.startsWith("{")) continue;
    let parsed: unknown;
    try {
      parsed = JSON.parse(trimmed);
    } catch {
      continue;
    }
    if (typeof parsed !== "object" || parsed === null) continue;
    const event = parsed as Record<string, unknown>;
    if (event.type === "text") {
      const part = event.part as Record<string, unknown> | undefined;
      if (part && typeof part.text === "string" && part.text
        && (typeof part.type !== "string" || part.type === "text")) {
        const id = typeof part.id === "string" ? part.id : `anon-${opencodeTexts.size}`;
        opencodeTexts.set(id, part.text);
      } else if (typeof event.text === "string" && event.text) {
        hermesDeltas.push(event.text);
      }
    }
    if (event.type === "result" && typeof event.result === "string") {
      claudeResult = event.result;
    }
    // hermes 的 result 事件用 text 字段（claude 用 result），且总在最后一条。
    if (event.type === "result" && typeof event.text === "string") {
      hermesResult = event.text;
    }
    if (event.type === "assistant") {
      const message = event.message as Record<string, unknown> | undefined;
      const content = message?.content;
      if (Array.isArray(content)) {
        const parts = content
          .map((block) => (typeof block === "object" && block !== null
            ? (block as Record<string, unknown>) : undefined))
          .filter((block): block is Record<string, unknown> =>
            block?.type === "text" && typeof block.text === "string")
          .map((block) => block.text as string);
        if (parts.length > 0) texts.push(parts.join(""));
      }
    }
    if (event.type === "item.completed") {
      const item = event.item as Record<string, unknown> | undefined;
      if (item?.type === "agent_message" && typeof item.text === "string") {
        texts.push(item.text);
      }
    }
  }
  if (claudeResult !== undefined && claudeResult.length > 0) return claudeResult;
  if (hermesResult !== undefined && hermesResult.trim().length > 0) return hermesResult;
  if (hermesDeltas.length > 0) {
    const hm = hermesDeltas.join("");
    if (hm.trim().length > 0) return hm;
  }
  if (opencodeTexts.size > 0) {
    const oc = [...opencodeTexts.values()].join("").trim();
    if (oc.length > 0) return oc;
  }
  const joined = texts.join("\n").trim();
  return joined.length > 0 ? joined : undefined;
}

// --- opencode 服务端模式（真流式）----------------------------------------
//
// `opencode run --format json` 只在 part 完成时上报**整段全文**：实测一轮 800
// 字长文只有 4 行事件（step_start / reasoning / text / step_finish），正文与思考
// 各出现一次。设备端因此只能发 1 帧正文，客户端看到的就是「憋两分钟 → 全文
// 一次性蹦出」。
//
// `opencode serve` 的 SSE 才是真增量：`message.part.delta` 一条一个 token 片段
// （同一 prompt 实测 12,620 条、平均 1.7 字/片、持续 101 秒）。所以 opencode 默认
// 走 serve，run 退化为「旧版本兜底」。
//
// opencode 2.x 换了协议但同样是真增量：serve 会多打一行 `server password`，
// 会话 API 挪到 `/api/*` 并要 Basic 鉴权，增量事件变成 `session.text.delta` /
// `session.reasoning.delta`（见 opencode-v2.ts）。判据就是那行密码 —— 有它就走
// 2.x，没有就是 1.x。
//
//   CLAWMESSENGER_OPENCODE_SERVER=0        关掉，回到 run --format json
//   CLAWMESSENGER_OPENCODE_FULL_CONFIG=1   不派生精简配置（保留 plugin / MCP）
const SERVE_STARTUP_TIMEOUT_MS = 25_000;
// 拿到 listening 行之后再等密码行的窗口。2.x 这两行是先后打出来的（实测相隔
// 几毫秒），但**别按实测值去压这个窗口**：Windows 冷启动、首次加载配置、杀软
// 扫描 stdout 时，密码行晚到 300ms 是常态。窗口一旦错过，`password` 就为空 →
// 走 1.x 协议 → `POST /session` 405 → 整轮退化成 `run --format json` 的「整段
// 落地」（实测一轮 12,620 事件、101 秒才一次蹦出），而且是**静默**退化：功能还
// 能用，用户只会觉得「1.3.5 变慢了」。1200ms 是这轮取舍的结果 —— 1.x 每轮白等
// 这点时间，换 2.x 稳定命中真流式。
const SERVE_PASSWORD_GRACE_MS = 1200;

export function opencodeServerEnabled(env: NodeJS.ProcessEnv = process.env): boolean {
  const raw = (env.CLAWMESSENGER_OPENCODE_SERVER ?? "").trim();
  if (!raw) return true;
  return !/^(0|false|no|off)$/i.test(raw);
}

function envFlag(raw: string | undefined): boolean {
  return /^(1|true|yes|on)$/i.test((raw ?? "").trim());
}

/** 设备端派生的 opencode 配置落盘位置（per-agent：里面的 instructions 指向本 agent 的行为规范）。 */
export function deviceOpencodeConfigPath(
  agentName: string | undefined,
  home: string = homedir(),
): string {
  const base = join(home, ".clawmessenger");
  // 没有 agent 名（老调用方 / 手工探测）时退回整机一份的老路径，行为与升级前一致。
  if (!agentName) return join(base, "opencode-device.json");
  return join(base, `opencode-device-${sanitizeAgentId(agentName)}.json`);
}

interface DeviceConfigInput {
  agentName?: string;
  /**
   * 该轮要注入的行为规范（仅私聊轮次非空）。
   *
   * - 非空串：写文件并让 `instructions` 指向它；
   * - 空串：**显式清掉**（删文件 + 不写 instructions），用于群聊/讨论组轮
   *   （服务端已拼 role_prompt，设备端不能重复注入）；
   * - `undefined`：**不要动**既有文件，存在就继续用。轮询侧的
   *   `syncAgentPermissionConfig` 走这条 —— 它只重算限制键，不能用空串把
   *   用户既有的行为规范删掉（审查 G10）。
   */
  behaviorPrompt?: string;
  /** 整机是否处于「文件读写禁止」限制态。 */
  fsRestricted?: boolean;
  home?: string;
}

/**
 * serve 进程的环境：**派生一份设备端专用 opencode 配置**，去掉 plugin 与 mcp，
 * 并按权限快照写入工具限制与行为规范指令。
 *
 * 为什么（2026-10-26 实测）：用户配置里的 `superpowers@git+https://…` 插件与两个
 * `uvx --from git+https://…` 的 MCP 会让 opencode 在 `message=init` 上卡七十秒
 * 以上、零输出；设备端每轮都要重新付这笔开销，表现就是「先想很久」。provider /
 * model / instructions / permission 全部保留，模型与人格不变。
 *
 * 用户显式指定过 OPENCODE_CONFIG（或 CLAWMESSENGER_OPENCODE_CONFIG）时不动 ——
 * 那说明他自己管这份配置。
 */
function opencodeServeEnv(input: DeviceConfigInput = {}): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...process.env };
  if (!envFlag(env.CLAWMESSENGER_OPENCODE_FULL_CONFIG)
    && !env.OPENCODE_CONFIG
    && !env.CLAWMESSENGER_OPENCODE_CONFIG) {
    const config = ensureDeviceOpencodeConfig(env, input);
    if (config) env.OPENCODE_CONFIG = config;
  }
  // serve 只绑 127.0.0.1、端口随机；带着密码只会让本地请求全部 401。
  delete env.OPENCODE_SERVER_PASSWORD;
  return env;
}

function ensureDeviceOpencodeConfig(
  env: NodeJS.ProcessEnv,
  input: DeviceConfigInput = {},
): string | null {
  let source: string | null = null;
  try {
    source = discoverOpencodeConfigPath({ env });
  } catch {
    return null;
  }
  if (!source) return null;
  let raw: string;
  try {
    raw = readFileSync(source, "utf8");
  } catch {
    return null;
  }
  const home = input.home ?? homedir();
  // 行为规范先落盘，再让配置指向它：顺序反了会出现「配置指向一个还不存在的文件」。
  //
  // undefined = 不要动既有文件（轮询侧重算限制时走这条）；空串 = 显式清掉。
  // 二者的区别见 DeviceConfigInput.behaviorPrompt 的注释（审查 G10）。
  const behaviorPath = opencodeBehaviorPath(input.agentName, home);
  const behaviorFile = input.behaviorPrompt === undefined
    ? (existsSync(behaviorPath) ? behaviorPath : null)
    : writeOpencodeBehaviorFile(input.agentName, input.behaviorPrompt, home);
  const built = buildDeviceOpencodeConfig(raw, {
    fsRestricted: input.fsRestricted === true,
    instructions: behaviorFile ? [behaviorFile] : [],
  });
  if (!built) return null;
  const target = deviceOpencodeConfigPath(input.agentName, home);
  try {
    const existing = existsSync(target) ? readFileSync(target, "utf8") : null;
    // 内容没变就不重写：既省 IO，也避免每轮都触发 opencode 重新加载配置。
    if (existing === built.text) return target;
    mkdirSync(dirname(target), { recursive: true });
    writeFileSync(target, built.text, "utf8");
  } catch {
    return null;
  }
  return target;
}

/**
 * 权限快照变化时重算并落盘派生配置（supervisor 在轮询里调）。
 *
 * 为什么要有这个显式入口：配置原本只在 `runOpenCodeServerTurn` 前算 —— 那意味着
 * 「用户在界面关掉读写」要等到该 agent 下一轮真的开口才落盘，中间这段时间磁盘上
 * 的配置仍是旧的可读写版本。轮询侧主动重算把生效点提前到「下一次轮询」。
 *
 * 只处理 opencode：其它 agent 的限制靠 spawn 时的 argv/env 表达，没有落盘的派生配置。
 *
 * @returns 写入的配置路径；不需要/失败返回 null。
 */
export function syncAgentPermissionConfig(
  agentName: string | undefined,
  snapshot: PermissionSnapshot | null,
  opts: { env?: NodeJS.ProcessEnv; home?: string } = {},
): string | null {
  if (agentName !== "opencode") return null;
  // 运维 agent 不被 allowFs 限制（默认新建绑定就是 false，否则运维一上线就废）。
  // 它的派生配置要保持「无限制键」。
  if (fsGateExempt(agentName)) return null;
  const env = opts.env ?? process.env;
  if (envFlag(env.CLAWMESSENGER_OPENCODE_FULL_CONFIG)
    || env.OPENCODE_CONFIG
    || env.CLAWMESSENGER_OPENCODE_CONFIG) {
    return null;
  }
  const restriction = fsRestriction(snapshot);
  // 行为规范是**私聊轮次**才注入的，重算时不知道下一轮是什么会话形态，所以这里
  // 只重算限制键：**不传 behaviorPrompt**（undefined = 不要动既有指令文件，见
  // DeviceConfigInput 的说明）。传空串会把用户的行为规范文件删掉（审查 G10）。
  return ensureDeviceOpencodeConfig(env, {
    agentName,
    fsRestricted: restriction.restricted,
    ...(opts.home ? { home: opts.home } : {}),
  });
}

/** 起 `opencode serve` 并等它把监听地址打到 stdout，然后跑完一轮。 */
async function runOpenCodeServerTurn(
  opts: RunAgentTurnOpts,
  timeoutMs: number,
  hardLimit: number,
): Promise<string> {
  const { file, args } = buildSpawnInvocation(opts.execPath, [
    "serve",
    // 端口 0 = 让内核挑空闲端口，多台设备/多个 agent 不会撞。
    "--port", "0",
    "--hostname", "127.0.0.1",
  ]);
  const child = spawn(file, args, {
    stdio: ["ignore", "pipe", "pipe"],
    windowsHide: true,
    cwd: opts.cwd,
    env: opencodeServeEnv({
      ...(opts.agentName ? { agentName: opts.agentName } : {}),
      // 一律传（可能为空串）：serve 每轮都会重写派生配置，空串表示「这一轮不该
      // 有行为规范」（群聊/讨论组），要显式摘掉上一轮私聊写进去的指令文件；
      // 传 undefined 会被理解成「不要动」，那份指令就会在群聊里重复生效。
      behaviorPrompt: opts.behaviorPrompt ?? "",
      ...(opts.fsRestricted ? { fsRestricted: true } : {}),
    }),
  });

  let stderr = "";
  child.stderr.on("data", (buf: Buffer) => {
    stderr = (stderr + buf.toString("utf8")).slice(-2000);
  });

  const serve = await new Promise<{ baseUrl: string; password?: string }>((resolve, reject) => {
    let tail = "";
    let done = false;
    // 2.x 的启动输出是「listening 行在前、password 行在后」，只等着 URL 会漏掉
    // 密码，因此拿到 URL 后再给它一个很短的窗口把密码行收进来；窗口过了就当
    // 1.x 处理（1.x 永远不打这行）。
    let passwordTimer: NodeJS.Timeout | undefined;
    const finish = (fn: () => void) => {
      if (done) return;
      done = true;
      clearTimeout(startTimer);
      if (passwordTimer) clearTimeout(passwordTimer);
      opts.signal?.removeEventListener("abort", onAbort);
      fn();
    };
    const onAbort = () => finish(() => reject(new Error("agent turn aborted")));
    opts.signal?.addEventListener("abort", onAbort, { once: true });
    const startTimer = setTimeout(() => {
      finish(() => reject(new Error(
        `opencode serve did not report its listening URL within ${SERVE_STARTUP_TIMEOUT_MS}ms: ${stderr.slice(-400)}`,
      )));
    }, SERVE_STARTUP_TIMEOUT_MS);
    child.stdout.on("data", (buf: Buffer) => {
      tail += buf.toString("utf8");
      const url = parseServeListeningUrl(tail);
      const password = parseServePassword(tail);
      if (url && password) {
        finish(() => resolve({ baseUrl: url, password }));
        return;
      }
      if (url && !passwordTimer) {
        passwordTimer = setTimeout(() => {
          finish(() => resolve({ baseUrl: url }));
        }, SERVE_PASSWORD_GRACE_MS);
      }
      // 启动期会打若干行日志；只留尾部，别让长跑把内存吃满。
      if (tail.length > 4000) tail = tail.slice(-2000);
    });
    child.on("error", (err) => finish(() => reject(err)));
    child.on("exit", (code) => {
      finish(() => reject(new Error(`opencode serve exited with code ${code}: ${stderr.slice(-400)}`)));
    });
  });

  try {
    const result = await runServerSession({
      baseUrl: serve.baseUrl,
      password: serve.password,
      prompt: opts.prompt,
      model: opts.model,
      timeoutMs,
      signal: opts.signal,
      onText: opts.onChunk,
      onReasoning: opts.onReasoningChunk,
      onActivity: opts.onActivity,
      // 字符上限按字节上限的一半估：中文 UTF-8 三字节，留余量不误杀长回答。
      maxTextChars: Math.max(1, Math.floor(hardLimit / 2)),
    });
    if (!result.sawDelta) {
      // serve 起来了但没有增量事件：多半是旧版 opencode。内容不丢，只是不流。
      // 正文事件的两种形态都要点到：1.x 的 serve 发 `message.part.delta`，
      // 2.x 的 /api/* 发 `session.text.delta` —— 只写前者会把排查方向带偏。
      console.warn(
        `opencode serve 未上报增量正文事件（message.part.delta / session.text.delta，正文 ${result.text.length} 字）——`
        + " 该版本可能不支持增量事件，本轮按整段返回。",
      );
    }
    if (result.endedOnFailedTool && result.failedStep) {
      const step = result.failedStep;
      const failedCmd = (step.detail ?? "").replace(/\s+/g, " ").trim().slice(0, 120);
      const failedOut = (step.outputDetail ?? "").replace(/\s+/g, " ").trim().slice(0, 160);
      throw new TurnIncompleteError(
        "agent_incomplete",
        result.text,
        `turn ended on a failed ${step.label} with no follow-up text`
          + (failedCmd ? ` [cmd: ${failedCmd}]` : "")
          + (failedOut ? ` [output: ${failedOut}]` : ""),
        { label: step.label, command: failedCmd || undefined, output: failedOut || undefined },
      );
    }
    return result.text;
  } finally {
    // serve 是本轮专用进程，收尾必须整棵杀掉：留着就是下一个孤儿端口。
    killProcessTree(child);
  }
}

// 「文件读写限制态」的日志只在该状态**变化**时打一行：限制是整机级的，每轮都
// 打会把 agent 日志淹掉；但状态翻转（管理员刚关掉/打开读写）必须留痕，否则现场
// 只能看到「工具突然不能用了」。
//
// 日志里必须带「谁触发 + 谁参与了判定」：运维 agent 是豁免的，运维看日志时要能
// 一眼判断「不是限我」还是「限的是对话 agent，但我在豁免名单里」。
let lastFsRestrictionLog = "";
function logFsRestrictionOnce(
  restricted: boolean,
  restriction: { because: readonly string[]; participating: readonly string[]; exempt: boolean },
): void {
  const key = restricted
    ? `on:${restriction.because.join(",")}|${restriction.participating.join(",")}`
    : `off:${restriction.exempt ? "exempt" : "clear"}`;
  if (key === lastFsRestrictionLog) return;
  lastFsRestrictionLog = key;
  if (restriction.exempt) {
    console.log(
      "agent fs gate: 运维 agent 豁免（不受 allowFs 限制，也不参与「有一个人关就都不给」的判定；"
      + `参与判定的 agent=[${restriction.participating.join(", ") || "无"}]）。`
      + " 残留：同机「受限对话 agent + 运维」并存时运维修复能力不降级，见报告 known issues。",
    );
    return;
  }
  if (!restricted) return;
  console.warn(
    `agent fs restricted: allowFs=false on [${restriction.because.join(", ") || "?"}] — `
    + `参与判定的 agent=[${restriction.participating.join(", ") || "无"}]（运维 agent 已豁免）；`
    + "同一台设备的派生配置是整机一份，按「有一个人关就都不给」处理。"
    + "bash 已一并禁用，但本轮没有 OS 级沙箱，见 implementer-report-cli.md 的残留风险。",
  );
}

export function runAgentTurn(opts: RunAgentTurnOpts): Promise<string> {
  const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const hardLimit = opts.hardOutputBytes ?? HARD_OUTPUT_BYTES;
  // stream-json 的 JSONL 冗余大（事件元数据 + 思考流），64KB 会截断长回答。
  const streamJsonTurn = opts.onReasoningChunk !== undefined || agentSupportsReasoning(opts.agentName);
  const bufferLimit = opts.maxOutputBytes ?? (streamJsonTurn
    ? STREAM_JSON_MAX_OUTPUT_BYTES
    : emitsJsonEnvelope(opts.agentName) ? ENVELOPE_MAX_OUTPUT_BYTES : DEFAULT_MAX_OUTPUT_BYTES);

  if (opts.signal?.aborted) return Promise.reject(new Error("agent turn aborted"));

  // 权限快照 → 本轮的两条限制：文件读写是否禁止、要不要注入行为规范。
  //
  // 快照读的是 supervisor 轮询落盘的那一份（带 5s 缓存，见 agent-permissions.ts），
  // 不是每条消息现查服务端 —— 私聊轮次很密，不能在这里加网络往返。
  // 只在**快照真的变了**的时候打一行日志，避免每轮刷屏。
  const restriction = fsRestriction(loadPermissionSnapshot(), opts.agentName);
  const fsRestricted = opts.fsRestricted ?? restriction.restricted;
  logFsRestrictionOnce(fsRestricted, restriction);

  // codex 会把执行过程写进正文（2026-10-26 现场：「我先核对每段字数…这里没有
  // 安装 Python，我改用 PowerShell 直接按字符计数」就是正文第一段）。这类内容
  // 属于思考。约束落在**指令层**（$CODEX_HOME/AGENTS.md）而不是 prompt：私聊的
  // prompt 就是用户原话，往里塞一句会污染输入，而且 agent 可能复述它。
  // 幂等且失败静默 —— 写不了指令文件不该让这一轮答不出来。
  if (opts.agentName === "codex") {
    ensureCodexOutputStyle();
    // 行为规范只在私聊轮次存在：群聊/讨论组由服务端拼进 role_prompt，这里传空
    // 表示「摘掉上一轮私聊写进去的那一节」，避免同一段话在群聊里出现两遍。
    ensureCodexBehaviorPrompt(opts.behaviorPrompt ?? "");
  }

  // opencode：默认走 serve + SSE（真 token 级增量）。只有 argv 策略成立时
  // 才走这条路 —— 超长 prompt 会退回 stdin 模式，那条路沿用 run 的老实现。
  if (opts.agentName === "opencode" && !opts.disableServerMode && opencodeServerEnabled()) {
    const strategyArgs = buildAgentArgs(opts.agentName, opts.prompt, opts.model, opts.sessionKey, {
      fsRestricted,
    });
    if (strategyArgs.length > 0) {
      // ⚠️ fsRestricted 是本轮算出来的**局部**值，必须合并进 opts 再往下传：
      // serve 侧的 opencodeServeEnv 只认 `opts.fsRestricted`，不合并就会恒 false
      // —— 受限配置不会被写进 OPENCODE_CONFIG 指向的文件，而上面的日志已经打了
      // 「fs restricted」（日志与事实相反），且 supervisor 写好的受限配置会被这一轮
      // 用同路径的无限制内容覆盖（审查 B1 阻断项）。
      return runOpenCodeServerTurn({ ...opts, fsRestricted }, timeoutMs, hardLimit).catch((err: unknown) => {
        if (!isServeStartupFailure(err)) throw err;
        // 起不来（旧版 opencode 没有 serve / 端口被占 / 配置坏）就退回 run：
        // 内容仍然拿得到，只是回到「整段落地」。绝不因为流式改造让一轮失败。
        console.warn(
          `opencode serve 不可用，回退到 run --format json：${err instanceof Error ? err.message : String(err)}`,
        );
        return runAgentTurn({ ...opts, disableServerMode: true });
      });
    }
  }

  return new Promise<string>((resolve, reject) => {
    if (opts.signal?.aborted) {
      reject(new Error("agent turn aborted"));
      return;
    }

    // Strategy agents take the prompt as argv; everything else (unknown
    // agent, or a prompt past the argv length cap) keeps the legacy stdin
    // invocation with the model flag as the only argument.
    const strategyArgs = opts.agentName
      ? buildAgentArgs(opts.agentName, opts.prompt, opts.model, opts.sessionKey, { fsRestricted })
      : [];
    const argvMode = strategyArgs.length > 0;
    const agentArgs = argvMode
      ? strategyArgs
      : buildModelArgs(opts.agentName ?? "", opts.model);
    // opencode 的 `run` 路径原本完全不碰派生配置（只有 serve 路径设 OPENCODE_CONFIG）。
    // 权限限制要靠配置表达，所以**只在需要时**补上：没有限制、也没有行为规范时
    // 保持老行为不变（不引入「run 也会剥 plugin/mcp」这种副作用）。
    const deviceConfig = opts.agentName === "opencode" && (fsRestricted || opts.behaviorPrompt)
      ? ensureDeviceOpencodeConfig(process.env, {
        ...(opts.agentName ? { agentName: opts.agentName } : {}),
        // 与 serve 路径同口径：空串 = 这一轮不该有行为规范（显式清掉），
        // 不能传 undefined（那会保留上一轮私聊写进去的指令，群聊里就重复了）。
        behaviorPrompt: opts.behaviorPrompt ?? "",
        ...(fsRestricted ? { fsRestricted: true } : {}),
      })
      : null;
    const spawnEnv = deviceConfig
      ? { ...process.env, OPENCODE_CONFIG: deviceConfig }
      : agentRuntimeEnv(opts.agentName, opts.model);
    const { file, args } = buildSpawnInvocation(opts.execPath, agentArgs);
    const child = spawn(file, args, {
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
      cwd: opts.cwd,
      // null = 继承当前进程环境（只有需要改网关的 agent 才返回对象）。
      ...(spawnEnv ? { env: spawnEnv } : {}),
    });

    // stdout accumulates raw bytes; decoding happens through a streaming
    // StringDecoder so code points split across chunk boundaries survive,
    // and truncation caps BYTES and backs off to a code-point boundary.
    const chunks: Buffer[] = [];
    let totalBytes = 0;
    let bufferedBytes = 0;
    const decoder = new StringDecoder("utf8");
    let stderr = "";
    /** 原始缓冲已满（只影响 chunks，不影响解析）。 */
    let capped = false;
    /** 超出硬上限，这一轮已不可信：收尾按 output_truncated 失败。 */
    let hardStopped = false;
    /** 已经交给 UI 的正文累积：缓冲被截断时以它为准（它才是完整的）。 */
    let streamedText = "";
    /** 最近一个工具步骤的 id：用来判断「正文是否发生在这步之后」。 */
    let lastToolId: string | null = null;
    /** 最后一个工具步骤之后是否还产出过正文（收尾判据，见 close 处理）。 */
    let textAfterLastTool = false;
    const pushText = (delta: string): void => {
      if (!delta) return;
      streamedText += delta;
      textAfterLastTool = true;
      opts.onChunk?.(delta);
    };
    let settled = false;

    const settle = (fn: () => void) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      opts.signal?.removeEventListener("abort", onAbort);
      fn();
    };

    const timer = setTimeout(() => {
      settle(() => {
        killProcessTree(child);
        reject(new Error(`agent turn timeout after ${timeoutMs}ms`));
      });
    }, timeoutMs);

    const onAbort = () => {
      settle(() => {
        killProcessTree(child);
        reject(new Error("agent turn aborted"));
      });
    };
    opts.signal?.addEventListener("abort", onAbort, { once: true });

    // stream-json 模式（claude/codex/opencode）：stdout 是逐行 JSON 事件流。行缓冲
    // 切分（\n 边界），thinking/text 增量分道回调；原始字节照常入 chunks，
    // 结束时用 extractStreamJsonFinalText 提取权威正文。
    const streamJsonMode = agentSupportsReasoning(opts.agentName) && argvMode;
    // 非 stream-json 的 agent 才走「原始 stdout 即正文」的增量回调；JSON 信封
    // 型 agent（openclaw）跳过，避免把整份信封当正文流出去（见 emitsJsonEnvelope）。
    const streamRawStdout = !emitsJsonEnvelope(opts.agentName);
    let lineBuf = "";
    // 本轮收到过多少段思考。hermes 的思考要靠本机 stream_json.py 补丁才能进
    // JSONL（见 scripts/patch-hermes-reasoning.py），缺补丁时给一行日志提示，
    // 免得又去 web 侧翻帧。
    let reasoningChunks = 0;
    const deliverReasoning = (chunk: string): void => {
      reasoningChunks += 1;
      opts.onReasoningChunk?.(chunk);
    };
    // 过程流追踪器：逐行消化工具事件，全量快照上抛。没有回调方就不建，省掉
    // 每行一次的解析开销。
    const activity = opts.onActivity ? createActivityTracker() : undefined;
    const emitActivity = (): void => {
      if (!activity || !opts.onActivity) return;
      opts.onActivity(activity.snapshot());
    };
    // opencode 上报的是「整段 part 全文」而不是增量，这里按 part 记住已经发出的
    // 长度，只补新增后缀。少了这一层，同一段文字会被反复追加 → 气泡里正文翻倍。
    const partSent = new Map<string, number>();
    const emitPart = (kind: "reasoning" | "text", partId: string, full: string): void => {
      const key = `${kind}:${partId}`;
      const prev = partSent.get(key) ?? 0;
      if (full.length <= prev) return;
      partSent.set(key, full.length);
      const suffix = full.slice(prev);
      if (kind === "reasoning") deliverReasoning(suffix);
      else pushText(suffix);
    };
    const emitStreamJson = (flushed: string) => {
      // 工具事件先消化：同一个 JSONL 行里 tool 事件与正文不会共存（claude 的
      // tool_use 走 content_block_*，codex 走 item.*），但收尾事件的顺序不能
      // 依赖 —— 先更新步骤再发正文，用户才不会看到「正文已出、步骤还没出」。
      if (activity && activity.push(flushed)) {
        emitActivity();
        // 只在**新步骤**出现时重置：同一步从 running 变 finished/error 不算
        // 「又调了一次工具」，否则正文与步骤的先后关系会被收尾事件抹掉。
        const snap = activity.snapshot();
        const newest = snap[snap.length - 1];
        if (newest && newest.id !== lastToolId) {
          lastToolId = newest.id;
          textAfterLastTool = false;
        }
      }
      const delta = parseStreamJsonLine(flushed);
      if (!delta) return;
      if (delta.replace && delta.partId) {
        if (delta.reasoning !== undefined) emitPart("reasoning", delta.partId, delta.reasoning);
        if (delta.text !== undefined) emitPart("text", delta.partId, delta.text);
        return;
      }
      if (delta.reasoning !== undefined) deliverReasoning(delta.reasoning);
      if (delta.text !== undefined) pushText(delta.text);
    };
    child.stdout.on("data", (buf: Buffer) => {
      totalBytes += buf.length;
      // 硬上限：agent 在喷日志/打印超大结果，这一轮已经不可能再有用。掐掉进程，
      // 收尾按 output_truncated 失败 —— 绝不能像过去那样「读到一半就 resolve」，
      // 那等于把半句话当完整回答发给用户。
      if (totalBytes > hardLimit) {
        hardStopped = true;
        killProcessTree(child);
        return;
      }
      // 缓冲满了只停止攒原始字节（内存保护），解析照旧继续：工具输出的洪峰之后
      // 往往还有最终回答，这里过去直接 return，后半轮就被整段丢掉了。
      if (!capped) {
        const room = bufferLimit - bufferedBytes;
        if (buf.length <= room) {
          chunks.push(buf);
          bufferedBytes += buf.length;
        } else {
          if (room > 0) {
            const tail = trimIncompleteUtf8Tail(buf.subarray(0, room));
            if (tail.length > 0) {
              chunks.push(tail);
              bufferedBytes += tail.length;
            }
          }
          capped = true;
          console.warn(
            `agent stdout exceeded ${bufferLimit} bytes; raw buffer capped (turn keeps parsing)`,
          );
        }
      }
      const delta = decoder.write(buf);
      if (streamJsonMode) {
        // 按行切分：完整行立即解析，残行留在 lineBuf 等下一个 chunk。
        lineBuf += delta;
        for (;;) {
          const nl = lineBuf.indexOf("\n");
          if (nl < 0) break;
          const line = lineBuf.slice(0, nl);
          lineBuf = lineBuf.slice(nl + 1);
          emitStreamJson(line);
        }
      } else if (delta && streamRawStdout) {
        pushText(delta);
      }
    });
    child.stderr.on("data", (buf: Buffer) => {
      stderr = (stderr + buf.toString("utf8")).slice(-2000);
    });
    child.on("error", (err) => {
      settle(() => reject(err));
    });
    child.on("close", (code) => {
      settle(() => {
        const stdout = Buffer.concat(chunks).toString("utf8");
        if (hardStopped) {
          // 硬上限触发：后半轮没读到，已产出的正文一并带出去（终帧还能显示
          // 用户刚才看到的那半句），但终态必须是失败。
          if (activity && activity.finish()) emitActivity();
          reject(new TurnIncompleteError(
            "output_truncated",
            streamedText.trim() || stdout,
            `agent stdout exceeded ${hardLimit} bytes`,
          ));
          return;
        }
        if (code === 0) {
          if (streamJsonMode) {
            // 冲刷残行后提取权威正文；提取失败（非标准输出）兜底原文。
            emitStreamJson(lineBuf);
            // 收尾：异常退出/事件流缺失都不会有 content_block_stop，悬着的
            // 「进行中」步骤必须在这里结掉，否则气泡里永远转圈。
            if (activity && activity.finish()) emitActivity();
            // 这一轮是不是「撞在失败的工具上就没下文了」。
            //
            // 现场（2026-10-20 10:54 运维助手）：opencode 跑到第 7 步 bash 报错，
            // 进程 exit 0，之后再没吐过一个字 —— 气泡里只剩「…拉取模型列表：」
            // 这半句前言，而我们把 exit 0 当成了「答完了」。opencode 自己的会话库
            // 可以确认：该轮最后一条 part 是 bash(error)，之后既无文本也无 step。
            //
            // 判据只认**确定的事实**：最后一步是 failed，且它之后再没出现正文。
            // （最后一步成功、或失败后模型解释了原因，都不算 —— 那两种情况用户
            // 至少拿到了交代。）
            const steps = activity?.snapshot() ?? [];
            const lastStep = steps[steps.length - 1];
            if (lastStep && lastStep.status === "failed" && !textAfterLastTool) {
              // 把「哪条命令、错在哪」一起写进日志：2026-10-20 13:07 现场就是
              // 模型把 PowerShell 的 Get-Content 塞进 bash，报 command not found
              // 之后一声不吭，只看 label「执行命令」根本查不出是 shell 不匹配。
              const failedCmd = (lastStep.detail ?? "").replace(/\s+/g, " ").trim().slice(0, 120);
              const failedOut = (lastStep.outputDetail ?? "").replace(/\s+/g, " ").trim().slice(0, 160);
              reject(new TurnIncompleteError(
                "agent_incomplete",
                streamedText,
                `turn ended on a failed ${lastStep.label} with no follow-up text`
                  + (failedCmd ? ` [cmd: ${failedCmd}]` : "")
                  + (failedOut ? ` [output: ${failedOut}]` : ""),
                // 结构化带上失败步骤：im.ts 据此自动补跑一轮（把命令与错误
                // 回喂给 agent 让它换写法继续），而不是直接把半句话丢给用户。
                { label: lastStep.label, command: failedCmd || undefined, output: failedOut || undefined },
              ));
              return;
            }
            // 缓冲被截断时原始 stdout 只是前缀，以流式正文为准（它才是全的）。
            resolve(capped ? streamedText : (extractStreamJsonFinalText(stdout) ?? stdout));
          } else {
            // JSON 信封型 agent（openclaw）：正文之外还能从信封里拿到本轮的
            // 思考与工具汇总。只能在一轮结束时整段补上（信封是一次性的），
            // 但足以让群聊/讨论组里的气泡有过程可展开。
            if (emitsJsonEnvelope(opts.agentName)) {
              if (capped) {
                // 信封是一次性 JSON：缓冲被截断就等于这份结果拿不全，解析出来
                // 的只会是半截对象或日志行。判失败，别把残片当回答。
                if (activity && activity.finish()) emitActivity();
                reject(new TurnIncompleteError(
                  "output_truncated",
                  "",
                  `${opts.agentName} envelope exceeded ${bufferLimit} bytes`,
                ));
                return;
              }
              const details = extractOpenclawEnvelopeDetails(stdout);
              if (details.reasoning) deliverReasoning(details.reasoning);
              if (details.tools && details.tools.length > 0 && opts.onActivity) {
                opts.onActivity(details.tools.map((tool) => {
                  const cls = classifyTool(tool.name);
                  return {
                    id: `oc-${tool.name}`,
                    kind: cls.kind,
                    label: cls.label,
                    ...(tool.calls > 1 ? { detail: `共 ${tool.calls} 次` } : {}),
                    status: tool.failures > 0 && tool.failures >= tool.calls ? "failed" : "done",
                  } satisfies ActivityStep;
                }));
              }
              // 信封自己说了「这一轮没正常收尾」时（aborted / length /
              // content_filter / error），正文往往只有半句话 —— 必须如实报失败，
              // 否则前端会把它当一次完整回答展示（「话说一半就结束了」）。
              const health = openclawTurnHealth(stdout);
              if (health.incomplete) {
                if (activity && activity.finish()) emitActivity();
                reject(new TurnIncompleteError(
                  "agent_incomplete",
                  extractOpenclawReply(stdout),
                  `openclaw turn did not finish: ${health.detail ?? "unknown"}`,
                ));
                return;
              }
            }
            if (opts.agentName === "hermes" && reasoningChunks === 0) {
              console.warn(
                "hermes 未上报思考：本机 hermes 的 stream_json.py 可能还没打补丁，"
                + "跑一次 scripts/patch-hermes-reasoning.py（hermes 升级会被覆盖）",
              );
            }
            // 非流式路径：stdout 本身就是回答（信封型另有上面那条分支），缓冲
            // 上限即回答长度上限，保持「按字节截断」的老语义；流式路径的正文
            // 是解析出来的，所以缓冲满了也照样给出完整正文（见上一条分支）。
            resolve(opts.agentName === "openclaw" ? extractOpenclawReply(stdout) : stdout);
          }
        } else {
          // 失败退出同样收尾：failed 帧要带着完整步骤，别留下「进行中」。
          if (activity && activity.finish()) emitActivity();
          // 只把 stderr 拼进错误串，会让「进程把原因打在 stdout」的 CLI 变成一句
          // 「没有任何输出」：kimi 的欢迎横幅（含 `send /login`）、opencode 的
          // usage dump（未知参数）都在 stdout 上。取尾部 800 字符就够
          // classifyTurnError 认出 login / unknown option，也不会让错误串膨胀
          // （几 MB 的 stdout 不能整个塞进去）。
          const tail = stdout.length > 800 ? stdout.slice(-800) : stdout;
          reject(new Error(
            `agent exited with code ${code}: ${stderr}${tail ? `\n[stdout tail]\n${tail}` : ""}`,
          ));
        }
      });
    });

    child.stdin.on("error", () => {
      // The agent may exit before reading all stdin (EPIPE); the close
      // handler reports the real outcome.
    });
    if (argvMode) {
      child.stdin.end();
    } else {
      child.stdin.write(opts.prompt);
      child.stdin.end();
    }
  });
}
