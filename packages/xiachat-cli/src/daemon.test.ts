// @vitest-environment node
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import type { SpawnOptions } from "node:child_process";
import { spawn } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import {
  createDaemon,
  defaultDaemonPaths,
  findLiveSupervisor,
  formatDaemonStatus,
  supervisorSpawnArgv,
  writeSupervisorState,
  type DaemonOverrides,
  type DaemonPaths,
} from "./daemon.js";

const EXE = "/opt/clawmessenger/clawmessenger";

const dirs: string[] = [];
afterEach(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
  dirs.length = 0;
});

interface Harness {
  paths: DaemonPaths;
  home: string;
  lines: string[];
  errors: string[];
  /** pid → alive? Default: nothing is alive. */
  alive: Set<number>;
  signals: Array<{ pid: number; signal: string }>;
  spawns: Array<{ exec: string; argv: string[]; options: SpawnOptions }>;
  commands: Array<{ file: string; args: string[] }>;
  daemon: ReturnType<typeof createDaemon>;
  /** Set to make spawn() also publish the supervisor state, like the real child. */
  publishOnSpawn?: (pid: number) => void;
  spawnPid: number;
  spawnError?: Error;
}

function harness(overrides: Partial<DaemonOverrides> = {}, opts: { spawnPid?: number; publish?: boolean } = {}): Harness {
  const home = mkdtempSync(join(tmpdir(), "clawmessenger-daemon-"));
  dirs.push(home);
  const paths = defaultDaemonPaths(join(home, ".clawmessenger"));
  const lines: string[] = [];
  const errors: string[] = [];
  const alive = new Set<number>();
  const signals: Array<{ pid: number; signal: string }> = [];
  const spawns: Array<{ exec: string; argv: string[]; options: SpawnOptions }> = [];
  const commands: Array<{ file: string; args: string[] }> = [];
  const spawnPid = opts.spawnPid ?? 555;

  const h: Harness = {
    paths,
    home,
    lines,
    errors,
    alive,
    signals,
    spawns,
    commands,
    spawnPid,
    daemon: undefined as unknown as ReturnType<typeof createDaemon>,
  };

  const spawnFn = ((exec: string, argv: string[], options: SpawnOptions) => {
    if (h.spawnError) throw h.spawnError;
    spawns.push({ exec, argv, options });
    h.publishOnSpawn?.(spawnPid);
    return { pid: spawnPid, unref: () => {} };
  }) as unknown as typeof spawn;

  h.daemon = createDaemon({
    platform: "linux",
    home,
    paths,
    env: { PATH: "/usr/bin:/bin", HOME: home },
    uid: 0,
    now: () => 1_000_000,
    sleep: async () => {},
    isAlive: (pid) => alive.has(pid),
    signal: (pid, signal) => {
      signals.push({ pid, signal });
      // SIGTERM is graceful: the fake supervisor goes away on it.
      if (signal === "SIGTERM") alive.delete(pid);
    },
    spawn: spawnFn,
    spawnSync: (() => ({ status: 0, stdout: "", stderr: "", pid: 1 })) as unknown as typeof import("node:child_process").spawnSync,
    selfArgv: () => ({ exec: EXE, args: [] }),
    stdout: { write: (s: string) => { lines.push(s); return true; } },
    stderr: { write: (s: string) => { errors.push(s); return true; } },
    runCommand: (file, args) => {
      commands.push({ file, args });
      return { status: 0, stdout: "", stderr: "" };
    },
    // 自启项一律落在临时目录里，绝不去碰真实的 /etc 与用户目录。
    autostartPaths: {
      systemUnit: join(paths.stateDir, "etc", "systemd", "system", "clawmessenger.service"),
      userUnit: join(home, ".config", "systemd", "user", "clawmessenger.service"),
      launchAgent: join(home, "Library", "LaunchAgents", "com.clawmessenger.supervisor.plist"),
    },
    ...overrides,
  });
  return h;
}

const state = (paths: DaemonPaths, pid: number, agents: string[] = ["claude"]) => {
  writeSupervisorState(
    { pid, startedAt: 999_000, execPath: EXE, cwd: "/home/u", agents },
    paths,
  );
};

describe("supervisorSpawnArgv", () => {
  it("re-invokes the bare exe when there is no .js entry point (SEA)", () => {
    // argv[1] 在 SEA 单文件 exe 里要么不存在、要么就是 exe 自己，都不是 .js。
    expect(supervisorSpawnArgv("")).toEqual([]);
    expect(supervisorSpawnArgv("/usr/local/bin/clawmessenger")).toEqual([]);
    expect(supervisorSpawnArgv("C:\\Users\\u\\.local\\bin\\clawmessenger.exe")).toEqual([]);
  });

  it("keeps the bundle path when running under node", () => {
    expect(supervisorSpawnArgv("/repo/packages/xiachat-cli/dist/bin.js")).toEqual([
      "/repo/packages/xiachat-cli/dist/bin.js",
    ]);
  });
});

describe("findLiveSupervisor", () => {
  const nothingAlive = (): boolean => false;

  it("reports nothing when the state dir is empty", () => {
    const h = harness();
    expect(findLiveSupervisor(h.paths, nothingAlive)).toBeNull();
  });

  it("ignores a pid file left behind by a process that is gone", () => {
    const h = harness();
    mkdirSync(h.paths.stateDir, { recursive: true });
    writeFileSync(h.paths.pidFile, "4242\n");
    expect(findLiveSupervisor(h.paths, nothingAlive)).toBeNull();
  });

  it("reports the live pid together with the agents it published", () => {
    const h = harness();
    state(h.paths, 4242, ["opencode", "ops"]);
    const found = findLiveSupervisor(h.paths, (pid) => pid === 4242);
    expect(found?.pid).toBe(4242);
    expect(found?.state?.agents).toEqual(["opencode", "ops"]);
  });

  it("finds a supervisor that has a pid file but has not published its state yet", () => {
    const h = harness();
    mkdirSync(h.paths.stateDir, { recursive: true });
    writeFileSync(h.paths.pidFile, "777\n");
    const found = findLiveSupervisor(h.paths, (pid) => pid === 777);
    expect(found?.pid).toBe(777);
    expect(found?.state).toBeNull();
  });

  it("falls back to the state file when the pid file went missing", () => {
    const h = harness();
    state(h.paths, 888, ["hermes"]);
    rmSync(h.paths.pidFile, { force: true });
    const found = findLiveSupervisor(h.paths, (pid) => pid === 888);
    expect(found?.pid).toBe(888);
    expect(found?.state?.agents).toEqual(["hermes"]);
  });
});

describe("daemon start", () => {
  it("refuses to start a second supervisor and reports the running one", async () => {
    const h = harness();
    state(h.paths, 4242, ["claude", "codex"]);
    h.alive.add(4242);

    await h.daemon.start();

    expect(h.spawns).toHaveLength(0);
    expect(h.lines.join("")).toContain("already running: supervisor PID 4242");
    expect(h.lines.join("")).toContain("claude, codex");
  });

  it("spawns a detached supervisor, records its pid and reports where the log is", async () => {
    const h = harness({}, { spawnPid: 777 });
    h.alive.add(777);
    h.publishOnSpawn = (pid) => {
      writeSupervisorState(
        { pid, startedAt: 999_500, execPath: EXE, cwd: h.home, agents: ["claude", "codex"] },
        h.paths,
      );
    };

    await h.daemon.start({ model: "gpt-5" });

    expect(h.spawns).toHaveLength(1);
    const [spawned] = h.spawns;
    expect(spawned.exec).toBe(EXE);
    expect(spawned.argv).toEqual(["run", "--model", "gpt-5"]);
    expect(spawned.options.detached).toBe(true);
    expect(spawned.options.windowsHide).toBe(true);
    expect(spawned.options.cwd).toBe(h.home);
    expect(spawned.options.stdio?.[0]).toBe("ignore");

    // The pid file is what `stop` uses, and it must be on disk right away.
    expect(readFileSync(h.paths.pidFile, "utf8").trim()).toBe("777");
    const text = h.lines.join("");
    expect(text).toContain("started in the background: supervisor PID 777");
    expect(text).toContain(join(h.paths.logDir, "supervisor.log"));
    expect(text).toContain("claude, codex");
  });

  it("defaults the workdir to the home directory", async () => {
    const h = harness();
    h.alive.add(555);
    await h.daemon.start();
    expect(h.spawns[0].options.cwd).toBe(h.home);
  });

  it("rejects a --workdir that does not exist", async () => {
    const h = harness();
    await expect(h.daemon.start({ workdir: join(h.home, "nope") })).rejects.toThrow(/--workdir does not exist/);
    expect(h.spawns).toHaveLength(0);
  });

  it("clears the pid file and surfaces the log tail when the supervisor dies at once", async () => {
    const h = harness();
    mkdirSync(h.paths.logDir, { recursive: true });
    writeFileSync(join(h.paths.logDir, "supervisor.log"), "boom: no credentials\n");
    // Nothing is alive, so the very first liveness poll fails.

    await expect(h.daemon.start()).rejects.toThrow(/failed to stay up/);

    expect(existsSync(h.paths.pidFile)).toBe(false);
    expect(existsSync(h.paths.stateFile)).toBe(false);
    expect(h.errors.join("")).toContain("exited immediately after start (PID 555)");
    expect(h.errors.join("")).toContain("boom: no credentials");
  });

  it("starts anyway when the leftover pid file points at a process that is gone", async () => {
    const h = harness({}, { spawnPid: 777 });
    state(h.paths, 4242); // 上次崩溃留下的残骸，进程已经不在了
    h.alive.add(777); // 这次真正起来的 supervisor
    h.publishOnSpawn = (pid) => {
      writeSupervisorState({ pid, startedAt: 1_000_000, execPath: EXE, cwd: h.home, agents: ["claude"] }, h.paths);
    };

    await h.daemon.start();

    expect(h.spawns).toHaveLength(1);
    expect(readFileSync(h.paths.pidFile, "utf8").trim()).toBe("777");
  });
});

describe("daemon stop", () => {
  it("SIGTERMs the supervisor, waits, then clears the pid file", async () => {
    const h = harness();
    state(h.paths, 4242, ["claude"]);
    h.alive.add(4242);

    await h.daemon.stop();

    expect(h.signals).toEqual([{ pid: 4242, signal: "SIGTERM" }]);
    expect(existsSync(h.paths.pidFile)).toBe(false);
    expect(existsSync(h.paths.stateFile)).toBe(false);
    expect(h.lines.join("")).toContain("stopping supervisor PID 4242 (and its 1 agent process(es))");
    expect(h.lines.join("")).toContain("stopped.");
  });

  it("escalates to SIGKILL when the supervisor ignores SIGTERM", async () => {
    const h = harness({
      signal: (pid, signal) => {
        h.signals.push({ pid, signal });
        // Never dies: the fake ignores every signal.
      },
    });
    state(h.paths, 4242);
    h.alive.add(4242);

    await h.daemon.stop();

    expect(h.signals.map((s) => s.signal)).toEqual(["SIGTERM", "SIGKILL"]);
    expect(h.lines.join("")).toContain("escalating to SIGKILL");
    expect(h.errors.join("")).toContain("stop failed");
    expect(existsSync(h.paths.pidFile)).toBe(false);
  });

  it("reports a no-op and clears a stale pid file when nothing is running", async () => {
    const h = harness();
    state(h.paths, 4242); // written, but the process is gone

    await h.daemon.stop();

    expect(h.signals).toHaveLength(0);
    expect(h.lines.join("")).toContain("cleared a stale pid/state file");
    expect(existsSync(h.paths.pidFile)).toBe(false);
  });

  it("reports a plain no-op when there is not even a stale file", async () => {
    const h = harness();
    await h.daemon.stop();
    expect(h.lines.join("")).toContain("no clawmessenger supervisor is running.");
    expect(h.lines.join("")).not.toContain("stale");
  });

  it("kills the whole process tree on Windows", async () => {
    const killed: Array<{ file: string; args: string[] }> = [];
    const h = harness({
      platform: "win32",
      spawnSync: ((file: string, args: string[]) => {
        killed.push({ file, args });
        return { status: 0, stdout: "", stderr: "" };
      }) as unknown as typeof import("node:child_process").spawnSync,
    });
    state(h.paths, 4242);
    h.alive.add(4242);

    await h.daemon.stop();

    expect(killed).toEqual([{ file: "taskkill", args: ["/PID", "4242", "/T", "/F"] }]);
    expect(h.signals).toHaveLength(0);
    expect(existsSync(h.paths.pidFile)).toBe(false);
  });
});

describe("daemon restart", () => {
  it("stops then starts", async () => {
    const h = harness({}, { spawnPid: 888 });
    state(h.paths, 4242);
    h.alive.add(4242);
    h.alive.add(888);
    h.publishOnSpawn = (pid) => {
      writeSupervisorState({ pid, startedAt: 1_000_000, execPath: EXE, cwd: h.home, agents: ["claude"] }, h.paths);
    };

    await h.daemon.restart();

    expect(h.signals).toEqual([{ pid: 4242, signal: "SIGTERM" }]);
    expect(h.spawns).toHaveLength(1);
    expect(readFileSync(h.paths.pidFile, "utf8").trim()).toBe("888");
  });
});

describe("daemon status", () => {
  it("reports not-running when there is nothing on disk", () => {
    const h = harness();
    const report = h.daemon.status();
    expect(report.running).toBe(false);
    expect(report.stale).toBe(false);
    const text = formatDaemonStatus(report, h.paths);
    expect(text).toContain("not running in the background");
    expect(text).toContain("clawmessenger start");
  });

  it("flags a leftover pid file as stale", () => {
    const h = harness();
    state(h.paths, 4242);
    const report = h.daemon.status();
    expect(report.running).toBe(false);
    expect(report.stale).toBe(true);
    expect(formatDaemonStatus(report, h.paths)).toContain("stale pid/state file");
  });

  it("reports the supervisor, its uptime and its agents", () => {
    const h = harness();
    state(h.paths, 4242, ["claude", "opencode"]);
    h.alive.add(4242);

    const report = h.daemon.status();

    expect(report.running).toBe(true);
    expect(report.pid).toBe(4242);
    expect(report.uptimeMs).toBe(1_000); // now() - startedAt
    expect(report.agents).toEqual(["claude", "opencode"]);
    expect(report.cwd).toBe("/home/u");
    const text = formatDaemonStatus(report, h.paths);
    expect(text).toContain("running in the background (PID 4242, up 1s)");
    expect(text).toContain("2 (claude, opencode)");
  });

  it("still reports running when the supervisor has not published its state yet", () => {
    const h = harness();
    mkdirSync(h.paths.stateDir, { recursive: true });
    writeFileSync(h.paths.pidFile, "4242\n");
    h.alive.add(4242);
    const report = h.daemon.status();
    expect(report.running).toBe(true);
    expect(report.agents).toEqual([]);
    expect(formatDaemonStatus(report, h.paths)).toContain("supervisor has not reported yet");
  });

  it("surfaces the configured autostart entry", () => {
    const h = harness();
    mkdirSync(join(h.home, ".config", "systemd", "user"), { recursive: true });
    writeFileSync(join(h.home, ".config", "systemd", "user", "clawmessenger.service"), "[Unit]\n");
    expect(h.daemon.status().autostart).toContain("systemd user service");
  });
});

describe("daemon logs", () => {
  it("prints the tail of the newest log file", async () => {
    const h = harness();
    mkdirSync(h.paths.logDir, { recursive: true });
    writeFileSync(join(h.paths.logDir, "supervisor.log"), "one\ntwo\nthree\nfour\n");
    writeFileSync(join(h.paths.logDir, "supervisor.log.1"), "old\n");

    h.daemon.logs({ lines: 2 });

    const text = h.lines.join("");
    expect(text).toContain("supervisor.log\n");
    expect(text).toContain("three\nfour\n");
    expect(text).not.toContain("old");
  });

  it("says so when no log exists yet", () => {
    const h = harness();
    h.daemon.logs();
    expect(h.lines.join("")).toContain("no supervisor log yet");
  });

  it("picks the newest file when the legacy script wrote timestamped names", () => {
    const h = harness();
    mkdirSync(h.paths.logDir, { recursive: true });
    writeFileSync(join(h.paths.logDir, "supervisor-20260101-000000.out.log"), "legacy\n");
    h.daemon.logs({ lines: 5 });
    expect(h.lines.join("")).toContain("legacy");
  });
});

describe("daemon install-service (Linux)", () => {
  it("writes a system-level unit for root and enables it", async () => {
    const h = harness({ uid: 0 });
    await h.daemon.installService({ model: "gpt-5" });

    const unitPath = join(h.paths.stateDir, "etc", "systemd", "system", "clawmessenger.service");
    expect(existsSync(unitPath)).toBe(true);
    const unit = readFileSync(unitPath, "utf8");
    expect(unit).toContain("WantedBy=multi-user.target");
    expect(unit).toContain(`ExecStart=${EXE} run --model gpt-5`);
    expect(unit).toContain(`WorkingDirectory=${h.home}`);
    expect(unit).toContain("Restart=always");
    expect(h.commands.some((c) => c.file === "systemctl" && c.args.join(" ") === "daemon-reload")).toBe(true);
    expect(h.commands.some((c) => c.file === "systemctl" && c.args.join(" ") === "enable clawmessenger")).toBe(true);
    expect(h.commands.some((c) => c.file === "systemctl" && c.args.join(" ") === "start clawmessenger")).toBe(true);
    expect(h.lines.join("")).toContain("systemctl {start|stop|restart|status} clawmessenger");
    expect(h.lines.join("")).toContain("journalctl -u clawmessenger -f");
  });

  it("writes a --user unit for a non-root user and tries to enable linger", async () => {
    const h = harness({ uid: 1001 });
    await h.daemon.installService();

    const unitPath = join(h.home, ".config", "systemd", "user", "clawmessenger.service");
    expect(existsSync(unitPath)).toBe(true);
    const unit = readFileSync(unitPath, "utf8");
    expect(unit).toContain("WantedBy=default.target");
    expect(unit).toContain(`ExecStart=${EXE} run`);
    expect(unit).toContain(`WorkingDirectory=${h.home}`);
    expect(unit).toContain("Environment=PATH=/usr/bin:/bin");
    // 回归护栏：systemd 的 WorkingDirectory= 不去引号，带引号的值会被判成「非绝对路径」，
    // 整个 unit 变成 bad unit file setting（0.2.15 线上就是这么炸的）。
    expect(unit).not.toContain('WorkingDirectory="');
    expect(unit).not.toContain('ExecStart="');
    expect(unit).not.toContain('Environment="');
    expect(h.commands.some((c) => c.file === "systemctl" && c.args[0] === "--user")).toBe(true);
    expect(h.commands.some((c) => c.file === "loginctl" && c.args[0] === "enable-linger")).toBe(true);
  });

  it("quotes an env value only when it really contains whitespace", async () => {
    const h = harness({ uid: 0, home: "/opt/my home", env: { PATH: "/opt/my agents/bin:/usr/bin" } });
    // 显式给 workdir：否则默认值会取带空格的 home，而它并不存在。
    await h.daemon.installService({ workdir: h.home });

    const unitPath = join(h.paths.stateDir, "etc", "systemd", "system", "clawmessenger.service");
    const unit = readFileSync(unitPath, "utf8");
    // 有空白必须加引号，否则 systemd 会按空白拆成多个赋值，PATH 直接丢。
    expect(unit).toContain('Environment="HOME=/opt/my home"');
    expect(unit).toContain('Environment="PATH=/opt/my agents/bin:/usr/bin"');
    // WorkingDirectory= 永远不加引号（systemd 不去引号，加了就是致命错误）。
    expect(unit).not.toContain('WorkingDirectory="');
  });

  it("aborts before touching the running supervisor when systemd-analyze rejects the unit", async () => {
    const calls: Array<{ file: string; args: string[] }> = [];
    const h = harness(
      {
        uid: 0,
        runCommand: (file, args) => {
          calls.push({ file, args });
          if (file === "systemd-analyze") {
            return {
              status: 1,
              stdout: "",
              stderr: 'clawmessenger.service: Working directory is not absolute, ignoring: "/root"',
            };
          }
          return { status: 0, stdout: "", stderr: "" };
        },
      },
      { spawnPid: 808 },
    );
    state(h.paths, 4242, ["claude", "codex"]);
    h.alive.add(4242);

    await h.daemon.installService();

    // 手工 supervisor 一根汗毛都没动：没有信号、没有新进程，pid 文件照旧。
    expect(h.signals).toEqual([]);
    expect(h.spawns).toHaveLength(0);
    expect(readFileSync(h.paths.pidFile, "utf8").trim()).toBe("4242");
    // 也没 enable / start —— 否则每次开机都会再撞一次同样的失败。
    expect(calls.some((c) => c.file === "systemctl" && c.args.join(" ") === "enable clawmessenger")).toBe(false);
    expect(calls.some((c) => c.file === "systemctl" && c.args.join(" ") === "start clawmessenger")).toBe(false);
    // systemd 的原话 + unit 正文都要带出来。
    const errs = h.errors.join("");
    expect(errs).toContain("Working directory is not absolute");
    expect(errs).toContain("unit file ");
    expect(errs).toContain("the device stays online");
  });

  it("does not treat a missing systemd-analyze as a broken unit", async () => {
    const calls: Array<{ file: string; args: string[] }> = [];
    const h = harness(
      {
        uid: 0,
        runCommand: (file, args) => {
          calls.push({ file, args });
          // spawnSync 跑不起来时 status 是 null（宿主机没装 systemd-analyze）。
          if (file === "systemd-analyze") return { status: null, stdout: "", stderr: "" };
          return { status: 0, stdout: "", stderr: "" };
        },
      },
      { spawnPid: 808 },
    );

    await h.daemon.installService();

    // 真的走下去了：enable + start 都发了，也没有把「校验不通过」当成结论。
    expect(calls.some((c) => c.file === "systemctl" && c.args.join(" ") === "start clawmessenger")).toBe(true);
    expect(h.errors.join("")).not.toContain("rejected the unit");
  });

  it("keeps the device online when systemd refuses the unit", async () => {
    const h = harness(
      {
        uid: 0,
        runCommand: (file, args) => {
          if (file === "systemctl" && args.join(" ") === "start clawmessenger") {
            return {
              status: 1,
              stdout: "",
              stderr: "Failed to start clawmessenger.service: Unit clawmessenger.service has a bad unit file setting.",
            };
          }
          if (file === "journalctl") {
            return {
              status: 0,
              stdout: 'clawmessenger.service: Invalid syntax in ExecStart=, ignoring: "/opt/x" run',
              stderr: "",
            };
          }
          return { status: 0, stdout: "", stderr: "" };
        },
      },
      { spawnPid: 808 },
    );
    state(h.paths, 4242, ["claude", "codex"]);
    h.alive.add(4242);
    h.alive.add(808);

    await h.daemon.installService();

    // 手工起的 supervisor 确实被收掉了……
    expect(h.signals).toEqual([{ pid: 4242, signal: "SIGTERM" }]);
    // ……但服务起不来也不能让设备就这么下线：自动把后台 supervisor 拉回来。
    expect(h.spawns).toHaveLength(1);
    expect(h.spawns[0].argv).toEqual(["run"]);
    expect(readFileSync(h.paths.pidFile, "utf8").trim()).toBe("808");
    expect(h.lines.join("")).toContain("restoring the background supervisor");
    expect(h.lines.join("")).toContain("supervisor PID 808");
    // systemd 的原话、unit 正文都要带出来，省得用户自己去翻 journal。
    const errs = h.errors.join("");
    expect(errs).toContain("Unit clawmessenger.service has a bad unit file setting");
    expect(errs).toContain("Invalid syntax in ExecStart=");
    expect(errs).toContain("unit file ");
    expect(errs).toContain("WantedBy=multi-user.target");
    // 失败后不再打「怎么管理服务」那一套，避免误导。
    expect(h.lines.join("")).not.toContain("the PATH baked into the unit");
  });
});

describe("daemon install-service (macOS / Windows)", () => {
  it("writes a LaunchAgent plist on macOS", async () => {
    const h = harness({ platform: "darwin" });
    await h.daemon.installService();

    const plistPath = join(h.home, "Library", "LaunchAgents", "com.clawmessenger.supervisor.plist");
    expect(existsSync(plistPath)).toBe(true);
    const plist = readFileSync(plistPath, "utf8");
    expect(plist).toContain("<key>RunAtLoad</key>");
    expect(plist).toContain(`<string>${EXE}</string>`);
    expect(plist).toContain("<string>run</string>");
    expect(h.commands.some((c) => c.file === "launchctl" && c.args[0] === "load")).toBe(true);
  });

  it("restores the supervisor when launchctl refuses to load the plist", async () => {
    const h = harness(
      {
        platform: "darwin",
        runCommand: (file, args) => {
          if (file === "launchctl" && args[0] === "load") {
            return { status: 1, stdout: "", stderr: "Load failed: 5: Input/output error" };
          }
          if (file === "launchctl" && args[0] === "list") {
            return { status: 0, stdout: "PID\tStatus\tLabel\n-\t0\tcom.clawmessenger.supervisor\n", stderr: "" };
          }
          return { status: 0, stdout: "", stderr: "" };
        },
      },
      { spawnPid: 909 },
    );
    state(h.paths, 4242);
    h.alive.add(4242);
    h.alive.add(909);

    await h.daemon.installService();

    expect(h.spawns).toHaveLength(1);
    expect(readFileSync(h.paths.pidFile, "utf8").trim()).toBe("909");
    expect(h.lines.join("")).toContain("restoring the background supervisor");
    expect(h.errors.join("")).toContain("Load failed: 5: Input/output error");
    expect(h.errors.join("")).toContain("com.clawmessenger.supervisor");
  });

  it("writes a hidden logon launcher into the Startup folder on Windows", async () => {
    const appData = join(tmpdir(), `clawmessenger-appdata-${Date.now()}`);
    dirs.push(appData);
    const h = harness({ platform: "win32", env: { APPDATA: appData, PATH: "C:\\bin" } });
    await h.daemon.installService();

    const vbs = join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "clawmessenger.vbs");
    expect(existsSync(vbs)).toBe(true);
    const body = readFileSync(vbs, "utf8");
    expect(body).toContain(`sh.Run ""${EXE}"" run, 0, False`);
    expect(h.lines.join("")).toContain("installed logon autostart");
  });
});

describe("daemon uninstall", () => {
  it("removes the autostart entry but leaves the pid file alone", async () => {
    const h = harness({ uid: 1001 });
    mkdirSync(join(h.home, ".config", "systemd", "user"), { recursive: true });
    const unitPath = join(h.home, ".config", "systemd", "user", "clawmessenger.service");
    writeFileSync(unitPath, "[Unit]\n");
    state(h.paths, 4242);
    h.alive.add(4242);

    await h.daemon.uninstall();

    expect(existsSync(unitPath)).toBe(false);
    expect(existsSync(h.paths.pidFile)).toBe(true);
    expect(h.lines.join("")).toContain("removed systemd user service");
    expect(h.lines.join("")).toContain("clawmessenger stop");
  });

  it("says nothing was configured when there is no autostart entry", async () => {
    const h = harness();
    await h.daemon.uninstall();
    expect(h.lines.join("")).toContain("no autostart entry was configured");
  });
});
