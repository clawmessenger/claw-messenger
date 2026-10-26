// @vitest-environment node
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { afterEach, describe, expect, it } from "vitest";

import {
  applyServeEvent,
  createServeEventState,
  isServeStartupFailure,
  parseServeListeningUrl,
  runServerSession,
  toRequestModel,
} from "./opencode-server.js";

const servers: Server[] = [];

afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => new Promise<void>((r) => s.close(() => r()))));
});

describe("parseServeListeningUrl", () => {
  it("从实测日志行里取出地址", () => {
    expect(parseServeListeningUrl("opencode server listening on http://127.0.0.1:49882"))
      .toBe("http://127.0.0.1:49882");
  });

  it("容忍 ANSI 颜色码与前后噪声", () => {
    expect(parseServeListeningUrl("\u001b[32mopencode server listening on http://127.0.0.1:5123\u001b[0m"))
      .toBe("http://127.0.0.1:5123");
    expect(parseServeListeningUrl("junk\nopencode server listening on http://127.0.0.1:65535\n"))
      .toBe("http://127.0.0.1:65535");
  });

  it("没有监听行就返回 null（启动失败靠它判定，不能猜）", () => {
    expect(parseServeListeningUrl("opencode 1.19.0")).toBeNull();
    expect(parseServeListeningUrl("listening")).toBeNull();
  });
});

describe("toRequestModel", () => {
  it("把 provider/model 路由拆成请求体字段", () => {
    expect(toRequestModel("quukk/glm-5.2")).toEqual({ providerID: "quukk", modelID: "glm-5.2" });
    expect(toRequestModel("agent-plan/kimi-k2.7-code"))
      .toEqual({ providerID: "agent-plan", modelID: "kimi-k2.7-code" });
  });

  it("拒绝没有斜杠或带空白的自由文本（交给配置默认更安全）", () => {
    expect(toRequestModel(undefined)).toBeNull();
    expect(toRequestModel("glm-5.2")).toBeNull();
    expect(toRequestModel("/glm-5.2")).toBeNull();
    expect(toRequestModel("quukk/")).toBeNull();
    expect(toRequestModel("quukk/glm 5.2")).toBeNull();
  });
});

describe("applyServeEvent", () => {
  it("part.updated 建立映射后，delta 按 part 类型分流到正文与思考", () => {
    const state = createServeEventState("s1", false);
    const texts: string[] = [];
    const reasons: string[] = [];
    const sink = { onText: (d: string) => texts.push(d), onReasoning: (d: string) => reasons.push(d) };

    applyServeEvent(state, "message.part.updated", { properties: { sessionID: "s1", part: { id: "r1", type: "reasoning" } } }, sink);
    applyServeEvent(state, "message.part.updated", { properties: { sessionID: "s1", part: { id: "t1", type: "text" } } }, sink);
    // 实测 field 恒为 "text"，正文与思考只靠 partID 区分 —— 这里必须锁住。
    applyServeEvent(state, "message.part.delta", { properties: { sessionID: "s1", partID: "r1", field: "text", delta: "先想想" } }, sink);
    applyServeEvent(state, "message.part.delta", { properties: { sessionID: "s1", partID: "t1", field: "text", delta: "答案" } }, sink);

    expect(reasons).toEqual(["先想想"]);
    expect(texts).toEqual(["答案"]);
    expect(state.reasoning).toBe("先想想");
    expect(state.text).toBe("答案");
    expect(state.sawDelta).toBe(true);
    expect(state.deltaCount).toBe(2);
  });

  it("delta 早于 part.updated 时先暂存，映射就绪后按思考放行（不串进正文）", () => {
    const state = createServeEventState("s1", false);
    const texts: string[] = [];
    const reasons: string[] = [];
    const sink = { onText: (d: string) => texts.push(d), onReasoning: (d: string) => reasons.push(d) };

    applyServeEvent(state, "message.part.delta", { properties: { sessionID: "s1", partID: "r1", field: "text", delta: "思考中" } }, sink);
    expect(texts).toHaveLength(0);
    expect(reasons).toHaveLength(0);
    expect(state.text).toBe("");

    applyServeEvent(state, "message.part.updated", { properties: { sessionID: "s1", part: { id: "r1", type: "reasoning" } } }, sink);
    expect(reasons).toEqual(["思考中"]);
    expect(texts).toHaveLength(0);
  });

  it("映射迟迟不来时 fail-open 按正文放行（宁可串道也不能吞内容）", () => {
    const state = createServeEventState("s1", false);
    const texts: string[] = [];
    const sink = { onText: (d: string) => texts.push(d) };

    applyServeEvent(state, "message.part.delta", { properties: { sessionID: "s1", partID: "x1", field: "text", delta: "被卡住的字" } }, sink, 1_000);
    // 2 秒后才来下一个事件：暂存已过期，先放行旧 part。
    applyServeEvent(state, "message.part.delta", { properties: { sessionID: "s1", partID: "x2", field: "text", delta: "后来的字" } }, sink, 4_000);
    expect(texts[0]).toBe("被卡住的字");
  });

  it("忽略别的会话的事件（serve 是全机的）", () => {
    const state = createServeEventState("s1", false);
    const texts: string[] = [];
    const sink = { onText: (d: string) => texts.push(d) };
    applyServeEvent(state, "message.part.updated", { properties: { sessionID: "s2", part: { id: "t1", type: "text" } } }, sink);
    applyServeEvent(state, "message.part.delta", { properties: { sessionID: "s2", partID: "t1", field: "text", delta: "别人的" } }, sink);
    expect(texts).toHaveLength(0);
    expect(state.partTypes.size).toBe(0);
  });

  it("工具 part 走同一条 activity 通道（与 run 模式共用分类逻辑）", () => {
    const state = createServeEventState("s1", true);
    const snaps: number[] = [];
    const sink = { onActivity: (steps: unknown[]) => snaps.push(steps.length) };
    applyServeEvent(state, "message.part.updated", {
      properties: {
        sessionID: "s1",
        part: {
          id: "prt_tool",
          type: "tool",
          tool: "bash",
          callID: "c1",
          state: { status: "completed", input: { command: "ls" } },
        },
      },
    }, sink);
    expect(snaps).toEqual([1]);
    expect(state.lastToolId).toBe("c1");
    // 工具之后来的正文要标记 textAfterLastTool，否则收尾会误判成半句话。
    applyServeEvent(state, "message.part.updated", { properties: { sessionID: "s1", part: { id: "t1", type: "text" } } }, sink);
    applyServeEvent(state, "message.part.delta", { properties: { sessionID: "s1", partID: "t1", field: "text", delta: "做好了" } }, sink);
    expect(state.textAfterLastTool).toBe(true);
  });

  it("session.idle 与 session.error 被记下来", () => {
    const state = createServeEventState("s1", false);
    const sink = {};
    applyServeEvent(state, "session.idle", { properties: { sessionID: "s1" } }, sink);
    expect(state.idle).toBe(true);
    applyServeEvent(state, "session.error", { properties: { sessionID: "s1", error: { message: "boom" } } }, sink);
    expect(state.error).toBe("boom");
  });
});

// --- 端到端：真起一个 HTTP 服务模拟 opencode serve -------------------------

interface MockOptions {
  /** 每条 = 一个 part，推完 part.updated 后逐字符推 delta。 */
  parts?: Array<{ id: string; type: "text" | "reasoning"; text: string }>;
  replyText?: string;
  sessionStatus?: number;
  messageStatus?: number;
  abortCalls?: { count: number };
}

async function startMockServe(options: MockOptions): Promise<string> {
  const { parts = [], replyText = "", sessionStatus = 200, messageStatus = 200 } = options;
  let sseRes: import("node:http").ServerResponse | null = null;
  const server = createServer((req, res) => {
    const url = req.url ?? "";
    if (req.method === "POST" && url === "/session") {
      res.writeHead(sessionStatus, { "content-type": "application/json" });
      res.end(sessionStatus === 200 ? JSON.stringify({ id: "ses_test" }) : "{}");
      return;
    }
    if (req.method === "GET" && url === "/event") {
      res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
      res.write("event: server.connected\ndata: {\"type\":\"server.connected\"}\n\n");
      sseRes = res;
      return;
    }
    if (req.method === "POST" && url === "/session/ses_test/abort") {
      if (options.abortCalls) options.abortCalls.count += 1;
      res.writeHead(200, { "content-type": "application/json" });
      res.end("{}");
      return;
    }
    if (req.method === "POST" && url === "/session/ses_test/message") {
      req.resume();
      req.on("end", () => {
        if (messageStatus !== 200) {
          res.writeHead(messageStatus, { "content-type": "application/json" });
          res.end(JSON.stringify({ message: "upstream boom" }));
          return;
        }
        // 像真 serve 一样：先建 part、再逐字推 delta、最后 idle。
        for (const part of parts) {
          sseRes?.write(`event: message.part.updated\ndata: ${JSON.stringify({ properties: { sessionID: "ses_test", part: { id: part.id, type: part.type } } })}\n\n`);
          for (const ch of part.text) {
            sseRes?.write(`event: message.part.delta\ndata: ${JSON.stringify({ properties: { sessionID: "ses_test", partID: part.id, field: "text", delta: ch } })}\n\n`);
          }
        }
        sseRes?.write(`event: session.idle\ndata: ${JSON.stringify({ properties: { sessionID: "ses_test" } })}\n\n`);
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ parts: [{ id: "t1", type: "text", text: replyText }] }));
      });
      return;
    }
    res.writeHead(404, { "content-type": "application/json" });
    res.end("{}");
  });
  servers.push(server);
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
  const { port } = server.address() as AddressInfo;
  return `http://127.0.0.1:${port}`;
}

describe("runServerSession", () => {
  it("逐 delta 上屏，正文以 POST 返回为准，并报告 sawDelta", async () => {
    const baseUrl = await startMockServe({
      parts: [
        { id: "r1", type: "reasoning", text: "想一想" },
        { id: "t1", type: "text", text: "长江很长。" },
      ],
      replyText: "长江很长。",
    });
    const texts: string[] = [];
    const reasons: string[] = [];
    const result = await runServerSession({
      baseUrl,
      prompt: "说说长江",
      timeoutMs: 10_000,
      onText: (d) => texts.push(d),
      onReasoning: (d) => reasons.push(d),
    });

    expect(result.text).toBe("长江很长。");
    expect(result.sawDelta).toBe(true);
    // 逐字：不是「整段一次」
    expect(texts.join("")).toBe("长江很长。");
    expect(texts.length).toBe("长江很长。".length);
    expect(reasons.join("")).toBe("想一想");
    expect(result.reasoningChars).toBe(3);
    expect(result.idleSeen).toBe(true);
  });

  it("服务端拒绝（非 200）时抛错并带上原因", async () => {
    const baseUrl = await startMockServe({ messageStatus: 500 });
    await expect(runServerSession({ baseUrl, prompt: "p", timeoutMs: 10_000 }))
      .rejects.toThrow(/HTTP 500/);
  });

  it("正文超过硬上限时按截断失败（truncated=true）", async () => {
    const long = "字".repeat(200);
    const baseUrl = await startMockServe({ parts: [{ id: "t1", type: "text", text: long }], replyText: long });
    await expect(runServerSession({ baseUrl, prompt: "p", timeoutMs: 10_000, maxTextChars: 50 }))
      .rejects.toMatchObject({ truncated: true });
  });

  it("超时被归类成 agent turn timeout（前端文案靠这句）", async () => {
    const baseUrl = await startMockServe({
      parts: [{ id: "t1", type: "text", text: "很慢".repeat(20) }],
      replyText: "x",
    });
    // 把 timeoutMs 压到 1ms：POST 还没回来就被中止。
    await expect(runServerSession({ baseUrl, prompt: "p", timeoutMs: 1 }))
      .rejects.toThrow(/agent turn timeout/);
  });
});

describe("isServeStartupFailure", () => {
  it("只把「serve 起不来」这类错误认作可回退", () => {
    expect(isServeStartupFailure(new Error("opencode serve did not report its listening URL within 25000ms: "))).toBe(true);
    // 旧版 opencode 没有 serve 子命令：进程直接退，同样该回退。
    expect(isServeStartupFailure(new Error("opencode serve exited with code 1: unknown command"))).toBe(true);
    expect(isServeStartupFailure(new Error("connect ECONNREFUSED 127.0.0.1:1"))).toBe(true);
    expect(isServeStartupFailure(new Error("GET /event -> HTTP 404"))).toBe(true);
    expect(isServeStartupFailure(new Error("POST /session -> HTTP 404: {}"))).toBe(true);
    // 超时与模型报错发生在 serve 已经在跑之后，回退只会把失败放大。
    expect(isServeStartupFailure(new Error("agent turn timeout after 1000ms"))).toBe(false);
    expect(isServeStartupFailure(new Error("POST /session/ses_x/message -> HTTP 401: bad key"))).toBe(false);
  });
});
