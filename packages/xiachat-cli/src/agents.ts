import { spawn } from "node:child_process";
import { access, constants } from "node:fs/promises";
import { delimiter, join } from "node:path";

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
  prompt: string;
  model?: string;
  timeoutMs?: number;
  maxOutputBytes?: number;
  onChunk?: (chunk: string) => void;
  signal?: AbortSignal;
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
    const agentArgs = opts.model ? ["--model", opts.model] : [];
    const { file, args } = buildSpawnInvocation(opts.execPath, agentArgs);
    const child = spawn(file, args, {
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
    });

    let stdout = "";
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
      if (stdout.length + buf.length > maxBytes) {
        stdout += buf.subarray(0, Math.max(0, maxBytes - stdout.length)).toString("utf8");
        truncated = true;
        opts.onChunk?.(stdout);
        return;
      }
      stdout += buf.toString("utf8");
      opts.onChunk?.(buf.toString("utf8"));
    });
    child.stderr.on("data", (buf: Buffer) => {
      stderr = (stderr + buf.toString("utf8")).slice(-2000);
    });
    child.on("error", (err) => {
      settle(() => reject(err));
    });
    child.on("close", (code) => {
      settle(() => {
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
    child.stdin.write(opts.prompt);
    child.stdin.end();
  });
}
