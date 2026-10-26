import { Command } from "commander";
import { spawn, type ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import { closeSync, existsSync, mkdirSync, openSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createServer as createNetServer } from "node:net";
import * as os from "node:os";
import * as path from "node:path";
import { Keystore, type StoredCredentials } from "./keystore.js";
import {
  XiachatApi,
  type DeviceUpgradeIntent,
  type DeviceUpgradeStatus,
  type MachineAgentCredential,
} from "./api.js";
import { discoverAgents } from "./agents.js";
import { DEFAULT_SERVER_URL, machineId, defaultProcessedUidsPath, defaultUpgradeInflightPath } from "./config.js";
import { agentNameFromRcUser, ensureOpencodeInstalled, OPS_AGENT_NAME, OPS_TURN_TIMEOUT_MS, setupOpsWorkdir } from "./ops.js";
import {
  ALLOW_DUPLICATE_SUPERVISOR_ENV,
  clearSupervisorState,
  createDaemon,
  defaultDaemonPaths,
  findLiveSupervisor,
  formatDaemonStatus,
  supervisorSpawnArgv,
  writeSupervisorState,
  type Daemon,
  type DaemonPaths,
} from "./daemon.js";
import { installPaths, isSeaExecutable } from "./install.js";
import {
  applyStagedUpdate,
  checkForUpdate,
  healInstall,
  stageUpdate,
  stageUpdateFromMirrors,
  type FetchLike,
} from "./self-update.js";
import { cliVersion } from "./version.js";

// Deterministic per (ticket, machine): a retried claim after a lost
// response reuses the same key, so the server deduplicates instead of
// answering a misleading 409 "another device". Different machines derive
// different keys.
export function pairIdempotencyKey(ticket: string, mid: string): string {
  return `idem-${createHash("sha256").update(`${ticket}:${mid}`).digest("hex").slice(0, 24)}`;
}

/**
 * 是否允许并排跑第二份 supervisor（见 ALLOW_DUPLICATE_SUPERVISOR_ENV）。
 * 默认不允许 —— 两份 supervisor 会派生完全相同的融云节点 id。
 */
export function allowDuplicateSupervisor(env: NodeJS.ProcessEnv = process.env): boolean {
  const raw = (env[ALLOW_DUPLICATE_SUPERVISOR_ENV] ?? "").trim().toLowerCase();
  return raw === "1" || raw === "true" || raw === "yes" || raw === "on";
}

/**
 * per-agent 运维子进程的作用域（supervisor 拉起 ops@<agentRC> 时注入）。
 *
 * 为什么要有它：运维子进程和它服务的 agent 由同一个 supervisor 拉起，但它
 * **不是**这台机器的主人 —— 同一台机器上还挂着别的账号的智能体。运维助手
 * 通过 `clawmessenger` 命令操作设备，如果允许它跑整机级的 stop/restart，
 * 一次「帮我上下线」就会把别人的智能体一起拉下来。作用域存在即代表「只准
 * 动自己那一个账号」，CLI 因此在命令层直接拒绝连坐整机的动作。
 */
export const OPS_SCOPE_ENV = "CLAWMESSENGER_OPS_SCOPE";

/**
 * 「我是 supervisor 拉起来的子进程」标记（supervisor 的 launch() 注入）。
 *
 * 为什么需要：`clawmessenger stop` 停的是 supervisor —— 而 agent 子进程就是
 * supervisor 的孩子。子进程自己执行 stop，等于当场把自己的父进程连根拔掉：
 * 命令还没返回，承载这一轮的进程已经没了，于是**失败/取消终帧永远发不出去**，
 * 前端只看到「正在思考」一直转圈（2026-10-20 11:33 的现场：运维助手执行
 * `clawmessenger stop`，整台设备的智能体全部下线，气泡卡在 3m42s）。
 *
 * 这个标记只在 supervisor 拉起的子进程里存在：用户在终端手敲 stop 不受影响。
 */
export const SUPERVISED_ENV = "CLAWMESSENGER_SUPERVISED";

export function isSupervisedChild(env: NodeJS.ProcessEnv = process.env): boolean {
  return (env[SUPERVISED_ENV] ?? "").trim() === "1";
}

/**
 * 托管子进程里执行整机 stop/restart 的拒绝文案。
 *
 * 与运维作用域（OPS_SCOPE_ENV）不同，这条**对所有 agent 都成立**：不管是不是
 * 运维助手，只要这一轮正跑在被 stop 的那个 supervisor 下面，命令一执行就会
 * 杀掉自己。宁可让模型拿到一句明确的拒绝，也不要让用户对着转圈的气泡等。
 */
export function supervisedSelfKillRejection(
  action: "stop" | "restart",
  env: NodeJS.ProcessEnv = process.env,
): string | null {
  if (!isSupervisedChild(env)) return null;
  return `\`clawmessenger ${action}\` 不能在智能体自己的进程里执行：它会停掉拉起本轮的 supervisor，`
    + "把这一轮连同回复一起杀掉（前端将一直停在「正在思考」）。\n"
    + "要停整台设备，请在设备本机的终端（不在智能体会话里）执行；只想让某个智能体上下线，"
    + "用 `clawmessenger agent stop|start <name>`。";
}

export function opsScope(env: NodeJS.ProcessEnv = process.env): string {
  return (env[OPS_SCOPE_ENV] ?? "").trim();
}

/**
 * 整机级动作（stop / restart）在运维作用域内要给的拒绝文案；不在作用域内
 * 返回 null（普通用户与管理员不受影响）。
 */
export function wholeMachineActionRejection(
  action: "stop" | "restart",
  env: NodeJS.ProcessEnv = process.env,
): string | null {
  const scope = opsScope(env);
  if (!scope) return null;
  if (action === "stop") {
    return `运维助手只负责本账号（${scope}），不能整机下线：\`clawmessenger stop\` 会把本机所有智能体一起停掉。\n`
      + "恢复本账号在线请用 `clawmessenger start`（幂等，不会动已运行的智能体）；确实要整机下线，请在虾说设备页操作。";
  }
  return `运维助手只负责本账号（${scope}），不能整机重启：\`clawmessenger restart\` 会连本机其他智能体一起重启。\n`
    + "确实需要整机重启时，先用 `clawmessenger restart --all-agents`，并先向用户确认影响范围。";
}

/** 运维作用域内要求操作别的智能体时的拒绝文案（本地拦一道，服务端还有一道）。 */
export function opsScopeTargetRejection(scopeAgent: string, target: string): string {
  return `运维助手只负责 ${scopeAgent} 这一个账号，不能操作 ${target}。\n`
    + "其他智能体的启停请在对应账号的运维会话里处理。";
}

/**
 * 运维作用域内的 `status` 只暴露本账号那一条。
 *
 * 模型的回复常常是直接抄命令输出 —— 只要它看得见别的 agent，就迟早会顺着
 * 名单去「顺手检查/修一下」。这台机器上还有谁的智能体在跑，不归它知道，
 * 所以清单在命令层就砍掉，而不是靠提示词劝。
 */
export function scopeDaemonReport<T extends { agents: string[] }>(report: T, scope: string): T {
  const scopedId = scope.trim();
  if (!scopedId) return report;
  const agentName = agentNameFromRcUser(scopedId);
  const own = new Set<string>([`ops@${scopedId}`]);
  if (agentName) {
    own.add(agentName);
    own.add(`ops@${agentName}`);
  }
  return { ...report, agents: report.agents.filter((agent) => own.has(agent)) };
}

export interface BuildProgramOpts {
  keystore: Keystore;
  apiFactory: (serverUrl: string) => XiachatApi;
  stdout: NodeJS.WriteStream;
  // Injectable for tests; defaults to the machine-id minted next to the
  // keystore (see config.ts machineId).
  machineId?: () => string;
  // Injectable for tests; defaults to ensureOpencodeInstalled. Called on bind
  // (pair) and at supervisor start so a device that lacks opencode installs it.
  ensureOpencode?: () => Promise<unknown>;
  // Injectable for tests; defaults to child_process.spawn.
  spawnChild?: typeof spawn;
  // Injectable for tests; defaults to registering the supervisor's shutdown on
  // process SIGINT/SIGTERM.
  onSupervisorSignal?: (shutdown: () => void) => void;
  // Injectable for tests; the supervisor's start/stop poll cadence. Tests need
  // it short because the interval is registered before they install fake
  // timers, so advancing the fake clock would never fire it.
  supervisorControlPollMs?: number;
  // Injectable for tests; how long `pair` waits between device-authorization
  // polls. Defaults to the interval the server returned (2s). Tests must not
  // wait on a real clock.
  devicePollMs?: number;
  // Injectable for tests; defaults to setTimeout-backed sleep. Same reason as
  // devicePollMs: the device-code poll loop is a real-time wait.
  sleep?: (ms: number) => Promise<void>;
  // Injectable for tests; defaults to a real cross-platform supervisor daemon.
  daemon?: Daemon;
  // Injectable for tests; the state/pid files the supervisor writes about itself
  // (default: ~/.clawmessenger). Must agree with whatever `daemon` above uses.
  daemonPaths?: DaemonPaths;
  // Injectable for tests; defaults to acquireSupervisorLock (loopback port
  // singleton). Tests substitute an in-memory fake so parallel test processes
  // don't collide on the real port.
  acquireLock?: (port: number) => Promise<SupervisorLock | null>;
  // Injectable for tests; overrides the CLI version the supervisor reports for
  // itself (defaults to cliVersion(), which is the build-time injected value).
  version?: string;
  // Injectable for tests; the platform/arch the upgrade branch decides for.
  platform?: string;
  arch?: string;
  // Injectable for tests; how long the device must have been quiet before an
  // upgrade may restart it (see SUPERVISOR_UPGRADE_IDLE_MS).
  upgradeIdleMs?: number;
  // Injectable for tests; how many consecutive quiet polls are required
  // (see SUPERVISOR_UPGRADE_QUIET_POLLS).
  upgradeQuietPolls?: number;
  // Injectable for tests; the fetch used to pull a release asset from the
  // mirrors handed out by the server.
  upgradeFetch?: UpgradeFetch;
  // Injectable for tests; replaces the detached `__relaunch` helper spawn.
  relaunch?: (target: string, waitPid: number, passthroughArgs?: string[]) => void;
  // Injectable for tests; where the in-flight upgrade bookkeeping file lives.
  upgradeInflightPath?: string;
  // Injectable for tests; the installed exe path the upgrade swaps in place
  // (defaults to installPaths(...).target for the current platform).
  upgradeTarget?: string;
  /**
   * supervisor 自己的启动参数里要原样交给新 supervisor 的部分（`--model x`）。
   * 默认从 `process.argv.slice(2)` 剥出 `run` 之后的那一段。
   */
  passthroughArgs?: string[];
  /**
   * `__relaunch` 放锁之后等新 supervisor 报「锁到手」的上限。真实值 10s；
   * 测试注入一个很小的值，否则「永远不 ready」的用例会真的等 10 秒。
   */
  relaunchReadyMs?: number;
  /** 见 RELAUNCH_ALIVE_MS；测试注入很小的值以免每条用例真等 1 秒。 */
  relaunchAliveMs?: number;
  /**
   * 注入登录自启脚本的模板刷新（`Daemon.refreshStartupScript`）。
   *
   * 生产路径由真 exe 自己调（见 run 分支里的 isSeaExecutable 判据）；测试注入一个
   * 桩函数，既不用碰真实 APPDATA，也能断言「启动时确实检查过」。
   */
  refreshStartupScript?: () => Promise<{ updated: boolean; path: string; reason?: string } | null>;
}

/** 升级资产下载用的 fetch（self-update.ts 的 FetchLike，测试可替换）。 */
export type UpgradeFetch = FetchLike;

/**
 * supervisor 单例锁：一个监听中的回环端口，随进程生死由 OS 释放。
 */
export interface SupervisorLock {
  release(): void;
}

/**
 * 第二道单例防线（第一道是 daemon 的 pid 文件检查 findLiveSupervisor）。
 *
 * pid 文件防线拦不住「活着但没登记」的孤儿 supervisor：强杀、沙箱回收
 * 拉起方进程等场景会让 pid 文件指向死进程，而真正的旧 supervisor 还活着
 * （2026-10-20 现场：13:23 的后台树与 18:38 的前台 `run` 并存两小时，同一
 * 条私聊被两套 ops 子进程各答一遍，chat_stop 只能停掉其中一套）。端口锁
 * 不依赖任何文件状态的准确性：绑不上 127.0.0.1:port 就是有另一份在跑。
 *
 * 只在 supervisor 模式（不带 --agent）获取；agent 子进程由 supervisor
 * 拉起，天然共享租约。EADDRINUSE 之外的绑定失败（如权限）只告警不放行
 * 拦截 —— 可用性优先，pid 文件防线仍在。
 */
export const SUPERVISOR_LOCK_PORT = 41307;

/**
 * 升级接力时，新 supervisor 等父进程放掉端口锁的上限（毫秒）。
 *
 * 为什么需要：__relaunch 现在是「拿锁 → spawn → 确认站起来 → 放锁」。父进程握着
 * 锁的那几毫秒里，新进程去绑 127.0.0.1:41307 必然 EADDRINUSE，而
 * acquireSupervisorLock **不做重试** —— 于是它一启动就自杀（静默离线）。反过来
 * 先放锁再 spawn，release 与 spawn 之间又会留出窗口让第三份进程抢先。
 * 这个环境变量只由 __relaunch 传给子进程，其余入口一律 0（保持原来的严格语义）。
 */
export const RELAUNCH_LOCK_WAIT_ENV = "CLAWMESSENGER_LOCK_WAIT_MS";

/** __relaunch 交给新进程的等待窗口。 */
export const RELAUNCH_LOCK_WAIT_MS = 15_000;

/**
 * `__relaunch` 放锁之后等新 supervisor 交回「锁已到手」的上限。
 *
 * 必须大于 {@link RELAUNCH_LOCK_WAIT_MS} 的一半、且给子进程留出启动时间：子进程
 * 启动本身要几百毫秒（SEA 解包 + 读凭据），它在锁上的重试预算另有 15s。10s 是
 * 「够长到不会误判、够短到父进程不留垃圾」的折中。
 */
export const RELAUNCH_READY_MS = 10_000;

/**
 * spawn 之后观察「有没有立刻死掉」的窗口。
 *
 * 只需要抓住「exe 起不来就退」这类瞬时失败：进程在 spawn 成功那一刻就已经存在，
 * 真正的存活证据是子进程自己交回的 ready（{@link RELAUNCH_READY_MS}）。窗口给
 * 1s —— 足够覆盖慢机器的启动抖动，又不至于让父进程一直捏着端口锁。
 */
export const RELAUNCH_ALIVE_MS = 1_000;

/**
 * 新 supervisor 拿到端口锁之后写这个文件，`__relaunch` 用它当**交接成功的正向证据**。
 *
 * 为什么不能只看「子进程还活着」：活着只说明进程没崩，不说明它绑上了端口。父进程
 * 在自己还握着锁的时候看到「活着」就放锁并宣告成功，等于宣告了一件没发生的事 ——
 * 一旦子进程其实还在等锁或者稍后才死，设备就是「磁盘已换新、但没有 supervisor」，
 * 而且没有任何痕迹（审查报告 B1）。所以判据改成子进程自己写出来的「我持有锁」。
 */
export const RELAUNCH_READY_ENV = "CLAWMESSENGER_RELAUNCH_READY";

/** 解析 {@link RELAUNCH_LOCK_WAIT_ENV}；非正整数一律当作 0（不放宽）。 */
export function relaunchLockWaitMs(raw: string | undefined = process.env[RELAUNCH_LOCK_WAIT_ENV]): number {
  const parsed = Number.parseInt(raw ?? "", 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
}

export async function acquireSupervisorLock(port: number = SUPERVISOR_LOCK_PORT): Promise<SupervisorLock | null> {
  const server = createNetServer();
  return await new Promise<SupervisorLock | null>((resolve, reject) => {
    server.once("error", (err: NodeJS.ErrnoException) => {
      if (err.code === "EADDRINUSE") {
        resolve(null);
        return;
      }
      reject(err);
    });
    server.listen(port, "127.0.0.1", () => {
      server.removeAllListeners("error");
      // 监听 socket 只作租约，不接收连接。
      resolve({ release: () => server.close() });
    });
  });
}

/**
 * 进程是否还活着（跨平台）：`kill(pid, 0)` 只做存在性与权限检查，不真的发信号。
 * EPERM 表示「存在但没权限」，对升级接力来说等于还活着（保守：宁可不启动）。
 */
export function processAlive(pid: number): boolean {
  if (!Number.isFinite(pid) || pid <= 0) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return (err as NodeJS.ErrnoException).code === "EPERM";
  }
}

/**
 * 从 supervisor 自己的 argv 里取出要原样交给新 supervisor 的部分。
 *
 * `process.argv` 在 SEA 与 `node bin.ts` 下都是 `[execPath, (entry), ...cliArgs]`，
 * cliArgs 的第一个是子命令 `run`；helper 自己会补 `run`，所以剥掉它再透传
 * （`--model x` 必须活下来，否则升级一次就静默换了模型，见审查报告 M3）。
 */
export function supervisorPassthroughArgs(cliArgs: string[]): string[] {
  const rest = cliArgs[0] === "run" ? cliArgs.slice(1) : [...cliArgs];
  return rest.filter((arg) => typeof arg === "string" && arg.length > 0);
}

/** 解析 `__relaunch --args` 里的 JSON 数组；坏输入当作「没有额外参数」。 */
export function parsePassthroughArgs(raw: string | undefined): string[] {
  if (!raw) return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((item): item is string => typeof item === "string");
  } catch {
    return [];
  }
}

/**
 * 等一个刚 spawn 的子进程「没有立刻死掉」。
 *
 * 参数名带 `Ms` 是刻意的：这里曾经是 `(pid, sleep, attempts = 15, intervalMs = 200)`，
 * 调用方写了 `3_000` 想表达「3 秒」，实际是 3000 次 × 200ms = 10 分钟，于是父进程
 * 一直握着端口锁等子进程、子进程在等锁超时后自杀，每次远程升级都以「设备离线」收尾
 * （审查报告 B1）。**不要再引入位置参数语义靠记的接口。**
 *
 * 语义：进程立刻死 → 立刻返回 false（不空等）；活满整个窗口 → true。注意 true 只
 * 代表「没崩」，不代表它拿到了端口锁 —— 那个判据在 {@link waitForRelaunchReady}。
 */
export async function waitForAliveFor(
  pid: number | undefined,
  sleep: (ms: number) => Promise<void>,
  timeoutMs: number,
  intervalMs = 200,
): Promise<boolean> {
  if (!pid) return false;
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (!processAlive(pid)) return false;
    if (Date.now() >= deadline) return true;
    await sleep(intervalMs);
  }
}

/** `__relaunch` 与新版 supervisor 之间的交接凭据。 */
export interface RelaunchReady {
  pid: number;
  /** 新 supervisor 是否真的持有端口锁（false 只在绑定异常被放宽时出现）。 */
  locked: boolean;
  at: string;
}

/**
 * 新 supervisor 在**拿到端口锁之后**调用：往通道文件里写「我持有锁」。
 *
 * 没有指定通道（普通启动、开机自启、人手 `run`）时是零开销的空操作 —— 这个协议
 * 只为升级接力存在。
 */
export function signalRelaunchReady(locked: boolean, env: NodeJS.ProcessEnv = process.env): boolean {
  const target = env[RELAUNCH_READY_ENV];
  if (!target) return false;
  try {
    mkdirSync(path.dirname(target), { recursive: true });
    const payload: RelaunchReady = { pid: process.pid, locked, at: new Date().toISOString() };
    writeFileSync(target, `${JSON.stringify(payload)}\n`, { mode: 0o600 });
    return true;
  } catch {
    // 写不进通道文件不能影响 supervisor 本体运行。
    return false;
  }
}

/**
 * 等新 supervisor 交回「锁已到手」。返回 null 表示窗口内没等到。
 *
 * 必须校验 `pid`：通道文件是上一轮可能残留的，父进程 spawn 之前已经删过一次，
 * 这里再按 pid 兜一遍，避免把「上一轮的 ready」当成本轮的成功。
 */
export async function waitForRelaunchReady(
  readyPath: string,
  pid: number | undefined,
  sleep: (ms: number) => Promise<void>,
  timeoutMs: number,
  intervalMs = 100,
): Promise<RelaunchReady | null> {
  if (!pid) return null;
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    try {
      const parsed = JSON.parse(readFileSync(readyPath, "utf8")) as Partial<RelaunchReady>;
      if (parsed.pid === pid) {
        return { pid, locked: parsed.locked !== false, at: String(parsed.at ?? "") };
      }
    } catch {
      // 还没写 / 写了一半：下一轮再读。
    }
    if (Date.now() >= deadline) return null;
    await sleep(intervalMs);
  }
}

/** `__relaunch` 的各种失败都要留痕：设备离线且零痕迹是最难查的那种故障。 */
export interface RelaunchFailure {
  target: string;
  args: string[];
  /** 哪个分支失败的（pid_timeout / port_held / target_missing / not_started）。 */
  reason: string;
  detail?: string;
  at: string;
}

/** 交接通道文件放在安装目录旁（与 target 同处，测试里就是临时目录）。 */
export function relaunchReadyPath(target: string): string {
  return path.join(path.dirname(target), "relaunch-ready.json");
}

/**
 * helper 的 stdout/stderr 落到 `<安装目录>/relaunch.log`，返回 fd（失败返回 null）。
 *
 * 这是「新 supervisor 起来后到底报了什么」唯一的落地处：原来 helper 用
 * `stdio: "ignore"`，子进程一死就什么都不剩，运维只能看到「设备离线」。
 */
export function openRelaunchLog(target: string): number | null {
  try {
    const file = path.join(path.dirname(target), "relaunch.log");
    mkdirSync(path.dirname(file), { recursive: true });
    return openSync(file, "a");
  } catch {
    return null;
  }
}

/** 失败标记文件放在 state 目录（`~/.clawmessenger`），与 supervisor 状态同处。 */
export function relaunchFailureMarkerPath(): string {
  // 与 daemon 的 state 目录同一个约定：CLAWMESSENGER_HOME 可以整体挪走（测试、
  // 多实例调试）。marker 必须跟着走，否则测试会往真实家目录里写文件。
  const stateDir = process.env.CLAWMESSENGER_HOME || path.dirname(defaultUpgradeInflightPath());
  return path.join(stateDir, "relaunch-failed.json");
}

/**
 * 留一个失败标记，别让设备静默离线且无人知晓。
 *
 * 写入点覆盖**所有**失败分支（原来只有一支写了），读取点在 supervisor 下次启动时
 * （{@link consumeRelaunchFailureMarker}）—— 只有写没有读的标记等于没留痕迹。
 */
export function writeRelaunchFailureMarker(
  target: string,
  args: string[],
  reason: string,
  detail?: string,
): void {
  try {
    const marker = relaunchFailureMarkerPath();
    mkdirSync(path.dirname(marker), { recursive: true });
    const payload: RelaunchFailure = {
      target,
      args,
      reason,
      ...(detail ? { detail } : {}),
      at: new Date().toISOString(),
    };
    writeFileSync(marker, `${JSON.stringify(payload)}\n`, { mode: 0o600 });
  } catch {
    // 写标记失败不能反过来把 helper 弄崩。
  }
}

/**
 * supervisor 启动时把上一轮留下的失败标记读出来（打到 stderr，进 supervisor.log）
 * 然后删掉。闭环：`__relaunch` 写 → 下次启动读 → 人工/日志能看到「上次接力失败在
 * 哪一步」，而不是只剩一句「设备怎么离线了」。
 */
export function consumeRelaunchFailureMarker(
  onFound: (failure: RelaunchFailure) => void,
  markerPath: string = relaunchFailureMarkerPath(),
): RelaunchFailure | null {
  let failure: RelaunchFailure | null = null;
  try {
    failure = JSON.parse(readFileSync(markerPath, "utf8")) as RelaunchFailure;
  } catch {
    return null;
  }
  try {
    onFound(failure);
  } finally {
    try {
      rmSync(markerPath, { force: true });
    } catch {
      // 删不掉就留着，下次启动还会打一遍（可接受）。
    }
  }
  return failure;
}

/**
 * 把 commander 的「控制流异常」翻译成退出码；不是 commander 抛的返回 null。
 *
 * `buildProgram` 里调了 `.exitOverride()`：测试要在同一进程里 parse，commander 不能
 * 直接 `process.exit`。代价是它改用抛 `CommanderError` 来表达「版本/帮助已打印、该
 * 正常退出了」。生产入口（bin.ts）必须把它翻译回去 —— 漏了这一步的表现是
 * `clawmessenger --version` 打印正确版本却 **exit 1**，还把版本号往 stderr 再打一遍，
 * 于是发布流水线里那条 `--version == tag` 的校验永远失败（B1 修复轮发现）。
 */
export function commanderExit(err: unknown): { exitCode: number; printMessage: boolean } | null {
  const candidate = err as { code?: unknown; exitCode?: unknown; message?: unknown };
  if (typeof candidate?.code !== "string" || !candidate.code.startsWith("commander.")) return null;
  // 版本/帮助是成功路径：commander 已经把内容写进 stdout 了，再打一遍就是重复。
  const informational = candidate.code === "commander.version"
    || candidate.code === "commander.help"
    || candidate.code === "commander.helpDisplayed";
  const exitCode = typeof candidate.exitCode === "number" ? candidate.exitCode : informational ? 0 : 1;
  return { exitCode: informational ? 0 : exitCode, printMessage: !informational };
}

export function buildProgram(opts: BuildProgramOpts): Command {
  const program = new Command();
  const version = opts.version ?? cliVersion();
  program
    .name("clawmessenger")
    .description("User-device agent CLI over RongCloud IM")
    // 版本号来自 version.ts：构建期由 scripts/build-exe.mjs 用 esbuild
    // --define 注入（等价于 Go 的 -ldflags -X），没注入时退回与 package.json
    // 手工同步的字面量（version.test.ts 盯着两者一致）。SEA 打包后读不到外部
    // package.json，所以不能靠读文件。
    // 这个值会被 supervisor 上报给服务端，管理端「exe 版本」列与「是否可升级」
    // 都以它为准 —— 报错了设备就会永远显示「可升级」或永远升不上去。
    .version(version)
    // Tests parse this program in-process; commander would otherwise
    // process.exit on missing options and kill the vitest runner.
    .exitOverride();

  const daemon = opts.daemon ?? createDaemon();
  const daemonPaths = opts.daemonPaths ?? defaultDaemonPaths();

  program
    .command("register")
    .description("Register this device as a RongCloud AI node")
    .requiredOption("--name <name>", "node display name")
    .requiredOption("--ai-type <type>", "agent platform (claude, codex, opencode, ...)")
    .requiredOption("--server <url>", "Quukk server base URL")
    .option("--pairing-ticket <ticket>", "attribute the node to a workspace via pairing ticket")
    .action(async (cmdOpts: { name: string; aiType: string; server: string; pairingTicket?: string }) => {
      const api = opts.apiFactory(cmdOpts.server);
      try {
        const result = await api.register({
          name: cmdOpts.name,
          aiType: cmdOpts.aiType,
          nodeType: "ai",
          // Stable per-machine id: keeps rc_user_id (and thus node_id)
          // consistent across re-registers on one machine.
          macAddress: (opts.machineId ?? machineId)(),
          pairingTicket: cmdOpts.pairingTicket,
        });
        const creds: StoredCredentials = {
          nodeId: result.nodeId,
          token: result.token,
          credentialId: result.deviceCredentialTicket,
          serverUrl: cmdOpts.server,
        };
        opts.keystore.save(creds);
        opts.stdout.write(`registered ${result.nodeId}\n`);
      } catch (err) {
        const message = err instanceof Error ? err.message : String(err);
        if (message.includes("attribution")) {
          throw new Error(`${message}\nhint: provide --pairing-ticket (get one from your workspace admin)`);
        }
        throw err;
      }
    });

  // Shared by both pairing paths: register the machine node against the
  // session (the server attributes it to the session's workspace and
  // backfills the candidate list), then claim it for device credentials. The
  // reported agent list rides on register so the browser page can offer
  // exactly what this machine actually has.
  const registerAndClaim = async (
    api: XiachatApi,
    args: { ticket: string; server: string; name?: string; aiType: string; agents: string[] },
  ): Promise<string> => {
    try {
      const reg = await api.register({
        name: args.name ?? `${os.hostname()} ${args.aiType}`,
        aiType: args.aiType,
        nodeType: "ai",
        macAddress: (opts.machineId ?? machineId)(),
        pairingTicket: args.ticket,
        agents: args.agents,
      });
      opts.keystore.save({
        nodeId: reg.nodeId,
        token: reg.token,
        credentialId: reg.deviceCredentialTicket,
        serverUrl: args.server,
      });
      opts.stdout.write(`registered ${reg.nodeId}\n`);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      if (message.includes("attribution")) {
        throw new Error(`${message}\nhint: provide --pairing-ticket (get one from your workspace admin)`);
      }
      throw err;
    }
    const idemKey = pairIdempotencyKey(args.ticket, (opts.machineId ?? machineId)());
    const result = await api.claimPairing(args.ticket, "", idemKey);
    if (result.session?.status === "claimed" && !result.deviceSecret) {
      throw new Error("ticket already claimed by another device");
    }
    opts.keystore.save({
      nodeId: result.nodeId,
      token: "",
      credentialId: result.deviceCredentialId,
      deviceSecret: result.deviceSecret,
      serverUrl: args.server,
    });
    return result.nodeId;
  };

  program
    .command("pair")
    .description(
      "Pair this device. By default it prints a URL for browser authorization; --ticket keeps the manual ticket flow",
    )
    .option("--ticket <ticket>", "manual pairing ticket (pt_...) from the web app instead of browser authorization")
    .option("--server <url>", "Quukk server base URL", DEFAULT_SERVER_URL)
    .option("--name <name>", "node display name")
    .option("--ai-type <type>", "agent platform (claude, codex, opencode, ...)", "opencode")
    .option("--no-start", "do not start the background supervisor after authorization")
    .action(
      async (cmdOpts: {
        ticket?: string;
        server: string;
        name?: string;
        aiType: string;
        start: boolean;
      }) => {
        const api = opts.apiFactory(cmdOpts.server);
        const ensureOpencode = opts.ensureOpencode ?? ensureOpencodeInstalled;
        // opencode is the default runtime for the built-in ops (运维) agent and
        // a common default agent. Install it up front on devices that lack it
        // so it is available to bind and shows up in the reported agent list.
        // Best effort: an install failure must not block pairing.
        try {
          await ensureOpencode();
        } catch (err) {
          console.warn("opencode install failed:", err instanceof Error ? err.message : String(err));
        }
        const foundAgents = await discoverAgents({});
        const agentNames = foundAgents.map((a) => a.name);
        const pairing = {
          server: cmdOpts.server,
          name: cmdOpts.name,
          aiType: cmdOpts.aiType,
          agents: agentNames,
        };

        // Manual path: a ticket someone copied out of the web app. Unchanged
        // behaviour, including not starting the supervisor.
        if (cmdOpts.ticket) {
          const nodeId = await registerAndClaim(api, { ...pairing, ticket: cmdOpts.ticket });
          opts.stdout.write(`paired as ${nodeId}\n`);
          return;
        }

        // Device-code path: the terminal asks for a one-time session and the
        // human authorizes it in a browser. No ticket ever reaches the user.
        const device = await api.startDevicePairing({
          machineId: (opts.machineId ?? machineId)(),
          hostname: os.hostname(),
          platform: process.platform,
          agents: agentNames,
        });
        opts.stdout.write(
          [
            "",
            "请在浏览器打开以下链接，完成这台设备的接入授权：",
            "",
            `  ${device.verifyUrl}`,
            "",
            `如果页面要求输入配对码，请输入：${device.userCode}`,
            "（链接 15 分钟内有效，等待授权中…）",
            "",
          ].join("\n"),
        );
        const nodeId = await registerAndClaim(api, { ...pairing, ticket: device.ticket });
        opts.stdout.write(`paired as ${nodeId}\n`);

        const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms)));
        const intervalMs = opts.devicePollMs ?? device.intervalSeconds * 1000;
        const parsedDeadline = Date.parse(device.expiresAt);
        const deadline = Number.isFinite(parsedDeadline) ? parsedDeadline : Date.now() + 15 * 60 * 1000;
        let bound: { agent: string; nodeId: string }[] = [];
        for (;;) {
          const state = await api.pollDevicePairing(device.ticket);
          if (state.boundAgents.length > 0) {
            bound = state.boundAgents;
            break;
          }
          if (state.status === "expired" || state.status === "cancelled") {
            throw new Error("授权已过期或被拒绝，请重新运行 clawmessenger pair");
          }
          if (Date.now() >= deadline) {
            throw new Error("授权已过期，请重新运行 clawmessenger pair");
          }
          await sleep(intervalMs);
        }
        opts.stdout.write(`授权完成，已接入：${bound.map((b) => b.agent).join(", ")}\n`);
        if (cmdOpts.start) {
          // daemon.start is idempotent: an already-running supervisor only
          // reports itself, so re-pairing a live device is safe.
          await daemon.start({});
        }
      },
    );

  program
    .command("login")
    .description("Refresh the IM token for the stored node")
    .action(async () => {
      const creds = opts.keystore.load();
      if (!creds) throw new Error("no credentials; run clawmessenger register or pair first");
      const api = opts.apiFactory(creds.serverUrl);
      const { token } = await api.refreshToken(creds.nodeId);
      opts.keystore.save({ ...creds, token });
      opts.stdout.write("token refreshed\n");
    });

  program
    .command("agents")
    .description("List agent CLIs discoverable on this machine's PATH")
    .action(async () => {
      const found = await discoverAgents({});
      // 运维作用域内只看得到自己那一个智能体：同一台机器上还有谁装了 CLI，
      // 不归它知道（见 scopeDaemonReport 的注释）。
      const scope = opsScope();
      const visible = scope ? found.filter((a) => a.name === agentNameFromRcUser(scope)) : found;
      if (visible.length === 0) {
        opts.stdout.write(scope
          ? `no agent CLI found for this ops scope (${scope})\n`
          : "no agent CLIs found on PATH\n");
        return;
      }
      for (const a of visible) opts.stdout.write(`${a.name}\t${a.path}\n`);
    });

  // 按账号启停：运维助手唯一需要的「上下线」动作，不影响同机其他智能体。
  // 服务端写进与管理端同一张 control 表，supervisor 下一次轮询收敛。
  program
    .command("agent <action> [name]")
    .description("Start / stop / restart ONE agent on this device, leaving the others alone")
    .action(async (action: string, name?: string) => {
      const normalized = action.trim().toLowerCase();
      if (normalized !== "start" && normalized !== "stop" && normalized !== "restart") {
        opts.stdout.write(`unknown action '${action}'; try: start | stop | restart\n`);
        process.exitCode = 1;
        return;
      }
      // 运维子进程带着自己的节点凭据（CLAWMESSENGER_AGENT_CREDS），普通用户
      // 走 keystore；两者都能过服务端的设备凭据校验。
      const envCreds = process.env.CLAWMESSENGER_AGENT_CREDS;
      const creds = (envCreds ? (JSON.parse(envCreds) as StoredCredentials) : undefined) ?? opts.keystore.load();
      if (!creds) {
        opts.stdout.write("not registered; run clawmessenger pair first\n");
        process.exitCode = 1;
        return;
      }
      if (!creds.credentialId || !creds.deviceSecret) {
        opts.stdout.write("no device credential; run clawmessenger pair on this device first\n");
        process.exitCode = 1;
        return;
      }
      const scope = opsScope();
      const scopeAgent = scope ? agentNameFromRcUser(scope) : "";
      const target = (name ?? scopeAgent).trim();
      if (!target) {
        opts.stdout.write("which agent? e.g. clawmessenger agent restart opencode\n");
        process.exitCode = 1;
        return;
      }
      // 本地先拦一道（服务端还会再校验一次）：per-agent 运维只能动自己那一个。
      if (scopeAgent && target !== scopeAgent) {
        opts.stdout.write(opsScopeTargetRejection(scopeAgent, target));
        process.exitCode = 1;
        return;
      }
      const api = opts.apiFactory(creds.serverUrl);
      try {
        const result = await api.setDeviceControl(
          creds.nodeId ?? "",
          creds.credentialId,
          creds.deviceSecret,
          target,
          normalized,
        );
        const verb = normalized === "stop" ? "停止" : normalized === "restart" ? "重启" : "启动";
        opts.stdout.write(
          `${result.agent}：${verb}指令已下发（version ${result.version}）。`
            + "supervisor 每 10 秒同步一次，可用 `clawmessenger status` 复核。\n",
        );
      } catch (err) {
        const message = err instanceof Error ? err.message : String(err);
        // 404 有两种，别混成一句话：
        //   1) 路由根本不存在（旧服务端 / nginx），响应体不是 JSON，我们拿不到
        //      message，报错里只有 "HTTP 404"；
        //   2) 路由在，但服务端按名字找不到这个智能体（「本机没有这个智能体」），
        //      响应体带 message，报错里是 "HTTP 404: ..."。
        // 以前一律按 (1) 播报「接口未上线」，2026-10-20 12:43 那次运维就把
        // 「target 名字算错了」当成「服务端要升级」，白等了一轮。
        const serverSaid = /HTTP \d{3}: /.test(message);
        if (message.includes("HTTP 404") && !serverSaid) {
          // 服务端还没上 control/set：别让运维助手退回整机 stop（会连坐别的账号）。
          opts.stdout.write(
            "服务端还不支持按账号启停（/api/claw/device/control/set 未上线）。"
              + "请先升级服务端；临时恢复本账号在线可用 `clawmessenger start`，不要用整机 stop。\n",
          );
        } else if (message.includes("HTTP 404")) {
          opts.stdout.write(
            `服务端没有找到智能体 \`${target}\`（它不在本机已绑定的清单里）。`
              + `本账号对应的名字是 \`${scopeAgent || "?"}\`；`
              + "可先用 `clawmessenger agents` 核对可用名字。\n",
          );
        } else {
          opts.stdout.write(`${message}\n`);
        }
        process.exitCode = 1;
      }
    });

  program
    .command("run")
    .description(
      "Connect to RongCloud IM and dispatch agent turns. Without --agent, run every agent bound to this device (supervisor mode).",
    )
    .option("--agent <name>", "agent CLI name; must be discoverable on PATH (see clawmessenger agents)")
    .option("--model <model>", "model override passed to the agent CLI")
    .option("--ops-suffix <suffix>", "ops agent only: per-agent workdir suffix (supervisor passes the served agent's RongCloud id)")
    .action(async (cmdOpts: { agent?: string; model?: string; opsSuffix?: string }) => {
      // Supervisor-spawned children carry their agent-specific credentials
      // in the environment; a plain `clawmessenger run` never sees them otherwise
      // because each child overwrites nothing in the shared keystore.
      const envCreds = process.env.CLAWMESSENGER_AGENT_CREDS;
      const parsedEnvCreds = envCreds ? (JSON.parse(envCreds) as StoredCredentials) : undefined;
      const creds = parsedEnvCreds ?? opts.keystore.load();
      if (!creds) throw new Error("no credentials; run clawmessenger register or pair first");
      const api = opts.apiFactory(creds.serverUrl);
      if (!cmdOpts.agent) {
        // `start` 已经会拒绝重复启动，但 supervisor 还会被别的入口直接拉起：
        // 开机自启的 .vbs、守护脚本的 `-Action run`、用户在终端里手敲
        // `clawmessenger run`。这些入口全部绕过 start，于是能在已有一份在跑时
        // 再起一份 —— 两份 supervisor 派生**完全相同**的融云节点 id，互相踢线
        // （设备列表一会在线一会离线），同一条私聊还会被两套各答一次
        // （重复回复）。所以 supervisor 自己必须再拦一道。
        const conflict = findLiveSupervisor(daemonPaths);
        if (conflict && conflict.pid !== process.pid && !allowDuplicateSupervisor()) {
          const agents = conflict.state?.agents.length ? `, agents: ${conflict.state.agents.join(", ")}` : "";
          opts.stdout.write(
            `another clawmessenger supervisor is already running (PID ${conflict.pid}${agents}).\n` +
              "refusing to start a second one: both would log in as the same RongCloud nodes, so the\n" +
              "device would flip online/offline and every private message would be answered twice.\n\n" +
              "  stop it       : clawmessenger stop\n" +
              "  or restart it : clawmessenger restart\n" +
              `  start anyway  : set ${ALLOW_DUPLICATE_SUPERVISOR_ENV}=1\n`,
          );
          process.exitCode = 1;
          return;
        }
        // pid 文件查不到不等于没有：孤儿 supervisor（pid 文件被后来者覆盖指向
        // 死进程）还活着时会放行出第二套进程树。端口锁与进程同生死，兜底这道缝。
        // 是否真的拿到单例锁（__relaunch 的交接凭据要看这个值）。
        let lockHeld = false;
        if (!allowDuplicateSupervisor()) {
          const acquire = opts.acquireLock ?? acquireSupervisorLock;
          const lockSleep = opts.sleep ?? ((ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms)));
          let lock: SupervisorLock | null = null;
          let lockCheckError = false;
          // 升级接力出来的新 supervisor 会撞见「父进程还差几毫秒才放锁」：父进程
          // 刻意先 spawn 再 release（见 __relaunch）。这里给一段有限等待，等待期间
          // 仍按 200ms 间隔重试；普通启动（环境变量为空）走的是原来的「一次定生死」。
          const lockWaitMs = relaunchLockWaitMs();
          const lockDeadline = Date.now() + lockWaitMs;
          for (;;) {
            try {
              lock = await acquire(SUPERVISOR_LOCK_PORT);
            } catch (err) {
              // 非 EADDRINUSE 的绑定失败（端口段被策略占掉、权限不足……）。
              //
              // 这条路径以前是「告警后静默放行」，而放行的代价恰恰是端口锁存在的
              // 理由：**pid 文件查不到不等于没有**（见上面 906 行的孤儿 supervisor），
              // 端口锁是唯一能证明「没有第二棵进程树」的东西。跳过它 = 同一条私聊
              // 被两套各答一遍。所以不再静默：把「没有端口锁」和后果直接写出来，
              // 让运维自己决定要不要用显式逃生阀（审查报告 R4）。
              lockCheckError = true;
              const reason = err instanceof Error ? err.message : String(err);
              console.warn("supervisor lock check failed:", reason);
              opts.stdout.write(
                `supervisor: 端口锁探测失败（${reason}）。\n` +
                  "将会在**没有**端口锁的情况下继续启动：如果另一棵进程树里还活着一份孤儿\n" +
                  "supervisor（它的 pid 文件已经指向死进程，所以上面的检查查不出来），同一条私聊\n" +
                  "会被回答两次，设备也会反复上下线。\n\n" +
                  "  find and stop it: clawmessenger stop (or kill the stale clawmessenger.exe processes)\n" +
                  `  start anyway     : set ${ALLOW_DUPLICATE_SUPERVISOR_ENV}=1\n`,
              );
              break;
            }
            if (lock || lockWaitMs <= 0 || Date.now() >= lockDeadline) break;
            await lockSleep(200);
          }
          lockHeld = lock !== null;
          if (!lock && !lockCheckError) {
            opts.stdout.write(
              "another clawmessenger supervisor is already running (singleton lock held, no live pid file —\n" +
                "an orphaned supervisor from an earlier start is still alive). Refusing a second one: both\n" +
                "would log in as the same RongCloud nodes and answer every private message twice.\n\n" +
                "  find and stop it: clawmessenger stop (or kill the stale clawmessenger.exe processes)\n" +
                `  start anyway     : set ${ALLOW_DUPLICATE_SUPERVISOR_ENV}=1\n`,
            );
            process.exitCode = 1;
            return;
          }
        }
        // 上一轮升级接力的失败标记：__relaunch 写、这里读，然后删掉。只写不读的
        // 标记等于没留痕迹 —— 设备离线时运维至少能看到「上次接力卡在哪一步」。
        // 放在守护检查之后：抢锁失败被拒的那次启动不该把证据吃掉。
        consumeRelaunchFailureMarker((failure) => {
          console.error(
            `supervisor: the previous relaunch failed (reason=${failure.reason}): ${failure.detail ?? "no detail"} ` +
              `[target=${failure.target} at=${failure.at}]`,
          );
        });
        // 存量设备磁盘上的登录脚本是**旧模板**（没有自愈块），而远程升级走的是
        // spawn `target run`，不会重写 VBS —— 不在这里补一刀的话，S4 对被远程升级
        // 的那批设备等于没修（审查报告 M3a）。只在真的安装版 exe 里做：`node
        // bin.ts run` 不该去动用户的开机自启脚本。
        const refreshStartup =
          opts.refreshStartupScript ??
          (isSeaExecutable(process.argv[1]) ? () => daemon.refreshStartupScript() : null);
        if (refreshStartup) {
          try {
            const refreshed = await refreshStartup();
            if (refreshed?.updated) {
              opts.stdout.write(`supervisor: refreshed ${refreshed.path} (added the self-heal block)\n`);
            }
          } catch (err) {
            // 补脚本失败不能挡住 supervisor 起来。
            console.warn("startup script refresh failed:", err instanceof Error ? err.message : String(err));
          }
        }
        // 交接凭据必须是「锁已到手」之后才写：__relaunch 用它当成功判据，
        // 提前写等于又回到「活着就算成功」的老问题（审查报告 B1）。
        signalRelaunchReady(lockHeld);
        await runSupervisor({
          opts,
          creds,
          api,
          model: cmdOpts.model,
          // supervisor 是唯一上报 exe 版本的进程（它是机器级进程，exe 也是
          // 机器级安装物），版本号必须从 buildProgram 传进来 —— 它自己读不到
          // commander 的 version。
          version,
          ...(opts.platform !== undefined ? { platform: opts.platform } : {}),
          ...(opts.arch !== undefined ? { arch: opts.arch } : {}),
          ...(opts.upgradeIdleMs !== undefined ? { upgradeIdleMs: opts.upgradeIdleMs } : {}),
          ...(opts.upgradeQuietPolls !== undefined ? { upgradeQuietPolls: opts.upgradeQuietPolls } : {}),
          ...(opts.upgradeFetch !== undefined ? { upgradeFetch: opts.upgradeFetch } : {}),
          ...(opts.relaunch !== undefined ? { relaunch: opts.relaunch } : {}),
          ...(opts.passthroughArgs !== undefined
            ? { passthroughArgs: opts.passthroughArgs }
            : { passthroughArgs: supervisorPassthroughArgs(process.argv.slice(2)) }),
          ...(opts.upgradeInflightPath !== undefined
            ? { upgradeInflightPath: opts.upgradeInflightPath }
            : {}),
          ...(opts.upgradeTarget !== undefined ? { upgradeTarget: opts.upgradeTarget } : {}),
          ...(opts.supervisorControlPollMs !== undefined
            ? { controlPollIntervalMs: opts.supervisorControlPollMs }
            : {}),
          // 不管是 `clawmessenger start` 拉起的，还是用户在终端里直接 `run`，
          // supervisor 都把自己的运行状态写下来 —— 于是 status / stop 都能看到它，
          // 顺带实现「前台跑着时 start 会拒绝，避免两份 supervisor 抢同一批 agent」。
          onStarted: (agents) => {
            writeSupervisorState(
              {
                pid: process.pid,
                startedAt: Date.now(),
                execPath: process.execPath,
                cwd: process.cwd(),
                ...(cmdOpts.model ? { model: cmdOpts.model } : {}),
                agents,
              },
              daemonPaths,
            );
          },
          onStopped: () => clearSupervisorState(daemonPaths),
        });
        return;
      }
      // The built-in ops agent runs on the local opencode CLI (auto-installed
      // when missing); it is never skipped by PATH discovery and runs with a
      // longer turn timeout and its own ops workdir (AGENTS.md system prompt).
      let agentExecPath: string;
      let agentName: string;
      let turnTimeoutMs: number | undefined;
      let cwd: string | undefined;
      let ensureRuntime: (() => Promise<void>) | undefined;
      if (cmdOpts.agent === OPS_AGENT_NAME) {
        const opencode = await ensureOpencodeInstalled();
        // per-agent 运维子进程（supervisor 传 --ops-suffix <agentRC>）用独立
        // workdir：opencode 的会话状态按目录隔离，共享目录会让多个 ops
        // 子进程互相踩会话文件。suffix 只能是 rc id 字符集，防目录穿越。
        let workdirOverride: string | undefined;
        if (cmdOpts.opsSuffix) {
          if (!/^[A-Za-z0-9_-]+$/.test(cmdOpts.opsSuffix)) {
            throw new Error(`invalid --ops-suffix ${JSON.stringify(cmdOpts.opsSuffix)}: only [A-Za-z0-9_-] allowed`);
          }
          workdirOverride = path.join(os.homedir(), ".clawmessenger", "ops", cmdOpts.opsSuffix);
        }
        const workdir = await setupOpsWorkdir(
          workdirOverride,
          cmdOpts.opsSuffix
            ? { opsFor: cmdOpts.opsSuffix, agentName: agentNameFromRcUser(cmdOpts.opsSuffix) }
            : {},
        );
        agentExecPath = opencode.path;
        agentName = "opencode";
        turnTimeoutMs = OPS_TURN_TIMEOUT_MS;
        cwd = workdir;
        // opencode is the one runtime this CLI knows how to install, so it is
        // the one that can be repaired by 「一键唤醒」 (see runtime-recovery.ts).
        ensureRuntime = async () => { await ensureOpencodeInstalled() };
      } else {
        const found = await discoverAgents({});
        const agent = found.find((a) => a.name === cmdOpts.agent);
        if (!agent) throw new Error(`agent CLI ${cmdOpts.agent} not found on PATH; run clawmessenger agents`);
        agentExecPath = agent.path;
        agentName = agent.name;
        if (agent.name === "opencode") {
          ensureRuntime = async () => { await (opts.ensureOpencode ?? ensureOpencodeInstalled)() };
        }
      }
      const { startRunLoop, createImlibTransport } = await import("./run.js");
      const transport = await createImlibTransport();
      const hb = setInterval(() => {
        void api
          .heartbeat(creds.nodeId ?? "", creds.credentialId ?? "", creds.deviceSecret ?? "")
          .catch(() => {});
      }, 30_000);
      try {
        await startRunLoop({
          creds,
          api,
          transport,
          agentExecPath,
          agentName,
          model: cmdOpts.model,
          stdout: opts.stdout,
          turnTimeoutMs,
          cwd,
          processedUidsPath: defaultProcessedUidsPath(creds.nodeId),
          ensureRuntime,
        });
      } finally {
        clearInterval(hb);
      }
    });

  program
    .command("status")
    .description("Show stored identity and whether the supervisor is running in the background")
    .action(async () => {
      const creds = opts.keystore.load();
      if (!creds) {
        opts.stdout.write("not registered\n");
      } else {
        opts.stdout.write(`node: ${creds.nodeId}\nserver: ${creds.serverUrl}\ntoken: ${creds.token ? "present" : "missing"}\n`);
      }
      const report = daemon.status();
      const scope = opsScope();
      opts.stdout.write(formatDaemonStatus(scope ? scopeDaemonReport(report, scope) : report, daemonPaths));
    });

  // 自更新：下载 + 校验到安装目录旁，真正换上去放在下一次启动。
  // 运行中的 exe 在 Windows 上无法被覆盖（EBUSY），但可以被改名 —— 所以这里
  // 只落一份 `<target>.new-<version>`，由 bin.ts 在下次启动时完成交换。
  program
    .command("update")
    .description("Fetch the latest release, verify its checksum, and stage it for the next start")
    .option("--check", "only report whether a newer version is available")
    .option("--force", "stage the latest release even when it is not newer than this build")
    .action(async (cmdOpts: { check?: boolean; force?: boolean }) => {
      const current = program.version() ?? "0.0.0";
      const target = installPaths(process.platform, os.homedir()).target;
      let plan;
      try {
        plan = await checkForUpdate(current, process.platform, process.arch);
      } catch (err) {
        throw new Error(`update check failed: ${err instanceof Error ? err.message : String(err)}`);
      }
      opts.stdout.write(`current: ${plan.currentVersion}\nlatest:  ${plan.latestVersion}\n`);
      if (plan.release.source === "cdn") {
        opts.stdout.write(
          "note: GitHub's API was unavailable (rate limit?), so the version came from the release CDN.\n" +
          "      set CLAWMESSENGER_UPDATE_TOKEN to raise the API quota.\n",
        );
      }
      if (!plan.updateAvailable && !cmdOpts.force) {
        opts.stdout.write("already up to date\n");
        return;
      }
      if (cmdOpts.check) {
        opts.stdout.write("update available\n");
        return;
      }
      const staged = await stageUpdate(plan.release, target, process.platform, process.arch);
      opts.stdout.write(
        `staged ${staged.version} (${staged.asset}, ${staged.bytes} bytes)\n` +
        `it takes effect on the next start — run \`clawmessenger restart\`\n`,
      );
    });

  // 隐藏命令：升级后把 supervisor 重新拉起来的接力进程（不出现在 help 里）。
  //
  // 为什么需要它 —— 「立即生效」的关键在顺序：
  //   1. 旧 supervisor 在**退出前**先做 rename 交换（Windows 允许改名运行中的
  //      exe，只拒绝覆盖），此刻磁盘上的 target 已是新 exe，跑着的仍是旧镜像；
  //   2. 旧 supervisor 优雅退出；
  //   3. 这个 helper 等旧 pid 消失、回环端口释放，然后拉起 target。
  // 如果反过来「先退出、下次启动再交换」（bin.ts 的兜底路径），执行交换的仍是
  // 旧镜像，于是只有它 spawn 的子进程是新版，supervisor 自己的代码永远落后一代
  // （下一版修的是升级逻辑本身时，就永远用旧逻辑）。
  //
  // 自愈：交换是「旧 → backup」「新 → target」两步 rename，中间有一个 target
  // 不存在的窗口。断电停在那里的话，登录自启的 VBS 直指 target，会静默找不到
  // exe —— 设备从此离线且无人知晓。所以拉起前必须先 healInstall。
  program
    .command("__relaunch", { hidden: true })
    .description("internal: relaunch the supervisor after an in-place exe swap")
    .requiredOption("--wait-pid <pid>", "pid of the supervisor that is exiting")
    .option("--target <path>", "installed exe path (default: this platform's install path)")
    .option("--timeout <ms>", "how long to wait for the old pid to disappear", "60000")
    // 透传旧 supervisor 的启动参数（`run --model x` 里的 `--model x`）。丢了这个
    // 参数，`install-service --model x` 装的设备在升级一次之后就静默回落到默认
    // 模型 —— 用户能感觉到「回答变了」，而日志里没有任何痕迹（审查报告 M3）。
    .option("--args <json>", "JSON array of supervisor args to pass through", "[]")
    .action(async (cmdOpts: { waitPid: string; target?: string; timeout: string; args?: string }) => {
      const waitPid = Number.parseInt(cmdOpts.waitPid, 10);
      const timeoutMs = Number.parseInt(cmdOpts.timeout, 10) || 60_000;
      const target = cmdOpts.target ?? installPaths(process.platform, os.homedir()).target;
      const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms)));
      const spawnFn = opts.spawnChild ?? spawn;
      const passthroughArgs = parsePassthroughArgs(cmdOpts.args);

      const deadline = Date.now() + timeoutMs;
      // 所有失败分支都留痕：设备「磁盘是新版、但没有 supervisor」是最难查的故障，
      // 而原来只有一支写了标记、标记还没人读、helper 的 stdout 又被整个丢掉。
      const fail = (reason: string, detail: string): void => {
        writeRelaunchFailureMarker(target, passthroughArgs, reason, detail);
        opts.stdout.write(`relaunch: ${detail}\n`);
        opts.stdout.write(`relaunch: wrote ${relaunchFailureMarkerPath()} (reason=${reason})\n`);
        process.exitCode = 1;
      };
      while (Number.isFinite(waitPid) && waitPid > 0 && processAlive(waitPid)) {
        if (Date.now() > deadline) {
          // 旧进程没退干净就不要拉起第二份 supervisor（两份会登录同一批融云
          // 账号、互相踢线）。宁可失败，让人工 `clawmessenger start` 收拾。
          fail("pid_timeout", `pid ${waitPid} did not exit within ${timeoutMs}ms; not starting`);
          return;
        }
        await sleep(200);
      }
      // 端口锁也等一等：旧 supervisor 可能已经退出但 socket 还没被 OS 回收。
      const acquire = opts.acquireLock ?? acquireSupervisorLock;
      const healed = healInstall(target);
      if (healed.healed) {
        opts.stdout.write(
          `relaunch: restored ${target} from ${healed.staged ?? healed.from ?? "a backup"}\n`,
        );
      }
      if (!existsSync(target)) {
        fail("target_missing", `${target} is missing and no backup could be restored`);
        return;
      }
      // 拿到锁 = 旧 supervisor 的端口确实已经放开。锁**先留着不放**：
      // release 与 spawn 之间那几毫秒里，登录自启的 VBS 或人手敲的 `start` 可能
      // 抢先起一份，我们这份就绑不上端口而自杀 —— 升级把设备弄成离线，这是最坏的
      // 失败方式。所以顺序是「拿锁 → spawn → 放锁 → 等子进程宣告锁到手」。
      let heldLock: SupervisorLock | null = null;
      let probeError: string | null = null;
      for (;;) {
        try {
          heldLock = await acquire(SUPERVISOR_LOCK_PORT);
          probeError = null;
        } catch (err) {
          // 区分「端口被别人占着」和「探测本身报错」：两者的处置完全不同，写进
          // marker 的 reason 必须能分辨（否则运维只会看到 port_held 去杀进程）。
          heldLock = null;
          probeError = err instanceof Error ? err.message : String(err);
        }
        if (heldLock) break;
        if (Date.now() > deadline) {
          if (probeError) {
            fail("lock_probe_failed", `supervisor lock probe failed: ${probeError}; not starting`);
          } else {
            fail("port_held", "supervisor port is still held; not starting");
          }
          return;
        }
        await sleep(200);
      }
      // 交接通道：spawn 之前先删掉上一轮可能残留的 ready，避免把旧证据当新成功。
      const readyPath = relaunchReadyPath(target);
      try {
        rmSync(readyPath, { force: true });
      } catch {
        // 删不掉也要继续：readiness 还按 pid 校验。
      }
      const readyMs = opts.relaunchReadyMs ?? RELAUNCH_READY_MS;
      const aliveMs = opts.relaunchAliveMs ?? RELAUNCH_ALIVE_MS;
      const logFd = openRelaunchLog(target);
      let started: ChildProcess | null = null;
      try {
        for (let attempt = 0; attempt < 2 && !started; attempt += 1) {
          const child = spawnFn(target, ["run", ...passthroughArgs], {
            detached: true,
            // helper 的 stdout/stderr 必须能落盘：`stdio: "ignore"` 让「为什么没起来」
            // 永远只有一句「设备离线」。日志写在安装目录旁的 relaunch.log。
            stdio: logFd === null ? "ignore" : ["ignore", logFd, logFd],
            windowsHide: true,
            env: {
              ...process.env,
              // 只给接力出来的这一个进程放宽锁等待；其它启动入口不受影响。
              [RELAUNCH_LOCK_WAIT_ENV]: String(RELAUNCH_LOCK_WAIT_MS),
              [RELAUNCH_READY_ENV]: readyPath,
            },
          });
          child.unref();
          // 阶段一：只排除「起来就死」。它活着**不算**交接成功（见 RELAUNCH_READY_ENV）。
          if (!(await waitForAliveFor(child.pid, sleep, aliveMs))) {
            opts.stdout.write(`relaunch: ${target} exited right after spawn (attempt ${attempt + 1})\n`);
            continue;
          }
          started = child;
        }
      } finally {
        // 无论成败都放锁：我们退出后端口由 OS 归还，但不能让子进程白等满 15s。
        heldLock.release();
        if (logFd !== null) {
          try {
            closeSync(logFd);
          } catch {
            // 已经关了/从未打开，忽略。
          }
        }
      }
      if (!started) {
        fail("not_started", `could not keep ${target} alive after two spawns`);
        return;
      }
      // 阶段二：判据是**子进程自己写的「我持有锁」**，不是「它还活着」。
      const ready = await waitForRelaunchReady(readyPath, started.pid, sleep, readyMs);
      if (!ready) {
        fail(
          "lock_handover_timeout",
          `pid ${started.pid ?? "?"} never reported holding the supervisor lock within ${readyMs}ms ` +
            "(process alive but not serving — device would look upgraded and be offline)",
        );
        return;
      }
      if (!ready.locked) {
        // 子进程活着、也写了 ready，但自报**没有拿到**端口锁（它自己的锁探测报错，
        // 走了「没有端口锁也继续启动」那条路）。这时不能宣告交接成功 —— 宣告成功
        // 等于向自己确认了一件没发生的事：两台 supervisor 会各答一遍同一条私聊，
        // 而父进程已经 exit 0，没人会去收拾（审查报告 R4）。
        fail(
          "lock_not_held",
          `pid ${started.pid ?? "?"} started but reported no singleton lock (its lock probe failed); ` +
            "two supervisors may answer the same message twice",
        );
        return;
      }
      opts.stdout.write(`relaunch: started ${target} run (pid ${started.pid ?? "?"}), lock handed over\n`);
      // 成功就把上一轮的失败标记清掉，避免下次启动读到过期证据。
      try {
        rmSync(relaunchFailureMarkerPath(), { force: true });
      } catch {
        // 删不掉不影响本次成功。
      }
    });

  // --- 后台常驻 -------------------------------------------------------------
  // `clawmessenger run` 只有前台模式，关掉终端整台设备的智能体就全下线。这几个
  // 动作把 supervisor 托管起来，命令名与随 Release 分发的守护脚本一一对应，但
  // 直接编进 exe —— 不用再去下载 / chmod 任何脚本。
  const daemonOpts = (cmd: Command): Command =>
    cmd
      .option("--model <model>", "model override passed through to the agent CLI")
      .option("--workdir <dir>", "working directory for agent turns (default: your home directory)");

  daemonOpts(
    program
      .command("start")
      .description("Start the supervisor in the background — every bound agent stays online after the terminal closes"),
  ).action(async (cmdOpts: { model?: string; workdir?: string }) => {
    await daemon.start(cmdOpts);
  });

  program
    .command("stop")
    .description("Stop the background supervisor and the agent processes it started")
    .action(async () => {
      // 托管子进程里执行 stop = 杀掉拉起自己的 supervisor，本轮回复永远发不出去：
      // 失败/取消终帧还没发，承载它的进程已经没了（前端一直转圈）。
      const selfKill = supervisedSelfKillRejection("stop");
      if (selfKill) {
        opts.stdout.write(`${selfKill}\n`);
        process.exitCode = 1;
        return;
      }
      // per-agent 运维子进程：整机 stop 会连其他账号的智能体一起下线，越权。
      const rejection = wholeMachineActionRejection("stop");
      if (rejection) {
        opts.stdout.write(`${rejection}\n`);
        process.exitCode = 1;
        return;
      }
      await daemon.stop();
    });

  daemonOpts(
    program
      .command("restart")
      .description("Stop the background supervisor, then start it again")
      .option("--all-agents", "整机重启：连同本机其他智能体一起重启（仅用户明确要求时使用）"),
  ).action(async (cmdOpts: { model?: string; workdir?: string; allAgents?: boolean }) => {
    // 同 stop：在托管子进程里重启整机，等于在命令执行途中把自己换掉。
    // --all-agents 也不例外 —— 那只是「范围更大」，并没有改变自杀这件事。
    const selfKill = supervisedSelfKillRejection("restart");
    if (selfKill) {
      opts.stdout.write(`${selfKill}\n`);
      process.exitCode = 1;
      return;
    }
    const rejection = wholeMachineActionRejection("restart");
    if (rejection && !cmdOpts.allAgents) {
      opts.stdout.write(`${rejection}\n`);
      process.exitCode = 1;
      return;
    }
    await daemon.restart({ model: cmdOpts.model, workdir: cmdOpts.workdir });
  });

  program
    .command("logs")
    .description("Show the tail of the background supervisor's log")
    .option("--lines <n>", "how many lines to show", "40")
    .action((cmdOpts: { lines: string }) => {
      const n = Number.parseInt(cmdOpts.lines, 10);
      daemon.logs({ lines: Number.isFinite(n) && n > 0 ? n : 40 });
    });

  daemonOpts(
    program
      .command("install-service")
      .description("Install autostart so the supervisor comes back after a reboot (systemd / launchd / Windows logon)"),
  ).action(async (cmdOpts: { model?: string; workdir?: string }) => {
    await daemon.installService(cmdOpts);
  });

  program
    .command("uninstall")
    .description("Remove the autostart entry (leaves the binary and the logs alone)")
    .action(async () => {
      await daemon.uninstall();
    });

  // 显式命名空间，等价于上面那些顶层命令，给「不想让 6 个动词混在命令列表里」的人用。
  program
    .command("daemon <action>")
    .description("Background-supervisor actions: start | stop | restart | status | logs | install-service | uninstall")
    .option("--model <model>", "model override passed through to the agent CLI")
    .option("--workdir <dir>", "working directory for agent turns (default: your home directory)")
    .option("--lines <n>", "lines to show for `daemon logs`", "40")
    .option("--all-agents", "整机重启：连同本机其他智能体一起重启（仅用户明确要求时使用）")
    .action(async (action: string, cmdOpts: { model?: string; workdir?: string; lines: string; allAgents?: boolean }) => {
      // --lines 只在 logs 有意义，别把它混进 start / restart / install-service 的选项里。
      const passthrough = { model: cmdOpts.model, workdir: cmdOpts.workdir };
      switch (action) {
        case "start":
          await daemon.start(passthrough);
          return;
        case "stop": {
          const rejection = wholeMachineActionRejection("stop");
          if (rejection) {
            opts.stdout.write(`${rejection}\n`);
            process.exitCode = 1;
            return;
          }
          await daemon.stop();
          return;
        }
        case "restart": {
          const rejection = wholeMachineActionRejection("restart");
          if (rejection && !cmdOpts.allAgents) {
            opts.stdout.write(`${rejection}\n`);
            process.exitCode = 1;
            return;
          }
          await daemon.restart(passthrough);
          return;
        }
        case "status": {
          const report = daemon.status();
          const scope = opsScope();
          opts.stdout.write(formatDaemonStatus(scope ? scopeDaemonReport(report, scope) : report, daemonPaths));
          return;
        }
        case "logs": {
          const n = Number.parseInt(cmdOpts.lines, 10);
          daemon.logs({ lines: Number.isFinite(n) && n > 0 ? n : 40 });
          return;
        }
        case "install-service":
          await daemon.installService(passthrough);
          return;
        case "uninstall":
          await daemon.uninstall();
          return;
        default:
          throw new Error(`unknown daemon action '${action}'; try: start | stop | restart | status | logs | install-service | uninstall`);
      }
    });

  return program;
}

// Supervisor mode: `clawmessenger run` with no --agent. Exchanges the machine
// node's device credential for every bound agent's IM credentials, then
// spawns one child `clawmessenger run --agent <name>` per agent. Each child gets
// its agent-specific credentials via CLAWMESSENGER_AGENT_CREDS (imlib-next is a
// module-level singleton, so one OS process per IM user is required).
interface RunSupervisorOpts {
  opts: BuildProgramOpts;
  creds: StoredCredentials;
  api: XiachatApi;
  model?: string;
  /** 本进程的 exe 版本（buildProgram 的 --version），上报给服务端并用于判断
   * 「这台机器现在到底是什么版本」。 */
  version: string;
  /** 平台/架构，决定要不要接受升级指令（本期只做 win32-x64）。 */
  platform?: string;
  arch?: string;
  /** 轮询间隔覆盖，测试用。 */
  controlPollIntervalMs?: number;
  /** 「空闲」判定阈值覆盖，测试用（见 SUPERVISOR_UPGRADE_IDLE_MS）。 */
  upgradeIdleMs?: number;
  /** 连续多少轮安静才动手，测试用（见 SUPERVISOR_UPGRADE_QUIET_POLLS）。 */
  upgradeQuietPolls?: number;
  /** 资产下载的 fetch 覆盖，测试用。 */
  upgradeFetch?: UpgradeFetch;
  /** 重启接力的实现覆盖，测试用（默认 spawn `__relaunch`）。 */
  relaunch?: (target: string, waitPid: number, passthroughArgs?: string[]) => void;
  /** 透传给新 supervisor 的参数（默认取本进程的 `run ...` 之后那一段）。 */
  passthroughArgs?: string[];
  /** 升级在途账本路径覆盖，测试用。 */
  upgradeInflightPath?: string;
  /** 被替换的 exe 路径覆盖，测试用（默认本平台的安装路径）。 */
  upgradeTarget?: string;
  /** Called once the agent set is known, so the caller can publish the
   * supervisor's runtime state for `clawmessenger status`. Best-effort. */
  onStarted?: (agents: string[]) => void;
  /** Called when the supervisor is shutting down, so the caller can clear that
   * state. Must fire exactly once. */
  onStopped?: () => void;
}

// --- supervised child restarts --------------------------------------------
// The old supervisor only logged `agent X exited with code N`. That is the most
// common way a device goes dark: the agent child dies, its heartbeat stops, the
// node shows up offline in the web's device list, and the user has to walk back
// to that machine and re-run `clawmessenger run` by hand.
//
// Backoff: 1s -> 2s -> 4s -> 8s -> 16s for the first five consecutive failures,
// then a slow lane of one attempt every five minutes. The slow lane exists so a
// permanently broken agent (an uninstalled CLI, a corrupted config) cannot spin
// the CPU forever; it still recovers by itself once the user fixes the machine.
export const SUPERVISOR_RESTART_BASE_MS = 1_000;
export const SUPERVISOR_RESTART_MAX_MS = 60_000;
/** Consecutive fast retries before switching to the slow lane. */
export const SUPERVISOR_RESTART_BACKOFF_AFTER = 5;
export const SUPERVISOR_RESTART_BACKOFF_MS = 5 * 60_000;
/** A child that stayed up this long is treated as healthy; its next exit is a
 * fresh failure (backoff restarts from 1s), not the continuation of a crash loop. */
export const SUPERVISOR_HEALTHY_UPTIME_MS = 60_000;
/** How often the supervisor asks the server for operator-desired run-states. */
export const SUPERVISOR_CONTROL_POLL_MS = 10_000;
/**
 * 设备要「安静」多久才允许自动升级重启。
 *
 * 300 秒（5 分钟）不是拍出来的：误判的两个方向不对称 —— 误判「忙」只是晚点升
 * （无害），误判「闲」会把一条正在生成的回答连根拔掉（用户丢一次对话，还可能留下
 * 一个永久转圈的气泡）。推理模型的首 token 超过 30 秒很常见，一个跑 5 分钟的
 * 工具调用更是全程静默，所以原来的 30s 明显偏短（审查报告 M7）。真正的修法是
 * 让子进程写 busy 标记（已排下期）；在那之前宁可让管理员多等几分钟，因为他随时
 * 可以点「强制升级」。
 */
export const SUPERVISOR_UPGRADE_IDLE_MS = 300_000;

/**
 * 还要**连续这么多轮**轮询都判定安静才动手。
 *
 * 单点采样会抖（一次 GC 停顿、一个短暂的 CPU 饥饿都能造出空窗），要求连续 3 轮
 * 都安静（10s 一轮 → 再多等 20s），代价可以忽略。
 */
export const SUPERVISOR_UPGRADE_QUIET_POLLS = 3;

/** 一条升级回传最多重投几轮 —— 再多就认了（网络长期不通时不要无限骚扰服务端）。 */
export const SUPERVISOR_UPGRADE_REPORT_ATTEMPTS = 5;

/** Delay before the `restarts`-th consecutive restart (1-based). */
export function supervisorRestartDelayMs(restarts: number): number {
  if (restarts > SUPERVISOR_RESTART_BACKOFF_AFTER) return SUPERVISOR_RESTART_BACKOFF_MS;
  return Math.min(SUPERVISOR_RESTART_MAX_MS, SUPERVISOR_RESTART_BASE_MS * 2 ** (Math.max(1, restarts) - 1));
}

/** 服务端下发的期望状态；version 用来区分「保持运行」与「重启一次」。 */
type ControlIntent = { state: "running" | "stopped"; version: number };

interface SupervisedAgent {
  agent: string;
  runArgs: string[];
  childCreds: StoredCredentials;
  child?: ChildProcess;
  /** Consecutive failures; reset when the previous run looked healthy. */
  restarts: number;
  startedAt: number;
  lastExitCode: number | null;
  state: "running" | "restarting" | "stopped";
  /** Operator intent, as last seen from the server. A child that exits while
   * this is "stopped" was stopped on purpose and must not be resurrected. */
  desired: "running" | "stopped";
  /** per-agent 运维子进程专属服务的 agent 融云 id；机器级 agent 为 undefined。 */
  opsScope?: string;
  /**
   * 上次看到的 control 版本号。版本变了但状态仍是 running = 明确要求「重启
   * 这一个 agent」（服务端 action=restart 时强制推进版本，见 control/set）。
   */
  seenVersion?: number;
  /** restart 已发起、正等子进程退出；exit 回调据此直接重拉而不是走退避。 */
  pendingRestart?: boolean;
  timer?: NodeJS.Timeout;
}

// per-agent 运维节点（opsFor 非空）在 supervisor 里以 "ops@<agentRC>" 为键，
// 与机器级 ops（键 "ops"）并存 —— 否则多台 ops 行在同一个 Map 键上互相覆盖，
// 只有最后一台会被拉起。
function supervisedKey(agent: MachineAgentCredential): string {
  return agent.agent === OPS_AGENT_NAME && agent.opsFor ? `ops@${agent.opsFor}` : agent.agent;
}

// 控制状态（/api/claw/device/control 的键是 ai_type）对所有 ops 子进程共享
// 同一个 "ops" 键：停 ops 即停全部运维子进程，与机型粒度的运维语义一致。
function controlKeyFor(supervisedKeyName: string): string {
  return supervisedKeyName.startsWith("ops@") ? OPS_AGENT_NAME : supervisedKeyName;
}

export async function runSupervisor(supervisor: RunSupervisorOpts): Promise<void> {
  const { opts, creds, api } = supervisor;
  if (!creds.credentialId || !creds.deviceSecret) {
    throw new Error("no machine device credential; run clawmessenger pair on this device first");
  }
  // Ensure the default opencode runtime is present before spawning children so
  // both the built-in ops agent and any bound opencode agent can run on a
  // device that has never installed it. Best effort: a failed install must not
  // stop the other agents from starting.
  const ensureOpencode = opts.ensureOpencode ?? ensureOpencodeInstalled;
  try {
    await ensureOpencode();
  } catch (err) {
    console.warn("opencode install failed:", err instanceof Error ? err.message : String(err));
  }
  // Best-effort auto-bind of the built-in ops agent so every device gets ops
  // capability; server-side binding is idempotent, so this is safe every start.
  try {
    await api.bindDeviceAgents(creds.nodeId, creds.credentialId, creds.deviceSecret, [OPS_AGENT_NAME]);
  } catch (err) {
    console.warn("ops auto-bind failed:", err instanceof Error ? err.message : String(err));
  }
  const bound = await api.fetchDeviceNodes(creds.nodeId, creds.credentialId, creds.deviceSecret);
  if (bound.length === 0) {
    throw new Error("no agents bound to this device; select and bind agents in the web binding dialog first");
  }
  const found = await discoverAgents({});
  // SEA single-file exe: re-invoke the exe itself. Node script mode: re-run
  // the bundle entry so argv keeps the [execPath, script, ...] shape.
  const spawnArgvPrefix = supervisorSpawnArgv();
  const spawnChild = opts.spawnChild ?? spawn;
  const managed = new Map<string, SupervisedAgent>();
  // Set by shutdown so a SIGTERM'd child is never resurrected by its own exit
  // handler — the previous supervisor had no such guard and would have kept
  // (re)starting agents after being told to stop.
  let stopping = false;
  let resolveStopped: () => void = () => {};
  const stopped = new Promise<void>((resolve) => { resolveStopped = resolve; });

  // --- 「空闲」判定 -----------------------------------------------------------
  // 升级最后一步是重启整台设备（supervisor 连带所有 agent 子进程）。正在跑的
  // turn 会被 SIGTERM 打断，而子进程的 SIGTERM 处理是「立刻退出」，不会补一条
  // 失败终帧 —— 前端气泡就会一直转圈（历史上的「气泡卡死」就是这个成因）。
  // 所以默认**等设备安静下来再升**，由管理员用「强制升级」显式跳过这一步。
  //
  // 信号来源：子进程的 stdout/stderr。supervisor 不持有 agent 的会话状态
  // （SessionQueue 在子进程里），run.ts 也不在这次改动的范围内；而 agent turn
  // 的产出本来就会流经 supervisor 的管道，所以「最近有没有输出」是唯一现成的、
  // 跨进程的忙碌信号。
  // 已知局限（见实现报告）：长时间静默的工具调用（比如跑 5 分钟测试）看起来
  // 和空闲一样，可能被误判为空闲。这是「不新增跨进程协议」的代价，用「强制升级」
  // 与 300s 静默窗口 + 连续多轮采样兜住 —— 误判「忙」只是晚点升（无害），误判
  // 「闲」会让用户丢一次回答（不可接受），所以宁可等久一点。
  let lastActivityAt = Date.now();
  const markActivity = (): void => { lastActivityAt = Date.now(); };
  const upgradeIdleMs = supervisor.upgradeIdleMs ?? SUPERVISOR_UPGRADE_IDLE_MS;
  const quietPollGoal = supervisor.upgradeQuietPolls ?? SUPERVISOR_UPGRADE_QUIET_POLLS;
  /** 连续多少轮轮询都判定安静（中断一次就清零）。 */
  let quietPolls = 0;
  const deviceQuiet = (): boolean => {
    if (Date.now() - lastActivityAt < upgradeIdleMs) {
      quietPolls = 0;
      return false;
    }
    quietPolls += 1;
    // 单点采样会抖：一次 CPU 抖动导致的空窗就重启设备太草率，要求连续 N 轮都
    // 安静（10s 一轮 → 再多等 20s，代价可忽略）。
    return quietPolls >= quietPollGoal;
  };

  // shutdown 在本函数后段才定义；升级流程要在轮询回调里触发它，用一个可变量
  // 转发，避免把整个初始化顺序倒过来。
  let triggerShutdown: (() => void) | null = null;

  const launch = (entry: SupervisedAgent): void => {
    if (stopping) return;
    const child = spawnChild(
      process.execPath,
      [...spawnArgvPrefix, ...entry.runArgs],
      {
        env: {
          ...process.env,
          CLAWMESSENGER_AGENT_CREDS: JSON.stringify(entry.childCreds),
          // 标记「我是 supervisor 的子进程」：这类进程执行整机 stop/restart 会
          // 把自己的父进程（连同本轮）杀掉，命令层直接拒绝（见
          // supervisedSelfKillRejection）。
          [SUPERVISED_ENV]: "1",
          // per-agent 运维子进程带上服务范围：CLI 据此拒绝整机级 stop/restart，
          // 也把同一个值写进 AGENTS.md 的「服务范围」，让模型自己守住边界。
          ...(entry.opsScope ? { [OPS_SCOPE_ENV]: entry.opsScope } : {}),
        },
        // supervisor 是 detached + windowsHide 拉起的，自己没有控制台；这里的子进程
        // 若不带 windowsHide，Windows 会为每个 console 子进程**新分配一个可见的
        // 控制台窗口**——绑了几个 agent 就弹几个黑窗。用户点 X 关窗只杀掉该 agent
        // 子进程，下方的退避重启（1s→2s→4s…）马上又把它拉回来，看起来就是
        // 「窗口关都关不掉」。stdio 走默认管道，输出仍进 supervisor 日志，不受影响。
        windowsHide: true,
      },
    );
    entry.child = child;
    entry.startedAt = Date.now();
    entry.state = "running";
    markActivity();
    child.stdout?.on("data", (chunk: Buffer) => {
      markActivity();
      opts.stdout.write(`[${entry.agent}] ${chunk}`);
    });
    child.stderr?.on("data", (chunk: Buffer) => {
      markActivity();
      process.stderr.write(`[${entry.agent}] ${chunk}`);
    });
    child.on("exit", (code) => {
      entry.child = undefined;
      entry.lastExitCode = code;
      // restart：这次退出是计划内的，立刻用全新预算重拉 —— 不进退避、不累计失败。
      if (entry.pendingRestart) {
        entry.pendingRestart = false;
        entry.state = "stopped";
        if (!stopping) {
          entry.timer = setTimeout(() => {
            entry.timer = undefined;
            launch(entry);
          }, SUPERVISOR_RESTART_BASE_MS);
        }
        return;
      }
      // `desired === "stopped"` covers a child the operator stopped: its exit is
      // the point, not a crash, so it must not enter the backoff path.
      if (stopping || entry.desired === "stopped") {
        entry.state = "stopped";
        return;
      }
      const uptimeMs = Date.now() - entry.startedAt;
      entry.restarts = uptimeMs >= SUPERVISOR_HEALTHY_UPTIME_MS ? 1 : entry.restarts + 1;
      const delayMs = supervisorRestartDelayMs(entry.restarts);
      entry.state = "restarting";
      console.error(
        `agent ${entry.agent} exited (code=${code}) after ${uptimeMs}ms, restarting in ${delayMs}ms (attempt ${entry.restarts})`,
      );
      entry.timer = setTimeout(() => {
        entry.timer = undefined;
        launch(entry);
      }, delayMs);
    });
  };

  // --- operator-driven start/stop ------------------------------------------
  // The server owns the desired state; this process owns the processes. A poll
  // reconciles them. IM cannot carry this: only this supervisor can start or
  // stop an agent, and it holds no RongCloud connection — and a stopped agent
  // has no process left to receive a "start". So we ask, rather than listen.
  const nodeID = creds.nodeId ?? "";
  const credentialID = creds.credentialId ?? "";
  const deviceSecret = creds.deviceSecret ?? "";

  // --- 设备 exe 升级 ---------------------------------------------------------
  // 与启停共用这一次轮询：请求体带上本机版本/平台，响应体可能带回一条升级
  // 指令（generation/targetVersion/sha256/mirrors/force）。服务端按 generation
  // 防抖，设备按 generation 只执行一次。
  const currentVersion = supervisor.version;
  const devicePlatform = supervisor.platform ?? process.platform;
  const deviceArch = supervisor.arch ?? process.arch;
  const platformTag = `${devicePlatform}-${deviceArch}`;
  const platformSupportsUpgrade = devicePlatform === "win32" && deviceArch === "x64";
  const upgradeTarget = supervisor.upgradeTarget ?? installPaths(devicePlatform, os.homedir()).target;
  const inflightPath = supervisor.upgradeInflightPath ?? defaultUpgradeInflightPath();
  const upgradeFetch: UpgradeFetch = supervisor.upgradeFetch
    ?? ((url) => fetch(url, { headers: { "User-Agent": "clawmessenger-cli" } }) as unknown as ReturnType<FetchLike>);
  const relaunch = supervisor.relaunch ?? ((target: string, waitPid: number, passthroughArgs: string[] = []) => {
    // helper 是**新** exe：交换已经完成，process.execPath 现在指向新镜像。
    // `--args` 用 JSON 数组而不是把它摊成多个 argv：参数里可能有以 `-` 开头的
    // 值（`--model -x`），摊平后 commander 会把它们当成自己的选项。
    const child = spawnChild(
      process.execPath,
      [
        ...spawnArgvPrefix,
        "__relaunch",
        "--wait-pid", String(waitPid),
        "--target", target,
        "--args", JSON.stringify(passthroughArgs),
      ],
      { detached: true, stdio: "ignore", windowsHide: true },
    );
    child.unref();
  });
  const passthroughArgs = supervisor.passthroughArgs ?? [];

  /** 已开始执行（下载/交换）的代次：同一代次只执行一次，绝不重下 88MB。 */
  let claimedGeneration = 0;
  /** 已上报过「等待空闲」的代次，避免每 10 秒刷同一条状态。 */
  let waitingGeneration = 0;
  /** 正在升级：轮询回调不重入。 */
  let upgrading = false;
  /** 已上报过的阶段，避免同一阶段反复打扰服务端。 */
  let lastReportedStage = "";

  type InflightUpgrade = { generation: number; targetVersion: string; phase: "applying" | "restarting" };

  const readInflight = (): InflightUpgrade | null => {
    try {
      const parsed = JSON.parse(readFileSync(inflightPath, "utf8")) as Partial<InflightUpgrade>;
      if (typeof parsed.generation === "number" && typeof parsed.targetVersion === "string") {
        return {
          generation: parsed.generation,
          targetVersion: parsed.targetVersion,
          phase: parsed.phase === "restarting" ? "restarting" : "applying",
        };
      }
    } catch {
      // 不存在 / 损坏都当作「没有在途升级」
    }
    return null;
  };
  const writeInflight = (record: InflightUpgrade): void => {
    try {
      mkdirSync(path.dirname(inflightPath), { recursive: true });
      writeFileSync(inflightPath, `${JSON.stringify(record)}\n`, { mode: 0o600 });
    } catch (err) {
      // 写不进去不影响升级本身，只是重启后无法自证「我刚升过」。
      console.error("supervisor: could not record in-flight upgrade:", err instanceof Error ? err.message : String(err));
    }
  };
  const clearInflight = (): void => {
    try {
      rmSync(inflightPath, { force: true });
    } catch {
      // 删不掉就留着，下次启动再判一次（幂等）。
    }
  };
  /** 只清「这一代次」的账本，避免把管理员后来又下发的一轮抹掉。 */
  const clearInflightIfMatches = (generation: number): void => {
    const inflight = readInflight();
    if (inflight && inflight.generation === generation) clearInflight();
  };

  /**
   * 还没送达的升级回传。键是代次，值是最近一次想报的状态。
   *
   * 为什么需要队列：回传失败过去会被永久丢弃（代次已置位、账本已删），于是服务端
   * 停在 waiting_idle/restarting 直到下次管理员下发 —— 而登录自启时 Wi-Fi 还没
   * 起来恰恰是最容易失败的时机（审查报告 M1）。现在每轮轮询顺带重投一次。
   */
  const pendingReports = new Map<
    number,
    { status: DeviceUpgradeStatus; attempts: number; errorCode?: string; errorDetail?: string }
  >();

  const reportUpgrade = async (
    generation: number,
    status: DeviceUpgradeStatus,
    extra?: { errorCode?: string; errorDetail?: string },
  ): Promise<boolean> => {
    try {
      await api.reportDeviceUpgrade(nodeID, credentialID, deviceSecret, {
        generation,
        status,
        reportedVersion: currentVersion,
        reportedPlatform: platformTag,
        ...(extra?.errorCode ? { errorCode: extra.errorCode } : {}),
        ...(extra?.errorDetail ? { errorDetail: extra.errorDetail.slice(0, 800) } : {}),
      });
      pendingReports.delete(generation);
      // 终态真的送到了才清账本：账本是「我已经换过盘、但还没告诉服务端」的唯一
      // 证据，提前删掉就等于把这次升级的状态永久丢了。
      if (status === "succeeded" || status === "failed") clearInflightIfMatches(generation);
      return true;
    } catch (err) {
      // 进度回传失败不能让升级中断（服务端下一轮轮询仍能看到版本）。但也不能就
      // 此丢掉这条回传：登录自启时 Wi-Fi 还没起来是最常见的场景，丢了它服务端
      // 就会停在 waiting_idle/restarting 直到管理员再点一次（审查报告 M1）。
      console.error("supervisor: upgrade report failed:", err instanceof Error ? err.message : String(err));
      const previous = pendingReports.get(generation);
      pendingReports.set(generation, {
        status,
        attempts: (previous?.attempts ?? 0) + 1,
        ...(extra?.errorCode ? { errorCode: extra.errorCode } : {}),
        ...(extra?.errorDetail ? { errorDetail: extra.errorDetail } : {}),
      });
      return false;
    }
  };

  /** 重投失败的回传。每轮轮询顺带跑一次，成功或收到 409（视为已过期）即出队。 */
  const flushPendingReports = async (): Promise<void> => {
    if (pendingReports.size === 0) return;
    for (const [generation, entry] of [...pendingReports]) {
      if (entry.attempts >= SUPERVISOR_UPGRADE_REPORT_ATTEMPTS) {
        pendingReports.delete(generation);
        console.error(
          `supervisor: giving up on upgrade report ${generation}/${entry.status} after ${entry.attempts} attempts`,
        );
        continue;
      }
      await reportUpgrade(generation, entry.status, {
        ...(entry.errorCode ? { errorCode: entry.errorCode } : {}),
        ...(entry.errorDetail ? { errorDetail: entry.errorDetail } : {}),
      });
    }
  };

  /**
   * 收尾上一轮升级：进程刚启动时先看有没有「交换完但还没回报」的账。
   *
   * 为什么必须由**新进程**来判：版本号编译进镜像，只有它自己知道现在跑的是新版
   * 还是旧版。旧进程报不了（它已经退出），服务端也猜不出来（它只看到 restarting）。
   * 没有这一步，页面会永远停在「重启中」。
   */
  const finishInflightUpgrade = async (): Promise<void> => {
    const inflight = readInflight();
    if (!inflight) return;
    // 回传成功才清账本：失败时账本留着，下一轮轮询重投（flushPendingReports），
    // 下次启动也会再判一次 —— 否则服务端永远停在「重启中」。
    if (inflight.targetVersion === currentVersion) {
      await reportUpgrade(inflight.generation, "succeeded");
      return;
    }
    await reportUpgrade(inflight.generation, "failed", {
      errorCode: "restart_failed",
      errorDetail: `expected ${inflight.targetVersion} on disk but this process runs ${currentVersion}`,
    });
  };

  const executeUpgrade = async (intent: DeviceUpgradeIntent): Promise<void> => {
    upgrading = true;
    try {
      if (intent.targetVersion === currentVersion) {
        // 服务端与设备版本判断有竞态时的兜底：已经在目标版本上了，直接报成功。
        await reportUpgrade(intent.generation, "succeeded");
        return;
      }
      writeInflight({ generation: intent.generation, targetVersion: intent.targetVersion, phase: "applying" });
      lastReportedStage = "";
      const staged = await stageUpdateFromMirrors(
        {
          version: intent.targetVersion,
          assetName: intent.assetName,
          sha256: intent.sha256,
          mirrors: intent.mirrors ?? [],
        },
        upgradeTarget,
        creds.serverUrl,
        upgradeFetch,
        (stage) => {
          // 只在阶段变化时回传：页面的进度条不需要每毫秒一条。
          if (stage === lastReportedStage) return;
          lastReportedStage = stage;
          void reportUpgrade(intent.generation, stage);
        },
      );
      console.error(
        `supervisor: staged ${staged.version} from ${staged.source} (${staged.bytes} bytes), swapping in place`,
      );
      // 退出前交换：Windows 允许改名运行中的 exe，于是磁盘上立刻是新版；本进程
      // 继续跑已经映射的旧镜像，直到 shutdown。
      const applied = applyStagedUpdate(upgradeTarget, devicePlatform);
      if (!applied.applied) {
        // 交换失败有可能把 target 弄没了（第一步 rename 成功、第二步失败且回滚
        // 也失败）。此刻本进程还活着，是**唯一**能修回来的时机 —— 等下一次启动
        // 就晚了：登录自启的 VBS 会发现 target 不存在而静默失败，设备从此离线
        // （审查报告 S4）。
        const healed = healInstall(upgradeTarget);
        if (healed.healed) {
          console.error(
            `supervisor: swap failed, restored ${upgradeTarget} from ${healed.staged ?? healed.from ?? "a backup"}`,
          );
        }
        await reportUpgrade(intent.generation, "failed", {
          errorCode: "swap_failed",
          errorDetail: applied.reason ?? "applyStagedUpdate refused the swap",
        });
        return;
      }
      writeInflight({ generation: intent.generation, targetVersion: intent.targetVersion, phase: "restarting" });
      await reportUpgrade(intent.generation, "restarting");
      try {
        relaunch(upgradeTarget, process.pid, passthroughArgs);
      } catch (err) {
        await reportUpgrade(intent.generation, "failed", {
          errorCode: "restart_failed",
          errorDetail: err instanceof Error ? err.message : String(err),
        });
        // 不 clearInflight：磁盘上已经是新版，下次任何启动都会自证成功。
        return;
      }
      triggerShutdown?.();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      const errorCode = /checksum mismatch/i.test(message) ? "checksum_mismatch" : "download_failed";
      // 下载/校验失败时磁盘上的旧 exe 一个字节都没动，账本没有存在意义；成功送达
      // 之后由 reportUpgrade 的终态分支清掉。这里显式清是因为「失败」这条也要让
      // 下次启动老实重来（否则新进程会拿旧账本自证一个根本没发生的升级）。
      await reportUpgrade(intent.generation, "failed", { errorCode, errorDetail: message });
      clearInflightIfMatches(intent.generation);
    } finally {
      upgrading = false;
    }
  };

  const handleUpgradeIntent = (intent: DeviceUpgradeIntent | undefined): void => {
    if (!intent || upgrading) return;
    if (intent.generation <= claimedGeneration) return;
    if (!platformSupportsUpgrade) {
      // 服务端也会按上报平台拦一道；这里是「万一收到了」的兜底：明确报失败，
      // 而不是静默丢掉让页面一直排队。
      claimedGeneration = intent.generation;
      void reportUpgrade(intent.generation, "failed", {
        errorCode: "platform_unsupported",
        errorDetail: `exe 升级本期只支持 win32-x64，本机是 ${platformTag}`,
      });
      return;
    }
    if (!intent.force && !deviceQuiet()) {
      if (waitingGeneration !== intent.generation) {
        waitingGeneration = intent.generation;
        void reportUpgrade(intent.generation, "waiting_idle");
      }
      return;
    }
    claimedGeneration = intent.generation;
    void executeUpgrade(intent);
  };

  /** 上次看到的期望状态与版本。失败时返回空 Map，applyControlStates 读作
   * 「什么都别改」—— 服务端抖动绝不能把操作员留着运行的 agent 停掉。 */
  const fetchControlSnapshot = async (): Promise<{
    desired: Map<string, ControlIntent>;
    upgrade?: DeviceUpgradeIntent;
  }> => {
    const desired = new Map<string, ControlIntent>();
    try {
      const snapshot = await api.fetchDeviceControl(nodeID, credentialID, deviceSecret, {
        version: currentVersion,
        platform: platformTag,
      });
      for (const state of snapshot.agents) {
        if (!state.agent) continue;
        // 只认识 running / stopped 两个动作；其它值（服务端以后新增的动作）
        // **静默忽略**。这条前向兼容是有代价换来的：旧设备遇到不认识的
        // desiredState 若按「非 stopped 即 running」处理，会在新服务端推出
        // 「drain」这类动作时反手把 agent 拉起来；若按「不认识就报错」处理，
        // 旧设备会在轮询时崩掉/退出，等于升级一次服务端就下线所有老设备。
        if (state.desiredState !== "running" && state.desiredState !== "stopped") continue;
        desired.set(state.agent, {
          state: state.desiredState,
          version: typeof state.version === "number" ? state.version : 0,
        });
      }
      return snapshot.upgrade ? { desired, upgrade: snapshot.upgrade } : { desired };
    } catch (err) {
      console.error("supervisor: control poll failed:", err instanceof Error ? err.message : String(err));
      // 轮询失败 = 什么都不改：既不收敛启停，也不碰升级。
      return { desired };
    }
  };

  const fetchControlStates = async (): Promise<Map<string, ControlIntent>> =>
    (await fetchControlSnapshot()).desired;

  const stopAgent = (entry: SupervisedAgent): void => {
    if (entry.desired === "stopped") return;
    entry.desired = "stopped";
    if (entry.timer) {
      clearTimeout(entry.timer);
      entry.timer = undefined;
    }
    entry.state = "stopped";
    entry.child?.kill("SIGTERM");
  };

  const startAgent = (entry: SupervisedAgent): void => {
    if (entry.desired === "running") return;
    entry.desired = "running";
    if (entry.timer) {
      clearTimeout(entry.timer);
      entry.timer = undefined;
    }
    // Fresh budget: an operator-stopped-then-started agent must not inherit the
    // backoff tier of a crash that predated the stop.
    entry.restarts = 0;
    launch(entry);
  };

  /**
   * 只重启这一个 agent（服务端 restart 指令）。杀进程 → exit 回调里立刻重拉，
   * 同机其他 agent 不受影响 —— 这正是它相对 `clawmessenger restart` 的意义。
   */
  const restartAgent = (entry: SupervisedAgent): void => {
    entry.restarts = 0;
    entry.desired = "running";
    if (entry.timer) {
      clearTimeout(entry.timer);
      entry.timer = undefined;
    }
    if (!entry.child) {
      launch(entry);
      return;
    }
    entry.pendingRestart = true;
    entry.child.kill("SIGTERM");
  };

  const applyControlStates = (desired: Map<string, ControlIntent>): void => {
    for (const [key, entry] of managed) {
      // per-agent 运维子进程（ops@<agentRC>）共享 "ops" 的控制状态。
      const intent = desired.get(controlKeyFor(key));
      // An agent the supervisor never started (its CLI is not on PATH) is not
      // ours to converge; the server still lists it because the node is bound.
      if (!entry || !intent) continue;
      // 第一次同步只记录版本，不当成「刚被要求重启」—— 否则每次 supervisor
      // 启动都会把已经在跑的 agent 停一遍。
      const versionChanged = entry.seenVersion !== undefined && entry.seenVersion !== intent.version;
      entry.seenVersion = intent.version;
      if (intent.state === "stopped") {
        stopAgent(entry);
        continue;
      }
      if (versionChanged && entry.desired === "running" && entry.state === "running") {
        restartAgent(entry);
        continue;
      }
      startAgent(entry);
    }
  };

  // Ask once before spawning: without this, rebooting a machine would briefly
  // start every agent the operator had stopped, then kill them again on the
  // first poll — visible as a spurious online blip on the admin page.
  const initialDesired = await fetchControlStates();

  for (const agent of bound) {
    // The built-in ops agent has no PATH-installed CLI of its own; the child
    // (`clawmessenger run --agent ops`) installs opencode itself before connecting.
    if (agent.agent !== OPS_AGENT_NAME) {
      const local = found.find((f) => f.name === agent.agent);
      if (!local) {
        console.error(`agent CLI ${agent.agent} not found on PATH; skipping`);
        continue;
      }
    }
    const key = supervisedKey(agent);
    const runArgs = ["run", "--agent", agent.agent];
    if (supervisor.model) runArgs.push("--model", supervisor.model);
    // per-agent 运维子进程用独立 workdir（opencode 会话状态按目录隔离，
    // 共享目录会让多个 ops 子进程互相踩会话文件）。
    if (agent.agent === OPS_AGENT_NAME && agent.opsFor) {
      runArgs.push("--ops-suffix", agent.opsFor);
    }
    const intent = initialDesired.get(agent.agent);
    const desired = intent?.state ?? "running";
    const entry: SupervisedAgent = {
      agent: key,
      runArgs,
      childCreds: {
        nodeId: agent.nodeId,
        token: agent.token,
        credentialId: agent.credentialId,
        deviceSecret: agent.deviceSecret,
        serverUrl: creds.serverUrl,
      },
      restarts: 0,
      startedAt: Date.now(),
      lastExitCode: null,
      state: desired === "running" ? "running" : "stopped",
      desired,
      ...(intent ? { seenVersion: intent.version } : {}),
      ...(agent.agent === OPS_AGENT_NAME && agent.opsFor ? { opsScope: agent.opsFor } : {}),
    };
    managed.set(key, entry);
    if (desired === "running") {
      launch(entry);
    } else {
      console.error(`agent ${agent.agent} is stopped by the operator; leaving it down`);
    }
  }
  if (managed.size === 0) {
    throw new Error("none of the bound agent CLIs is available on this machine's PATH");
  }
  const runningCount = [...managed.values()].filter((entry) => entry.desired === "running").length;
  opts.stdout.write(`clawmessenger supervisor: ${runningCount} of ${managed.size} agent(s) online\n`);
  // Publish the runtime state now that the agent set is final: this is what
  // `clawmessenger status` reads back, so it must happen after the map is built
  // (and not before, or it would report zero agents).
  try {
    supervisor.onStarted?.([...managed.values()].map((entry) => entry.agent));
  } catch (err) {
    console.error("supervisor: could not record runtime state:", err instanceof Error ? err.message : String(err));
  }

  // 上一轮升级的收尾（交换已做、重启已发生）：由**新进程**自证版本并回报。
  void finishInflightUpgrade();

  const controlPollMs = supervisor.controlPollIntervalMs ?? SUPERVISOR_CONTROL_POLL_MS;
  const controlTimer = setInterval(() => {
    void fetchControlSnapshot().then(({ desired, upgrade }) => {
      applyControlStates(desired);
      handleUpgradeIntent(upgrade);
      // 顺手重投之前没送达的进度回传（网络刚恢复时最先该做的事）。
      void flushPendingReports();
    });
  }, controlPollMs);
  // The poll must never be the reason the process stays alive; `await stopped`
  // is what holds the supervisor open.
  if (typeof controlTimer.unref === "function") controlTimer.unref();

  const shutdown = (): void => {
    const first = !stopping;
    stopping = true;
    clearInterval(controlTimer);
    for (const entry of managed.values()) {
      if (entry.timer) {
        clearTimeout(entry.timer);
        entry.timer = undefined;
      }
      entry.state = "stopped";
      entry.child?.kill("SIGTERM");
    }
    // 只通知一次：SIGINT 之后往往还会来一个 SIGTERM。
    if (first) {
      try {
        supervisor.onStopped?.();
      } catch {
        // 清不掉状态文件不影响退出。
      }
    }
    // Returning instead of looping forever lets the process exit once the
    // children are gone; `process.on` below replaces Node's default SIGTERM
    // behaviour, so without this the supervisor would outlive its agents.
    resolveStopped();
  };
  // 升级流程在轮询回调里触发退出（交换完成之后），这里把 shutdown 交出去。
  triggerShutdown = shutdown;
  if (opts.onSupervisorSignal) {
    opts.onSupervisorSignal(shutdown);
  } else {
    process.on("SIGINT", shutdown);
    process.on("SIGTERM", shutdown);
  }
  await stopped; // supervise until told to stop
}
