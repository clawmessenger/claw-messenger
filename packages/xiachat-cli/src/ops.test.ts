// @vitest-environment node
import { describe, expect, it } from "vitest";
import { readFile, rm } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { EventEmitter } from "node:events";
import { agentNameFromRcUser, ensureOpencodeInstalled, setupOpsWorkdir } from "./ops.js";
import type { AgentInfo } from "./agents.js";

function agent(pathOverride: string): AgentInfo {
  return { name: "opencode", path: pathOverride };
}

describe("ensureOpencodeInstalled", () => {
  it("returns the discovered opencode without spawning npm when already installed", async () => {
    const spawnCalls: unknown[][] = [];
    const spawnFn = (...args: unknown[]): EventEmitter => {
      spawnCalls.push(args);
      return new EventEmitter();
    };
    const result = await ensureOpencodeInstalled({
      discover: async () => [agent("/usr/local/bin/opencode")],
      spawnFn: spawnFn as unknown as typeof import("node:child_process").spawn,
    });
    expect(result.path).toBe("/usr/local/bin/opencode");
    expect(spawnCalls).toHaveLength(0);
  });

  it("installs via npm when missing and re-discovers afterwards", async () => {
    const spawnCalls: { command: string; args: string[] }[] = [];
    const emitters: EventEmitter[] = [];
    const spawnFn = (command: string, args: string[]): EventEmitter => {
      spawnCalls.push({ command, args });
      const emitter = new EventEmitter();
      emitters.push(emitter);
      return emitter;
    };
    let discoverCount = 0;
    const discover = async (): Promise<AgentInfo[]> => {
      discoverCount += 1;
      return discoverCount === 1 ? [] : [agent("C:\\tools\\opencode.cmd")];
    };
    const promise = ensureOpencodeInstalled({
      discover,
      spawnFn: spawnFn as unknown as typeof import("node:child_process").spawn,
    });
    await new Promise((resolve) => setTimeout(resolve, 0));
    emitters[0].emit("close", 0);
    const result = await promise;
    expect(result.path).toBe("C:\\tools\\opencode.cmd");
    expect(spawnCalls).toEqual([{ command: "npm", args: ["install", "-g", "opencode-ai@latest"] }]);
    expect(discoverCount).toBe(2);
  });

  it("rejects when npm install exits non-zero", async () => {
    const spawnCalls: { command: string; args: string[] }[] = [];
    const emitters: EventEmitter[] = [];
    const spawnFn = (command: string, args: string[]): EventEmitter => {
      spawnCalls.push({ command, args });
      const emitter = new EventEmitter();
      emitters.push(emitter);
      return emitter;
    };
    const promise = ensureOpencodeInstalled({
      discover: async () => [],
      spawnFn: spawnFn as unknown as typeof import("node:child_process").spawn,
    });
    await new Promise((resolve) => setTimeout(resolve, 0));
    emitters[0].emit("close", 1);
    await expect(promise).rejects.toThrow(/exited with code 1/);
  });

  it("rejects when npm succeeds but opencode is still not discoverable", async () => {
    const emitters: EventEmitter[] = [];
    const spawnFn = (): EventEmitter => {
      const emitter = new EventEmitter();
      emitters.push(emitter);
      return emitter;
    };
    const promise = ensureOpencodeInstalled({
      discover: async () => [],
      spawnFn: spawnFn as unknown as typeof import("node:child_process").spawn,
    });
    await new Promise((resolve) => setTimeout(resolve, 0));
    emitters[0].emit("close", 0);
    await expect(promise).rejects.toThrow(/still not discoverable/);
  });
});

describe("setupOpsWorkdir", () => {
  it("creates the workdir with an AGENTS.md ops system prompt", async () => {
    const dir = path.join(os.tmpdir(), `clawmessenger-ops-test-${Date.now()}`);
    try {
      const returned = await setupOpsWorkdir(dir);
      expect(returned).toBe(dir);
      const md = await readFile(path.join(dir, "AGENTS.md"), "utf8");
      expect(md).toContain("虾说运维助手");
      expect(md).toContain("opencode");
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it("scopes a per-agent ops prompt to its own account", async () => {
    const dir = path.join(os.tmpdir(), `clawmessenger-ops-test-${Date.now()}`);
    try {
      await setupOpsWorkdir(dir, { opsFor: "opencode_330076275" });
      const md = await readFile(path.join(dir, "AGENTS.md"), "utf8");
      expect(md).toContain("服务范围");
      expect(md).toContain("opencode_330076275");
      expect(md).toContain("不准操作其他智能体");
      // 整机级 stop 会连坐别的账号：提示词必须把它换成按账号的命令。
      expect(md).toContain("clawmessenger agent start|stop|restart");
      expect(md).toContain("clawmessenger stop");
      expect(md).toContain("--all-agents");
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it("keeps a machine-level ops prompt without a per-agent scope", async () => {
    const dir = path.join(os.tmpdir(), `clawmessenger-ops-test-${Date.now()}`);
    try {
      await setupOpsWorkdir(dir);
      const md = await readFile(path.join(dir, "AGENTS.md"), "utf8");
      expect(md).toContain("服务范围");
      expect(md).not.toContain("你只服务本机智能体账号");
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  // 现场（2026-10-20 13:07）：模型把 PowerShell 的 Get-Content 塞进 bash，报
  // command not found 之后一声不吭，这一轮被判「没有正常收尾」。提示词得先说清
  // 命令工具是 git-bash，并给出等价写法。
  it("tells the ops agent which shell its command tool really is", async () => {
    const dir = path.join(os.tmpdir(), `clawmessenger-ops-test-${Date.now()}`);
    try {
      await setupOpsWorkdir(dir, { opsFor: "rc_node_3300776275_openclaw", agentName: "openclaw" });
      const md = await readFile(path.join(dir, "AGENTS.md"), "utf8");
      expect(md).toContain("git-bash");
      expect(md).toContain("Get-Content");
      expect(md).toContain("$env:USERPROFILE");
      expect(md).toContain("必须先用一句话说明失败原因");
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it("rewrites AGENTS.md idempotently", async () => {
    const dir = path.join(os.tmpdir(), `clawmessenger-ops-test-${Date.now()}`);
    try {
      await setupOpsWorkdir(dir);
      await setupOpsWorkdir(dir);
      const md = await readFile(path.join(dir, "AGENTS.md"), "utf8");
      expect(md).toContain("虾说运维助手");
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });
});

describe("agentNameFromRcUser", () => {
  it("reads the agent out of the current <agent>_<machineKey> scheme", () => {
    expect(agentNameFromRcUser("opencode_330076275")).toBe("opencode");
    expect(agentNameFromRcUser("hermes_330076275")).toBe("hermes");
  });

  // 旧方案 rc_node_<machineKey>_<agent> 的节点还在服务端跑着，设备上经常新旧混着。
  // 只按新方案切尾巴会返回 rc_node_<machineKey>，拿它当 agent 名去启停就成了
  // 「服务端没有这个智能体」。
  it("reads the agent out of the legacy rc_node_<machineKey>_<agent> scheme", () => {
    expect(agentNameFromRcUser("rc_node_3300776275_hermes")).toBe("hermes");
    expect(agentNameFromRcUser("rc_node_3300776275_opencode")).toBe("opencode");
  });

  it("strips the _ops suffix before parsing", () => {
    expect(agentNameFromRcUser("rc_node_3300776275_hermes_ops")).toBe("hermes");
    expect(agentNameFromRcUser("hermes_330076275_ops")).toBe("hermes");
  });

  it("returns empty for a machine node, which has no agent segment", () => {
    expect(agentNameFromRcUser("rc_node_3300776275")).toBe("");
    expect(agentNameFromRcUser("rc_node_3300776275_ops")).toBe("");
    expect(agentNameFromRcUser("")).toBe("");
    expect(agentNameFromRcUser("   ")).toBe("");
  });
});
