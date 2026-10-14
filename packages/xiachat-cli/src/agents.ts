import { spawn } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { access, constants } from "node:fs/promises";
import { delimiter, dirname, join } from "node:path";
import { StringDecoder } from "node:string_decoder";

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
  maxOutputBytes?: number;
  // Working directory for the agent CLI process. The ops agent sets this to
  // its workdir so `opencode run` picks up the ops AGENTS.md system prompt.
  cwd?: string;
  onChunk?: (chunk: string) => void;
  signal?: AbortSignal;
  // 会话隔离键（可选）。多个会话形态共用同一个 agent 二进制，而 agent CLI
  // 默认把它们全塞进自己的「主会话」：openclaw 的默认键就是 agent:main:main，
  // 于是讨论组、普通群聊、私聊共享一串上下文。讨论组里达成过的共识（例如
  // 「没有主题就回『等待主题发布』占位」）会被后续群聊继承，群聊于是答非
  // 所问。支持显式会话键的 agent（当前只有 openclaw 的 --session-key）按
  // 「会话形态 + 会话 id」分桶；不支持的 agent 忽略此参数。
  sessionKey?: string;
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
type AgentArgvStrategy = (prompt: string, model?: string, sessionKey?: string) => string[];

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

/** Renders the model-selection flags for one agent. */
export function buildModelArgs(name: string, model: string | undefined): string[] {
  if (!model) return [];
  const route = splitModelRoute(model);
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

const agentArgvStrategies: Readonly<Record<string, AgentArgvStrategy>> = {
  claude: (prompt, model) => ["-p", prompt, ...buildModelArgs("claude", model)],
  codex: (prompt, model) => ["exec", prompt, ...buildModelArgs("codex", model)],
  opencode: (prompt, model) => ["run", prompt, ...buildModelArgs("opencode", model)],
  // --session-key 把这一轮绑到「会话形态 + 会话 id」的独立会话上。不带它时
  // openclaw 一律走默认主会话（agent:main:main），群聊会被讨论组的上下文
  // 带跑（见 RunAgentTurnOpts.sessionKey）。
  openclaw: (prompt, model, sessionKey) => [
    "agent", "--local", "--json", "-m", prompt,
    ...(sessionKey ? ["--session-key", sessionKey] : []),
    ...buildModelArgs("openclaw", model),
  ],
  hermes: (prompt, model) => ["-z", prompt, ...buildModelArgs("hermes", model)],
};

// Windows CreateProcess caps the whole command line near 32k chars (and
// .cmd wrappers shrink that further), so very long prompts cannot ride as
// argv — past this cap every agent falls back to stdin mode regardless of
// strategy.
const ARGV_PROMPT_MAX_CHARS = 8000;

// Returns the argv for an agent turn; an empty array means stdin mode
// (unknown agent, or a prompt too long for argv).
export function buildAgentArgs(
  name: string,
  prompt: string,
  model?: string,
  sessionKey?: string,
): string[] {
  const strategy = agentArgvStrategies[name];
  if (!strategy || prompt.length > ARGV_PROMPT_MAX_CHARS) return [];
  return strategy(prompt, model, sessionKey);
}

const DEFAULT_TIMEOUT_MS = 120_000;
const DEFAULT_MAX_OUTPUT_BYTES = 65_536;

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
      .map((p) => (typeof p === "object" && p !== null ? (p as { text?: unknown }).text : undefined))
      .filter((t): t is string => typeof t === "string" && t.length > 0);
    if (texts.length > 0) return texts.join("\n");
  }
  const visible = envelope.meta?.finalAssistantVisibleText;
  if (typeof visible === "string" && visible.length > 0) return visible;
  return undefined;
}

// Extracts the clean reply from an openclaw agent turn: the --json
// envelope when present, otherwise the ANSI/log-stripped plain stdout.
// Falls back to the raw trimmed text when both extractions come up empty
// so a turn never yields a blank reply.
export function extractOpenclawReply(raw: string): string {
  const fromJson = parseOpenclawJsonOutput(raw);
  if (fromJson !== undefined && fromJson.trim().length > 0) return fromJson.trim();
  const normalized = normalizeOpenclawOutput(raw);
  if (normalized.length > 0) return normalized;
  return raw.trim();
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

export function runAgentTurn(opts: RunAgentTurnOpts): Promise<string> {
  const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const maxBytes = opts.maxOutputBytes ?? DEFAULT_MAX_OUTPUT_BYTES;

  return new Promise<string>((resolve, reject) => {
    if (opts.signal?.aborted) {
      reject(new Error("agent turn aborted"));
      return;
    }

    // Strategy agents take the prompt as argv; everything else (unknown
    // agent, or a prompt past the argv length cap) keeps the legacy stdin
    // invocation with the model flag as the only argument.
    const strategyArgs = opts.agentName
      ? buildAgentArgs(opts.agentName, opts.prompt, opts.model, opts.sessionKey)
      : [];
    const argvMode = strategyArgs.length > 0;
    const agentArgs = argvMode
      ? strategyArgs
      : buildModelArgs(opts.agentName ?? "", opts.model);
    const { file, args } = buildSpawnInvocation(opts.execPath, agentArgs);
    const child = spawn(file, args, {
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
      cwd: opts.cwd,
    });

    // stdout accumulates raw bytes; decoding happens through a streaming
    // StringDecoder so code points split across chunk boundaries survive,
    // and truncation caps BYTES and backs off to a code-point boundary.
    const chunks: Buffer[] = [];
    let totalBytes = 0;
    let decoder = new StringDecoder("utf8");
    let stderr = "";
    let truncated = false;
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

    child.stdout.on("data", (buf: Buffer) => {
      if (truncated) return;
      if (totalBytes + buf.length > maxBytes) {
        const remaining = Math.max(0, maxBytes - totalBytes);
        const tail = trimIncompleteUtf8Tail(buf.subarray(0, remaining));
        if (tail.length > 0) {
          chunks.push(tail);
          totalBytes += tail.length;
          const delta = decoder.write(tail);
          if (delta) opts.onChunk?.(delta);
        }
        decoder = new StringDecoder("utf8"); // drop any held partial sequence
        truncated = true;
        return;
      }
      chunks.push(buf);
      totalBytes += buf.length;
      const delta = decoder.write(buf);
      if (delta) opts.onChunk?.(delta);
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
          resolve(opts.agentName === "openclaw" ? extractOpenclawReply(stdout) : stdout);
        } else {
          reject(new Error(`agent exited with code ${code}: ${stderr}`));
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
