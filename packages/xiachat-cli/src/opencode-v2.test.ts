/**
 * opencode 2.x `/api/*` 协议的单测。
 *
 * 事件形状不是猜的：2026-10-27 在本机 opencode v2.1.4 上跑了一轮真实对话
 * （deepseek/deepseek-flash，prompt「请只回复两个字：收到」），抓到的帧就是：
 *   session.text.delta       {"data":{"sessionID":"ses_…","delta":"收到"}}
 *   session.reasoning.delta  {"data":{"sessionID":"ses_…","delta":"The"}}
 *   session.execution.succeeded {"data":{"sessionID":"ses_…"}}
 * 鉴权是 Basic `opencode:<serve 打印的 server password>`。
 */
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";

import { afterEach, describe, expect, it } from "vitest";

import type { ActivityStep } from "./agent-activity.js";
import {
  OpenCodeV2Error,
  interruptV2Session,
  runV2Session,
  toV2ModelRef,
  v2AuthHeader,
} from "./opencode-v2.js";
import { isServeStartupFailure, parseServePassword } from "./opencode-server.js";

const servers: Server[] = [];

afterEach(async () => {
  await Promise.all(
    servers.splice(0).map((s) => new Promise<void>((r) => {
      // SSE 连接是长连接，不掐掉 close() 会一直挂着。
      s.closeAllConnections?.();
      s.close(() => r());
    })),
  );
});

interface FakeV2 {
  baseUrl: string;
  /** 收到的 prompt 请求体。 */
  prompts: unknown[];
  /** 打过的 interrupt 次数。 */
  interrupts: number;
  /** 各请求带的 authorization 头。 */
  auths: string[];
  /** 建会话时收到的请求体（用来断言 Model.Ref 形状）。 */
  sessionBody?: unknown;
  /** prompt 到达后才推流的延迟绑定。 */
  pendingScenario?: () => void;
}

type Scenario = (ctx: {
  write: (event: unknown) => void;
  end: () => void;
}) => void;

/**
 * 一个最小的 2.x serve：`/api/*` 带 Basic 鉴权，`POST /api/session` 建会话，
 * `GET /api/event` 推 SSE，`/prompt` 触发场景脚本。
 *
 * `hang` 用来复现 M1 的原始症状：某个阶段收到请求后**既不回也不关**（像 serve
 * 冷启动卡在加载配置上），验证超时能兜住建连阶段而不只是「prompt 之后不产终态」。
 */
async function startFakeV2(opts: {
  password?: string;
  scenario: Scenario;
  hang?: "session" | "event" | "prompt";
}): Promise<FakeV2> {
  const password = opts.password ?? "test-password";
  const state: FakeV2 = { baseUrl: "", prompts: [], interrupts: 0, auths: [] };
  let openEvent: ServerResponse | null = null;
  let scenario: Scenario | null = opts.scenario;

  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    const url = new URL(req.url ?? "/", "http://127.0.0.1");
    state.auths.push(String(req.headers.authorization ?? ""));

    // `GET /session`、`GET /event` 在 2.x 上是 Web UI 的 catch-all，这里用 405/HTML
    // 复现，确保走 /api 的客户端不会误用它。
    if (url.pathname === "/session") {
      res.statusCode = 405;
      res.end();
      return;
    }
    if (url.pathname === "/event") {
      res.writeHead(200, { "content-type": "text/html" });
      res.end("<!doctype html><title>OpenCode</title>");
      return;
    }

    if (!url.pathname.startsWith("/api/")) {
      res.statusCode = 404;
      res.end();
      return;
    }
    if (req.headers.authorization !== v2AuthHeader(password)) {
      res.statusCode = 401;
      res.end("unauthorized");
      return;
    }

    if (url.pathname === "/api/session" && req.method === "POST") {
      // 挂起：读完请求体也不回响应头（serve 冷启动卡住的形状）。
      if (opts.hang === "session") {
        req.resume();
        return;
      }
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        try {
          state.sessionBody = JSON.parse(body);
        } catch {
          state.sessionBody = body;
        }
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ data: { id: "ses_test" } }));
      });
      return;
    }
    if (url.pathname === "/api/event") {
      // 挂起：连响应头都不 flush，客户端会一直等在建连上。
      if (opts.hang === "event") return;
      res.writeHead(200, {
        "content-type": "text/event-stream",
        "cache-control": "no-cache",
        connection: "keep-alive",
      });
      res.flushHeaders?.();
      openEvent = res;
      let closed = false;
      res.on("close", () => {
        closed = true;
      });
      const write = (event: unknown) => {
        if (closed) return;
        res.write(`: heartbeat\n\n`);
        res.write(`data: ${JSON.stringify(event)}\n\n`);
      };
      const end = () => {
        if (closed) return;
        res.end();
      };
      const run = scenario;
      scenario = null;
      // 场景脚本在 prompt 请求到达时才跑（真实 v2 也是这样：事件由 prompt 触发）。
      state.pendingScenario = () => run?.({ write, end });
      return;
    }
    if (/^\/api\/session\/[^/]+\/prompt$/.test(url.pathname) && req.method === "POST") {
      // 挂起：prompt 收下就不回了（实测正常是立刻 200，这里复现异常）。
      if (opts.hang === "prompt") {
        req.resume();
        return;
      }
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        try {
          state.prompts.push(JSON.parse(body));
        } catch {
          state.prompts.push(body);
        }
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ data: { id: "msg_1" } }));
        state.pendingScenario?.();
      });
      return;
    }
    if (/^\/api\/session\/[^/]+\/interrupt$/.test(url.pathname) && req.method === "POST") {
      state.interrupts += 1;
      res.writeHead(200, { "content-type": "application/json" });
      res.end("{}");
      openEvent?.end();
      return;
    }
    res.statusCode = 404;
    res.end();
  });

  servers.push(server);
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  state.baseUrl = `http://127.0.0.1:${port}`;
  return state;
}

// 见 startFakeV2 里的延迟绑定：prompt 到达时才真正推流。

/** `finally` 里的 interruptV2Session 是 fire-and-forget（void），给它一点送达时间。 */
async function waitFor(predicate: () => boolean, timeoutMs = 2_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((r) => setTimeout(r, 10));
  }
}

describe("v2AuthHeader / parseServePassword", () => {
  it("鉴权是 Basic opencode:<password>", () => {
    expect(v2AuthHeader("pw")).toBe(`Basic ${Buffer.from("opencode:pw").toString("base64")}`);
  });

  it("从 serve 的启动输出里取密码，容忍 ANSI 与前后噪声", () => {
    const out = [
      "server listening on http://127.0.0.1:5238",
      "\u001b[2mserver password r5iAZPoENlJxmCQUx86Qa5ylLNxrn3eJcK4Rl1tCYQ4\u001b[0m",
      "",
    ].join("\n");
    expect(parseServePassword(out)).toBe("r5iAZPoENlJxmCQUx86Qa5ylLNxrn3eJcK4Rl1tCYQ4");
  });

  it("1.x 的输出里没有密码行", () => {
    expect(parseServePassword("server listening on http://127.0.0.1:49882\n")).toBeNull();
  });
});

describe("toV2ModelRef", () => {
  it("provider/model → {providerID,id}", () => {
    expect(toV2ModelRef("deepseek/deepseek-flash")).toEqual({
      providerID: "deepseek",
      id: "deepseek-flash",
    });
  });

  it("思考等级骑在 # 后面，变成 variant", () => {
    expect(toV2ModelRef("quukk/glm-5.2#high")).toEqual({
      providerID: "quukk",
      id: "glm-5.2",
      variant: "high",
    });
  });

  it("没有路由时返回 null（交给配置默认模型）", () => {
    expect(toV2ModelRef(undefined)).toBeNull();
    expect(toV2ModelRef("bare-model")).toBeNull();
  });
});

describe("runV2Session", () => {
  it("按 session.text.delta 逐片回调正文，reasoning 走另一路", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        write({ type: "session.reasoning.delta", data: { sessionID: "ses_test", delta: "thinking" } });
        write({ type: "session.text.delta", data: { sessionID: "ses_test", delta: "收" } });
        write({ type: "session.text.delta", data: { sessionID: "ses_test", delta: "到" } });
        write({ type: "session.execution.succeeded", data: { sessionID: "ses_test" } });
      },
    });

    const chunks: string[] = [];
    const reasoning: string[] = [];
    const result = await runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "只回复两个字：收到",
      timeoutMs: 5_000,
      onText: (d) => chunks.push(d),
      onReasoning: (d) => reasoning.push(d),
    });

    expect(chunks).toEqual(["收", "到"]);
    expect(reasoning).toEqual(["thinking"]);
    expect(result.text).toBe("收到");
    expect(result.sawDelta).toBe(true);
    expect(result.deltaCount).toBe(2);
    expect(result.reasoningChars).toBe("thinking".length);
    expect(fake.prompts).toEqual([{ text: "只回复两个字：收到" }]);
    // 每次请求都带 Basic，不是 Bearer。
    expect(fake.auths.every((a) => a.startsWith("Basic "))).toBe(true);
  });

  it("模型路由按 2.x 的 Model.Ref 投递", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        write({ type: "session.execution.succeeded", data: { sessionID: "ses_test" } });
      },
    });
    await runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      model: "quukk/glm-5.2#high",
      timeoutMs: 5_000,
    });
    expect(fake.sessionBody).toMatchObject({
      model: { providerID: "quukk", id: "glm-5.2", variant: "high" },
    });
  });

  it("execution.failed 要把供应商的原话透出来，不能吞成空回答", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        write({
          type: "session.execution.failed",
          data: {
            sessionID: "ses_test",
            error: { type: "provider.invalid-request", message: "Insufficient Balance" },
          },
        });
      },
    });

    await expect(runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 5_000,
    })).rejects.toThrow(/Insufficient Balance/);
  });

  it("密码不对时给出 401，而不是静默空回答", async () => {
    const fake = await startFakeV2({
      password: "real-password",
      scenario: () => {},
    });
    await expect(runV2Session({
      baseUrl: fake.baseUrl,
      password: "wrong",
      prompt: "hi",
      timeoutMs: 5_000,
    })).rejects.toBeInstanceOf(OpenCodeV2Error);
  });

  it("别的会话的 delta 不能串进来（/api/event 是全机的）", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        write({ type: "session.text.delta", data: { sessionID: "ses_other", delta: "别人的" } });
        write({ type: "session.text.delta", data: { sessionID: "ses_test", delta: "我的" } });
        write({ type: "session.execution.succeeded", data: { sessionID: "ses_test" } });
      },
    });
    const result = await runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 5_000,
    });
    expect(result.text).toBe("我的");
  });
});

describe("interruptV2Session", () => {
  it("用 /api/session/{id}/interrupt 停一轮（1.x 叫 abort，2.x 不认）", async () => {
    const fake = await startFakeV2({ scenario: () => {} });
    await interruptV2Session(fake.baseUrl, "ses_test", "test-password");
    expect(fake.interrupts).toBe(1);
  });
});

// --- 过程流（activity）-----------------------------------------------------
// 事件顺序照抄 2026-10-27 在 v2.1.4 上抓到的真实一轮（prompt 要求用 shell 执行
// `echo cm-probe-ok`）：input.started → input.ended → called → progress → success。
// 工具名 `shell` 只出现在 input.started 里。

const S = "ses_test";

describe("runV2Session 的过程流（session.tool.*）", () => {
  it("shell 调用落成「执行命令」步骤，带命令摘要与输出摘要", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        write({ type: "session.tool.input.started", data: { sessionID: S, id: "call_1", name: "shell" } });
        write({
          type: "session.tool.input.ended",
          data: { sessionID: S, id: "call_1", text: JSON.stringify({ command: "echo cm-probe-ok" }) },
        });
        write({
          type: "session.tool.called",
          data: { sessionID: S, id: "call_1", input: { command: "echo cm-probe-ok" }, executed: false },
        });
        write({ type: "session.tool.progress", data: { sessionID: S, id: "call_1", metadata: { shellID: "sh_1" } } });
        write({
          type: "session.tool.success",
          data: {
            sessionID: S,
            id: "call_1",
            content: [{ type: "text", text: "cm-probe-ok\r\n" }],
            metadata: { status: "completed", exit: 0, truncated: false },
          },
        });
        write({ type: "session.text.delta", data: { sessionID: S, delta: "输出是 cm-probe-ok" } });
        write({ type: "session.execution.succeeded", data: { sessionID: S } });
      },
    });

    const snapshots: ActivityStep[][] = [];
    const result = await runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "用 shell 执行 echo cm-probe-ok",
      timeoutMs: 5_000,
      onActivity: (steps) => snapshots.push(steps),
    });

    const last = snapshots[snapshots.length - 1];
    expect(last).toHaveLength(1);
    expect(last[0]).toMatchObject({
      id: "call_1",
      kind: "command",
      label: "执行命令",
      detail: "echo cm-probe-ok",
      status: "done",
    });
    // 输出摘要来自 success 的 content（不是猜的 title）。
    expect(last[0].outputDetail).toContain("cm-probe-ok");
    // 过程是先 running 后 done，不是一次性落地。
    expect(snapshots.some((s) => s[0]?.status === "running")).toBe(true);
    // 工具之后有正文 → 不是「半句话」。
    expect(result.endedOnFailedTool).toBe(false);
    expect(result.failedStep).toBeNull();
  });

  it("工具失败：状态 failed、原因进 outputDetail，且没正文时判成半句话", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        write({ type: "session.tool.input.started", data: { sessionID: S, id: "call_2", name: "read" } });
        write({
          type: "session.tool.called",
          data: { sessionID: S, id: "call_2", input: { path: "C:\\tmp\\package.json" }, executed: false },
        });
        write({
          type: "session.tool.failed",
          data: {
            sessionID: S,
            id: "call_2",
            error: { type: "tool.execution", message: "File not found: C:\\tmp\\package.json" },
            executed: false,
          },
        });
        write({ type: "session.execution.succeeded", data: { sessionID: S } });
      },
    });

    const result = await runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "读取 package.json",
      timeoutMs: 5_000,
      onActivity: () => {},
    });

    expect(result.endedOnFailedTool).toBe(true);
    expect(result.failedStep).toMatchObject({ id: "call_2", kind: "file", label: "读取文件", status: "failed" });
    expect(result.failedStep?.outputDetail).toContain("File not found");
    expect(result.text).toBe("");
  });

  it("别的会话的工具事件不串进本轮的步骤列表", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        write({ type: "session.tool.input.started", data: { sessionID: "ses_other", id: "call_x", name: "shell" } });
        write({ type: "session.tool.input.started", data: { sessionID: S, id: "call_1", name: "shell" } });
        write({ type: "session.text.delta", data: { sessionID: S, delta: "ok" } });
        write({ type: "session.execution.succeeded", data: { sessionID: S } });
      },
    });

    const snapshots: ActivityStep[][] = [];
    await runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 5_000,
      onActivity: (steps) => snapshots.push(steps),
    });

    // 快照是全量的：同一步会在多份快照里重复出现，所以只看 id 集合。
    const ids = new Set(snapshots.flat().map((s) => s.id));
    expect([...ids]).toEqual(["call_1"]);
  });

  it("没给 onActivity 时不解析过程流（省掉无用的 JSON 往返）", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        write({ type: "session.tool.input.started", data: { sessionID: S, id: "call_1", name: "shell" } });
        write({ type: "session.text.delta", data: { sessionID: S, delta: "ok" } });
        write({ type: "session.execution.succeeded", data: { sessionID: S } });
      },
    });
    const result = await runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 5_000,
    });
    expect(result.text).toBe("ok");
    expect(result.endedOnFailedTool).toBe(false);
  });
});

describe("runV2Session 的空回答与 step 失败", () => {
  it("step.failed 且一个字都没有 → 抛原因，不当成空回答交上去", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        write({
          type: "session.step.failed",
          data: { sessionID: S, error: { type: "provider.error", message: "Insufficient Balance" } },
        });
        write({ type: "session.execution.succeeded", data: { sessionID: S } });
      },
    });

    await expect(runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 5_000,
    })).rejects.toThrow(/Insufficient Balance/);
  });

  it("step.failed 之后正常出正文 → 以正文为准，不误判失败", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        // 实测语义：step 失败后 opencode 可能重试，重试成功就该算成功。
        write({
          type: "session.step.failed",
          data: { sessionID: S, error: { type: "provider.error", message: "rate limited" } },
        });
        write({ type: "session.text.delta", data: { sessionID: S, delta: "重试后的正文" } });
        write({ type: "session.execution.succeeded", data: { sessionID: S } });
      },
    });

    const result = await runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 5_000,
    });
    expect(result.text).toBe("重试后的正文");
  });
});

// --- 失败/中断路径 ---------------------------------------------------------
// 这一组对应审查报告 M3（超时与外部中止必须显式打断 serve 里的执行，光断 SSE
// 停不住）、M2（SSE 中途断开要快速失败）、m1（缺 sessionID 的终态帧不能结束
// 本轮）、m4（超过 ACTIVITY_MAX_STEPS 后仍要能判「半句话」）。

describe("runV2Session 的超时与中止", () => {
  it("超时会打断服务端执行（finally 里调 /interrupt），不是只断 SSE", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        // 只推正文，**不写任何终态帧** → 只能靠 timeoutMs 收场。
        write({ type: "session.text.delta", data: { sessionID: S, delta: "说到一半" } });
      },
    });

    await expect(runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 200,
    })).rejects.toThrow(/timeout/i);

    await waitFor(() => fake.interrupts > 0);
    expect(fake.interrupts).toBe(1);
  });

  it("外部中止同样打断服务端执行", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        write({ type: "session.text.delta", data: { sessionID: S, delta: "第一片" } });
      },
    });

    const ctl = new AbortController();
    await expect(runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 5_000,
      signal: ctl.signal,
      // 收到第一片正文后再中止：保证 abort 发生在 SSE 已经连着的时候。
      onText: () => ctl.abort(),
    })).rejects.toThrow(/abort/i);

    await waitFor(() => fake.interrupts > 0);
    expect(fake.interrupts).toBe(1);
  });

  it("SSE 中途断开要快速失败（不能等到超时，也不能误报成 timeout）", async () => {
    const fake = await startFakeV2({
      scenario: ({ write, end }) => {
        write({ type: "session.text.delta", data: { sessionID: S, delta: "说一半" } });
        // serve 崩了：对端直接关流，不写终态。
        end();
      },
    });

    const startedAt = Date.now();
    let error: unknown = null;
    await runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 5_000,
    }).catch((err: unknown) => {
      error = err;
    });
    const elapsed = Date.now() - startedAt;

    // 1) 必须是 reject，不能把半句话当正常结果交上去。
    expect(error).not.toBeNull();
    // 2) 快速失败：远早于 timeoutMs（留 2000ms 余量，慢机器也够）。
    expect(elapsed).toBeLessThan(2_000);
    // 3) 错误归因不能说成超时（文案由实现定，只要求不含 timeout）。
    expect(String((error as Error).message ?? error)).not.toMatch(/timeout/i);
  });
});

describe("runV2Session 的帧归属", () => {
  it("缺 sessionID 的终态/错误帧不能结束本轮（否则半句话或无关失败）", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        // 全局事件（session.created / server.* 之类）不带 sessionID：不能被当成本轮终态。
        write({ type: "session.execution.succeeded", data: {} });
        write({ type: "session.error", data: {} });
        write({ type: "session.text.delta", data: { sessionID: S, delta: "前半句" } });
        write({ type: "session.text.delta", data: { sessionID: S, delta: "后半句" } });
        write({ type: "session.execution.succeeded", data: { sessionID: S } });
      },
    });

    const chunks: string[] = [];
    const result = await runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 5_000,
      onText: (d) => chunks.push(d),
    });

    // 强断言：两片正文都进来了 → 没被前面那两个无主帧提前结束。
    expect(result.text).toBe("前半句后半句");
    expect(chunks).toEqual(["前半句", "后半句"]);
    expect(result.deltaCount).toBe(2);
  });
});

describe("runV2Session 的「半句话」判定（超过 ACTIVITY_MAX_STEPS）", () => {
  it("第 21 个工具失败（步骤被截断）仍判成半句话", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        // 前 20 个工具成功（正好顶到 ACTIVITY_MAX_STEPS），事件顺序照抓包。
        for (let i = 1; i <= 20; i += 1) {
          const id = `call_${i}`;
          write({ type: "session.tool.input.started", data: { sessionID: S, id, name: "shell" } });
          write({
            type: "session.tool.input.ended",
            data: { sessionID: S, id, text: JSON.stringify({ command: `echo ${i}` }) },
          });
          write({
            type: "session.tool.called",
            data: { sessionID: S, id, input: { command: `echo ${i}` }, executed: false },
          });
          write({
            type: "session.tool.success",
            data: {
              sessionID: S,
              id,
              content: [{ type: "text", text: `${i}\r\n` }],
              metadata: { status: "completed", exit: 0, truncated: false },
            },
          });
        }
        // 第 21 个工具失败：它不会进快照（steps 已满），但判定必须认得它。
        write({ type: "session.tool.input.started", data: { sessionID: S, id: "call_21", name: "shell" } });
        write({
          type: "session.tool.input.ended",
          data: { sessionID: S, id: "call_21", text: JSON.stringify({ command: "exit 1" }) },
        });
        write({
          type: "session.tool.failed",
          data: {
            sessionID: S,
            id: "call_21",
            error: { type: "tool.execution", message: "Command failed with exit code 1" },
            executed: false,
          },
        });
        // 全程无正文。
        write({ type: "session.execution.succeeded", data: { sessionID: S } });
      },
    });

    const snapshots: ActivityStep[][] = [];
    const result = await runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "循环跑 21 个命令",
      timeoutMs: 5_000,
      onActivity: (steps) => snapshots.push(steps),
    });

    expect(result.text).toBe("");
    // 快照被截断是设计如此（上限 20 步），但「半句话」判定不能被截断影响。
    expect(result.endedOnFailedTool).toBe(true);
    expect(result.failedStep?.status).toBe("failed");
    expect(snapshots.length).toBeGreaterThan(0);
  });
});

// --- M1 原始症状：建连阶段自己挂住 -----------------------------------------
// 上一组只覆盖「prompt 之后不产终态」。M1 报的是 `POST /api/session` / `GET
// /api/event` 的响应头迟迟不来时**整轮没有时间上界**（timer 原来挂在 SSE 建连
// 成功之后），设备端表现是永久「正在思考」。这三例分别把三个阶段钉住。

describe("runV2Session 的建连阶段超时（M1）", () => {
  it("POST /api/session 挂住不响应时，timeoutMs 内以超时收场", async () => {
    const fake = await startFakeV2({ hang: "session", scenario: () => {} });

    const startedAt = Date.now();
    await expect(runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 300,
    })).rejects.toThrow(/timeout/i);
    const elapsed = Date.now() - startedAt;

    expect(elapsed).toBeLessThan(2_000);
    // 还没有 sessionID：/api/session/{id}/interrupt 无从投递，只能由调用方杀进程树。
    // 这里断言 0 是为了把「建会话阶段拿不到 id = 无法打断」这件事写死，别让后来人
    // 以为这一阶段也有 interrupt 兜底（要改成 1 就得先有 id）。
    expect(fake.interrupts).toBe(0);
  });

  it("GET /api/event 建连挂住（不 flush 响应头）时，timeoutMs 内以超时收场并打断服务端", async () => {
    const fake = await startFakeV2({ hang: "event", scenario: () => {} });

    const startedAt = Date.now();
    await expect(runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 300,
    })).rejects.toThrow(/timeout/i);
    const elapsed = Date.now() - startedAt;

    expect(elapsed).toBeLessThan(2_000);
    // 会话已建好（id 拿到了）：挂在建连阶段同样要显式打断，别让 serve 里空跑。
    await waitFor(() => fake.interrupts > 0);
    expect(fake.interrupts).toBe(1);
  });

  it("POST /api/session/{id}/prompt 挂住不响应时，timeoutMs 内以超时收场并打断服务端", async () => {
    const fake = await startFakeV2({ hang: "prompt", scenario: () => {} });

    const startedAt = Date.now();
    await expect(runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 300,
    })).rejects.toThrow(/timeout/i);
    const elapsed = Date.now() - startedAt;

    expect(elapsed).toBeLessThan(2_000);
    await waitFor(() => fake.interrupts > 0);
    expect(fake.interrupts).toBe(1);
  });
});

describe("isServeStartupFailure 的防误伤（供应商 401 不算 serve 降级）", () => {
  it("供应商在 session.execution.failed 里报 401 → 不能判成 serve 起不来", async () => {
    const fake = await startFakeV2({
      scenario: ({ write }) => {
        write({
          type: "session.execution.failed",
          data: {
            sessionID: S,
            error: { type: "provider.unauthorized", message: "401 Unauthorized: invalid api key" },
          },
        });
      },
    });

    const err = await runV2Session({
      baseUrl: fake.baseUrl,
      password: "test-password",
      prompt: "hi",
      timeoutMs: 5_000,
    }).then(
      () => null,
      (e: unknown) => e,
    );

    expect(err).toBeInstanceOf(Error);
    // 模型 key 401 说明 serve 跑得好好的：退成 run 只会把同一次失败再放大一遍。
    expect(isServeStartupFailure(err)).toBe(false);
  });
});
