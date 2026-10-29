// @vitest-environment node
import { EventEmitter } from "node:events";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import type { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  buildProgram,
  supervisorRestartDelayMs,
  RELAUNCH_LOCK_WAIT_ENV,
  RELAUNCH_LOCK_WAIT_MS,
  RELAUNCH_READY_ENV,
  SUPERVISOR_CONTROL_POLL_MS,
  SUPERVISOR_RESTART_BACKOFF_MS,
} from "./main.js";
import { Keystore } from "./keystore.js";
import {
  XiachatApi,
  type AgentControlState,
  type DeviceControlSnapshot,
  type DeviceUpgradeIntent,
} from "./api.js";
import { defaultDaemonPaths } from "./daemon.js";

interface FakeChild extends EventEmitter {
  stdout: null;
  stderr: null;
  kill: (signal?: string) => boolean;
  kills: string[];
}

function fakeChild(): FakeChild {
  const child = new EventEmitter() as FakeChild;
  child.stdout = null;
  child.stderr = null;
  child.kills = [];
  child.kill = (signal?: string) => {
    child.kills.push(String(signal));
    return true;
  };
  return child;
}

interface Harness {
  /** Temp state dir (keystore, daemon paths, upgrade bookkeeping). */
  dir: string;
  spawned: Array<{ args: string[]; env: NodeJS.ProcessEnv; child: FakeChild }>;
  /** Everything the supervisor wrote to its stdout stream. */
  output: string[];
  api: XiachatApi;
  getShutdown: () => () => void;
  run: Promise<unknown>;
  /** Always resolves, so a failing assertion can never leak a live supervisor. */
  stop: () => Promise<void>;
}

const BOUND_OPS = [{
  agent: "ops",
  nodeId: "node_ops",
  token: "tok",
  credentialId: "dc",
  deviceSecret: "sec",
}];

/**
 * The supervisor registers its start/stop poll interval during startup, which
 * happens *before* these tests install fake timers — so advancing the fake clock
 * would never fire it. Tests therefore run the poll on a short real interval and
 * wait on the real clock. `realSetTimeout` is captured up front for that: once
 * fake timers are installed, the global setTimeout is no longer the real one.
 */
const CONTROL_TICK_MS = 20;
const realSetTimeout = globalThis.setTimeout;
const realSleep = (ms: number): Promise<void> =>
  new Promise((resolve) => { realSetTimeout(resolve, ms); });

function makeHarness(
  initialControl: AgentControlState[] | DeviceControlSnapshot = [],
  bound: unknown[] = BOUND_OPS,
  overrides: Record<string, unknown> = {},
): Harness {
  const dir = mkdtempSync(join(tmpdir(), "clawmessenger-supervisor-"));
  dirs.push(dir);
  const keystore = new Keystore(join(dir, "creds.json"));
  keystore.save({ nodeId: "node_machine", token: "t", credentialId: "dc", deviceSecret: "sec", serverUrl: "http://unused.local" });

  const spawned: Harness["spawned"] = [];
  const output: string[] = [];
  let shutdown: (() => void) | undefined;
  let onFirstSpawn: () => void = () => {};
  const firstSpawn = new Promise<void>((resolve) => { onFirstSpawn = resolve });

  const api = new XiachatApi("http://unused.local");
  vi.spyOn(api, "bindDeviceAgents").mockImplementation(async () => ({}) as never);
  vi.spyOn(api, "fetchDeviceNodes").mockImplementation(async () => bound as never);
  // The control poll runs on a timer, so leaving it unmocked would fire real
  // requests at http://unused.local the moment the clock advances. `upgrade` 为
  // 空的快照 = 服务端没有下发升级指令（本组用例只关心启停）。
  const initialSnapshot: DeviceControlSnapshot = Array.isArray(initialControl)
    ? { agents: initialControl }
    : initialControl;
  vi.spyOn(api, "fetchDeviceControl").mockImplementation(async () => initialSnapshot);

  const program = buildProgram({
    keystore,
    apiFactory: () => api,
    stdout: { write: (chunk: unknown) => { output.push(String(chunk)); return true } } as unknown as NodeJS.WriteStream,
    ensureOpencode: async () => ({}),
    // supervisor 会跳过「本机 PATH 上没有」的 agent。CI runner 上一个 agent CLI
    // 都没装，bound 里的会全被跳过，run 直接抛 "none of the bound agent CLIs"。
    // 这里按 bound 伪造一份 PATH 扫描结果，用例只关心启停逻辑，不该依赖宿主机。
    discoverAgents: async () => [...new Set((bound as Array<{ agent?: string }>)
      .map((entry) => entry?.agent)
      .filter((name): name is string => typeof name === "string"))]
      .map((name) => ({ name, path: join(dir, "bin", name) })),
    spawnChild: ((_command: string, args: string[], options: { env: NodeJS.ProcessEnv }) => {
      const child = fakeChild();
      spawned.push({ args, env: options.env, child });
      onFirstSpawn();
      return child;
    }) as unknown as typeof spawn,
    onSupervisorSignal: (fn) => { shutdown = fn },
    supervisorControlPollMs: CONTROL_TICK_MS,
    // 这些用例跑的是真正的 `run`，state/pid 文件必须落在临时目录里：
    //   · 用默认的 ~/.clawmessenger，`run` 会读到**这台机器上真实在跑的 supervisor**
    //     → 被新加的防重复护栏拒绝启动 → 所有用例超时；
    //   · 更要命的是 onStopped 会 clearSupervisorState(真实路径)，把线上的
    //     supervisor.pid 删掉 —— 下次 `run` / 开机自启就以为没人跑而多起一份。
    daemonPaths: defaultDaemonPaths(join(dir, ".clawmessenger")),
    // 单例端口锁同理：测试里必须用假锁，否则会在真实 127.0.0.1:41307 上
    // 和其他并发测试进程（或本机活着的 supervisor）抢同一个端口。
    acquireLock: () => Promise.resolve({ release: () => {} }),
    // 升级相关的默认值全部指向临时目录：真正的安装路径是
    // %LOCALAPPDATA%\clawmessenger\clawmessenger.exe，测试一旦真的交换就会
    // 把开发机上装着的 CLI 换掉。
    version: "1.2.0",
    platform: "win32",
    arch: "x64",
    upgradeTarget: join(dir, "install", "clawmessenger.exe"),
    upgradeInflightPath: join(dir, "upgrade-inflight.json"),
    // 默认下载失败：不想让「忘了 mock」的用例悄悄跑通一次升级。
    upgradeFetch: (async () => ({
      ok: false,
      status: 503,
      json: async () => ({}),
      arrayBuffer: async () => new ArrayBuffer(0),
    })) as never,
    // 默认不真的 spawn `__relaunch`：那会多出一个子进程并影响子进程计数断言。
    relaunch: () => {},
    // 退出硬上限默认会调 process.exit（生产行为），测试进程不能被它结束 ——
    // 这里默认关掉，专门的用例注入 exitProcess 之后再打开。
    shutdownHardExit: false,
    ...overrides,
  });

  const run = program.parseAsync(["node", "clawmessenger", "run"]);
  const harness: Harness = {
    dir,
    spawned,
    output,
    api,
    run,
    getShutdown: () => {
      if (!shutdown) throw new Error("supervisor never registered a shutdown hook");
      return shutdown;
    },
    stop: async () => {
      shutdown?.();
      await run.catch(() => undefined);
    },
  };
  live.push(harness);
  return harness;
}

/**
 * Real timers until the first child is up: startup scans PATH with real fs I/O
 * (discoverAgents), which never settles under vitest's fake clock. Restart
 * timing — the thing under test — then runs on fake timers.
 */
async function startSupervisor(): Promise<Harness> {
  const harness = makeHarness();
  // startWorker() resolves this the moment the first child is spawned.
  await vi.waitFor(() => expect(harness.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });
  vi.useFakeTimers();
  return harness;
}

const dirs: string[] = [];
const live: Harness[] = [];
const errorLog: string[] = [];
let errorSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  delete process.env.CLAWMESSENGER_AGENT_CREDS;
  errorLog.length = 0;
  errorSpy = vi.spyOn(console, "error").mockImplementation((...args: unknown[]) => {
    errorLog.push(args.map(String).join(" "));
  });
});

afterEach(async () => {
  vi.useRealTimers();
  for (const harness of live.splice(0)) await harness.stop();
  errorSpy.mockRestore();
  for (const dir of dirs) rmSync(dir, { recursive: true, force: true });
  dirs.length = 0;
});

describe("supervisorRestartDelayMs", () => {
  it("ramps 1s -> 2s -> 4s -> 8s -> 16s", () => {
    expect([1, 2, 3, 4, 5].map(supervisorRestartDelayMs)).toEqual([1_000, 2_000, 4_000, 8_000, 16_000]);
  });

  it("switches to the slow lane after five consecutive failures", () => {
    expect(supervisorRestartDelayMs(6)).toBe(SUPERVISOR_RESTART_BACKOFF_MS);
    expect(supervisorRestartDelayMs(50)).toBe(SUPERVISOR_RESTART_BACKOFF_MS);
  });

  it("never exceeds the 60s cap inside the fast lane", () => {
    for (let n = 1; n <= 5; n += 1) expect(supervisorRestartDelayMs(n)).toBeLessThanOrEqual(60_000);
  });
});

describe("runSupervisor restart backoff", () => {
  it("spawns one child per bound agent with its own credentials", async () => {
    const h = await startSupervisor();
    expect(h.spawned).toHaveLength(1);
    expect(h.spawned[0].args.slice(-3)).toEqual(["run", "--agent", "ops"]);
    expect(JSON.parse(String(h.spawned[0].env.CLAWMESSENGER_AGENT_CREDS))).toMatchObject({
      nodeId: "node_ops",
      token: "tok",
      serverUrl: "http://unused.local",
    });
  });

  // 运维会话按智能体隔离：服务端为每个 agent 下发专属 ops 节点（opsFor 非空），
  // supervisor 必须为它们各起一个子进程 —— 用同一个 Map 键会互相覆盖。
  it("spawns a dedicated ops child per agent when per-agent ops nodes come back", async () => {
    const harness = makeHarness([], [
      { agent: "ops", nodeId: "node_ops", rongCloudUser: "ops_key1", token: "tok", credentialId: "dc", deviceSecret: "sec" },
      { agent: "ops", nodeId: "node_ops_hermes", rongCloudUser: "hermes_key1_ops", token: "tok_h", credentialId: "dc_h", deviceSecret: "sec_h", opsFor: "hermes_key1" },
      { agent: "ops", nodeId: "node_ops_opencode", rongCloudUser: "opencode_key1_ops", token: "tok_o", credentialId: "dc_o", deviceSecret: "sec_o", opsFor: "opencode_key1" },
    ]);
    await vi.waitFor(() => expect(harness.spawned.length).toBe(3), { timeout: 5_000 });

    // 机器级 ops：不带 --ops-suffix（共享 workdir，旧行为）。
    expect(harness.spawned[0].args.slice(-3)).toEqual(["run", "--agent", "ops"]);
    // per-agent ops：各带自己的 workdir 后缀与子进程凭据。
    expect(harness.spawned[1].args.slice(-5)).toEqual(["run", "--agent", "ops", "--ops-suffix", "hermes_key1"]);
    expect(harness.spawned[2].args.slice(-5)).toEqual(["run", "--agent", "ops", "--ops-suffix", "opencode_key1"]);
    expect(JSON.parse(String(harness.spawned[1].env.CLAWMESSENGER_AGENT_CREDS))).toMatchObject({ nodeId: "node_ops_hermes", token: "tok_h" });
    expect(JSON.parse(String(harness.spawned[2].env.CLAWMESSENGER_AGENT_CREDS))).toMatchObject({ nodeId: "node_ops_opencode", token: "tok_o" });
  });

  // 操作员在控制台停掉 ops，per-agent 运维子进程必须一起停（共享 "ops" 控制键）。
  it("stops per-agent ops children when the operator stops ops", async () => {
    const harness = makeHarness(
      [{ agent: "ops", nodeId: "node_ops", desiredState: "stopped", version: 1 }],
      [
        { agent: "ops", nodeId: "node_ops", rongCloudUser: "ops_key1", token: "tok", credentialId: "dc", deviceSecret: "sec" },
        { agent: "ops", nodeId: "node_ops_hermes", rongCloudUser: "hermes_key1_ops", token: "tok_h", credentialId: "dc_h", deviceSecret: "sec_h", opsFor: "hermes_key1" },
      ],
    );
    // 两个 ops 子进程都被期望状态按住：一个都不该拉起。等 supervisor 横幅
    // 出现再断言（启动要扫 PATH，固定 sleep 不可靠）。
    await vi.waitFor(() => expect(harness.output.join("")).toContain("supervisor:"), { timeout: 5_000 });
    expect(harness.spawned).toHaveLength(0);
    expect(harness.output.join("")).toContain("0 of 2 agent(s) online");
  });

  it("restarts a dead agent on an exponential backoff", async () => {
    const h = await startSupervisor();

    h.spawned[0].child.emit("exit", 1);
    expect(errorLog.some((m) => m.includes("restarting in 1000ms (attempt 1)"))).toBe(true);

    // Nothing happens until the full 1s has elapsed.
    await vi.advanceTimersByTimeAsync(999);
    expect(h.spawned).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(h.spawned).toHaveLength(2);

    // The second consecutive failure doubles the delay.
    h.spawned[1].child.emit("exit", 1);
    expect(errorLog.some((m) => m.includes("restarting in 2000ms (attempt 2)"))).toBe(true);
    await vi.advanceTimersByTimeAsync(1_999);
    expect(h.spawned).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(h.spawned).toHaveLength(3);
  });

  it("moves to the slow lane once the crashes keep coming", async () => {
    const h = await startSupervisor();

    // Five rapid failures: 1s, 2s, 4s, 8s, 16s.
    for (const delayMs of [1_000, 2_000, 4_000, 8_000, 16_000]) {
      h.spawned[h.spawned.length - 1].child.emit("exit", 1);
      await vi.advanceTimersByTimeAsync(delayMs);
    }
    expect(h.spawned).toHaveLength(6);

    // The sixth failure enters the five-minute lane instead of 32s.
    h.spawned[5].child.emit("exit", 1);
    expect(errorLog.some((m) => m.includes(`restarting in ${SUPERVISOR_RESTART_BACKOFF_MS}ms`))).toBe(true);
    await vi.advanceTimersByTimeAsync(SUPERVISOR_RESTART_BACKOFF_MS - 1);
    expect(h.spawned).toHaveLength(6);
    await vi.advanceTimersByTimeAsync(1);
    expect(h.spawned).toHaveLength(7);
  });

  it("treats a long-lived run as healthy and restarts from 1s again", async () => {
    const h = await startSupervisor();

    // Crash twice in quick succession to get the delay up to 2s.
    h.spawned[0].child.emit("exit", 1);
    await vi.advanceTimersByTimeAsync(1_000);
    h.spawned[1].child.emit("exit", 1);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(h.spawned).toHaveLength(3);

    // ...then let the third child survive past the healthy-uptime threshold.
    await vi.advanceTimersByTimeAsync(61_000);
    h.spawned[2].child.emit("exit", 1);
    expect(errorLog.some((m) => m.includes("restarting in 1000ms (attempt 1)"))).toBe(true);
  });

  it("stops restarting once shutdown is requested, and lets the process exit", async () => {
    const h = await startSupervisor();

    const child = h.spawned[0].child;
    let resolved = false;
    void h.run.then(() => { resolved = true });

    h.getShutdown()();
    expect(child.kills).toEqual(["SIGTERM"]);
    // A SIGTERM'd child's own exit event must not schedule a resurrection.
    child.emit("exit", 0);
    await vi.advanceTimersByTimeAsync(120_000);
    expect(h.spawned).toHaveLength(1);

    await h.run;
    expect(resolved).toBe(true);
  });

  // 2026-10-29 现场：agent 子进程不理会 SIGTERM，`shutdown()` 清完状态文件就不再干活
  // （所以 status 说没在跑、服务端判离线），进程本身却一直活着 —— __relaunch 等满
  // 60 秒判 pid_timeout 并放弃拉起，设备永久离线。所以退出必须自己兜底：
  // 宽限到点 SIGKILL 子进程，硬上限到点直接 exit。
  it("escalates to SIGKILL and hard-exits when an agent child ignores SIGTERM", async () => {
    const exits: number[] = [];
    const h = makeHarness([], BOUND_OPS, {
      shutdownHardExit: true,
      shutdownAgentGraceMs: 5,
      shutdownHardExitMs: 40,
      exitProcess: (code: number) => { exits.push(code) },
    });
    await vi.waitFor(() => expect(h.spawned.length).toBe(1), { timeout: 5_000 });

    h.getShutdown()();
    expect(h.spawned[0].child.kills).toEqual(["SIGTERM"]);

    await vi.waitFor(
      () => expect(h.spawned[0].child.kills).toEqual(["SIGTERM", "SIGKILL"]),
      { timeout: 2_000 },
    );
    // 子进程仍然「活着」（fakeChild 从不发 exit），硬上限必须真的把我们带走。
    await vi.waitFor(() => expect(exits).toEqual([0]), { timeout: 2_000 });
  });

  it("cancels a pending restart timer at shutdown", async () => {
    const h = await startSupervisor();

    h.spawned[0].child.emit("exit", 1); // schedules a 1s restart
    h.getShutdown()();
    await vi.advanceTimersByTimeAsync(120_000);
    expect(h.spawned).toHaveLength(1);
  });
});

describe("operator-driven start/stop", () => {
  const stopped = (version: number): DeviceControlSnapshot =>
    ({ agents: [{ agent: "ops", nodeId: "node_ops", desiredState: "stopped", version }] });
  const running = (version: number): DeviceControlSnapshot =>
    ({ agents: [{ agent: "ops", nodeId: "node_ops", desiredState: "running", version }] });

  it("defaults to a 10s poll cadence", () => {
    expect(SUPERVISOR_CONTROL_POLL_MS).toBe(10_000);
  });

  it("kills the agent when the server reports desired=stopped, and keeps it down", async () => {
    const h = await startSupervisor();
    vi.mocked(h.api.fetchDeviceControl).mockResolvedValue(stopped(1));

    await realSleep(CONTROL_TICK_MS * 3);
    expect(h.spawned[0].child.kills).toEqual(["SIGTERM"]);

    // The stopped child's own exit must not be mistaken for a crash.
    h.spawned[0].child.emit("exit", 0);
    await vi.advanceTimersByTimeAsync(120_000);
    expect(h.spawned).toHaveLength(1);
  });

  it("brings the agent back when the operator starts it again", async () => {
    const h = await startSupervisor();
    const control = vi.mocked(h.api.fetchDeviceControl);

    control.mockResolvedValue(stopped(1));
    await realSleep(CONTROL_TICK_MS * 3);
    h.spawned[0].child.emit("exit", 0);
    expect(h.spawned).toHaveLength(1);

    control.mockResolvedValue(running(2));
    await realSleep(CONTROL_TICK_MS * 3);
    expect(h.spawned).toHaveLength(2);
    expect(h.spawned[1].args.slice(-3)).toEqual(["run", "--agent", "ops"]);
  });

  it("leaves an already-stopped agent down on boot", async () => {
    const h = makeHarness(stopped(3));
    // No child is spawned, so wait for the supervisor's banner instead.
    await vi.waitFor(() => expect(h.output.join("")).toContain("supervisor:"), { timeout: 5_000 });
    expect(h.spawned).toHaveLength(0);
    expect(h.output.join("")).toContain("0 of 1 agent(s) online");
  });

  it("does not stop anything when the control poll fails", async () => {
    const h = await startSupervisor();
    vi.mocked(h.api.fetchDeviceControl).mockRejectedValue(new Error("network down"));

    await realSleep(CONTROL_TICK_MS * 5);
    expect(h.spawned[0].child.kills).toEqual([]);
    expect(h.spawned).toHaveLength(1);
  });
});

// 按账号重启：服务端 action=restart 时状态仍是 running、只把 version +1。
// supervisor 必须只杀那一个子进程 —— 这是运维助手「只操作本账号」的执行端。
describe("per-agent restart via control version", () => {
  const bound = [
    { agent: "opencode", nodeId: "node_open", token: "tok_o", credentialId: "dc_o", deviceSecret: "sec_o" },
    { agent: "hermes", nodeId: "node_h", token: "tok_h", credentialId: "dc_h", deviceSecret: "sec_h" },
  ];

  it("restarts only the agent whose control version moved", async () => {
    const h = makeHarness([], bound);
    await vi.waitFor(() => expect(h.spawned).toHaveLength(2), { timeout: 5_000 });
    vi.useFakeTimers();
    const control = vi.mocked(h.api.fetchDeviceControl);

    // 版本没动：谁都不该被杀。
    control.mockResolvedValue({ agents: [
      { agent: "opencode", nodeId: "node_open", desiredState: "running", version: 0 },
      { agent: "hermes", nodeId: "node_h", desiredState: "running", version: 0 },
    ] });
    await realSleep(CONTROL_TICK_MS * 3);
    expect(h.spawned[0].child.kills).toEqual([]);
    expect(h.spawned[1].child.kills).toEqual([]);

    // 运维助手执行 `agent restart opencode`：running 不变，版本 0 -> 1。
    control.mockResolvedValue({ agents: [
      { agent: "opencode", nodeId: "node_open", desiredState: "running", version: 1 },
      { agent: "hermes", nodeId: "node_h", desiredState: "running", version: 0 },
    ] });
    await realSleep(CONTROL_TICK_MS * 3);
    expect(h.spawned[0].child.kills).toEqual(["SIGTERM"]);
    expect(h.spawned[1].child.kills).toEqual([]);

    // 退出后立刻用同一份凭据重拉，另一个 agent 始终只有一个子进程。
    h.spawned[0].child.emit("exit", 0);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(h.spawned).toHaveLength(3);
    expect(JSON.parse(String(h.spawned[2].env.CLAWMESSENGER_AGENT_CREDS))).toMatchObject({ nodeId: "node_open" });
    expect(errorLog.some((m) => m.includes("restarting in"))).toBe(false);
  });
});

// 设备 exe 升级：领取（同一次轮询）→ 判空闲 → 下载校验（mirrors 回退）→
// 退出前交换 → __relaunch。这里覆盖的是设备侧最容易搞错的三件事：不该动的时候
// 不动（等待空闲）、校验不过就放弃且保留原文件、不认识的 control 动作静默忽略。
describe("device exe upgrade", () => {
  const NEW_BINARY = Buffer.from("BRAND-NEW-EXE");
  const NEW_SHA = createHash("sha256").update(NEW_BINARY).digest("hex");

  const intent = (over: Partial<DeviceUpgradeIntent> = {}): DeviceUpgradeIntent => ({
    generation: 7,
    targetVersion: "1.3.0",
    assetName: "clawmessenger-windows-x64.exe",
    sha256: NEW_SHA,
    force: false,
    mirrors: [
      "https://github.com/clawmessenger/claw-messenger/releases/download/clawmessenger-v1.3.0/clawmessenger-windows-x64.exe",
      "/api/agent/upgrade/download?version=1.3.0&asset=clawmessenger-windows-x64.exe&token=t",
    ],
    ...over,
  });

  /** 等一个真实时钟条件（fake timers 下 fetch/文件 IO 的 promise 仍按真实微任务推进）。 */
  async function waitUntil(check: () => boolean, timeoutMs = 3_000): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    while (!check()) {
      if (Date.now() > deadline) throw new Error("condition not met in time");
      await realSleep(10);
    }
  }

  function upgradeHarness(overrides: Record<string, unknown> = {}) {
    const relaunched: Array<{ target: string; pid: number }> = [];
    const h = makeHarness([], BOUND_OPS, {
      relaunch: (target: string, pid: number) => { relaunched.push({ target, pid }); },
      ...overrides,
    });
    return { h, relaunched };
  }

  it("reports waiting_idle and leaves the device alone while a turn is running", async () => {
    const { h, relaunched } = upgradeHarness({ upgradeIdleMs: 600_000 });
    await vi.waitFor(() => expect(h.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });
    vi.useFakeTimers();
    const report = vi.spyOn(h.api, "reportDeviceUpgrade").mockResolvedValue(undefined);
    vi.mocked(h.api.fetchDeviceControl).mockResolvedValue({
      agents: [{ agent: "ops", nodeId: "node_ops", desiredState: "running", version: 0 }],
      upgrade: intent(),
    });

    await realSleep(CONTROL_TICK_MS * 4);

    const waiting = report.mock.calls.filter((call) => call[3].status === "waiting_idle");
    expect(waiting).toHaveLength(1);
    // 只报一次「等待空闲」，不要每 10 秒刷一条。
    await realSleep(CONTROL_TICK_MS * 4);
    expect(report.mock.calls.filter((call) => call[3].status === "waiting_idle")).toHaveLength(1);
    expect(relaunched).toHaveLength(0);
    expect(h.spawned[0].child.kills).toEqual([]);
  });

  it("downloads, verifies, swaps in place and asks for a relaunch when forced", async () => {
    const { h, relaunched } = upgradeHarness({
      upgradeIdleMs: 600_000,
      upgradeFetch: (async () => ({
        ok: true,
        status: 200,
        json: async () => ({}),
        arrayBuffer: async () => NEW_BINARY,
      })) as never,
    });
    await vi.waitFor(() => expect(h.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });
    vi.useFakeTimers();
    const report = vi.spyOn(h.api, "reportDeviceUpgrade").mockResolvedValue(undefined);
    const target = join(h.dir, "install", "clawmessenger.exe");
    mkdirSync(join(h.dir, "install"), { recursive: true });
    writeFileSync(target, "OLD-BINARY");
    vi.mocked(h.api.fetchDeviceControl).mockResolvedValue({
      agents: [{ agent: "ops", nodeId: "node_ops", desiredState: "running", version: 0 }],
      upgrade: intent({ force: true }),
    });

    await waitUntil(() => relaunched.length === 1);

    // 磁盘上已经是新版，而本进程（旧镜像）正在退出。
    expect(readFileSync(target, "utf8")).toBe("BRAND-NEW-EXE");
    expect(relaunched[0]).toMatchObject({ target });
    const statuses = report.mock.calls.map((call) => call[3].status);
    expect(statuses).toContain("downloading");
    expect(statuses).toContain("restarting");
    expect(statuses).not.toContain("failed");
    // 退出走的是 shutdown 那条路径（子进程收到 SIGTERM），不是静默 process.exit。
    await waitUntil(() => h.spawned[0].child.kills.length > 0);
  });

  it("refuses bytes with a wrong checksum and keeps the old binary running", async () => {
    const { h, relaunched } = upgradeHarness({
      upgradeIdleMs: 0,
      upgradeFetch: (async () => ({
        ok: true,
        status: 200,
        json: async () => ({}),
        arrayBuffer: async () => Buffer.from("TAMPERED"),
      })) as never,
    });
    await vi.waitFor(() => expect(h.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });
    vi.useFakeTimers();
    const report = vi.spyOn(h.api, "reportDeviceUpgrade").mockResolvedValue(undefined);
    const target = join(h.dir, "install", "clawmessenger.exe");
    mkdirSync(join(h.dir, "install"), { recursive: true });
    writeFileSync(target, "OLD-BINARY");
    vi.mocked(h.api.fetchDeviceControl).mockResolvedValue({
      agents: [{ agent: "ops", nodeId: "node_ops", desiredState: "running", version: 0 }],
      upgrade: intent(),
    });

    await waitUntil(() => report.mock.calls.some((call) => call[3].status === "failed"));

    const failure = report.mock.calls.find((call) => call[3].status === "failed")!;
    expect(failure[3]).toMatchObject({ errorCode: "checksum_mismatch" });
    // 旧版本仍然完好（这是「升级失败不能把设备搞挂」的底线）。
    expect(readFileSync(target, "utf8")).toBe("OLD-BINARY");
    expect(relaunched).toHaveLength(0);
  });

  it("needs several consecutive quiet polls before it touches a device", async () => {
    // 单点采样会抖：一次短暂空窗就重启设备太草率。用一个大得不可能达到的
    // upgradeQuietPolls，断言「就算完全空闲也不会在第一轮就动手」——把这条门槛
    // 去掉（quietPolls >= 1）时本用例会立刻红。
    const { h, relaunched } = upgradeHarness({
      upgradeIdleMs: 0,
      upgradeQuietPolls: 99,
      upgradeFetch: (async () => ({
        ok: true,
        status: 200,
        json: async () => ({}),
        arrayBuffer: async () => NEW_BINARY,
      })) as never,
    });
    await vi.waitFor(() => expect(h.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });
    vi.useFakeTimers();
    const report = vi.spyOn(h.api, "reportDeviceUpgrade").mockResolvedValue(undefined);
    mkdirSync(join(h.dir, "install"), { recursive: true });
    writeFileSync(join(h.dir, "install", "clawmessenger.exe"), "OLD-BINARY");
    vi.mocked(h.api.fetchDeviceControl).mockResolvedValue({
      agents: [{ agent: "ops", nodeId: "node_ops", desiredState: "running", version: 0 }],
      upgrade: intent(),
    });

    await realSleep(CONTROL_TICK_MS * 10);

    const started = report.mock.calls.some(
      (call) => call[3].status === "downloading" || call[3].status === "restarting",
    );
    expect(started).toBe(false);
    expect(relaunched).toHaveLength(0);
    // 但它必须已经在报「等待空闲」，而不是一声不吭。
    expect(report.mock.calls.some((call) => call[3].status === "waiting_idle")).toBe(true);
  });

  it("ignores a control action it does not know instead of killing agents", async () => {
    const { h } = upgradeHarness();
    await vi.waitFor(() => expect(h.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });
    vi.useFakeTimers();
    // 未来服务端新增的动作（比如 drain）。旧设备必须当作「什么都没说」：
    // 既不重启、也不把 agent 当 running 重新拉起。
    vi.mocked(h.api.fetchDeviceControl).mockResolvedValue({
      agents: [{ agent: "ops", nodeId: "node_ops", desiredState: "drain", version: 9 }],
    });

    await realSleep(CONTROL_TICK_MS * 5);

    expect(h.spawned[0].child.kills).toEqual([]);
    expect(h.spawned).toHaveLength(1);
    expect(errorLog.some((line) => line.includes("control poll failed"))).toBe(false);
  });

  it("finishes the previous upgrade on startup when the new binary is the one running", async () => {
    const inflightPath = join(mkdtempSync(process.env.TEMP ?? "."), "upgrade-inflight.json");
    writeFileSync(
      inflightPath,
      JSON.stringify({ generation: 7, targetVersion: "1.3.0", phase: "restarting" }),
    );
    const h = makeHarness([], BOUND_OPS, {
      version: "1.3.0",
      upgradeInflightPath: inflightPath,
    });
    const report = vi.spyOn(h.api, "reportDeviceUpgrade").mockResolvedValue(undefined);

    await waitUntil(() => report.mock.calls.some((call) => call[3].status === "succeeded"));

    expect(report.mock.calls[0][3]).toMatchObject({ generation: 7, reportedVersion: "1.3.0" });
    // 账本消费掉，否则下次启动会再报一遍。
    expect(() => readFileSync(inflightPath, "utf8")).toThrow();
  });

  // R2 的回归用例：这条 `if (inflight.targetVersion === currentVersion)` 是**唯一**
  // 阻止「设备报成功但其实没升上去」的护栏。Vic 把它改成 `if (true)` 时 76 条用例
  // 全绿 —— 因为只有「账本说 1.3.1、进程跑 1.3.0」这一支没人测。这条补的就是它。
  it("reports a failure when the ledger asked for a version this process is not running", async () => {
    const inflightPath = join(mkdtempSync(process.env.TEMP ?? "."), "upgrade-inflight.json");
    writeFileSync(
      inflightPath,
      JSON.stringify({ generation: 9, targetVersion: "1.3.1", phase: "restarting" }),
    );
    // 账本说该升到 1.3.1，但真正跑起来的还是 1.3.0 → 交换没生效（或者新 exe 没被拉起）。
    const h = makeHarness([], BOUND_OPS, { version: "1.3.0", upgradeInflightPath: inflightPath });
    const report = vi.spyOn(h.api, "reportDeviceUpgrade").mockResolvedValue(undefined);

    await waitUntil(() => report.mock.calls.length > 0);
    // 第一条（也是唯一一条）回传必须是 failed —— 报成 succeeded 就等于告诉管理端
    // 「已升级完成」，而设备其实还在旧版本上跑。
    expect(report.mock.calls.map((call) => call[3].status)).toEqual(["failed"]);
    const failed = report.mock.calls[0][3];
    expect(failed).toMatchObject({ generation: 9, reportedVersion: "1.3.0", errorCode: "restart_failed" });
    expect(failed.errorDetail).toContain("1.3.1");
    expect(failed.errorDetail).toContain("1.3.0");
  });

  it("keeps the ledger and retries when the report cannot be delivered", async () => {
    // 登录自启时 Wi-Fi 还没起来是最常见的失败场景。这条回传一旦丢了，服务端就
    // 永远停在「重启中」——所以它必须留在队列里，等下一轮轮询重投（审查报告 M1）。
    const inflightPath = join(mkdtempSync(process.env.TEMP ?? "."), "upgrade-inflight.json");
    writeFileSync(
      inflightPath,
      JSON.stringify({ generation: 8, targetVersion: "1.3.0", phase: "restarting" }),
    );
    const h = makeHarness([], BOUND_OPS, { version: "1.3.0", upgradeInflightPath: inflightPath });
    let attempts = 0;
    vi.spyOn(h.api, "reportDeviceUpgrade").mockImplementation(async () => {
      attempts += 1;
      if (attempts <= 2) throw new Error("network down");
    });

    await waitUntil(() => attempts >= 1);
    // 前两次没送到：账本必须还在。
    expect(existsSync(inflightPath)).toBe(true);

    await waitUntil(() => attempts >= 3, 10_000);
    // 送达之后才清账本。
    await waitUntil(() => !existsSync(inflightPath), 10_000);

    // 送达之后必须出队：否则每 10 秒给服务端刷同一条 succeeded，永远停不下来。
    const settled = attempts;
    await realSleep(CONTROL_TICK_MS * 6);
    expect(attempts).toBe(settled);
  });
});

/**
 * 模拟「子进程已经拿到端口锁」：写 __relaunch 等待的交接凭据。
 * 真实实现是 main.ts 的 signalRelaunchReady（在锁到手之后调用）。
 */
function writeRelaunchReadyFile(target: string, pid: number, locked = true): void {
  const readyPath = join(dirname(target), "relaunch-ready.json");
  writeFileSync(readyPath, `${JSON.stringify({ pid, locked, at: new Date().toISOString() })}\n`);
}

describe("__relaunch helper", () => {
  it("heals a missing target from the backup before starting it", async () => {
    const dir = mkdtempSync(process.env.TEMP ?? ".");
    dirs.push(dir);
    const target = join(dir, "install", "clawmessenger.exe");
    mkdirSync(join(dir, "install"));
    // 交换只做了一半：target 不见了，只剩备份。
    writeFileSync(`${target}.old-1234`, "OLD-BINARY");

    // 它返回的 pid 必须**真的存在**：helper 起完之后会确认一次「它活着吗」，
    // 一个假的 pid 会被判定为「起来就死了」并触发重试（见 waitForAlive）。
    const spawned: Array<{ command: string; args: string[] }> = [];
    const program = buildProgram({
      keystore: new Keystore(join(dir, "creds.json")),
      apiFactory: () => ({}) as unknown as XiachatApi,
      stdout: { write: () => true } as unknown as NodeJS.WriteStream,
      acquireLock: () => Promise.resolve({ release: () => {} }),
      sleep: () => Promise.resolve(),
      relaunchAliveMs: 20,
      spawnChild: ((command: string, args: string[]) => {
        spawned.push({ command, args });
        // process.pid 是当前进程：一定活着。然后像真 supervisor 那样交回
        // 「我持有端口锁」的凭据（见 signalRelaunchReady）—— 活着本身不再算成功。
        writeRelaunchReadyFile(target, process.pid);
        return { unref: () => {}, pid: process.pid };
      }) as unknown as typeof spawn,
    });

    await program.parseAsync([
      "node", "clawmessenger", "__relaunch", "--wait-pid", "0", "--target", target, "--timeout", "1000",
      "--args", JSON.stringify(["--model", "kimi"]),
    ]);

    expect(readFileSync(target, "utf8")).toBe("OLD-BINARY");
    expect(spawned).toHaveLength(1);
    expect(spawned[0].command).toBe(target);
    // 透传参数：`install-service --model kimi` 装的设备升级一次之后不能悄悄换模型。
    expect(spawned[0].args).toEqual(["run", "--model", "kimi"]);
  });

  it("retries once and leaves a marker when the target dies right after spawn", async () => {
    const dir = mkdtempSync(process.env.TEMP ?? ".");
    dirs.push(dir);
    const target = join(dir, "install", "clawmessenger.exe");
    mkdirSync(join(dir, "install"));
    writeFileSync(target, "SOME-BINARY");

    let attempts = 0;
    const program = buildProgram({
      keystore: new Keystore(join(dir, "creds.json")),
      apiFactory: () => ({}) as unknown as XiachatApi,
      stdout: { write: () => true } as unknown as NodeJS.WriteStream,
      acquireLock: () => Promise.resolve({ release: () => {} }),
      sleep: () => Promise.resolve(),
      // pid 用一个几乎不可能存在的值：waitForAlive 立即判死 → 重试 → 仍死。
      relaunchAliveMs: 20,
      spawnChild: (() => {
        attempts += 1;
        return { unref: () => {}, pid: 999_999 };
      }) as unknown as typeof spawn,
    });

    await program.parseAsync([
      "node", "clawmessenger", "__relaunch", "--wait-pid", "0", "--target", target, "--timeout", "1000",
    ]);

    expect(attempts).toBe(2);
    // 标记文件落在 ~/.clawmessenger/ 下供人工排查；测试里不校验具体 home，只要
    // 不抛异常即可（写入失败也不能让 helper 崩掉）。
    expect(process.exitCode ?? 0).toBe(1);
    process.exitCode = undefined;
  });

  // 2026-10-29 那台 Linux 设备的原样复现：换盘成功、shutdown 也走完了，但旧进程
  // 退不出事件循环（agent 子进程不理会 SIGTERM）。原来这里 60 秒后只写个 marker
  // 就放弃 —— 磁盘是新版、一个 supervisor 都没有，设备永久离线。现在必须先礼后兵。
  it("kills a supervisor that never exits instead of leaving the device without one", async () => {
    const dir = mkdtempSync(join(process.env.TEMP ?? ".", "relaunch-stuck-"));
    dirs.push(dir);
    const target = join(dir, "install", "clawmessenger.exe");
    mkdirSync(join(dir, "install"));
    writeFileSync(target, "NEW-BINARY");

    const signals: string[] = [];
    const spawned: string[] = [];
    const output: string[] = [];
    let alive = true; // 只有 SIGKILL 才让它「消失」，SIGTERM 被无视
    const program = buildProgram({
      keystore: new Keystore(join(dir, "creds.json")),
      apiFactory: () => ({}) as unknown as XiachatApi,
      stdout: { write: (chunk: unknown) => { output.push(String(chunk)); return true } } as unknown as NodeJS.WriteStream,
      acquireLock: () => Promise.resolve({ release: () => {} }),
      sleep: () => Promise.resolve(),
      relaunchAliveMs: 20,
      relaunchTermGraceMs: 0,
      relaunchKillSettleMs: 0,
      pidProbe: () => alive,
      killProcess: (_pid, signal) => {
        signals.push(signal);
        if (signal === "SIGKILL") alive = false;
      },
      spawnChild: ((command: string) => {
        spawned.push(command);
        writeRelaunchReadyFile(target, process.pid);
        return { unref: () => {}, pid: process.pid };
      }) as unknown as typeof spawn,
    });

    await program.parseAsync([
      "node", "clawmessenger", "__relaunch", "--wait-pid", String(process.pid), "--target", target, "--timeout", "1",
    ]);

    expect(signals).toEqual(["SIGTERM", "SIGKILL"]);
    expect(spawned).toEqual([target]);
    expect(output.join("")).toContain("killed it and continuing");
    expect(process.exitCode ?? 0).toBe(0);
  });

  // 连 SIGKILL 都杀不掉只可能是 D 状态（不可中断睡眠）。这时仍然不能起第二份
  // supervisor —— 两份会登录同一批融云账号、互相踢线，比离线更难收拾。
  it("still refuses to start a second supervisor when the pid survives SIGKILL", async () => {
    const dir = mkdtempSync(join(process.env.TEMP ?? ".", "relaunch-unkillable-"));
    dirs.push(dir);
    const target = join(dir, "install", "clawmessenger.exe");
    mkdirSync(join(dir, "install"));
    writeFileSync(target, "NEW-BINARY");

    const home = process.env.CLAWMESSENGER_HOME;
    // 失败标记按 CLAWMESSENGER_HOME 落盘，别写进真实家目录。
    process.env.CLAWMESSENGER_HOME = dir;
    try {
      const signals: string[] = [];
      const spawned: string[] = [];
      const program = buildProgram({
        keystore: new Keystore(join(dir, "creds.json")),
        apiFactory: () => ({}) as unknown as XiachatApi,
        stdout: { write: () => true } as unknown as NodeJS.WriteStream,
        acquireLock: () => Promise.resolve({ release: () => {} }),
        sleep: () => Promise.resolve(),
        relaunchAliveMs: 20,
        relaunchTermGraceMs: 0,
        relaunchKillSettleMs: 0,
        pidProbe: () => true,
        killProcess: (_pid, signal) => { signals.push(signal) },
        spawnChild: ((command: string) => {
          spawned.push(command);
          return { unref: () => {}, pid: process.pid };
        }) as unknown as typeof spawn,
      });

      await program.parseAsync([
        "node", "clawmessenger", "__relaunch", "--wait-pid", String(process.pid), "--target", target, "--timeout", "1",
      ]);

      expect(signals).toEqual(["SIGTERM", "SIGKILL"]);
      expect(spawned).toEqual([]);
      expect(process.exitCode).toBe(1);
      // 留痕要能分辨「它就是不退」，而不是原来那句笼统的 did not exit。
      const marker = JSON.parse(readFileSync(join(dir, "relaunch-failed.json"), "utf8")) as {
        reason: string; detail?: string;
      };
      expect(marker.reason).toBe("pid_timeout");
      expect(marker.detail).toContain("survived SIGTERM");
    } finally {
      process.exitCode = undefined;
      if (home === undefined) delete process.env.CLAWMESSENGER_HOME;
      else process.env.CLAWMESSENGER_HOME = home;
    }
  });
});

// 单例端口锁与升级接力的交界。判据只有一条：**子进程自己交出「我持有锁」**。
//
// 历史教训（审查报告 B1）：原来父进程「拿锁 → spawn → 看子进程还活着 → 放锁」，
// 而「活着」不证明它绑上了端口。子进程其实还在等锁、随后超时自杀，父进程却已经
// 宣告成功并退出 —— 磁盘上是新版 exe、没有任何 supervisor 在跑，服务端永远停在
// restarting。那次还有第二层错误：`waitForAlive(pid, sleep, 3_000)` 的第三个位置
// 参数其实是「次数」，3000 次 × 200ms = 最多 10 分钟，正好把子进程等死。
describe("relaunch lock handover", () => {
  it("spawns and releases the lock before the child reports it holds it", async () => {
    const dir = mkdtempSync(join(process.env.TEMP ?? ".", "relaunch-lock-"));
    dirs.push(dir);
    const target = join(dir, "clawmessenger.exe");
    writeFileSync(target, "CURRENT-BINARY");

    const events: string[] = [];
    const spawned: Array<{ args: string[]; env?: NodeJS.ProcessEnv; stdio?: unknown }> = [];
    const program = buildProgram({
      keystore: new Keystore(join(dir, "creds.json")),
      apiFactory: () => ({}) as unknown as XiachatApi,
      stdout: { write: () => true } as unknown as NodeJS.WriteStream,
      acquireLock: () => Promise.resolve({ release: () => { events.push("release") } }),
      sleep: () => Promise.resolve(),
      relaunchAliveMs: 20,
      spawnChild: ((_command: string, args: string[], options: { env?: NodeJS.ProcessEnv; stdio?: unknown }) => {
        events.push("spawn");
        spawned.push({ args, env: options?.env, stdio: options?.stdio });
        // 子进程拿到锁之后才写凭据 —— 此时父进程必须已经放锁（事件顺序断言）。
        writeRelaunchReadyFile(target, process.pid);
        return { unref: () => {}, pid: process.pid };
      }) as unknown as typeof spawn,
    });

    await program.parseAsync([
      "node", "clawmessenger", "__relaunch", "--wait-pid", "0", "--target", target, "--timeout", "1000",
    ]);

    expect(events).toEqual(["spawn", "release"]);
    expect(spawned[0]?.env?.[RELAUNCH_LOCK_WAIT_ENV]).toBe(String(RELAUNCH_LOCK_WAIT_MS));
    // 交接通道必须显式传给子进程，否则它不知道往哪写「我持有锁」。
    expect(spawned[0]?.env?.[RELAUNCH_READY_ENV]).toBe(join(dir, "relaunch-ready.json"));
    // helper 的输出必须落盘（原来 stdio:"ignore" 把「为什么没起来」整个丢掉）。
    // 形状：["ignore", fd, fd]，fd 指向安装目录旁的 relaunch.log。
    expect(Array.isArray(spawned[0]?.stdio)).toBe(true);
    expect((spawned[0]?.stdio as unknown[])[0]).toBe("ignore");
    expect(typeof (spawned[0]?.stdio as unknown[])[1]).toBe("number");
    expect(readFileSync(join(dir, "relaunch.log"), "utf8")).toBe("");
    expect(process.exitCode ?? 0).toBe(0);
  });

  // B1 的回归用例：子进程**活着**但从不交回「我持有锁」。
  // 旧实现会在这里报成功（它只看 waitForAlive），于是设备「升级完成」却没人值守。
  it("fails when the child stays alive but never takes the lock", async () => {
    const dir = mkdtempSync(join(process.env.TEMP ?? ".", "relaunch-lock-"));
    dirs.push(dir);
    const target = join(dir, "clawmessenger.exe");
    writeFileSync(target, "CURRENT-BINARY");
    process.env.CLAWMESSENGER_HOME = join(dir, ".clawmessenger");
    const output: string[] = [];
    try {
      const program = buildProgram({
        keystore: new Keystore(join(dir, "creds.json")),
        apiFactory: () => ({}) as unknown as XiachatApi,
        stdout: { write: (chunk: unknown) => { output.push(String(chunk)); return true } } as unknown as NodeJS.WriteStream,
        acquireLock: () => Promise.resolve({ release: () => {} }),
        // 真实时钟 + 很短的窗口：这条用例要的是「窗口内没等到」，不是真等 10 秒。
        relaunchAliveMs: 20,
        relaunchReadyMs: 120,
        spawnChild: (() => {
          // 一直活着，但永不写 ready：等价于「进程起来了、却始终没绑上端口」。
          return { unref: () => {}, pid: process.pid };
        }) as unknown as typeof spawn,
      });

      await program.parseAsync([
        "node", "clawmessenger", "__relaunch", "--wait-pid", "0", "--target", target, "--timeout", "1000",
      ]);

      const text = output.join("");
      expect(process.exitCode ?? 0).toBe(1);
      expect(text).not.toContain("lock handed over");
      expect(text).toMatch(/never reported holding the supervisor lock/);
      // 零痕迹是这条 bug 最难查的部分：失败必须留下可读的标记。
      const marker = JSON.parse(readFileSync(join(dir, ".clawmessenger", "relaunch-failed.json"), "utf8")) as {
        reason: string;
      };
      expect(marker.reason).toBe("lock_handover_timeout");
    } finally {
      delete process.env.CLAWMESSENGER_HOME;
      process.exitCode = undefined;
    }
  });

  // R4 的回归用例：子进程起来了、也写了 ready，但自报「没拿到锁」。父进程这时
  // 不能宣告交接成功 —— 那等于向自己确认了一件没发生的事（两台 supervisor 各答一遍）。
  it("does not report a handover when the child says it never got the lock", async () => {
    const dir = mkdtempSync(join(process.env.TEMP ?? ".", "relaunch-lock-"));
    dirs.push(dir);
    const target = join(dir, "clawmessenger.exe");
    writeFileSync(target, "CURRENT-BINARY");
    process.env.CLAWMESSENGER_HOME = join(dir, ".clawmessenger");
    const output: string[] = [];
    try {
      const program = buildProgram({
        keystore: new Keystore(join(dir, "creds.json")),
        apiFactory: () => ({}) as unknown as XiachatApi,
        stdout: { write: (chunk: unknown) => { output.push(String(chunk)); return true } } as unknown as NodeJS.WriteStream,
        acquireLock: () => Promise.resolve({ release: () => {} }),
        relaunchAliveMs: 20,
        relaunchReadyMs: 2_000,
        spawnChild: (() => {
          // 子进程的锁探测报错 → 它会在「没有端口锁」的情况下继续启动并写 locked:false。
          writeRelaunchReadyFile(target, process.pid, false);
          return { unref: () => {}, pid: process.pid };
        }) as unknown as typeof spawn,
      });

      await program.parseAsync([
        "node", "clawmessenger", "__relaunch", "--wait-pid", "0", "--target", target, "--timeout", "1000",
      ]);

      const text = output.join("");
      expect(process.exitCode ?? 0).toBe(1);
      expect(text).not.toContain("lock handed over");
      expect(text).toMatch(/reported no singleton lock/);
      const marker = JSON.parse(readFileSync(join(dir, ".clawmessenger", "relaunch-failed.json"), "utf8")) as {
        reason: string;
      };
      expect(marker.reason).toBe("lock_not_held");
    } finally {
      delete process.env.CLAWMESSENGER_HOME;
      process.exitCode = undefined;
    }
  });

  it("keeps the old one-shot lock check when the relaunch did not ask for a wait", async () => {
    // 普通启动（开机自启、人手 run）不放宽：抢不到锁就拒绝，避免两份 supervisor
    // 登录同一批融云账号互相踢线。
    delete process.env[RELAUNCH_LOCK_WAIT_ENV];
    let attempts = 0;
    const h = makeHarness([], BOUND_OPS, {
      acquireLock: async () => {
        attempts += 1;
        return null;
      },
    });
    await h.run.catch(() => undefined);
    expect(attempts).toBe(1);
    expect(h.spawned).toHaveLength(0);
  });

  it("waits for the outgoing supervisor when the relaunch asks it to", async () => {
    process.env[RELAUNCH_LOCK_WAIT_ENV] = "5000";
    try {
      let attempts = 0;
      const h = makeHarness([], BOUND_OPS, {
        acquireLock: async () => {
          attempts += 1;
          // 前两次是父进程还没放锁（EADDRINUSE），第三次才拿到。
          return attempts < 3 ? null : { release: () => {} };
        },
        sleep: () => Promise.resolve(),
      });
      await vi.waitFor(() => expect(h.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });
      expect(attempts).toBe(3);
    } finally {
      delete process.env[RELAUNCH_LOCK_WAIT_ENV];
    }
  });
});

// 双开红线（2026-10-20 现场两套进程树并存两小时，同一条私聊被各答一遍）的正面
// 回归网。以前只有两条用例碰过这道防线：一条把 `acquireLock` 直接 mock 成 null
// （不碰真实端口），daemon.test.ts 那条测的是 pid 文件。这里用「只能成功一次的
// 内存开关」真跑两次 `run`，钉住「第二份一定进不来」（审查报告 R3）。
describe("singleton port lock（两个 supervisor 同时启动）", () => {
  function oneShotLock(): { acquire: () => Promise<{ release: () => void } | null>; attempts: () => number } {
    let held = false;
    let attempts = 0;
    return {
      acquire: async () => {
        attempts += 1;
        if (held) return null;
        held = true;
        return { release: () => { held = false } };
      },
      attempts: () => attempts,
    };
  }

  it("第二份启动即退出，一台设备只允许一棵进程树", async () => {
    const previousExitCode = process.exitCode;
    try {
      const lock = oneShotLock();
      // 两份 supervisor 各有自己的 state 目录，于是 pid 文件那道防线**帮不上忙**
      // —— 这正是要测的场景：只有端口锁能拦住它们。
      const first = makeHarness([], BOUND_OPS, { acquireLock: lock.acquire });
      await vi.waitFor(() => expect(first.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });

      const second = makeHarness([], BOUND_OPS, { acquireLock: lock.acquire });
      // 有界等待而不是直接 await：如果护栏失效，第二份不会退出（它会一直跑下去），
      // 那样用例会以「超时」收场，读起来像是测试写坏了。这里让失败信息直说
      // 「第二份还在跑」。
      const outcome = await Promise.race([
        second.run.then(() => "exited" as const),
        new Promise<"still running">((resolve) => { setTimeout(() => resolve("still running"), 3_000) }),
      ]);
      expect(outcome).toBe("exited");
      expect(process.exitCode).toBe(1);
      expect(second.spawned).toHaveLength(0);
      expect(second.output.join("")).toContain("singleton lock held, no live pid file");
    } finally {
      process.exitCode = previousExitCode;
    }
  });

  it("CLAWMESSENGER_LOCK_WAIT_MS 只让接力子进程多等一会儿，不能把第二份放进来", async () => {
    const previousExitCode = process.exitCode;
    process.env[RELAUNCH_LOCK_WAIT_ENV] = "300";
    try {
      const lock = oneShotLock();
      const first = makeHarness([], BOUND_OPS, { acquireLock: lock.acquire });
      await vi.waitFor(() => expect(first.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });

      const second = makeHarness([], BOUND_OPS, { acquireLock: lock.acquire });
      await second.run.catch(() => undefined);

      // 确实重试了（不是侥幸一次失败），但等完窗口仍然被拒。
      expect(lock.attempts()).toBeGreaterThan(2);
      expect(process.exitCode).toBe(1);
      expect(second.spawned).toHaveLength(0);
    } finally {
      delete process.env[RELAUNCH_LOCK_WAIT_ENV];
      process.exitCode = previousExitCode;
    }
  });

  // R4：探测报错时不再「告警后静默放行」。放行仍然放行（否则权限问题会把整台设备
  // 弄离线），但必须把「没有端口锁」和「会被回答两次」说清楚。
  it("锁探测报错时明说「没有端口锁、会被回答两次」，而不是悄悄放行", async () => {
    const h = makeHarness([], BOUND_OPS, {
      acquireLock: () => Promise.reject(new Error("EACCES: permission denied 127.0.0.1:41307")),
    });
    await vi.waitFor(() => expect(h.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });

    const text = h.output.join("");
    expect(text).toContain("没有");
    expect(text).toContain("端口锁");
    expect(text).toContain("会被回答两次");
    // 警告里必须带上真正的原因，否则运维只能猜。
    expect(text).toContain("EACCES");
  });
});
// 上一轮接力失败的证据必须被**读到**：只写不读的标记等于没留痕迹（审查报告 M1）。
describe("relaunch failure marker", () => {
  it("logs the previous relaunch failure on startup and clears it", async () => {
    const dir = mkdtempSync(join(process.env.TEMP ?? ".", "relaunch-marker-"));
    dirs.push(dir);
    const markerDir = join(dir, ".clawmessenger");
    mkdirSync(markerDir, { recursive: true });
    const markerPath = join(markerDir, "relaunch-failed.json");
    writeFileSync(
      markerPath,
      `${JSON.stringify({
        target: join(dir, "clawmessenger.exe"),
        args: [],
        reason: "lock_handover_timeout",
        detail: "pid 123 never reported holding the supervisor lock",
        at: new Date().toISOString(),
      })}\n`,
    );
    process.env.CLAWMESSENGER_HOME = markerDir;
    try {
      const h = makeHarness([], BOUND_OPS);
      await vi.waitFor(() => expect(h.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });
      expect(errorLog.join("")).toContain("the previous relaunch failed (reason=lock_handover_timeout)");
      expect(existsSync(markerPath)).toBe(false);
    } finally {
      delete process.env.CLAWMESSENGER_HOME;
    }
  });
});

describe("startup script backfill", () => {
  it("refreshes a stale logon script on startup and reports it", async () => {
    let calls = 0;
    const h = makeHarness([], BOUND_OPS, {
      refreshStartupScript: async () => {
        calls += 1;
        return { updated: true, path: "/tmp/clawmessenger.vbs" };
      },
    });
    await vi.waitFor(() => expect(h.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });
    expect(calls).toBe(1);
    expect(h.output.join("")).toContain("refreshed /tmp/clawmessenger.vbs");
  });

  it("does not report anything when the script was already current", async () => {
    const h = makeHarness([], BOUND_OPS, {
      refreshStartupScript: async () => ({ updated: false, path: "/tmp/clawmessenger.vbs", reason: "already-current" }),
    });
    await vi.waitFor(() => expect(h.spawned.length).toBeGreaterThan(0), { timeout: 5_000 });
    expect(h.output.join("")).not.toContain("refreshed");
  });
});
