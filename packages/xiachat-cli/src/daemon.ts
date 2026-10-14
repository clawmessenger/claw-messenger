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

  const readPidFile = (): number | undefined => {
    try {
      const digits = readFileSync(paths.pidFile, "utf8").replace(/[^0-9]/g, "");
      if (!digits) return undefined;
      const pid = Number.parseInt(digits, 10);
      return Number.isInteger(pid) && pid > 0 ? pid : undefined;
    } catch {
      return undefined;
    }
  };

  /** pid 文件优先（Windows 上没有命令行可核对），状态文件补上详情。 */
  const findRunning = (): { pid: number; state: SupervisorState | null } | null => {
    const state = readSupervisorState(paths);
    for (const candidate of [readPidFile(), state?.pid]) {
      if (candidate && isAlive(candidate)) {
        return { pid: candidate, state: state && state.pid === candidate ? state : null };
      }
    }
    return null;
  };

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
    try {
      const digits = readFileSync(paths.pidFile, "utf8").replace(/[^0-9]/g, "");
      const pid = Number.parseInt(digits, 10);
      return Number.isInteger(pid) && pid > 0 ? String(pid) : undefined;
    } catch {
      return undefined;
    }
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
    // systemd 只把「带空格或引号的参数」用引号包起来就够；给每个参数都加引号虽然
    // 也能跑，但会让人以为参数里真含引号。
    const quote = (a: string): string => (/[\s"]/.test(a) ? `"${a.replace(/"/g, '\\"')}"` : a);
    const execStart = [`"${exec.replace(/"/g, '\\"')}"`, ...rest.map(quote)].join(" ");
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
      `Environment="HOME=${home}"`,
      `Environment="PATH=${env.PATH ?? "/usr/local/bin:/usr/bin:/bin"}"`,
      `WorkingDirectory="${workdir}"`,
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
    // WScript 的引号规则：命令里的引号要写成两个，所以可执行文件路径写成
    // ""C:\...\clawmessenger.exe""。0 = 隐藏窗口，False = 不等它结束。
    const quote = (a: string): string => (/\s/.test(a) ? `""${a}""` : a);
    const cmdline = [`""${argv[0]}""`, ...argv.slice(1).map(quote)].join(" ");
    return [
      "' clawmessenger: start the agent supervisor at logon, without a console window.",
      "' Generated by `clawmessenger install-service`. Remove with `clawmessenger uninstall`.",
      'Set sh = CreateObject("WScript.Shell")',
      `sh.Run ${cmdline}, 0, False`,
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

  return { start, stop, restart, status, logs, installService, uninstall };
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
