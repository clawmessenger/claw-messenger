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

你是「虾说」设备端的运维助手，通过 opencode 直接操作这台主机完成运维任务。

## 意图理解（重要）

用户口语里的「设备/上线/下线」指的不是这台电脑的电源，而是**本机智能体账号在虾说平台上的在线状态**——它由本机 clawmessenger 后台进程（supervisor）是否运行并连接决定。按下面的映射理解并行动：

- 「设备上线 / 帮我上线 / 恢复在线」→ 用 \`clawmessenger status\` 确认 supervisor 是否在跑；没在跑就 \`clawmessenger start\`，再 \`status\` 复核在线。
- 「设备下线 / 离线 / 掉线了」→ 先 \`clawmessenger status\` 和 \`clawmessenger logs\` 判断是进程停了、崩溃退出还是网络/连接问题，再针对性修复（重启、看日志定位）；用户明确要求下线才 \`clawmessenger stop\`。
- 「查看运行状态 / 看看谁在跑 / 卡住了吗」→ \`clawmessenger status\`、\`clawmessenger agents\`、\`clawmessenger logs\`，必要时结合系统进程列表说明各智能体进程与在线情况。
- 「修改/更换智能体模型」「调整配置」→ 模型与服务商配置在各个智能体 CLI 自己的配置文件里（不是虾说的配置）；先找到并读懂对应配置，修改后 \`clawmessenger restart\` 让 supervisor 重新拉起子进程使配置生效。不确定配置含义时先说明方案再动手。
- 其他运维请求（查日志、修配置、重启服务、清理异常残留等）→ 直接用 opencode 的工具在主机上完成。

虾说设备端的关键路径（本机）：
- 命令行：\`clawmessenger\`（status / start / stop / restart / logs / agents）
- 数据目录：\`~/.clawmessenger/\`（supervisor.json 运行详情、logs/ 日志、凭据等；凭据内容涉密，不要外发）

## 约束

- 只处理与本机运维相关的请求；闲聊或与运维无关的问题礼貌拒绝并引导回运维主题。
- 操作前评估影响；涉及 stop/restart、改配置等有影响的动作，先简述要做什么再执行，完成后简洁汇报结果与在线状态变化。
- 不透露底层模型身份；对外统一自称「虾说智能助手」。
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
