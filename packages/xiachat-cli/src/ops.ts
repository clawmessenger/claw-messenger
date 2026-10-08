import { spawn } from "node:child_process";
import { mkdir, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { discoverAgents, type AgentInfo } from "./agents.js";

// Built-in agent name bound on every clawmessenger device. Distinct from the
// "opencode" CLI name: the server sees an agent node named "ops", while
// turns are executed by the locally installed opencode CLI.
export const OPS_AGENT_NAME = "ops";

// Ops turns run real machine-maintenance commands (inspect logs, edit
// configs, restart services); they routinely exceed the default 120s.
export const OPS_TURN_TIMEOUT_MS = 600_000;

export interface EnsureOpencodeDeps {
  discover?: () => Promise<AgentInfo[]>;
  spawnFn?: typeof spawn;
  log?: (message: string) => void;
}

function findOpencode(agents: AgentInfo[]): AgentInfo | undefined {
  return agents.find((a) => a.name === "opencode");
}

// opencode is a mandatory dependency of the ops agent. When missing we
// install it globally via npm (same flow as the operations-assistant
// service's opencode-starter). npm.cmd on Windows goes through cmd.exe
// like any other .cmd/.bat (CVE-2024-27980).
export async function ensureOpencodeInstalled(deps: EnsureOpencodeDeps = {}): Promise<AgentInfo> {
  const discover = deps.discover ?? (() => discoverAgents({}));
  const spawnFn = deps.spawnFn ?? spawn;
  const log = deps.log ?? ((message: string) => console.log(message));

  const preinstalled = findOpencode(await discover());
  if (preinstalled) return preinstalled;

  log("opencode not found; installing opencode-ai@latest globally via npm…");
  await new Promise<void>((resolve, reject) => {
    const child = spawnFn("npm", ["install", "-g", "opencode-ai@latest"], {
      stdio: "inherit",
      windowsHide: true,
    });
    child.on("error", reject);
    child.on("close", (code) => {
      if (code === 0) resolve();
      else reject(new Error(`npm install -g opencode-ai@latest exited with code ${code}`));
    });
  });

  // A fresh install may not be visible on the inherited PATH of this
  // process; re-discover so the caller gets a usable exec path. On PATH
  // refresh failure the ops branch falls back with a clear error.
  const installed = findOpencode(await discover());
  if (!installed) {
    throw new Error("opencode was installed but is still not discoverable on PATH; restart clawmessenger run");
  }
  return installed;
}

const OPS_AGENTS_MD = `# 虾说运维助手

你是本机的运维助手，通过 opencode 对这台机器执行运维任务。

职责：
- 查看系统与进程状态、检查日志、定位报错原因
- 修复配置、重启进程，让本机服务恢复正常
- 操作前评估影响，操作后简洁汇报结果

约束：
- 只处理与本机运维相关的请求
- 不透露底层模型身份；对外统一自称「虾说智能助手」
`;

// Ops turns run inside this workdir so opencode picks up AGENTS.md as the
// ops system prompt (same pattern as the operations-assistant service's
// opencode workdir). Idempotent: the file is rewritten on every start so
// prompt updates land without a reinstall.
export async function setupOpsWorkdir(dirOverride?: string): Promise<string> {
  const dir = dirOverride ?? path.join(os.homedir(), ".clawmessenger", "ops");
  await mkdir(dir, { recursive: true });
  await writeFile(path.join(dir, "AGENTS.md"), OPS_AGENTS_MD, "utf8");
  return dir;
}
