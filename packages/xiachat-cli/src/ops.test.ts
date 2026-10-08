// @vitest-environment node
import { describe, expect, it } from "vitest";
import { readFile, rm } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { EventEmitter } from "node:events";
import { ensureOpencodeInstalled, setupOpsWorkdir } from "./ops.js";
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
    const dir = path.join(os.tmpdir(), `xiachat-ops-test-${Date.now()}`);
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

  it("rewrites AGENTS.md idempotently", async () => {
    const dir = path.join(os.tmpdir(), `xiachat-ops-test-${Date.now()}`);
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
