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

function makeHarness(initialControl: AgentControlState[] = []): Harness {
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
  vi.spyOn(api, "fetchDeviceNodes").mockImplementation(async () => BOUND_OPS as never);
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
