// @vitest-environment node
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { defaultUserModelsPath, FileUserModelStore, MemoryUserModelStore } from "./user-models.js";

describe("defaultUserModelsPath", () => {
  it("separates the file per node so supervisor children never clobber each other", () => {
    const a = defaultUserModelsPath("opencode_1234567890");
    const b = defaultUserModelsPath("codex_1234567890");
    expect(a).not.toBe(b);
    expect(a.endsWith("user-models-opencode_1234567890.json")).toBe(true);
  });

  it("sanitises ids that would otherwise change the path", () => {
    expect(defaultUserModelsPath("a/../b")).toBe(defaultUserModelsPath("a_.._b"));
    expect(defaultUserModelsPath("../../etc/passwd")).not.toContain("/etc/");
  });

  it("falls back to an unsuffixed name without a node id", () => {
    expect(defaultUserModelsPath()).toContain("user-models.json");
  });
});

describe("FileUserModelStore", () => {
  let dir = "";
  let path = "";

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "cm-user-models-"));
    path = join(dir, "user-models-node_1.json");
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it("round-trips a preference through disk", () => {
    const store = new FileUserModelStore(path);
    store.set("user-1", "quukk/glm-5.3");

    // 新实例 = 重启后的进程：偏好必须还在，否则用户会以为「我设的模型自己变了」。
    const reopened = new FileUserModelStore(path);
    expect(reopened.get("user-1")).toBe("quukk/glm-5.3");
  });

  it("keeps one user's choice away from another's", () => {
    const store = new FileUserModelStore(path);
    store.set("user-1", "quukk/glm-5.3");
    store.set("user-2", "quukk/claude-sonnet-4");
    expect(store.get("user-1")).toBe("quukk/glm-5.3");
    expect(store.get("user-2")).toBe("quukk/claude-sonnet-4");
  });

  it("clears a preference and falls back to the node default", () => {
    const store = new FileUserModelStore(path);
    store.set("user-1", "quukk/glm-5.3");
    store.set("user-1", null);
    expect(store.get("user-1")).toBeUndefined();
    expect(new FileUserModelStore(path).get("user-1")).toBeUndefined();
  });

  it("treats a blank route as a clear rather than storing an empty model", () => {
    const store = new FileUserModelStore(path);
    store.set("user-1", "quukk/glm-5.3");
    store.set("user-1", "   ");
    expect(store.get("user-1")).toBeUndefined();
  });

  it("survives a truncated file instead of failing every later switch", () => {
    // 进程被杀在写盘中途会留下半截 JSON。读不回来只能当空表，绝不能抛 ——
    // 抛了就是「文件坏过一次，这台节点以后每次切模型都报错」。
    writeFileSync(path, '{"user-1": "quukk/glm');
    const store = new FileUserModelStore(path);
    expect(store.get("user-1")).toBeUndefined();
    store.set("user-1", "quukk/glm-5.3");
    expect(JSON.parse(readFileSync(path, "utf8"))).toEqual({ "user-1": "quukk/glm-5.3" });
  });

  it("drops non-string junk from a hand-edited file", () => {
    writeFileSync(path, JSON.stringify({ "user-1": { route: "x" }, "user-2": "quukk/glm-5.3", "user-3": "" }));
    const store = new FileUserModelStore(path);
    expect(store.snapshot()).toEqual({ "user-2": "quukk/glm-5.3" });
  });

  it("writes atomically enough that no .tmp file is left behind", () => {
    const store = new FileUserModelStore(path);
    store.set("user-1", "quukk/glm-5.3");
    // rename 成功时临时文件应该已经被搬走。
    expect(() => readFileSync(`${path}.tmp`, "utf8")).toThrow();
  });

  it("does not throw when the preference cannot be persisted", () => {
    // 用「同名文件占住目录位」制造 ENOTDIR：mkdir 必然失败，等价于只读 home。
    // 这时必须只降级（重启回退），不能把用户的这条消息打断。
    const blocker = join(dir, "blocker");
    writeFileSync(blocker, "not a directory");
    const store = new FileUserModelStore(join(blocker, "user-models.json"));
    expect(() => store.set("user-1", "quukk/glm-5.3")).not.toThrow();
    // 内存里已经生效：本轮及之后都用新模型。
    expect(store.get("user-1")).toBe("quukk/glm-5.3");
  });
});

describe("MemoryUserModelStore", () => {
  it("mirrors the file store's get/set/clear contract", () => {
    const store = new MemoryUserModelStore({ "user-9": "quukk/glm-5.3" });
    expect(store.get("user-9")).toBe("quukk/glm-5.3");
    store.set("user-9", null);
    expect(store.get("user-9")).toBeUndefined();
    store.set("user-8", "quukk/claude-sonnet-4");
    expect(store.snapshot()).toEqual({ "user-8": "quukk/claude-sonnet-4" });
  });
});
