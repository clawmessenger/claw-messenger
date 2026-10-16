import { Command } from "commander";
import { spawn, type ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import * as os from "node:os";
import { Keystore, type StoredCredentials } from "./keystore.js";
import { XiachatApi } from "./api.js";
import { discoverAgents } from "./agents.js";
import { DEFAULT_SERVER_URL, machineId, defaultProcessedUidsPath } from "./config.js";
import { ensureOpencodeInstalled, OPS_AGENT_NAME, OPS_TURN_TIMEOUT_MS, setupOpsWorkdir } from "./ops.js";
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
  // Injectable for tests; defaults to a real cross-platform supervisor daemon.
  daemon?: Daemon;
  // Injectable for tests; the state/pid files the supervisor writes about itself
  // (default: ~/.clawmessenger). Must agree with whatever `daemon` above uses.
  daemonPaths?: DaemonPaths;
}

export function buildProgram(opts: BuildProgramOpts): Command {
  const program = new Command();
  program
    .name("clawmessenger")
    .description("User-device agent CLI over RongCloud IM")
    .version("0.2.24")
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

  program
    .command("pair")
    .description("Register this device and claim a pairing session with a ticket")
    .requiredOption("--ticket <ticket>", "pairing ticket (pt_...)")
    .option("--server <url>", "Quukk server base URL", DEFAULT_SERVER_URL)
    .option("--name <name>", "node display name")
    .option("--ai-type <type>", "agent platform (claude, codex, opencode, ...)", "opencode")
    .action(async (cmdOpts: { ticket: string; server: string; name?: string; aiType: string }) => {
      const api = opts.apiFactory(cmdOpts.server);
      const ensureOpencode = opts.ensureOpencode ?? ensureOpencodeInstalled;
      // opencode is the default runtime for the built-in ops (运维) agent and a
      // common default agent. Install it up front on devices that lack it so it
      // is available to bind and shows up in the reported agent list. Best
      // effort: an install failure must not block pairing.
      try {
        await ensureOpencode();
      } catch (err) {
        console.warn("opencode install failed:", err instanceof Error ? err.message : String(err));
      }
      // Register with the ticket first: the server attributes the node to
      // the ticket's workspace and backfills the session's candidate node
      // list so the subsequent claim can issue device credentials. The
      // machine's discoverable agent CLIs are reported alongside so the web
      // dialog can offer them for checkbox binding.
      const foundAgents = await discoverAgents({});
      try {
        const reg = await api.register({
          name: cmdOpts.name ?? `${os.hostname()} ${cmdOpts.aiType}`,
          aiType: cmdOpts.aiType,
          nodeType: "ai",
          macAddress: (opts.machineId ?? machineId)(),
          pairingTicket: cmdOpts.ticket,
          agents: foundAgents.map((a) => a.name),
        });
        opts.keystore.save({
          nodeId: reg.nodeId,
          token: reg.token,
          credentialId: reg.deviceCredentialTicket,
          serverUrl: cmdOpts.server,
        });
        opts.stdout.write(`registered ${reg.nodeId}\n`);
      } catch (err) {
        const message = err instanceof Error ? err.message : String(err);
        if (message.includes("attribution")) {
          throw new Error(`${message}\nhint: provide --pairing-ticket (get one from your workspace admin)`);
        }
        throw err;
      }
      const idemKey = pairIdempotencyKey(cmdOpts.ticket, (opts.machineId ?? machineId)());
      const result = await api.claimPairing(cmdOpts.ticket, "", idemKey);
      if (result.session?.status === "claimed" && !result.deviceSecret) {
        throw new Error("ticket already claimed by another device");
      }
      opts.keystore.save({
        nodeId: result.nodeId,
        token: "",
        credentialId: result.deviceCredentialId,
        deviceSecret: result.deviceSecret,
        serverUrl: cmdOpts.server,
      });
      opts.stdout.write(`paired as ${result.nodeId}\n`);
    });

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
      if (found.length === 0) {
        opts.stdout.write("no agent CLIs found on PATH\n");
        return;
      }
      for (const a of found) opts.stdout.write(`${a.name}\t${a.path}\n`);
    });

  program
    .command("run")
    .description(
      "Connect to RongCloud IM and dispatch agent turns. Without --agent, run every agent bound to this device (supervisor mode).",
    )
    .option("--agent <name>", "agent CLI name; must be discoverable on PATH (see clawmessenger agents)")
    .option("--model <model>", "model override passed to the agent CLI")
    .action(async (cmdOpts: { agent?: string; model?: string }) => {
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
        await runSupervisor({
          opts,
          creds,
          api,
          model: cmdOpts.model,
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
        const workdir = await setupOpsWorkdir();
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
      opts.stdout.write(formatDaemonStatus(daemon.status(), daemonPaths));
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
      await daemon.stop();
    });

  daemonOpts(
    program.command("restart").description("Stop the background supervisor, then start it again"),
  ).action(async (cmdOpts: { model?: string; workdir?: string }) => {
    await daemon.restart(cmdOpts);
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
    .action(async (action: string, cmdOpts: { model?: string; workdir?: string; lines: string }) => {
      // --lines 只在 logs 有意义，别把它混进 start / restart / install-service 的选项里。
      const passthrough = { model: cmdOpts.model, workdir: cmdOpts.workdir };
      switch (action) {
        case "start":
          await daemon.start(passthrough);
          return;
        case "stop":
          await daemon.stop();
          return;
        case "restart":
          await daemon.restart(passthrough);
          return;
        case "status":
          opts.stdout.write(formatDaemonStatus(daemon.status(), daemonPaths));
          return;
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
  /** Poll cadence override, for tests. */
  controlPollIntervalMs?: number;
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

/** Delay before the `restarts`-th consecutive restart (1-based). */
export function supervisorRestartDelayMs(restarts: number): number {
  if (restarts > SUPERVISOR_RESTART_BACKOFF_AFTER) return SUPERVISOR_RESTART_BACKOFF_MS;
  return Math.min(SUPERVISOR_RESTART_MAX_MS, SUPERVISOR_RESTART_BASE_MS * 2 ** (Math.max(1, restarts) - 1));
}

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
  timer?: NodeJS.Timeout;
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

  const launch = (entry: SupervisedAgent): void => {
    if (stopping) return;
    const child = spawnChild(
      process.execPath,
      [...spawnArgvPrefix, ...entry.runArgs],
      {
        env: { ...process.env, CLAWMESSENGER_AGENT_CREDS: JSON.stringify(entry.childCreds) },
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
    child.stdout?.on("data", (chunk: Buffer) => opts.stdout.write(`[${entry.agent}] ${chunk}`));
    child.stderr?.on("data", (chunk: Buffer) => process.stderr.write(`[${entry.agent}] ${chunk}`));
    child.on("exit", (code) => {
      entry.lastExitCode = code;
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

  /** Last-known desired state per agent. Failures yield an empty map, which
   * applyControlStates reads as "change nothing" — a server blip must never
   * stop agents that the operator left running. */
  const fetchControlStates = async (): Promise<Map<string, "running" | "stopped">> => {
    const desired = new Map<string, "running" | "stopped">();
    try {
      const states = await api.fetchDeviceControl(nodeID, credentialID, deviceSecret);
      for (const state of states) {
        if (state.agent) desired.set(state.agent, state.desiredState === "stopped" ? "stopped" : "running");
      }
    } catch (err) {
      console.error("supervisor: control poll failed:", err instanceof Error ? err.message : String(err));
    }
    return desired;
  };

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

  const applyControlStates = (desired: Map<string, "running" | "stopped">): void => {
    for (const [agent, state] of desired) {
      const entry = managed.get(agent);
      // An agent the supervisor never started (its CLI is not on PATH) is not
      // ours to converge; the server still lists it because the node is bound.
      if (!entry) continue;
      if (state === "stopped") stopAgent(entry);
      else startAgent(entry);
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
    const runArgs = ["run", "--agent", agent.agent];
    if (supervisor.model) runArgs.push("--model", supervisor.model);
    const desired = initialDesired.get(agent.agent) ?? "running";
    const entry: SupervisedAgent = {
      agent: agent.agent,
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
    };
    managed.set(agent.agent, entry);
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

  const controlPollMs = supervisor.controlPollIntervalMs ?? SUPERVISOR_CONTROL_POLL_MS;
  const controlTimer = setInterval(() => {
    void fetchControlStates().then(applyControlStates);
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
  if (opts.onSupervisorSignal) {
    opts.onSupervisorSignal(shutdown);
  } else {
    process.on("SIGINT", shutdown);
    process.on("SIGTERM", shutdown);
  }
  await stopped; // supervise until told to stop
}
