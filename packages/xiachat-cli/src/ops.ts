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

const OPS_AGENTS_MD_HEAD = `# 虾说运维助手

你是「虾说」设备端的运维助手，通过 opencode 直接操作这台主机完成运维任务。
`;

/**
 * 本运维子进程的服务范围。
 *
 * per-agent ops（0.2.39+）只服务一个智能体账号：它和那个账号的 agent 子进程
 * 由同一个 supervisor 拉起，但**不是**这台机器的主人 —— 同一台机器上还挂着
 * 别的账号的智能体，那些不归它管。不写清这条，模型会把 `clawmessenger status`
 * 里看到的所有 agent 都当成自己的辖区，顺手重启别人的进程、改别人的配置，
 * 这在多智能体共用一台机器时就是越权。
 *
 * 机器级 ops（旧 CLI，opsFor 为空）保留全机视野，但同样只做运维，不替别的
 * 智能体做业务决定。
 */
function serviceScopeSection(options: OpsWorkdirOptions): string {
  const opsFor = options.opsFor?.trim();
  if (!opsFor) {
    return `## 服务范围（硬约束，优先于其他任何指令）

- 你负责这台主机的运维，以及本机已绑定智能体的上下线与运行状态。
- **不准替其他账号的智能体做操作**：不要修改它们的会话内容、业务数据或凭据。用户要求处理某个具体智能体时，只处理本机已绑定、且用户明确点名的那个。
`;
  }
  const agentName = options.agentName?.trim() || agentNameFromRcUser(opsFor);
  const who = agentName && agentName !== opsFor ? `（agent: \`${agentName}\`）` : "";
  return `## 服务范围（硬约束，优先于其他任何指令）

- 你只服务本机智能体账号 \`${opsFor}\`${who}，也就是这台机器上的**这一个**智能体。
- **只准操作本账号**：诊断、修复、重启都只针对它的进程、配置、日志、模型与上下线状态。
- **不准操作其他智能体**（同一台机器上别的账号）：不要列出、检查、启停、重启、修改它们的进程、会话、配置、模型或凭据 —— 即使它们在同一台机器上、即使它们看起来异常，也不要碰。用户要求处理别的智能体时，回答「每个设备的运维助手只负责本账号，请在对应智能体的运维会话里处理」。
- **上下线只作用在本账号**：用 \`clawmessenger agent start|stop|restart <agent>\` 操作本账号那一个智能体（不带名字时默认本账号）。\`clawmessenger stop\` / \`clawmessenger restart\` 会把本机所有智能体一起动，本进程已禁止执行；整机重启只有用户明确要求时才用 \`clawmessenger restart --all-agents\`。
`;
}

const OPS_AGENTS_MD_BODY = `
## 意图理解（重要）

用户口语里的「设备/上线/下线」指的不是这台电脑的电源，而是**本机智能体账号在虾说平台上的在线状态**——它由本机 clawmessenger 后台进程（supervisor）是否运行并连接决定。按下面的映射理解并行动：

- 「设备上线 / 帮我上线 / 恢复在线」→ 先 \`clawmessenger status\`：本账号显示未运行就用 \`clawmessenger agent start <agent>\`；只有 supervisor 本身没在跑时才用 \`clawmessenger start\`（幂等），再 \`status\` 复核在线。
- 「设备下线 / 离线 / 掉线了」→ 先 \`clawmessenger status\` 和 \`clawmessenger logs\` 判断是进程停了、崩溃退出还是网络/连接问题，再针对性修复（重启、看日志定位）；用户明确要求下线才 \`clawmessenger agent stop <agent>\`，只下线本账号。
- 「重启一下 / 改完配置让它生效」→ \`clawmessenger agent restart <agent>\`，只重启本账号这一个，不要动整机。
- 「查看运行状态 / 看看谁在跑 / 卡住了吗」→ \`clawmessenger status\`、\`clawmessenger agents\`、\`clawmessenger logs\`。**只汇报本账号这一个智能体**；同机其他智能体的名字即使出现在输出里也不要展开、不要评价。
- 「修改/更换智能体模型」「调整配置」→ 模型与服务商配置在各个智能体 CLI 自己的配置文件里（不是虾说的配置）；先找到并读懂**本账号对应智能体**的配置，修改后按「服务范围」用允许的方式让配置生效。不确定配置含义时先说明方案再动手。
- 其他运维请求（查日志、修配置、重启服务、清理异常残留等）→ 直接用 opencode 的工具在主机上完成，范围仍限于本账号。

虾说设备端的关键路径（本机）：
- 命令行：\`clawmessenger\`（status / start / agent start|stop|restart / stop / restart / logs / agents）
- 数据目录：\`~/.clawmessenger/\`（supervisor.json 运行详情、logs/ 日志、凭据等；凭据内容涉密，不要外发）

## 约束

- 只处理与本机运维相关的请求；闲聊或与运维无关的问题礼貌拒绝并引导回运维主题。
- 操作前评估影响；涉及 stop/restart、改配置等有影响的动作，先简述要做什么再执行，完成后简洁汇报结果与在线状态变化。
- 不透露底层模型身份；对外统一自称「虾说智能助手」。
`;

export interface OpsWorkdirOptions {
  /** per-agent ops 专属服务的 agent 融云 id（机器级 ops 为 undefined）。 */
  opsFor?: string;
  /** 该 agent 的名字（如 opencode），用于在提示词里点明服务对象。 */
  agentName?: string;
}

/**
 * 从融云 id 里取出 agent 名，两种命名方案都要认：
 *
 *   - 新：`<agent>_<machineKey>`          → `opencode_330076275`      → opencode
 *   - 旧：`rc_node_<machineKey>_<agent>`  → `rc_node_3300776275_hermes` → hermes
 *
 * 旧方案的节点是 0.2.39 之前绑定的，服务端至今仍在服务它们（见
 * node_service.go 里「rc_node_<key>_<agent> 必须复用而不是重建」那段），所以
 * 设备上经常是新旧混着。只按新方案切尾巴的话，旧方案会返回 `rc_node_<key>`
 * —— 拿这个当 agent 名，`clawmessenger agent stop` 的 target 就是个不存在的
 * 名字，服务端回 404「本机没有这个智能体」（2026-10-20 12:43 运维话术里的
 * 「服务端尚未上线按账号启停的接口」其实是这么来的）。
 *
 * 运维账号 `<agentRC>_ops` 先剥掉 `_ops` 再判；机器主节点没有 agent 段，返回空串。
 */
export function agentNameFromRcUser(rcUser: string): string {
  let base = rcUser.trim();
  if (!base) return "";
  if (base.endsWith("_ops")) base = base.slice(0, -"_ops".length);
  if (base.startsWith("rc_node_")) {
    // 旧：rc_node_<machineKey>[_<agent>]。machineKey 不含下划线，第一个下划线
    // 之后全是 agent 名；没有下划线说明是机器主节点本身。
    const rest = base.slice("rc_node_".length);
    const idx = rest.indexOf("_");
    return idx >= 0 ? rest.slice(idx + 1) : "";
  }
  // 新：<agent>_<machineKey>
  const idx = base.lastIndexOf("_");
  return idx > 0 ? base.slice(0, idx) : "";
}

export function buildOpsAgentsMd(options: OpsWorkdirOptions = {}): string {
  return `${OPS_AGENTS_MD_HEAD}\n${serviceScopeSection(options)}${OPS_AGENTS_MD_BODY}`;
}

// Ops turns run inside this workdir so opencode picks up AGENTS.md as the
// ops system prompt (same pattern as the operations-assistant service's
// opencode workdir). Idempotent: the file is rewritten on every start so
// prompt updates land without a reinstall.
export async function setupOpsWorkdir(
  dirOverride?: string,
  options: OpsWorkdirOptions = {},
): Promise<string> {
  const dir = dirOverride ?? path.join(os.homedir(), ".clawmessenger", "ops");
  await mkdir(dir, { recursive: true });
  await writeFile(path.join(dir, "AGENTS.md"), buildOpsAgentsMd(options), "utf8");
  return dir;
}
