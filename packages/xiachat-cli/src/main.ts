import { Command } from "commander";
import { spawn, type ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import { createServer as createNetServer } from "node:net";
import * as os from "node:os";
import * as path from "node:path";
import { Keystore, type StoredCredentials } from "./keystore.js";
import { XiachatApi, type MachineAgentCredential } from "./api.js";
import { discoverAgents } from "./agents.js";
import { DEFAULT_SERVER_URL, machineId, defaultProcessedUidsPath } from "./config.js";
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
import { installPaths } from "./install.js";
import { checkForUpdate, stageUpdate } from "./self-update.js";

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
}

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

export function buildProgram(opts: BuildProgramOpts): Command {
  const program = new Command();
  program
    .name("clawmessenger")
    .description("User-device agent CLI over RongCloud IM")
    // ⚠️ 与 package.json 的 version 手动同步：SEA 打包后读不到外部 package.json，
    //    改版本时两处一起改（1.0.0：生产环境正式版；1.1.0：内置自更新；
    //    1.2.0：codebuddy/qwen 模型目录 + quukk 直连）
    .version("1.2.0")
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
        if (!allowDuplicateSupervisor()) {
          const acquire = opts.acquireLock ?? acquireSupervisorLock;
          let lock: SupervisorLock | null = null;
          let lockCheckError = false;
          try {
            lock = await acquire(SUPERVISOR_LOCK_PORT);
          } catch (err) {
            // 非 EADDRINUSE 的绑定失败（如权限）不误伤正常启动：告警后放行，
            // pid 文件防线仍在。
            lockCheckError = true;
            console.warn("supervisor lock check failed:", err instanceof Error ? err.message : String(err));
          }
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
    child.stdout?.on("data", (chunk: Buffer) => opts.stdout.write(`[${entry.agent}] ${chunk}`));
    child.stderr?.on("data", (chunk: Buffer) => process.stderr.write(`[${entry.agent}] ${chunk}`));
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

  /** 上次看到的期望状态与版本。失败时返回空 Map，applyControlStates 读作
   * 「什么都别改」—— 服务端抖动绝不能把操作员留着运行的 agent 停掉。 */
  const fetchControlStates = async (): Promise<Map<string, ControlIntent>> => {
    const desired = new Map<string, ControlIntent>();
    try {
      const states = await api.fetchDeviceControl(nodeID, credentialID, deviceSecret);
      for (const state of states) {
        if (state.agent) {
          desired.set(state.agent, {
            state: state.desiredState === "stopped" ? "stopped" : "running",
            version: typeof state.version === "number" ? state.version : 0,
          });
        }
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
