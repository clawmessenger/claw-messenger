// 后台常驻（supervisor 托管）——CLI 原生实现，跨平台，不依赖任何外部脚本。
//
// 为什么需要它
//   `clawmessenger run` 只有前台模式，而且 supervisor 只处理 SIGINT / SIGTERM
//   （**不处理 SIGHUP**）：关掉终端窗口、Ctrl+D、注销，这台设备上的所有智能体
//   会一起下线。CLI 过去没有托管命令，只能靠随 Release 分发的 PowerShell / bash
//   守护脚本 —— 脚本得先下载再 chmod，国内网络还经常取不到（GitHub 443 超时）。
//   现在这些动作直接编进 exe，命令面与脚本一一对应：
//
//     clawmessenger start / stop / restart / status / logs
//     clawmessenger install-service / uninstall
//     clawmessenger daemon <action>        # 等价的显式命名空间
//
// 实现要点
//   * 启动：把 `run`（supervisor 模式）作为 **detached** 子进程拉起，脱离调用方
//     的会话与终端；PID 写 supervisor.pid，运行详情写 supervisor.json。
//   * 运行状态由 supervisor **自己**在启动/退出时写、删（main.ts 传进来的
//     onStarted / onStopped）。所以 `status` 不需要去扫 /proc 或 tasklist：
//     跨平台行为一致，也不会把同名的无关进程误判成我们的 supervisor。
//   * 停止：POSIX 先发 SIGTERM，让 supervisor 优雅地把它拉起的 agent 一起停掉，
//     约 15 秒还没退才升级 SIGKILL；Windows 用 `taskkill /T /F` 收整棵树
//     （Node 的 process.kill 在 Windows 上是 TerminateProcess，等于直接拔电，
//     会留下孤儿的 agent 子进程，所以这条路径不用它）。
//   * 自启：Linux 写 systemd unit（root → 系统级 / 普通用户 → --user + linger），
//     macOS 写 LaunchAgent plist，Windows 写「启动」文件夹里的 .vbs（免管理员）。
import { spawn, spawnSync, type ChildProcess, type SpawnOptions } from "node:child_process";
import {
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  readFileSync,
  readdirSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { homedir, userInfo } from "node:os";
import { join } from "node:path";

export const DAEMON_SERVICE_NAME = "clawmessenger";

/**
 * 登录自启脚本的模板版本标记。
 *
 * `install-service` 落盘的 VBS 里带这一行；**旧模板没有**。supervisor 每次启动都
 * 检查一次这个标记 —— 远程升级不会重写 VBS，只有靠这个检查才能让存量设备在
 * 「下一次启动」就拿到自愈块（审查报告 M3a）。
 */
export const STARTUP_VBS_TEMPLATE_MARKER = "clawmessenger-vbs-template: 2 (self-heal)";

/**
 * VBS 里的自愈块：target 不存在就从同目录的 `.old-*` / `.new-*` 里挑最新的一个
 * 恢复回去。
 *
 * 为什么必须在 exe **之外**：两步 rename 交换的中间态里 target 不存在，此刻没有
 * 任何进程能跑起来执行 healInstall（bin.ts / __relaunch 都住在需要被启动的那个
 * exe 里）。登录自启是唯一的「exe 之外」的执行点。
 *
 * `.part` 必须排除：下载中间态是 `<dest>.part`，即
 * `clawmessenger.exe.new-<version>.part`。它命中 `.new-` 前缀且 `Size > 0`，
 * 断电留下的半截 exe 会被当成恢复源 MoveFile 成 target —— 而 target 一旦存在，
 * 后面的 heal 链就认为「没事」，故障比没有恢复更隐蔽（审查报告 M3b）。
 */
export function startupVbsSelfHealLines(exePath: string): string[] {
  const literal = `"${exePath.replace(/"/g, '""')}"`;
  return [
    "' Self-heal: if an upgrade left the exe renamed aside with nothing in its place",
    "' (power loss between the two renames), put the newest complete copy back first.",
    `exePath = ${literal}`,
    'Set fso = CreateObject("Scripting.FileSystemObject")',
    "If Not fso.FileExists(exePath) Then",
    "  exeDir = fso.GetParentFolderName(exePath)",
    "  If fso.FolderExists(exeDir) Then",
    "    exeName = fso.GetFileName(exePath)",
    '    best = ""',
    "    bestTime = CDate(0)",
    "    For Each f In fso.GetFolder(exeDir).Files",
    '      If (Left(f.Name, Len(exeName) + 5) = exeName & ".old-" Or Left(f.Name, Len(exeName) + 5) = exeName & ".new-") And InStr(f.Name, ".part") = 0 Then',
    "        If f.Size > 0 And f.DateLastModified > bestTime Then",
    "          best = f.Path",
    "          bestTime = f.DateLastModified",
    "        End If",
    "      End If",
    "    Next",
    '    If best <> "" Then',
    "      On Error Resume Next",
    "      fso.MoveFile best, exePath",
    "      On Error GoTo 0",
    "    End If",
    "  End If",
    "End If",
  ];
}

/**
 * 把自愈块**就地**插进一份已有的登录自启脚本，返回新内容；已经是新模板就返回 null。
 *
 * 为什么是「就地插入」而不是整份重写：那份文件里还有 install-service 时的
 * `sh.Run "... run --model kimi"` 参数和用户可能手改过的内容。整份重写会把它们
 * 一起冲掉（审查报告 M3a 的「别无脑覆盖」）。
 */
export function injectStartupVbsSelfHeal(content: string, exePath: string): string | null {
  if (content.includes(STARTUP_VBS_TEMPLATE_MARKER)) return null;
  const lines = content.split(/\r?\n/);
  const runAt = lines.findIndex((line) => /^\s*sh\.Run\s/i.test(line));
  if (runAt < 0) return null; // 认不出的形状：宁可不改，也不要写坏别人的开机脚本
  const block = [`' ${STARTUP_VBS_TEMPLATE_MARKER}`, ...startupVbsSelfHealLines(exePath)];
  return [...lines.slice(0, runAt), ...block, ...lines.slice(runAt)].join("\r\n");
}
/** 日志超过这个大小就先轮转成 supervisor.log.1，避免无限增长。 */
const LOG_ROTATE_BYTES = 5 * 1024 * 1024;

export interface DaemonPaths {
  stateDir: string;
  logDir: string;
  pidFile: string;
  stateFile: string;
}

/** supervisor 自己写下的运行详情；`status` 的全部依据。 */
export interface SupervisorState {
  pid: number;
  startedAt: number;
  execPath: string;
  cwd: string;
  model?: string;
  agents: string[];
  version?: string;
}

export interface DaemonStartOptions {
  model?: string;
  workdir?: string;
  version?: string;
}

export interface DaemonStatusReport {
  platform: NodeJS.Platform;
  running: boolean;
  /** 有 pid/状态文件但进程已经不在 —— 典型是一次崩掉或强杀留下的残骸。 */
  stale: boolean;
  pid?: number;
  startedAt?: number;
  uptimeMs?: number;
  agents: string[];
  model?: string;
  execPath?: string;
  cwd?: string;
  logs: string[];
  autostart?: string;
  serviceName: string;
}

export interface Daemon {
  start(o?: DaemonStartOptions): Promise<void>;
  stop(): Promise<void>;
  restart(o?: DaemonStartOptions): Promise<void>;
  status(): DaemonStatusReport;
  logs(o?: { lines?: number }): void;
  installService(o?: DaemonStartOptions): Promise<void>;
  uninstall(): Promise<void>;
  /**
   * 把存量设备磁盘上那份**旧模板**的登录自启脚本就地补上自愈块。
   *
   * 为什么必须有：自愈块写在 `windowsStartupVbs()` 里，但那份模板只在
   * `install-service` 时落盘；远程升级走的是 spawn `target run`，**不会重写 VBS**。
   * 于是「已经把设备升到新版」的那批机器，磁盘上仍是旧模板 —— 而 S4 要救的正是
   * 「没有任何进程能启动」这个场景，对被远程升级的设备等于没修（审查报告 M3a）。
   */
  refreshStartupScript(): Promise<{ updated: boolean; path: string; reason?: string }>;
}

/** 只依赖 write()，这样测试里塞个收集数组就能当 stdout。 */
export interface Writable {
  write(chunk: string): unknown;
}

export interface DaemonOverrides {
  platform?: NodeJS.Platform;
  paths?: DaemonPaths;
  home?: string;
  env?: NodeJS.ProcessEnv;
  version?: string;
  spawn?: typeof spawn;
  spawnSync?: typeof spawnSync;
  isAlive?: (pid: number) => boolean;
  signal?: (pid: number, signal: NodeJS.Signals) => void;
  sleep?: (ms: number) => Promise<void>;
  now?: () => number;
  selfArgv?: () => { exec: string; args: string[] };
  stdout?: Writable;
  stderr?: Writable;
  /** 0 表示 root（决定 systemd 写系统级还是 --user）。 */
  uid?: number | undefined;
  /** 注入 systemd / launchd 的动作，测试里记下来断言即可。 */
  runCommand?: (file: string, args: string[]) => { status: number | null; stdout: string; stderr: string };
  /** 自启项的落地路径。测试必须覆盖，否则会写到真实的 /etc 或用户目录里去。 */
  autostartPaths?: {
    systemUnit?: string;
    userUnit?: string;
    launchAgent?: string;
    startupVbs?: string;
  };
}

// ---------------------------------------------------------------- 路径与默认实现

export function defaultDaemonPaths(stateDir = process.env.CLAWMESSENGER_HOME || join(homedir(), ".clawmessenger")): DaemonPaths {
  const logDir = join(stateDir, "logs");
  return {
    stateDir,
    logDir,
    pidFile: join(stateDir, "supervisor.pid"),
    stateFile: join(stateDir, "supervisor.json"),
  };
}

/**
 * 重新拉起「自己」时该带什么参数。
 *   SEA 单文件 exe：argv[1] 不是 .js（根本没有脚本路径）→ 直接用 exe 本身。
 *   node 直跑 bundle：argv[1] 是 dist/bin.js，必须把它带上。
 * 与 main.ts 里 runSupervisor 拉起 agent 子进程时用的是同一套判断。
 */
export function supervisorSpawnArgv(script: string | undefined = process.argv[1]): string[] {
  const isSea = !script || !script.endsWith(".js");
  return isSea ? [] : [script];
}

function defaultIsAlive(pid: number): boolean {
  if (!Number.isInteger(pid) || pid <= 0) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    // EPERM = 进程存在但不归我们管，依旧算活着。
    return (err as NodeJS.ErrnoException).code === "EPERM";
  }
}

/**
 * `run` 用这个环境变量强行绕过「已有 supervisor 在跑」的拦截。
 * 只在真的要并排跑第二份（例如本机调试）时才用 —— 两份 supervisor 会派生
 * **完全相同**的融云节点 id，互相踢线，且每条私聊被各答一次。
 */
export const ALLOW_DUPLICATE_SUPERVISOR_ENV = "CLAWMESSENGER_ALLOW_DUPLICATE_SUPERVISOR";

export function readSupervisorPid(paths: DaemonPaths = defaultDaemonPaths()): number | undefined {
  try {
    const digits = readFileSync(paths.pidFile, "utf8").replace(/[^0-9]/g, "");
    if (!digits) return undefined;
    const pid = Number.parseInt(digits, 10);
    return Number.isInteger(pid) && pid > 0 ? pid : undefined;
  } catch {
    return undefined;
  }
}

/**
 * 「现在有没有另一个 supervisor 在跑」的**唯一判据**，`start` 与 `run` 共用。
 *
 * pid 文件优先（Windows 上没有命令行可核对），状态文件补上详情。返回 null 表示没有。
 *
 * 为什么 `run` 也必须自己看一遍：`start` 的拦截只覆盖「通过 start 启动」这一条路，
 * 而 supervisor 还会被别的入口直接拉起 —— 开机自启的 .vbs / 守护脚本的
 * `-Action run` / 用户在终端里手敲 `clawmessenger run`。这些入口全部绕过 start，
 * 于是能在已有一份在跑的情况下再起一份：两份都派生同样的融云节点 id，互相踢线
 * （设备列表一会在线一会离线），同一条私聊被两套各答一次（重复回复）。
 */
export function findLiveSupervisor(
  paths: DaemonPaths = defaultDaemonPaths(),
  isAlive: (pid: number) => boolean = defaultIsAlive,
): { pid: number; state: SupervisorState | null } | null {
  const state = readSupervisorState(paths);
  for (const candidate of [readSupervisorPid(paths), state?.pid]) {
    if (candidate && isAlive(candidate)) {
      return { pid: candidate, state: state && state.pid === candidate ? state : null };
    }
  }
  return null;
}

export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "?";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h${String(m % 60).padStart(2, "0")}m`;
  return `${Math.floor(h / 24)}d${String(h % 24).padStart(2, "0")}h`;
}

// ---------------------------------------------------------------- 状态文件

export function readSupervisorState(paths: DaemonPaths = defaultDaemonPaths()): SupervisorState | null {
  try {
    const parsed = JSON.parse(readFileSync(paths.stateFile, "utf8")) as SupervisorState;
    if (!parsed || typeof parsed.pid !== "number") return null;
    return { ...parsed, agents: Array.isArray(parsed.agents) ? parsed.agents : [] };
  } catch {
    return null;
  }
}

export function writeSupervisorState(state: SupervisorState, paths: DaemonPaths = defaultDaemonPaths()): void {
  try {
    mkdirSync(paths.stateDir, { recursive: true });
    writeFileSync(paths.stateFile, `${JSON.stringify(state, null, 2)}\n`, { mode: 0o600 });
    writeFileSync(paths.pidFile, `${state.pid}\n`, { mode: 0o600 });
  } catch {
    // 尽力而为：写不进去只影响 status 的显示，不影响 supervisor 本身运行。
  }
}

export function clearSupervisorState(paths: DaemonPaths = defaultDaemonPaths()): void {
  for (const p of [paths.stateFile, paths.pidFile]) {
    try {
      rmSync(p, { force: true });
    } catch {
      // 忽略：清不掉就不是致命问题。
    }
  }
}

// ---------------------------------------------------------------- 日志

/** 最新一份日志（兼容脚本时代的 supervisor-<时间戳>.out.log 命名）。 */
export function latestLogPath(paths: DaemonPaths = defaultDaemonPaths()): string | undefined {
  try {
    const names = readdirSync(paths.logDir).filter((n) => /^supervisor.*\.log$/.test(n));
    if (names.length === 0) return undefined;
    const scored = names.map((n) => {
      const full = join(paths.logDir, n);
      let mtime = 0;
      try {
        mtime = statSync(full).mtimeMs;
      } catch {
        // 读不到时间就当最旧。
      }
      return { full, mtime };
    });
    scored.sort((a, b) => b.mtime - a.mtime);
    return scored[0].full;
  } catch {
    return undefined;
  }
}

export function tailLines(text: string, lines: number): string {
  const all = text.split(/\r?\n/);
  // 末尾换行会产生一个空元素，去掉它行数才准。
  if (all.length > 0 && all[all.length - 1] === "") all.pop();
  return all.slice(Math.max(0, all.length - lines)).join("\n");
}

// ---------------------------------------------------------------- 构造

export function createDaemon(overrides: DaemonOverrides = {}): Daemon {
  const paths = overrides.paths ?? defaultDaemonPaths();
  const platform = overrides.platform ?? process.platform;
  const home = overrides.home ?? homedir();
  const env = overrides.env ?? process.env;
  const stdout: Writable = overrides.stdout ?? process.stdout;
  const stderr: Writable = overrides.stderr ?? process.stderr;
  const out = (s: string): void => {
    stdout.write(s);
  };
  const err = (s: string): void => {
    stderr.write(s);
  };
  const isAlive = overrides.isAlive ?? defaultIsAlive;
  const spawnFn = overrides.spawn ?? spawn;
  const spawnSyncFn = overrides.spawnSync ?? spawnSync;
  const sleep = overrides.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
  const now = overrides.now ?? (() => Date.now());
  const selfArgv = overrides.selfArgv ?? (() => ({ exec: process.execPath, args: supervisorSpawnArgv() }));
  const uid = overrides.uid !== undefined ? overrides.uid : typeof process.getuid === "function" ? process.getuid() : undefined;
  const runCommand =
    overrides.runCommand ??
    ((file: string, args: string[]) => {
      const r = spawnSyncFn(file, args, { encoding: "utf8" });
      return { status: r.status, stdout: r.stdout ?? "", stderr: r.stderr ?? "" };
    });

  const autostartPaths = overrides.autostartPaths ?? {};
  const SYSTEM_UNIT = autostartPaths.systemUnit ?? `/etc/systemd/system/${DAEMON_SERVICE_NAME}.service`;
  const USER_UNIT =
    autostartPaths.userUnit ??
    join(env.XDG_CONFIG_HOME || join(home, ".config"), "systemd", "user", `${DAEMON_SERVICE_NAME}.service`);
  const LAUNCH_AGENT =
    autostartPaths.launchAgent ?? join(home, "Library", "LaunchAgents", "com.clawmessenger.supervisor.plist");
  const STARTUP_VBS =
    autostartPaths.startupVbs ??
    join(
      env.APPDATA || join(home, "AppData", "Roaming"),
      "Microsoft",
      "Windows",
      "Start Menu",
      "Programs",
      "Startup",
      "clawmessenger.vbs",
    );

  const readPidFile = (): number | undefined => readSupervisorPid(paths);

  /** pid 文件优先（Windows 上没有命令行可核对），状态文件补上详情。 */
  const findRunning = (): { pid: number; state: SupervisorState | null } | null =>
    findLiveSupervisor(paths, isAlive);

  const stateFilesPresent = (): boolean => existsSync(paths.pidFile) || existsSync(paths.stateFile);

  const logPath = (): string => {
    const dir = paths.logDir;
    const target = join(dir, "supervisor.log");
    try {
      const st = statSync(target);
      if (st.size > LOG_ROTATE_BYTES) renameSync(target, join(dir, "supervisor.log.1"));
    } catch {
      // 文件不存在或改不动：继续追加即可。
    }
    return target;
  };

  const resolveWorkdir = (workdir?: string): string => {
    const wd = workdir || env.CLAWMESSENGER_WORKDIR || home;
    if (!existsSync(wd)) throw new Error(`--workdir does not exist: ${wd}`);
    return wd;
  };

  const autostartDescription = (): string | undefined => {
    if (platform === "linux") {
      if (existsSync(SYSTEM_UNIT)) return `systemd system service (${SYSTEM_UNIT})`;
      if (existsSync(USER_UNIT)) return `systemd user service (${USER_UNIT})`;
      return undefined;
    }
    if (platform === "darwin") return existsSync(LAUNCH_AGENT) ? `LaunchAgent (${LAUNCH_AGENT})` : undefined;
    if (platform === "win32") return existsSync(STARTUP_VBS) ? `Startup folder script (${STARTUP_VBS})` : undefined;
    return undefined;
  };

  const targetPidFile = (): string | undefined => {
    const pid = readSupervisorPid(paths);
    return pid === undefined ? undefined : String(pid);
  };

  async function start(o: DaemonStartOptions = {}): Promise<void> {
    const running = findRunning();
    if (running) {
      out(`already running: supervisor PID ${running.pid}`);
      if (running.state?.agents.length) out(` (agents: ${running.state.agents.join(", ")})`);
      out("\n  restart with: clawmessenger restart\n  stop with:    clawmessenger stop\n");
      return;
    }
    const workdir = resolveWorkdir(o.workdir);
    mkdirSync(paths.logDir, { recursive: true });
    const target = logPath();
    const fd = openSync(target, "a");
    const { exec, args } = selfArgv();
    const argv = [...args, "run"];
    if (o.model) argv.push("--model", o.model);
    let child: ChildProcess;
    try {
      child = spawnFn(exec, argv, {
        detached: true,
        stdio: ["ignore", fd, fd],
        cwd: workdir,
        env,
        windowsHide: true,
      } as SpawnOptions);
    } finally {
      closeSync(fd);
    }
    // unref 之后调用方（终端）退出不会再牵连它；detached 已经把它放进独立会话/进程组。
    child.unref();
    const pid = child.pid;
    if (!pid) throw new Error("failed to start the supervisor (child has no pid)");

    // 自己先写一份 pid：万一 supervisor 还没来得及写状态就崩溃，stop 也有东西可用。
    try {
      mkdirSync(paths.stateDir, { recursive: true });
      writeFileSync(paths.pidFile, `${pid}\n`, { mode: 0o600 });
    } catch {
      // 尽力而为。
    }

    // 等 supervisor 把状态文件写出来（要先连 IM、拉绑定列表，通常 1-3 秒）。
    let state: SupervisorState | null = null;
    for (let i = 0; i < 40; i++) {
      await sleep(250);
      if (!isAlive(pid)) break;
      state = readSupervisorState(paths);
      if (state) break;
    }

    if (!isAlive(pid)) {
      clearSupervisorState(paths);
      err(`supervisor exited immediately after start (PID ${pid}). Log tail:\n`);
      try {
        const tail = tailLines(readFileSync(target, "utf8"), 20);
        if (tail.trim()) err(`${tail.replace(/^/gm, "  ")}\n`);
      } catch {
        // 没日志就算了。
      }
      throw new Error("supervisor failed to stay up");
    }

    out(`started in the background: supervisor PID ${pid}\n`);
    out(`  exe     : ${exec}\n  workdir : ${workdir}\n  log     : ${target}\n`);
    if (state?.agents.length) out(`  agents  : ${state.agents.join(", ")}\n`);
    out("  status  : clawmessenger status      stop: clawmessenger stop\n");
    out("  note    : heartbeats take a few seconds; refresh the device list shortly.\n");
  }

  function safeSignal(pid: number, signal: NodeJS.Signals): void {
    try {
      (overrides.signal ?? ((p: number, s: NodeJS.Signals) => process.kill(p, s)))(pid, signal);
    } catch {
      // 进程可能已经没了。
    }
  }

  async function stop(): Promise<void> {
    const running = findRunning();
    if (!running) {
      const stale = stateFilesPresent();
      clearSupervisorState(paths);
      out(
        stale
          ? "no clawmessenger supervisor is running (cleared a stale pid/state file).\n"
          : "no clawmessenger supervisor is running.\n",
      );
      return;
    }
    const pid = running.pid;
    const count = running.state?.agents.length ?? 0;
    out(`stopping supervisor PID ${pid}${count ? ` (and its ${count} agent process(es))` : ""}...\n`);
    if (platform === "win32") {
      // /T 收整棵进程树；Node 的 process.kill 在 Windows 上做不到这一点。
      spawnSyncFn("taskkill", ["/PID", String(pid), "/T", "/F"], { stdio: "ignore" });
      await sleep(500);
    } else {
      safeSignal(pid, "SIGTERM");
      let alive = true;
      for (let i = 0; i < 30; i++) {
        await sleep(500);
        if (!isAlive(pid)) {
          alive = false;
          break;
        }
      }
      if (alive) {
        out("graceful stop timed out (~15s), escalating to SIGKILL.\n");
        safeSignal(pid, "SIGKILL");
        await sleep(500);
      }
    }
    clearSupervisorState(paths);
    if (isAlive(pid)) {
      err(`stop failed: PID ${pid} is still alive — run clawmessenger stop again.\n`);
      return;
    }
    out("stopped. agent heartbeats stop now; the admin page shows the device offline in ~90s.\n");
  }

  async function restart(o: DaemonStartOptions = {}): Promise<void> {
    await stop();
    await sleep(1_000);
    await start(o);
  }

  function status(): DaemonStatusReport {
    const state = readSupervisorState(paths);
    const pidFromFile = readPidFile();
    const pid = pidFromFile ?? state?.pid;
    const alive = pid !== undefined && isAlive(pid);
    const report: DaemonStatusReport = {
      platform,
      running: alive,
      stale: !alive && (pid !== undefined || state !== null),
      agents: [],
      logs: [],
      serviceName: DAEMON_SERVICE_NAME,
    };
    const autostart = autostartDescription();
    if (autostart) report.autostart = autostart;
    const latest = latestLogPath(paths);
    if (latest) report.logs.push(latest);
    if (!alive) return report;
    report.pid = pid;
    if (state && state.pid === pid) {
      report.startedAt = state.startedAt;
      report.uptimeMs = Math.max(0, now() - state.startedAt);
      report.agents = state.agents;
      report.execPath = state.execPath;
      report.cwd = state.cwd;
      if (state.model) report.model = state.model;
    }
    return report;
  }

  function logs(o: { lines?: number } = {}): void {
    const latest = latestLogPath(paths);
    if (!latest) {
      out(`no supervisor log yet (looked in ${paths.logDir}).\n`);
      return;
    }
    let text: string;
    try {
      text = readFileSync(latest, "utf8");
    } catch {
      err(`cannot read log: ${latest}\n`);
      return;
    }
    out(`${latest}\n`);
    out(`${tailLines(text, o.lines ?? 40)}\n`);
  }

  function escapeXml(s: string): string {
    return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  }

  function runArgvFor(o: DaemonStartOptions): string[] {
    const { exec, args } = selfArgv();
    return [exec, ...args, "run", ...(o.model ? ["--model", o.model] : [])];
  }

  function systemdUnit(o: DaemonStartOptions): { path: string; scope: "system" | "user"; wantedBy: string; content: string } {
    const workdir = resolveWorkdir(o.workdir);
    const [exec, ...rest] = runArgvFor(o);
    const scope = uid === 0 ? "system" : "user";
    const path = scope === "system" ? SYSTEM_UNIT : USER_UNIT;
    const wantedBy = scope === "system" ? "multi-user.target" : "default.target";
    // 引号策略（踩过坑，别改回去）—— systemd 对各个指令的引号处理**不一致**：
    //   · Environment= / ExecStart= 的解析器会自己去引号（EXTRACT_UNQUOTE），加引号安全；
    //   · WorkingDirectory= **完全不去引号**，值被当成字面量路径。于是
    //     WorkingDirectory="/root" 里的 `"/root"` 不是绝对路径，走到
    //     path_simplify_and_warn(..., PATH_CHECK_ABSOLUTE|PATH_CHECK_FATAL) 就返回 -ENOEXEC
    //     （src/core/load-fragment.c，v240 与 v257 逐字相同），unit 被记成 UNIT_BAD_SETTING，
    //     `systemctl start` 只报一句 "Unit ... has a bad unit file setting."。
    // 所以：值里没有空白就一律裸写（任何版本都最稳），有空白才加引号；
    // WorkingDirectory= 永远裸写（它本来也不支持引号，路径里有空格直接写空格即可）。
    const quote = (a: string): string => (/[\s"]/.test(a) ? `"${a.replace(/"/g, '\\"')}"` : a);
    const execStart = [quote(exec), ...rest.map(quote)].join(" ");
    const envLine = (name: string, value: string): string => `Environment=${quote(`${name}=${value}`)}`;
    // PATH 必须写死：systemd 的默认 PATH 很简，而 agent CLI（opencode / codex /
    // claude）常装在 nvm 之类不在默认 PATH 里的位置，漏掉就会被跳过。
    const content = [
      "[Unit]",
      "Description=clawmessenger agent supervisor (RongCloud IM)",
      "After=network-online.target",
      "Wants=network-online.target",
      "",
      "[Service]",
      "Type=simple",
      envLine("HOME", home),
      envLine("PATH", env.PATH ?? "/usr/local/bin:/usr/bin:/bin"),
      `WorkingDirectory=${workdir}`,
      `ExecStart=${execStart}`,
      "Restart=always",
      "RestartSec=5",
      "KillMode=control-group",
      "TimeoutStopSec=20",
      "StandardOutput=journal",
      "StandardError=journal",
      "",
      "[Install]",
      `WantedBy=${wantedBy}`,
      "",
    ].join("\n");
    return { path, scope, wantedBy, content };
  }

  function launchAgentPlist(o: DaemonStartOptions): string {
    const workdir = resolveWorkdir(o.workdir);
    const argv = runArgvFor(o);
    const args = argv.map((a) => `    <string>${escapeXml(a)}</string>`).join("\n");
    return [
      '<?xml version="1.0" encoding="UTF-8"?>',
      '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">',
      '<plist version="1.0">',
      "<dict>",
      "  <key>Label</key>",
      "  <string>com.clawmessenger.supervisor</string>",
      "  <key>ProgramArguments</key>",
      "  <array>",
      args,
      "  </array>",
      "  <key>RunAtLoad</key>",
      "  <true/>",
      "  <key>KeepAlive</key>",
      "  <true/>",
      "  <key>WorkingDirectory</key>",
      `  <string>${escapeXml(workdir)}</string>`,
      "  <key>EnvironmentVariables</key>",
      "  <dict>",
      "    <key>HOME</key>",
      `    <string>${escapeXml(home)}</string>`,
      "    <key>PATH</key>",
      `    <string>${escapeXml(env.PATH ?? "/usr/local/bin:/usr/bin:/bin")}</string>`,
      "  </dict>",
      "  <key>StandardOutPath</key>",
      `  <string>${escapeXml(join(paths.logDir, "supervisor.log"))}</string>`,
      "  <key>StandardErrorPath</key>",
      `  <string>${escapeXml(join(paths.logDir, "supervisor.log"))}</string>`,
      "</dict>",
      "</plist>",
      "",
    ].join("\n");
  }

  function windowsStartupVbs(o: DaemonStartOptions): string {
    const argv = runArgvFor(o);
    // 这里有**两层**引号，别混：
    //   第一层是 shell 命令行，只有含空白的参数才需要 `"…"`；
    //   第二层是 VBScript 字符串字面量，字面量里的每个 `"` 都要写成 `""`。
    // 合起来 `"C:\...\clawmessenger.exe" run` 落盘正是
    //   sh.Run """C:\...\clawmessenger.exe"" run", 0, False
    // 0.2.49 及之前漏了最外层的开引号（写成 `""C:\...exe"" run`），VBScript 把它解析成
    // 空字符串紧跟一个裸标识符 → 语法错误弹窗，开机自启从来没生效过（2026-10-22 重启
    // 后运维节点全离线就是这个）。daemon.test.ts 之前也按错的形状断言，所以一直是绿的。
    const quote = (a: string): string => (/[\s"]/.test(a) ? `"${a}"` : a);
    const cmdline = [quote(argv[0]), ...argv.slice(1).map(quote)].join(" ");
    const literal = `"${cmdline.replace(/"/g, '""')}"`;
    // CurrentDirectory 必须钉死：登录自启时 wscript 的 cwd 是 System32，agent 会去
    // 那里找 opencode.json / 相对路径产物，运维轮次直接翻车。
    const workdir = resolveWorkdir(o.workdir);
    // 自愈块与「就地补进旧模板」的那份共用 startupVbsSelfHealLines()：这段是设备
    // 唯一的自救路径，两处各写一份迟早漂移（审查报告 M3b 就是这么漏掉 .part 的）。
    return [
      `' ${STARTUP_VBS_TEMPLATE_MARKER}`,
      "' clawmessenger: start the agent supervisor at logon, without a console window.",
      "' Generated by `clawmessenger install-service`. Remove with `clawmessenger uninstall`.",
      'Set sh = CreateObject("WScript.Shell")',
      `sh.CurrentDirectory = "${workdir}"`,
      ...startupVbsSelfHealLines(argv[0]),
      `sh.Run ${literal}, 0, False`,
      "",
    ].join("\r\n");
  }

  /**
   * 服务管理器拒收 unit 时，把「它自己那句话」原样带出来。
   * `systemctl start` 只会说一句 "has a bad unit file setting"，真正哪一行不合法只在
   * status / journal 里；与其让用户自己去翻，不如我们连着 unit 正文一起打出来。
   */
  function dumpSystemdDiagnostics(sctl: string[], unitPath: string, unitBody: string): void {
    const show = (file: string, args: string[], label: string): void => {
      const r = runCommand(file, args);
      const text = `${r.stdout ?? ""}${r.stderr ?? ""}`.trim();
      if (text) err(`${label}\n${tailLines(text, 25).replace(/^/gm, "  ")}\n`);
    };
    show(sctl[0], [...sctl.slice(1), "status", DAEMON_SERVICE_NAME, "--no-pager", "-l"], `$ ${sctl.join(" ")} status ${DAEMON_SERVICE_NAME}`);
    // LoadState 能看出是不是 unit 文件本身被判坏了（bad-setting）；LoadError 才是被 systemd
    // 藏起来的那句真话（例如 "Invalid argument"）。LoadError 属性 systemd v240 起才有，老版本这里
    // 会多一行「Unknown property」，出现在失败转储里可以接受。
    show(
      sctl[0],
      [...sctl.slice(1), "show", DAEMON_SERVICE_NAME, "-p", "LoadState", "-p", "LoadError", "--no-pager"],
      `$ ${sctl.join(" ")} show ${DAEMON_SERVICE_NAME} -p LoadState -p LoadError`,
    );
    show(
      "journalctl",
      [...(sctl.includes("--user") ? ["--user"] : []), "-u", DAEMON_SERVICE_NAME, "--no-pager", "-n", "25"],
      `$ journalctl -u ${DAEMON_SERVICE_NAME}`,
    );
    show("systemd-analyze", ["verify", unitPath], `$ systemd-analyze verify ${unitPath}`);
    err(`unit file ${unitPath}:\n${unitBody.replace(/^/gm, "  ")}`);
  }

  /**
   * 自启服务没能起来时，把「刚才被我们停掉的」后台 supervisor 拉回来。
   * 反面教材：先把 supervisor 停掉、再 `systemctl start`，一旦 start 失败，
   * 装个自启反而把在线设备搞下线了。
   */
  async function restoreAfterFailedService(o: DaemonStartOptions, message: string): Promise<void> {
    err(`${message}\n`);
    out("restoring the background supervisor so the device stays online...\n");
    try {
      await start(o);
    } catch (e) {
      const detail = e instanceof Error ? e.message : String(e);
      err(`could not restart the background supervisor either: ${detail}\n`);
      err("this device is offline now — after fixing the autostart entry, run `clawmessenger start`.\n");
    }
  }

  /**
   * 用 systemd 自己的解析器（`systemd-analyze verify`）先把 unit 过一遍。
   * 这一步**零副作用**：能在「还没把手上的 supervisor 停掉」之前，就把
   * 「Unit ... has a bad unit file setting.」这类 unit 自身的问题拦下来，设备不掉线。
   *
   * 返回 null = 通过（或者这台机器上没有 systemd-analyze，此时不拦，交给真正的 start 去判）；
   * 返回字符串 = 被拒，内容是 systemd 的原文。
   */
  function verifySystemdUnit(unitPath: string): string | null {
    const r = runCommand("systemd-analyze", ["verify", unitPath]);
    // status === null 是 spawnSync 的「命令没跑起来」（没装 / 找不到二进制），
    // 不能据此判定 unit 有问题，否则会在精简的容器镜像里误伤。
    if (r.status === null || r.status === 0) return null;
    const detail = `${r.stdout ?? ""}${r.stderr ?? ""}`.trim();
    return detail || `systemd-analyze verify exited ${r.status}`;
  }

  /**
   * 存量设备的登录自启脚本补自愈块。**就地插入**，不整份重写（保留
   * `--model` 参数与用户手改的内容），按模板标记判幂等。
   *
   * 三种情况明确不写：不是 Windows、本来就没有登录脚本（普通 `run` 不该把用户
   * 变成开机自启）、内容形状认不出来（宁可不动）。
   */
  async function refreshStartupScript(): Promise<{ updated: boolean; path: string; reason?: string }> {
    if (platform !== "win32") return { updated: false, path: STARTUP_VBS, reason: "not-windows" };
    if (!existsSync(STARTUP_VBS)) return { updated: false, path: STARTUP_VBS, reason: "no-logon-script" };
    let content: string;
    try {
      content = readFileSync(STARTUP_VBS, "utf8");
    } catch {
      return { updated: false, path: STARTUP_VBS, reason: "unreadable" };
    }
    if (content.includes(STARTUP_VBS_TEMPLATE_MARKER)) {
      return { updated: false, path: STARTUP_VBS, reason: "already-current" };
    }
    const injected = injectStartupVbsSelfHeal(content, selfArgv().exec);
    if (injected === null) {
      return { updated: false, path: STARTUP_VBS, reason: "unrecognized-shape" };
    }
    try {
      writeFileSync(STARTUP_VBS, injected, { mode: 0o644 });
    } catch {
      return { updated: false, path: STARTUP_VBS, reason: "write-failed" };
    }
    return { updated: true, path: STARTUP_VBS };
  }

  async function installService(o: DaemonStartOptions = {}): Promise<void> {
    if (platform === "linux") {
      const unit = systemdUnit(o);
      mkdirSync(join(unit.path, ".."), { recursive: true });
      writeFileSync(unit.path, unit.content, { mode: 0o644 });
      const sctl = unit.scope === "system" ? ["systemctl"] : ["systemctl", "--user"];
      const reload = runCommand(sctl[0], [...sctl.slice(1), "daemon-reload"]);
      if (reload.status !== 0) {
        err(`systemctl daemon-reload failed: ${reload.stderr.trim() || `exit ${reload.status}`}\n`);
      }

      // 先校验再动手：unit 本身不合法时直接返回，既不停 supervisor、也不 enable，
      // 免得装个开机自启反而把在线设备搞下线（0.2.15 的 WorkingDirectory="…" 就是这个坑）。
      const rejected = verifySystemdUnit(unit.path);
      if (rejected !== null) {
        err(`systemd-analyze verify rejected the unit:\n${tailLines(rejected, 25).replace(/^/gm, "  ")}\n`);
        err(`unit file ${unit.path}:\n${unit.content.replace(/^/gm, "  ")}\n`);
        err("nothing was stopped and the autostart entry was NOT enabled — the device stays online.\n");
        err("fix the line reported above, or drop the entry with `clawmessenger uninstall`.\n");
        return;
      }

      const enable = runCommand(sctl[0], [...sctl.slice(1), "enable", DAEMON_SERVICE_NAME]);
      if (enable.status !== 0) {
        err(`systemctl enable failed: ${enable.stderr.trim() || `exit ${enable.status}`}\n`);
      }
      out(`installed systemd ${unit.scope} service: ${unit.path}\n`);
      // 之前可能有一套手工/脚本方式起的 supervisor，先收掉，避免两份重复拉起同一批 agent。
      await stop();
      const startRes = runCommand(sctl[0], [...sctl.slice(1), "start", DAEMON_SERVICE_NAME]);
      if (startRes.status !== 0) {
        err(`systemctl start failed: ${startRes.stderr.trim() || `exit ${startRes.status}`}\n`);
        dumpSystemdDiagnostics(sctl, unit.path, unit.content);
        await restoreAfterFailedService(
          o,
          `the unit is at ${unit.path}, but systemd refused it (details above). It is left in place so the output can be inspected; \`clawmessenger uninstall\` removes it.`,
        );
        return;
      }
      if (unit.scope === "user") {
        const user = safeUsername();
        const linger = runCommand("loginctl", ["enable-linger", user]);
        if (linger.status !== 0) {
          out(`  hint: could not enable linger; your user service may stop at logout. Run: sudo loginctl enable-linger ${user}\n`);
        }
      }
      out(`  manage : ${sctl.join(" ")} {start|stop|restart|status} ${DAEMON_SERVICE_NAME}\n`);
      out(`  logs   : journalctl${unit.scope === "user" ? " --user" : ""} -u ${DAEMON_SERVICE_NAME} -f\n`);
      out("  remove : clawmessenger uninstall\n");
      out("  note   : the PATH baked into the unit is the one from this command — re-run install-service after moving an agent CLI.\n");
      return;
    }
    if (platform === "darwin") {
      mkdirSync(join(LAUNCH_AGENT, ".."), { recursive: true });
      mkdirSync(paths.logDir, { recursive: true });
      writeFileSync(LAUNCH_AGENT, launchAgentPlist(o), { mode: 0o644 });
      await stop();
      runCommand("launchctl", ["unload", LAUNCH_AGENT]);
      const loadRes = runCommand("launchctl", ["load", "-w", LAUNCH_AGENT]);
      if (loadRes.status !== 0) {
        err(`launchctl load failed: ${loadRes.stderr.trim() || `exit ${loadRes.status}`}\n`);
        const list = runCommand("launchctl", ["list"]);
        const hit = `${list.stdout ?? ""}${list.stderr ?? ""}`
          .split(/\r?\n/)
          .filter((l) => l.includes("clawmessenger"))
          .join("\n");
        if (hit.trim()) err(`$ launchctl list (clawmessenger entries)\n${hit.replace(/^/gm, "  ")}\n`);
        err(`plist ${LAUNCH_AGENT}:\n${launchAgentPlist(o).replace(/^/gm, "  ")}\n`);
        await restoreAfterFailedService(o, "the LaunchAgent is written, but launchctl refused to load it (details above).");
        return;
      }
      out(`installed LaunchAgent: ${LAUNCH_AGENT}\n`);
      out("  manage : launchctl list | grep clawmessenger ; launchctl unload/load -w <plist>\n");
      out(`  logs   : ${join(paths.logDir, "supervisor.log")}\n`);
      out("  remove : clawmessenger uninstall\n");
      return;
    }
    if (platform === "win32") {
      mkdirSync(join(STARTUP_VBS, ".."), { recursive: true });
      writeFileSync(STARTUP_VBS, windowsStartupVbs(o), { mode: 0o644 });
      out(`installed logon autostart: ${STARTUP_VBS}\n`);
      out("  it runs `clawmessenger run` hidden at every logon (no admin rights needed).\n");
      out("  start it now with: clawmessenger start\n");
      out("  remove with:       clawmessenger uninstall\n");
      out("  note: the launcher carries the PATH from this command; re-run install-service if an agent CLI moves.\n");
      return;
    }
    throw new Error(`install-service is not supported on platform ${platform}`);
  }

  function safeUsername(): string {
    try {
      return userInfo().username;
    } catch {
      return env.USER || env.USERNAME || "root";
    }
  }

  async function uninstall(): Promise<void> {
    const removed: string[] = [];
    if (existsSync(SYSTEM_UNIT)) {
      runCommand("systemctl", ["disable", "--now", DAEMON_SERVICE_NAME]);
      rmSync(SYSTEM_UNIT, { force: true });
      runCommand("systemctl", ["daemon-reload"]);
      removed.push(`systemd system service (${SYSTEM_UNIT})`);
    }
    if (existsSync(USER_UNIT)) {
      runCommand("systemctl", ["--user", "disable", "--now", DAEMON_SERVICE_NAME]);
      rmSync(USER_UNIT, { force: true });
      runCommand("systemctl", ["--user", "daemon-reload"]);
      removed.push(`systemd user service (${USER_UNIT})`);
    }
    if (existsSync(LAUNCH_AGENT)) {
      runCommand("launchctl", ["unload", LAUNCH_AGENT]);
      rmSync(LAUNCH_AGENT, { force: true });
      removed.push(`LaunchAgent (${LAUNCH_AGENT})`);
    }
    if (existsSync(STARTUP_VBS)) {
      rmSync(STARTUP_VBS, { force: true });
      removed.push(`logon autostart (${STARTUP_VBS})`);
    }
    if (removed.length === 0) {
      out("no autostart entry was configured.\n");
    } else {
      for (const r of removed) out(`removed ${r}\n`);
    }
    out("(the running supervisor was left alone; stop it with `clawmessenger stop`)\n");
  }

  return { start, stop, restart, status, logs, installService, uninstall, refreshStartupScript };
}

// ---------------------------------------------------------------- 展示

/** `status` 命令里追加的运行状态区块；身份部分由 main.ts 自己打印。 */
export function formatDaemonStatus(report: DaemonStatusReport, paths: DaemonPaths = defaultDaemonPaths()): string {
  const lines: string[] = [];
  if (report.running) {
    const up = report.uptimeMs !== undefined ? `, up ${formatDuration(report.uptimeMs)}` : "";
    lines.push(`daemon : running in the background (PID ${report.pid}${up})`);
    if (report.execPath) lines.push(`exe    : ${report.execPath}`);
    lines.push(`agents : ${report.agents.length ? `${report.agents.length} (${report.agents.join(", ")})` : "unknown (supervisor has not reported yet)"}`);
    if (report.model) lines.push(`model  : ${report.model}`);
    if (report.cwd) lines.push(`workdir: ${report.cwd}`);
  } else {
    lines.push("daemon : not running in the background (agents should be offline)");
    if (report.stale) {
      lines.push("         a stale pid/state file was left behind (a previous crash or `kill -9`);");
      lines.push("         run `clawmessenger stop` to clear it, then `clawmessenger start`.");
    }
    lines.push("         start it with: clawmessenger start");
  }
  const logShown = report.logs[0] ?? report.logs.join(", ");
  lines.push(`logs   : ${logShown || paths.logDir}`);
  lines.push(`autostart: ${report.autostart ?? "not configured (clawmessenger install-service)"}`);
  return `${lines.join("\n")}\n`;
}
