// @vitest-environment node
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import { allowDuplicateSupervisor, buildProgram, OPS_SCOPE_ENV, scopeDaemonReport, SUPERVISED_ENV, supervisedSelfKillRejection, wholeMachineActionRejection } from "./main.js";
import { Keystore } from "./keystore.js";
import type { XiachatApi } from "./api.js";
import { defaultDaemonPaths, type Daemon } from "./daemon.js";

const dirs: string[] = [];
function tempKeystore(): Keystore {
  const d = mkdtempSync(join(tmpdir(), "clawmessenger-main-"));
  dirs.push(d);
  return new Keystore(join(d, "creds.json"));
}
afterEach(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
  dirs.length = 0;
});

describe("clawmessenger register", () => {
  it("stores credentials from the server response", async () => {
    const keystore = tempKeystore();
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async () => ({ nodeId: "node_1", token: "tok_1", deviceCredentialTicket: "dc_1", bindingVersion: 1 }),
      claimPairing: async () => ({ deviceCredentialId: "", deviceSecret: "", nodeId: "" }),
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      machineId: () => "test-machine-1",
    });
    await program.parseAsync(["node", "clawmessenger", "register", "--name", "我的Claude", "--ai-type", "claude", "--server", "http://srv"]);
    const stored = keystore.load();
    expect(stored?.nodeId).toBe("node_1");
    expect(stored?.token).toBe("tok_1");
    expect(stored?.serverUrl).toBe("http://srv");
  });

  it("sends a non-empty stable mac_address on register", async () => {
    const keystore = tempKeystore();
    const seenMac: string[] = [];
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async (params: { macAddress?: string }) => {
        seenMac.push(params.macAddress ?? "");
        return { nodeId: "node_1", token: "tok_1" };
      },
      claimPairing: async () => ({ deviceCredentialId: "", deviceSecret: "", nodeId: "" }),
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      machineId: () => "test-machine-2",
    });
    await program.parseAsync(["node", "clawmessenger", "register", "--name", "n", "--ai-type", "claude", "--server", "http://srv"]);
    await program.parseAsync(["node", "clawmessenger", "register", "--name", "n", "--ai-type", "claude", "--server", "http://srv"]);
    expect(seenMac[0]).toBe("test-machine-2");
    expect(seenMac[1]).toBe(seenMac[0]);
  });

  it("register --server is required", async () => {
    const program = buildProgram({
      keystore: tempKeystore(),
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: process.stdout,
    });
    await expect(
      program.parseAsync(["node", "clawmessenger", "register", "--name", "n", "--ai-type", "claude"]),
    ).rejects.toThrow();
  });

  it("surfaces a pairing-ticket hint on workspace attribution errors", async () => {
    const keystore = tempKeystore();
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async () => {
        throw new Error("POST /api/ai/register: HTTP 400: workspace attribution required: provide a pairing_ticket");
      },
      claimPairing: async () => ({ deviceCredentialId: "", deviceSecret: "", nodeId: "" }),
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      machineId: () => "test-machine-1",
    });
    await expect(
      program.parseAsync(["node", "clawmessenger", "register", "--name", "n", "--ai-type", "claude", "--server", "http://srv"]),
    ).rejects.toThrow(/--pairing-ticket/);
    expect(keystore.load()).toBeNull();
  });
});

describe("clawmessenger pair", () => {
  it("does not overwrite the keystore when the ticket was already claimed", async () => {
    const keystore = tempKeystore();
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async () => ({ nodeId: "node_1", token: "tok_1" }),
      claimPairing: async () => ({
        deviceCredentialId: "dc_9",
        deviceSecret: "",
        nodeId: "node_2",
        session: { status: "claimed", ticket: "st_1" },
      }),
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      ensureOpencode: async () => ({}),
    });
    await expect(
      program.parseAsync(["node", "clawmessenger", "pair", "--ticket", "pt_1"]),
    ).rejects.toThrow(/already claimed/);
    // The register step still persisted its own node identity, but the
    // claimed-by-another-device result must not overwrite it.
    expect(keystore.load()?.nodeId).toBe("node_1");
  });

  it("derives the same idempotency key across retries on one machine", async () => {
    const keystore = tempKeystore();
    const seenIdemKeys: string[] = [];
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async () => ({ nodeId: "node_5", token: "tok_5" }),
      claimPairing: async (_ticket: string, _cck: string, idemKey: string) => {
        seenIdemKeys.push(idemKey);
        return {
          deviceCredentialId: "dc_1",
          deviceSecret: "sec_1",
          nodeId: "node_5",
        };
      },
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      machineId: () => "machine-A",
      ensureOpencode: async () => ({}),
    });
    // A retried claim after a lost response: same ticket, same machine.
    await program.parseAsync(["node", "clawmessenger", "pair", "--ticket", "pt_same"]);
    await program.parseAsync(["node", "clawmessenger", "pair", "--ticket", "pt_same"]);
    expect(seenIdemKeys).toHaveLength(2);
    expect(seenIdemKeys[0]).toBe(seenIdemKeys[1]);
    expect(seenIdemKeys[0]).toMatch(/^idem-[0-9a-f]{24}$/);

    // A different ticket on the same machine derives a different key.
    await program.parseAsync(["node", "clawmessenger", "pair", "--ticket", "pt_other"]);
    expect(seenIdemKeys[2]).not.toBe(seenIdemKeys[0]);
  });

  it("derives a different idempotency key per machine", async () => {
    const keystore = tempKeystore();
    const seenIdemKeys: string[] = [];
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async () => ({ nodeId: "node_6", token: "tok_6" }),
      claimPairing: async (_ticket: string, _cck: string, idemKey: string) => {
        seenIdemKeys.push(idemKey);
        return { deviceCredentialId: "dc_1", deviceSecret: "s", nodeId: "node_6" };
      },
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      machineId: () => "machine-B",
      ensureOpencode: async () => ({}),
    });
    await program.parseAsync(["node", "clawmessenger", "pair", "--ticket", "pt_same"]);
    const machineBKey = seenIdemKeys[0];
    const program2 = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      machineId: () => "machine-C",
      ensureOpencode: async () => ({}),
    });
    await program2.parseAsync(["node", "clawmessenger", "pair", "--ticket", "pt_same"]);
    expect(seenIdemKeys[1]).not.toBe(machineBKey);
  });
});

// 后台常驻命令面：这些动作以前只能靠随 Release 分发的守护脚本提供
// （clawmessenger-daemon.sh / .cmd），现在直接编进 CLI。
function fakeDaemon(overrides: Partial<Daemon> = {}): { daemon: Daemon; calls: string[] } {
  const calls: string[] = [];
  const daemon: Daemon = {
    start: async (o) => { calls.push(`start:${JSON.stringify(o ?? {})}`); },
    stop: async () => { calls.push("stop"); },
    restart: async (o) => { calls.push(`restart:${JSON.stringify(o ?? {})}`); },
    status: () => ({ platform: "linux", running: false, stale: false, agents: [], logs: [], serviceName: "clawmessenger" }),
    logs: (o) => { calls.push(`logs:${o?.lines ?? 40}`); },
    installService: async (o) => { calls.push(`install-service:${JSON.stringify(o ?? {})}`); },
    uninstall: async () => { calls.push("uninstall"); },
    ...overrides,
  };
  return { daemon, calls };
}

describe("clawmessenger status", () => {
  it("prints stored identity", async () => {
    const keystore = tempKeystore();
    keystore.save({ nodeId: "node_9", token: "t", serverUrl: "http://srv" });
    const lines: string[] = [];
    const program = buildProgram({
      keystore,
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: { write: (s: string) => { lines.push(s); return true; } } as unknown as NodeJS.WriteStream,
      daemon: fakeDaemon().daemon,
      daemonPaths: defaultDaemonPaths(join(tmpdir(), "clawmessenger-daemon-unused")),
    });
    await program.parseAsync(["node", "clawmessenger", "status"]);
    expect(lines.join("")).toContain("node_9");
  });

  it("appends whether the supervisor is running in the background", async () => {
    const keystore = tempKeystore();
    keystore.save({ nodeId: "node_9", token: "t", serverUrl: "http://srv" });
    const lines: string[] = [];
    const program = buildProgram({
      keystore,
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: { write: (s: string) => { lines.push(s); return true; } } as unknown as NodeJS.WriteStream,
      daemon: fakeDaemon({
        status: () => ({
          platform: "linux",
          running: true,
          stale: false,
          pid: 4242,
          uptimeMs: 65_000,
          agents: ["claude", "codex"],
          logs: ["/home/u/.clawmessenger/logs/supervisor.log"],
          serviceName: "clawmessenger",
        }),
      }).daemon,
      daemonPaths: defaultDaemonPaths(join(tmpdir(), "clawmessenger-daemon-unused")),
    });
    await program.parseAsync(["node", "clawmessenger", "status"]);
    const text = lines.join("");
    expect(text).toContain("node_9");
    expect(text).toContain("daemon : running in the background (PID 4242, up 1m05s)");
    expect(text).toContain("2 (claude, codex)");
    expect(text).toContain("supervisor.log");
  });
});

describe("clawmessenger background commands", () => {
  it("start passes --model and --workdir through", async () => {
    const { daemon, calls } = fakeDaemon();
    const program = buildProgram({
      keystore: tempKeystore(),
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: process.stdout,
      daemon,
    });
    await program.parseAsync(["node", "clawmessenger", "start", "--model", "gpt-5", "--workdir", "/srv/work"]);
    expect(calls).toEqual(['start:{"model":"gpt-5","workdir":"/srv/work"}']);
  });

  it("stop and restart reach the daemon", async () => {
    const { daemon, calls } = fakeDaemon();
    const program = buildProgram({
      keystore: tempKeystore(),
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: process.stdout,
      daemon,
    });
    await program.parseAsync(["node", "clawmessenger", "stop"]);
    await program.parseAsync(["node", "clawmessenger", "restart"]);
    expect(calls).toEqual(["stop", "restart:{}"]);
  });

  it("logs honours --lines and defaults to 40", async () => {
    const { daemon, calls } = fakeDaemon();
    const program = buildProgram({
      keystore: tempKeystore(),
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: process.stdout,
      daemon,
    });
    await program.parseAsync(["node", "clawmessenger", "logs", "--lines", "5"]);
    await program.parseAsync(["node", "clawmessenger", "logs"]);
    await program.parseAsync(["node", "clawmessenger", "logs", "--lines", "not-a-number"]);
    expect(calls).toEqual(["logs:5", "logs:40", "logs:40"]);
  });

  it("install-service and uninstall reach the daemon", async () => {
    const { daemon, calls } = fakeDaemon();
    const program = buildProgram({
      keystore: tempKeystore(),
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: process.stdout,
      daemon,
    });
    await program.parseAsync(["node", "clawmessenger", "install-service"]);
    await program.parseAsync(["node", "clawmessenger", "uninstall"]);
    expect(calls).toEqual(["install-service:{}", "uninstall"]);
  });

  it("the `daemon` namespace dispatches every action", async () => {
    const { daemon, calls } = fakeDaemon();
    const lines: string[] = [];
    const program = buildProgram({
      keystore: tempKeystore(),
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: { write: (s: string) => { lines.push(s); return true; } } as unknown as NodeJS.WriteStream,
      daemon,
      daemonPaths: defaultDaemonPaths(join(tmpdir(), "clawmessenger-daemon-unused")),
    });
    for (const action of ["start", "stop", "restart", "logs", "install-service", "uninstall"]) {
      await program.parseAsync(["node", "clawmessenger", "daemon", action]);
    }
    await program.parseAsync(["node", "clawmessenger", "daemon", "status"]);
    expect(calls).toEqual(["start:{}", "stop", "restart:{}", "logs:40", "install-service:{}", "uninstall"]);
    expect(lines.join("")).toContain("not running in the background");
  });

  it("rejects an unknown daemon action instead of silently doing nothing", async () => {
    const { daemon, calls } = fakeDaemon();
    const program = buildProgram({
      keystore: tempKeystore(),
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: process.stdout,
      daemon,
    });
    await expect(program.parseAsync(["node", "clawmessenger", "daemon", "explode"])).rejects.toThrow(
      /unknown daemon action 'explode'/,
    );
    expect(calls).toEqual([]);
  });
});

describe("clawmessenger run (supervisor mode)", () => {
  // 回归护栏：supervisor 会被 start 之外的入口直接拉起（开机自启 .vbs、守护脚本的
  // -Action run、手敲 `clawmessenger run`），那些入口绕过 start 的检查。两份
  // supervisor 派生同样的融云节点 id —— 互相踢线 + 同一条私聊被各答一次。
  it("refuses to start a second supervisor while one is already running", async () => {
    const keystore = tempKeystore();
    keystore.save({ nodeId: "node_9", token: "t", serverUrl: "http://srv" });
    const stateDir = mkdtempSync(join(tmpdir(), "clawmessenger-supervisor-guard-"));
    dirs.push(stateDir);
    const paths = defaultDaemonPaths(stateDir);
    // 需要一个「确实活着、但不是本进程」的 pid，否则会命中「pid 就是我自己」的放行分支。
    const sleeper = spawn(process.execPath, ["-e", "setInterval(() => {}, 1e9)"], { stdio: "ignore" });
    const previousExitCode = process.exitCode;
    try {
      await once(sleeper, "spawn");
      const pid = sleeper.pid as number;
      writeFileSync(paths.pidFile, `${pid}\n`);
      writeFileSync(
        paths.stateFile,
        JSON.stringify({
          pid,
          startedAt: 1,
          execPath: "/opt/clawmessenger/clawmessenger",
          cwd: "/home/u",
          agents: ["opencode", "ops"],
        }),
      );

      const lines: string[] = [];
      const program = buildProgram({
        keystore,
        apiFactory: () => ({}) as unknown as XiachatApi,
        stdout: { write: (s: string) => { lines.push(s); return true; } } as unknown as NodeJS.WriteStream,
        daemonPaths: paths,
      });
      await program.parseAsync(["node", "clawmessenger", "run"]);

      const text = lines.join("");
      expect(text).toContain(`another clawmessenger supervisor is already running (PID ${pid}`);
      expect(text).toContain("opencode, ops");
      expect(text).toContain("clawmessenger stop");
      expect(text).toContain("CLAWMESSENGER_ALLOW_DUPLICATE_SUPERVISOR=1");
      expect(process.exitCode).toBe(1);
    } finally {
      process.exitCode = previousExitCode;
      sleeper.kill();
    }
  });
});

describe("allowDuplicateSupervisor", () => {
  it("is off unless the operator asks for it", () => {
    expect(allowDuplicateSupervisor({})).toBe(false);
    for (const v of ["0", "false", "no", "", "  ", "maybe"]) {
      expect(allowDuplicateSupervisor({ CLAWMESSENGER_ALLOW_DUPLICATE_SUPERVISOR: v })).toBe(false);
    }
  });

  it("accepts the documented truthy spellings, whitespace and case included", () => {
    for (const v of ["1", "true", "TRUE", "yes", "on", " on "]) {
      expect(allowDuplicateSupervisor({ CLAWMESSENGER_ALLOW_DUPLICATE_SUPERVISOR: v })).toBe(true);
    }
  });
});

describe("clawmessenger agent", () => {
  const credsKeystore = () => {
    const keystore = tempKeystore();
    keystore.save({
      nodeId: "node_machine",
      token: "t",
      credentialId: "dc",
      deviceSecret: "sec",
      serverUrl: "http://unused.local",
    });
    return keystore;
  };

  function setScope(value: string | undefined): () => void {
    const previousScope = process.env[OPS_SCOPE_ENV];
    const previousCreds = process.env.CLAWMESSENGER_AGENT_CREDS;
    if (value === undefined) delete process.env[OPS_SCOPE_ENV];
    else process.env[OPS_SCOPE_ENV] = value;
    delete process.env.CLAWMESSENGER_AGENT_CREDS;
    return () => {
      if (previousScope === undefined) delete process.env[OPS_SCOPE_ENV];
      else process.env[OPS_SCOPE_ENV] = previousScope;
      if (previousCreds === undefined) delete process.env.CLAWMESSENGER_AGENT_CREDS;
      else process.env.CLAWMESSENGER_AGENT_CREDS = previousCreds;
    };
  }

  it("defaults to the scoped agent and forwards the action", async () => {
    const restore = setScope("opencode_330076275");
    try {
      const lines: string[] = [];
      const setDeviceControl = vi.fn(async () => ({
        agent: "opencode",
        nodeId: "node_open",
        desiredState: "running" as const,
        version: 2,
        restart: true,
      }));
      const program = buildProgram({
        keystore: credsKeystore(),
        apiFactory: () => ({ setDeviceControl } as unknown as XiachatApi),
        stdout: { write: (s: string) => { lines.push(s); return true; } } as unknown as NodeJS.WriteStream,
        daemonPaths: defaultDaemonPaths(),
      });

      await program.parseAsync(["node", "clawmessenger", "agent", "restart"]);

      expect(setDeviceControl).toHaveBeenCalledWith("node_machine", "dc", "sec", "opencode", "restart");
      expect(lines.join("")).toContain("重启指令已下发");
    } finally {
      restore();
    }
  });

  it("refuses another account's agent without calling the server", async () => {
    const restore = setScope("opencode_330076275");
    const previousExitCode = process.exitCode;
    try {
      const lines: string[] = [];
      const setDeviceControl = vi.fn();
      const program = buildProgram({
        keystore: credsKeystore(),
        apiFactory: () => ({ setDeviceControl } as unknown as XiachatApi),
        stdout: { write: (s: string) => { lines.push(s); return true; } } as unknown as NodeJS.WriteStream,
        daemonPaths: defaultDaemonPaths(),
      });

      await program.parseAsync(["node", "clawmessenger", "agent", "stop", "hermes"]);

      expect(setDeviceControl).not.toHaveBeenCalled();
      expect(lines.join("")).toContain("只负责 opencode");
      expect(process.exitCode).toBe(1);
    } finally {
      process.exitCode = previousExitCode;
      restore();
    }
  });
});

describe("ops scope guard", () => {
  it("leaves whole-machine actions alone outside an ops scope", () => {
    expect(wholeMachineActionRejection("stop", {})).toBeNull();
    expect(wholeMachineActionRejection("restart", {})).toBeNull();
  });

  it("refuses to stop or restart the whole machine from a per-agent ops child", () => {
    const env = { [OPS_SCOPE_ENV]: "opencode_330076275" };
    const stop = wholeMachineActionRejection("stop", env) ?? "";
    expect(stop).toContain("只负责本账号");
    expect(stop).toContain("clawmessenger start");
    expect(wholeMachineActionRejection("restart", env) ?? "").toContain("--all-agents");
  });

  it("hides other agents from status inside an ops scope", () => {
    const report = {
      agents: ["opencode", "hermes", "openclaw", "ops@opencode_330076275", "ops@hermes_111"],
    };
    expect(scopeDaemonReport(report, "opencode_330076275").agents)
      .toEqual(["opencode", "ops@opencode_330076275"]);
    expect(scopeDaemonReport(report, "").agents).toEqual(report.agents);
  });

  it("blocks `clawmessenger stop` before it reaches the daemon", async () => {
    const previous = process.env[OPS_SCOPE_ENV];
    const previousExitCode = process.exitCode;
    process.env[OPS_SCOPE_ENV] = "opencode_330076275";
    try {
      const lines: string[] = [];
      const stopDaemon = vi.fn(async () => {});
      const program = buildProgram({
        keystore: tempKeystore(),
        apiFactory: () => ({}) as unknown as XiachatApi,
        stdout: { write: (s: string) => { lines.push(s); return true; } } as unknown as NodeJS.WriteStream,
        daemon: { stop: stopDaemon } as unknown as Daemon,
        daemonPaths: defaultDaemonPaths(),
      });

      await program.parseAsync(["node", "clawmessenger", "stop"]);

      expect(stopDaemon).not.toHaveBeenCalled();
      expect(lines.join("")).toContain("只负责本账号");
      expect(process.exitCode).toBe(1);
    } finally {
      process.exitCode = previousExitCode;
      if (previous === undefined) delete process.env[OPS_SCOPE_ENV];
      else process.env[OPS_SCOPE_ENV] = previous;
    }
  });
});

// 2026-10-20 11:33 的现场：运维助手在会话里执行 `clawmessenger stop`，把拉起它的
// supervisor 连同自己一起杀掉 —— 失败/取消终帧没机会发，前端气泡永远停在
// 「正在思考 3m42s」，整台设备的智能体也全部下线。
// 这条守卫与 ops 作用域无关：**任何**由 supervisor 拉起的子进程都不许停整机。
describe("supervised self-kill guard", () => {
  it("leaves the commands alone outside a supervised child", () => {
    expect(supervisedSelfKillRejection("stop", {})).toBeNull();
    expect(supervisedSelfKillRejection("restart", {})).toBeNull();
  });

  it("refuses whole-machine stop/restart inside a supervised child", () => {
    const env = { [SUPERVISED_ENV]: "1" };
    const stop = supervisedSelfKillRejection("stop", env) ?? "";
    expect(stop).toContain("supervisor");
    expect(stop).toContain("clawmessenger agent stop|start");
    expect(supervisedSelfKillRejection("restart", env) ?? "").toContain("supervisor");
  });

  it("blocks `clawmessenger stop` before it reaches the daemon", async () => {
    const previous = process.env[SUPERVISED_ENV];
    const previousExitCode = process.exitCode;
    process.env[SUPERVISED_ENV] = "1";
    try {
      const lines: string[] = [];
      const stopDaemon = vi.fn(async () => {});
      const program = buildProgram({
        keystore: tempKeystore(),
        apiFactory: () => ({}) as unknown as XiachatApi,
        stdout: { write: (s: string) => { lines.push(s); return true; } } as unknown as NodeJS.WriteStream,
        daemon: { stop: stopDaemon } as unknown as Daemon,
        daemonPaths: defaultDaemonPaths(),
      });

      await program.parseAsync(["node", "clawmessenger", "stop"]);

      expect(stopDaemon).not.toHaveBeenCalled();
      expect(lines.join("")).toContain("supervisor");
      expect(process.exitCode).toBe(1);
    } finally {
      process.exitCode = previousExitCode;
      if (previous === undefined) delete process.env[SUPERVISED_ENV];
      else process.env[SUPERVISED_ENV] = previous;
    }
  });

  it("blocks whole-machine restart even with --all-agents", async () => {
    const previous = process.env[SUPERVISED_ENV];
    const previousExitCode = process.exitCode;
    process.env[SUPERVISED_ENV] = "1";
    try {
      const lines: string[] = [];
      const restartDaemon = vi.fn(async () => {});
      const program = buildProgram({
        keystore: tempKeystore(),
        apiFactory: () => ({}) as unknown as XiachatApi,
        stdout: { write: (s: string) => { lines.push(s); return true; } } as unknown as NodeJS.WriteStream,
        daemon: { restart: restartDaemon } as unknown as Daemon,
        daemonPaths: defaultDaemonPaths(),
      });

      await program.parseAsync(["node", "clawmessenger", "restart", "--all-agents"]);

      expect(restartDaemon).not.toHaveBeenCalled();
      expect(lines.join("")).toContain("supervisor");
      expect(process.exitCode).toBe(1);
    } finally {
      process.exitCode = previousExitCode;
      if (previous === undefined) delete process.env[SUPERVISED_ENV];
      else process.env[SUPERVISED_ENV] = previous;
    }
  });
});
