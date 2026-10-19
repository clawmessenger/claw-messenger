import { describe, expect, it } from "vitest";

import {
  ACTIVITY_MAX_STEPS,
  classifyTool,
  createActivityTracker,
  summarizeToolDetail,
} from "./agent-activity.js";

const line = (value: unknown): string => JSON.stringify(value);

// claude stream-json 的 content_block_start（工具开始）
const claudeToolStart = (index: number, id: string, name: string, input: unknown): string =>
  line({ type: "stream_event", event: { type: "content_block_start", index,
    content_block: { type: "tool_use", id, name, input } } });

const claudeBlockStop = (index: number): string =>
  line({ type: "stream_event", event: { type: "content_block_stop", index } });

// codex exec --json 的 item 事件
const codexItem = (state: "started" | "completed", item: Record<string, unknown>): string =>
  line({ type: `item.${state}`, item });

describe("classifyTool", () => {
  it("把已知工具映射成中文名与类别", () => {
    expect(classifyTool("WebSearch")).toEqual({ kind: "search", label: "联网搜索" });
    expect(classifyTool("web_search")).toEqual({ kind: "search", label: "联网搜索" });
    expect(classifyTool("Bash")).toEqual({ kind: "command", label: "执行命令" });
    expect(classifyTool("Read")).toEqual({ kind: "file", label: "读取文件" });
    expect(classifyTool("command_execution")).toEqual({ kind: "command", label: "执行命令" });
  });

  it("未登记的工具按原名当作普通工具", () => {
    expect(classifyTool("LinearSearch")).toEqual({ kind: "tool", label: "LinearSearch" });
    expect(classifyTool("   ")).toEqual({ kind: "tool", label: "工具" });
  });
});

describe("summarizeToolDetail", () => {
  it("优先挑能说明在干什么的字段", () => {
    expect(summarizeToolDetail({ command: "ls -la", other: 1 })).toBe("ls -la");
    expect(summarizeToolDetail({ query: "2026 年 AI 新闻" })).toBe("2026 年 AI 新闻");
    expect(summarizeToolDetail({ file_path: "src/a.ts" })).toBe("src/a.ts");
  });

  it("codex 的 file_change 从 changes[].path 取路径", () => {
    expect(summarizeToolDetail({ changes: [{ path: "a.ts" }, { path: "b.ts" }] })).toBe("a.ts、b.ts");
  });

  it("超长摘要会被裁剪成单行", () => {
    const detail = summarizeToolDetail({ query: "x".repeat(500) });
    expect(detail?.length).toBe(120);
    expect(detail?.endsWith("…")).toBe(true);
  });

  it("取不到像样的字符串就返回 undefined", () => {
    expect(summarizeToolDetail({ n: 1 })).toBeUndefined();
    expect(summarizeToolDetail(undefined)).toBeUndefined();
    expect(summarizeToolDetail("   ")).toBeUndefined();
  });
});

describe("createActivityTracker · claude", () => {
  it("content_block_start 起步、content_block_stop 收尾", () => {
    const tracker = createActivityTracker();
    expect(tracker.push(claudeToolStart(1, "toolu_1", "WebSearch", { query: "虾说 讨论组" }))).toBe(true);
    expect(tracker.snapshot()).toMatchObject([{
      id: "toolu_1", kind: "search", label: "联网搜索", detail: "虾说 讨论组", status: "running",
    }]);
    expect(tracker.push(claudeBlockStop(1))).toBe(true);
    expect(tracker.snapshot()[0]?.status).toBe("done");
  });

  it("assistant 整段消息的 tool_use 带完整 input", () => {
    const tracker = createActivityTracker();
    tracker.push(line({
      type: "assistant",
      message: { content: [{ type: "tool_use", id: "t2", name: "Bash", input: { command: "git status" } }] },
    }));
    expect(tracker.snapshot()).toMatchObject([{
      id: "t2", kind: "command", label: "执行命令", detail: "git status", status: "running",
    }]);
  });

  it("tool_result 回灌标完成，is_error 标失败", () => {
    const tracker = createActivityTracker();
    tracker.push(claudeToolStart(0, "tok", "Read", { file_path: "x.md" }));
    tracker.push(line({ type: "user", message: { content: [{ type: "tool_result", tool_use_id: "tok", is_error: true }] } }));
    expect(tracker.snapshot()[0]?.status).toBe("failed");
    expect(tracker.snapshot()[0]?.detail).toBe("x.md");
  });

  it("result 事件把悬着的 running 收尾", () => {
    const tracker = createActivityTracker();
    tracker.push(claudeToolStart(0, "tok", "Glob", { pattern: "*.ts" }));
    expect(tracker.push(line({ type: "result", result: "ok" }))).toBe(true);
    expect(tracker.snapshot()[0]?.status).toBe("done");
    // 已经是终态后再收尾不再报变化，避免多发一帧。
    expect(tracker.finish()).toBe(false);
  });
});

describe("createActivityTracker · codex", () => {
  it("item.started → running，item.completed → done", () => {
    const tracker = createActivityTracker();
    tracker.push(codexItem("started", { id: "i1", type: "command_execution", command: "npm test" }));
    expect(tracker.snapshot()[0]).toMatchObject({ id: "i1", label: "执行命令", status: "running", detail: "npm test" });
    expect(tracker.push(codexItem("completed", { id: "i1", type: "command_execution", command: "npm test" }))).toBe(true);
    expect(tracker.snapshot()[0]?.status).toBe("done");
  });

  it("web_search 归类为联网搜索", () => {
    const tracker = createActivityTracker();
    tracker.push(codexItem("completed", { id: "i2", type: "web_search", query: "openclaw 版本" }));
    expect(tracker.snapshot()[0]).toMatchObject({ kind: "search", label: "联网搜索", detail: "openclaw 版本", status: "done" });
  });

  it("reasoning / agent_message 不是工具，不产生步骤", () => {
    const tracker = createActivityTracker();
    expect(tracker.push(codexItem("completed", { id: "r1", type: "reasoning", text: "想想" }))).toBe(false);
    expect(tracker.push(codexItem("completed", { id: "m1", type: "agent_message", text: "答案" }))).toBe(false);
    expect(tracker.snapshot()).toEqual([]);
  });

  it("turn.completed 收尾所有进行中的步骤", () => {
    const tracker = createActivityTracker();
    tracker.push(codexItem("started", { id: "i3", type: "mcp_tool_call", tool: "LinearSearch" }));
    expect(tracker.push(line({ type: "turn.completed", usage: {} }))).toBe(true);
    expect(tracker.snapshot()[0]).toMatchObject({ status: "done", label: "LinearSearch" });
  });
});

describe("createActivityTracker · 健壮性", () => {
  it("非 JSON 行与噪声行不产生任何步骤", () => {
    const tracker = createActivityTracker();
    for (const noise of ["", "not json", "WARNING: something", "{broken", "[1,2]"]) {
      expect(tracker.push(noise)).toBe(false);
    }
    expect(tracker.snapshot()).toEqual([]);
  });

  it("未知事件类型被忽略", () => {
    const tracker = createActivityTracker();
    expect(tracker.push(line({ type: "system", subtype: "init" }))).toBe(false);
    expect(tracker.push(line({ type: "stream_event", event: { type: "content_block_delta", delta: { type: "text_delta", text: "hi" } } }))).toBe(false);
    expect(tracker.snapshot()).toEqual([]);
  });

  it("步骤数封顶，不把长任务刷爆帧", () => {
    const tracker = createActivityTracker();
    for (let i = 0; i < ACTIVITY_MAX_STEPS + 5; i += 1) {
      tracker.push(claudeToolStart(i, `id-${i}`, "Bash", { command: `cmd ${i}` }));
    }
    expect(tracker.snapshot()).toHaveLength(ACTIVITY_MAX_STEPS);
  });

  it("快照是拷贝，外部改动不影响内部状态", () => {
    const tracker = createActivityTracker();
    tracker.push(claudeToolStart(0, "a", "Read", { file_path: "x" }));
    const snapshot = tracker.snapshot();
    snapshot[0]!.status = "failed";
    expect(tracker.snapshot()[0]?.status).toBe("running");
  });

  // opencode `run --format json` 的真实 tool_use 事件（1.19 实测抓取）。
  describe("opencode tool_use", () => {
    const opencodeTool = (part: Record<string, unknown>): string =>
      line({ type: "tool_use", timestamp: 1, part: { type: "tool", ...part } });

    it("按 callID 落一步，命令与状态都取到", () => {
      const tracker = createActivityTracker();
      expect(tracker.push(opencodeTool({
        tool: "bash",
        callID: "call_1",
        state: { status: "completed", input: { command: "ls" }, title: "ls" },
      }))).toBe(true);
      expect(tracker.snapshot()).toMatchObject([
        { id: "call_1", kind: "command", label: "执行命令", detail: "ls", status: "done" },
      ]);
    });

    it("先 running 后 completed 只更新同一步，不新增步骤", () => {
      const tracker = createActivityTracker();
      tracker.push(opencodeTool({
        tool: "bash", callID: "call_2", state: { status: "running", input: { command: "npm test" } },
      }));
      expect(tracker.snapshot()[0]?.status).toBe("running");
      tracker.push(opencodeTool({
        tool: "bash", callID: "call_2", state: { status: "completed", input: { command: "npm test" } },
      }));
      const steps = tracker.snapshot();
      expect(steps).toHaveLength(1);
      expect(steps[0]?.status).toBe("done");
    });

    it("error 状态记成失败；读文件走 file 类别", () => {
      const tracker = createActivityTracker();
      tracker.push(opencodeTool({
        tool: "read", callID: "call_3", state: { status: "error", input: { filePath: "/tmp/a.txt" } },
      }));
      expect(tracker.snapshot()[0]).toMatchObject({
        kind: "file", label: "读取文件", detail: "/tmp/a.txt", status: "failed",
      });
    });

    it("step_finish 把仍 running 的步骤收尾", () => {
      const tracker = createActivityTracker();
      tracker.push(opencodeTool({
        tool: "grep", callID: "call_4", state: { status: "running", input: { pattern: "TODO" } },
      }));
      expect(tracker.push(line({ type: "step_finish", part: { type: "step-finish", reason: "stop" } }))).toBe(true);
      expect(tracker.snapshot()[0]?.status).toBe("done");
    });

    it("没有 callID 时用 part.id 兜底，纯文本 part 不算工具", () => {
      const tracker = createActivityTracker();
      expect(tracker.push(line({ type: "text", part: { id: "p1", type: "text", text: "hi" } }))).toBe(false);
      expect(tracker.push(opencodeTool({
        id: "prt_9", tool: "webfetch", state: { status: "completed", input: { url: "https://x.dev" } },
      }))).toBe(true);
      expect(tracker.snapshot()[0]).toMatchObject({ id: "prt_9", kind: "fetch", detail: "https://x.dev" });
    });
  });

  describe("步骤时间戳（web 展示每步耗时用）", () => {
    it("首见打 startedAt，进终态才打 endedAt", () => {
      const tracker = createActivityTracker();
      tracker.push(claudeToolStart(0, "t1", "Bash", { command: "ls" }));
      const running = tracker.snapshot()[0]!;
      expect(typeof running.startedAt).toBe("number");
      expect(running.endedAt).toBeUndefined();

      tracker.push(claudeBlockStop(0));
      const done = tracker.snapshot()[0]!;
      expect(done.startedAt).toBe(running.startedAt);
      expect(typeof done.endedAt).toBe("number");
      expect(done.endedAt!).toBeGreaterThanOrEqual(done.startedAt!);
    });

    it("同一步重复上报不刷新 startedAt（否则每步耗时恒为 0）", () => {
      const tracker = createActivityTracker();
      tracker.push(claudeToolStart(0, "t1", "Bash", { command: "ls" }));
      const first = tracker.snapshot()[0]!.startedAt;
      tracker.push(line({ type: "assistant", message: { content: [
        { type: "tool_use", id: "t1", name: "Bash", input: { command: "ls -la" } },
      ] } }));
      expect(tracker.snapshot()[0]!.startedAt).toBe(first);
    });

    it("被 finish 收尾的 running 步也有 endedAt", () => {
      const tracker = createActivityTracker();
      tracker.push(claudeToolStart(0, "t1", "Bash", { command: "ls" }));
      tracker.push(line({ type: "result", result: "ok" }));
      const step = tracker.snapshot()[0]!;
      expect(step.status).toBe("done");
      expect(typeof step.endedAt).toBe("number");
    });
  });
  describe("hermes（chat -q --format stream-json）", () => {
    it("tool_use 建步骤、tool_result 收尾，工具名映射成中文", () => {
      const tracker = createActivityTracker();
      expect(tracker.push(line({ type: "tool_use", name: "web_search",
        input: { query: "今天上海天气" }, tool_call_id: "call_1" }))).toBe(true);
      expect(tracker.snapshot()).toMatchObject([
        { id: "call_1", kind: "search", label: "联网搜索", detail: "今天上海天气", status: "running" },
      ]);
      expect(tracker.push(line({ type: "tool_result", name: "web_search", tool_call_id: "call_1" }))).toBe(true);
      expect(tracker.snapshot()[0]).toMatchObject({ status: "done" });
      expect(typeof tracker.snapshot()[0]!.endedAt).toBe("number");
    });

    it("terminal 映射成「执行命令」；is_error 的 result 标失败", () => {
      const tracker = createActivityTracker();
      tracker.push(line({ type: "tool_use", name: "terminal", input: { command: "ls -la" } }));
      expect(tracker.snapshot()[0]).toMatchObject({ kind: "command", label: "执行命令", detail: "ls -la" });
      tracker.push(line({ type: "tool_result", name: "terminal", is_error: true }));
      expect(tracker.snapshot()[0]).toMatchObject({ status: "failed" });
    });

    it("result 事件把悬着的步骤收尾", () => {
      const tracker = createActivityTracker();
      tracker.push(line({ type: "tool_use", name: "read_file", input: { path: "a.md" }, tool_call_id: "c9" }));
      tracker.push(line({ type: "result", exit_code: 0, text: "ok" }));
      expect(tracker.snapshot()[0]).toMatchObject({ status: "done" });
    });
  });
});
