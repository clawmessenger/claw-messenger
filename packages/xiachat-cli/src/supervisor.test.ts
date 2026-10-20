// @vitest-environment node
import { EventEmitter } from "node:events";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { spawn } from "node:child_process";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  buildProgram,
  supervisorRestartDelayMs,
  SUPERVISOR_CONTROL_POLL_MS,
  SUPERVISOR_RESTART_BACKOFF_MS,
} from "./main.js";
import { Keystore } from "./keystore.js";
import { XiachatApi, type AgentControlState } from "./api.js";
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

function makeHarness(initialControl: AgentControlState[] = [], bound: unknown[] = BOUND_OPS): Harness {
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
  // requests at http://unused.local the moment the clock advances.
  vi.spyOn(api, "fetchDeviceControl").mockImplementation(async () => initialControl);

  const program = buildProgram({
    keystore,
    apiFactory: () => api,
    stdout: { write: (chunk: unknown) => { output.push(String(chunk)); return true } } as unknown as NodeJS.WriteStream,
    ensureOpencode: async () => ({}),
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
  });

  const run = program.parseAsync(["node", "clawmessenger", "run"]);
  const harness: Harness = {
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

  it("cancels a pending restart timer at shutdown", async () => {
    const h = await startSupervisor();

    h.spawned[0].child.emit("exit", 1); // schedules a 1s restart
    h.getShutdown()();
    await vi.advanceTimersByTimeAsync(120_000);
    expect(h.spawned).toHaveLength(1);
  });
});

describe("operator-driven start/stop", () => {
  const stopped = (version: number): AgentControlState[] =>
    [{ agent: "ops", nodeId: "node_ops", desiredState: "stopped", version }];
  const running = (version: number): AgentControlState[] =>
    [{ agent: "ops", nodeId: "node_ops", desiredState: "running", version }];

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
    control.mockResolvedValue([
      { agent: "opencode", nodeId: "node_open", desiredState: "running", version: 0 },
      { agent: "hermes", nodeId: "node_h", desiredState: "running", version: 0 },
    ]);
    await realSleep(CONTROL_TICK_MS * 3);
    expect(h.spawned[0].child.kills).toEqual([]);
    expect(h.spawned[1].child.kills).toEqual([]);

    // 运维助手执行 `agent restart opencode`：running 不变，版本 0 -> 1。
    control.mockResolvedValue([
      { agent: "opencode", nodeId: "node_open", desiredState: "running", version: 1 },
      { agent: "hermes", nodeId: "node_h", desiredState: "running", version: 0 },
    ]);
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
