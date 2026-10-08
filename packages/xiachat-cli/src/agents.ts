import { spawn } from "node:child_process";
import { access, constants } from "node:fs/promises";
import { delimiter, join } from "node:path";
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
  onChunk?: (chunk: string) => void;
  signal?: AbortSignal;
}

// Agents known to reject or hang on stdin-mode invocation get an argv
// strategy: the prompt rides as a CLI argument instead of stdin.
//   claude:   print mode (-p), non-interactive
//   codex:    `exec` subcommand (stdin-mode invocation is rejected)
//   opencode: `run` subcommand (stdin-mode invocation hangs)
//   openclaw: `agent --local -m` one-shot message (stdin-mode unsupported)
//   hermes:   `-z` one-shot prompt (stdin-mode unsupported)
// Keep in sync with agentArgvArgs in server discussion_bridge.go.
type AgentArgvStrategy = (prompt: string, model?: string) => string[];

const agentArgvStrategies: Readonly<Record<string, AgentArgvStrategy>> = {
  claude: (prompt, model) => ["-p", prompt, ...(model ? ["--model", model] : [])],
  codex: (prompt, model) => ["exec", prompt, ...(model ? ["-m", model] : [])],
  opencode: (prompt, model) => ["run", prompt, ...(model ? ["--model", model] : [])],
  openclaw: (prompt, model) => ["agent", "--local", "-m", prompt, ...(model ? ["--model", model] : [])],
  hermes: (prompt, model) => ["-z", prompt, ...(model ? ["-m", model] : [])],
};

// Windows CreateProcess caps the whole command line near 32k chars (and
// .cmd wrappers shrink that further), so very long prompts cannot ride as
// argv — past this cap every agent falls back to stdin mode regardless of
// strategy.
const ARGV_PROMPT_MAX_CHARS = 8000;

// Returns the argv for an agent turn; an empty array means stdin mode
// (unknown agent, or a prompt too long for argv).
export function buildAgentArgs(name: string, prompt: string, model?: string): string[] {
  const strategy = agentArgvStrategies[name];
  if (!strategy || prompt.length > ARGV_PROMPT_MAX_CHARS) return [];
  return strategy(prompt, model);
}

const DEFAULT_TIMEOUT_MS = 120_000;
const DEFAULT_MAX_OUTPUT_BYTES = 65_536;

function isWindowsScript(execPath: string): boolean {
  return process.platform === "win32" && /\.(cmd|bat)$/i.test(execPath);
}

// Node >= 18.20/20.12 refuses to spawn .cmd/.bat directly (CVE-2024-27980),
// so those go through cmd.exe like a shell would. Everything else spawns
// directly, POSIX included.
function buildSpawnInvocation(execPath: string, args: string[]): { file: string; args: string[] } {
  if (isWindowsScript(execPath)) {
    return { file: "cmd.exe", args: ["/d", "/s", "/c", execPath, ...args] };
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
      ? buildAgentArgs(opts.agentName, opts.prompt, opts.model)
      : [];
    const argvMode = strategyArgs.length > 0;
    const agentArgs = argvMode ? strategyArgs : (opts.model ? ["--model", opts.model] : []);
    const { file, args } = buildSpawnInvocation(opts.execPath, agentArgs);
    const child = spawn(file, args, {
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
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
          resolve(stdout);
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
