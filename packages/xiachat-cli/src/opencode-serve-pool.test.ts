// @vitest-environment node
//
// opencode serve 常驻池的行为闸门。
//
// 这一组断言的动机是量化过的：以前每一轮 agent 回合都 spawn 一个
// `opencode serve`、跑完整棵杀掉，冷启动（加载派生配置 + 初始化 provider +
// 打 listening 行）实测 3-8s **每轮都付**。角色推荐这类「用户盯着转圈」的入口
// 感受最直接，20-30s 里有 1/4 是纯冷启动。
//
// 复用之后必须保证三件事，缺一件就会出比「慢」更严重的问题：
//   1. 同配置连续两轮只有一个进程（这就是提速本身）；
//   2. 配置指纹不同必须换进程 —— behaviorPrompt / fsRestricted 是在 serve 启动
//      时写进 OPENCODE_CONFIG 的，共用就会「关了文件读写还能写」；
//   3. 关掉开关（CLAWMESSENGER_OPENCODE_SERVE_REUSE=0）能回到每轮新建的老行为，
//      线上出问题时有立刻可用的退路。
//
// runServerSession 被替换掉（真跑 HTTP 会和假 serve 的固定端口打架），但
// spawn / 打印 listening 行 / 进程存活判定全部走真实实现 —— 池子逻辑正是这一层。
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("./opencode-server.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./opencode-server.js")>();
  return {
    ...actual,
    runServerSession: vi.fn(async () => ({
      text: "ok",
      sawDelta: true,
      deltaCount: 1,
      reasoningChars: 0,
      idleSeen: true,
      endedOnFailedTool: false,
    })),
  };
});

import { opencodeServeReuseEnabled, resetOpencodeServePoolForTest, runAgentTurn } from "./agents.js";

/** 假 opencode：打一行 listening、记一次启动、然后吊着不死（模拟常驻）。 */
function createFakeServe(dir: string): string {
  const isWin = process.platform === "win32";
  const jsPath = join(dir, "fake-serve.cjs");
  writeFileSync(
    jsPath,
    [
      'const fs = require("node:fs");',
      // 启动计数写进文件：spawn 发生在子进程里，父进程只能靠这个数。
      'if (process.env.POOL_SPAWN_LOG) fs.appendFileSync(process.env.POOL_SPAWN_LOG, process.pid + "\\n");',
      'process.stdout.write("opencode server listening on http://127.0.0.1:59999\\n");',
      "setTimeout(() => process.exit(0), 60000);",
    ].join("\n"),
  );
  const script = join(dir, isWin ? "opencode.cmd" : "opencode");
  if (isWin) {
    writeFileSync(script, `@echo off\r\nnode "${jsPath}"\r\n`, { mode: 0o755 });
  } else {
    writeFileSync(script, `#!/bin/sh\nexec node "${jsPath}"\n`, { mode: 0o755 });
    chmodSync(script, 0o755);
  }
  return script;
}

describe("opencode serve 常驻池", () => {
  let dir = "";
  let spawnLog = "";
  let script = "";
  const saved: Record<string, string | undefined> = {};

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "claw-serve-pool-"));
    spawnLog = join(dir, "spawn.log");
    script = createFakeServe(dir);
    for (const key of [
      "POOL_SPAWN_LOG",
      "CLAWMESSENGER_OPENCODE_FULL_CONFIG",
      "CLAWMESSENGER_OPENCODE_SERVER",
      "CLAWMESSENGER_OPENCODE_SERVE_REUSE",
    ]) {
      saved[key] = process.env[key];
    }
    process.env.POOL_SPAWN_LOG = spawnLog;
    // 别在测试里派生设备端配置（那会写 ~/.clawmessenger）。
    process.env.CLAWMESSENGER_OPENCODE_FULL_CONFIG = "1";
    delete process.env.CLAWMESSENGER_OPENCODE_SERVER;
    delete process.env.CLAWMESSENGER_OPENCODE_SERVE_REUSE;
  });

  afterEach(() => {
    resetOpencodeServePoolForTest();
    for (const [key, value] of Object.entries(saved)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
    // Windows 上 taskkill 之后文件句柄还会残留一小会儿（假 serve 的 cwd 就是
    // 这个目录），删不掉不影响断言 —— 临时目录留给系统回收。
    try {
      rmSync(dir, { recursive: true, force: true });
    } catch {
      // ignore
    }
  });

  const spawnCount = (): number =>
    existsSync(spawnLog) ? readFileSync(spawnLog, "utf8").split("\n").filter(Boolean).length : 0;

  const turn = (behaviorPrompt?: string) =>
    runAgentTurn({
      execPath: script,
      agentName: "opencode",
      prompt: "p",
      timeoutMs: 20_000,
      cwd: dir,
      ...(behaviorPrompt === undefined ? {} : { behaviorPrompt }),
    });

  it("同一份配置的连续两轮只起一个 serve 进程", async () => {
    await turn();
    await turn();
    expect(spawnCount()).toBe(1);
  });

  // 第二轮的耗时必须明显短于第一轮：第一轮含 serve 冷启动与 1.2s 的 2.x 密码
  // 窗口，第二轮一个都不用付。这条比「进程数」更贴近用户感受到的东西。
  it("第二轮不再付冷启动的时间", async () => {
    const startedFirst = Date.now();
    await turn();
    const firstMs = Date.now() - startedFirst;

    const startedSecond = Date.now();
    await turn();
    const secondMs = Date.now() - startedSecond;

    expect(secondMs).toBeLessThan(firstMs - 500);
  });

  // behaviorPrompt 与 fsRestricted 是 serve 启动时写进派生配置的，进程起来就固定。
  // 指纹不同却复用，就是「管理员刚关了文件读写，这一轮照样能写」。
  it("行为规范变了就换进程，不吃同一份派生配置", async () => {
    await turn();
    await turn("你是运维助手");
    expect(spawnCount()).toBe(2);
  });

  it("关掉复用开关就回到每轮新建的老行为", async () => {
    process.env.CLAWMESSENGER_OPENCODE_SERVE_REUSE = "0";
    await turn();
    await turn();
    expect(spawnCount()).toBe(2);
  });

  it("开关默认开，只有显式 falsy 才关", () => {
    expect(opencodeServeReuseEnabled({})).toBe(true);
    expect(opencodeServeReuseEnabled({ CLAWMESSENGER_OPENCODE_SERVE_REUSE: "1" })).toBe(true);
    expect(opencodeServeReuseEnabled({ CLAWMESSENGER_OPENCODE_SERVE_REUSE: "0" })).toBe(false);
    expect(opencodeServeReuseEnabled({ CLAWMESSENGER_OPENCODE_SERVE_REUSE: "off" })).toBe(false);
    expect(opencodeServeReuseEnabled({ CLAWMESSENGER_OPENCODE_SERVE_REUSE: "false" })).toBe(false);
  });
});
