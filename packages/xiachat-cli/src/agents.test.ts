// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  agentRuntimeEnv,
  buildAgentArgs,
  buildSpawnInvocation,
  buildModelArgs,
  discoverAgents,
  emitsJsonEnvelope,
  extractOpenclawEnvelopeDetails,
  extractOpenclawReply,
  KNOWN_AGENT_CLIS,
  normalizeOpenclawOutput,
  openclawTurnHealth,
  parseOpenclawJsonOutput,
  resolveWindowsShim,
  runAgentTurn,
  splitModelRoute,
  stripAnsiCodes,
  trimIncompleteUtf8Tail,
} from "./agents.js";
import { createEchoAgentScript, type EchoAgentFixture } from "./test-fixtures.js";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { classifyTurnError } from "./turn-error.js";

interface FailureAgentFixture {
  agentPath: string;
  cleanup: () => void;
}

// 一个「只往 stdout 打固定文本、stderr 一个字都不打、然后以 code 退出」的假
// agent。现场（2026-10-25）kimi 就是这么失败的：横幅落在 stdout，我们只把
// stderr 拼进错误串，于是 web 收到的是「进程异常退出，没有任何输出」。
function createStdoutFailureAgent(
  name: string,
  stdoutText: string,
  code = 1,
): FailureAgentFixture {
  const dir = mkdtempSync(join(tmpdir(), "cm-failure-agent-"));
  const helper = join(dir, "failure-agent.cjs");
  writeFileSync(helper, [
    // 回调里退出，保证 stdout 已经冲刷完（否则管道异步写会被截断）。
    `process.stdout.write(${JSON.stringify(stdoutText)}, () => process.exit(${code}));`,
  ].join("\n"));
  const isWin = process.platform === "win32";
  const agentPath = join(dir, isWin ? `${name}.cmd` : name);
  if (isWin) {
    writeFileSync(agentPath, `@echo off\r\nnode "${helper}"\r\n`, { mode: 0o755 });
  } else {
    writeFileSync(agentPath, `#!/bin/sh\nexec node "${helper}"\n`, { mode: 0o755 });
    chmodSync(agentPath, 0o755);
  }
  return {
    agentPath,
    cleanup: () => rmSync(dir, { recursive: true, force: true }),
  };
}

describe("KNOWN_AGENT_CLIS", () => {
  it("matches the server-side knownAgentCLIs list", () => {
    expect(KNOWN_AGENT_CLIS).toEqual([
      "claude", "codex", "opencode", "codebuddy", "codearts", "deveco",
      "openclaw", "hermes", "pi", "omp", "cursor-agent", "kimi",
      "reasonix", "dsh", "kiro-cli", "agy", "qodercli", "qoderclicn",
      "traecli", "grok", "qwen", "qwenpaw", "mcode", "dim", "zeroclaw",
    ]);
  });
});

describe("buildAgentArgs", () => {
  it("claude uses print mode with stream-json and --model", () => {
    expect(buildAgentArgs("claude", "hi", "sonnet-4")).toEqual([
      "-p", "hi", "--output-format", "stream-json", "--verbose", "--model", "sonnet-4",
    ]);
  });

  it("codex uses exec subcommand with --json and -m", () => {
    expect(buildAgentArgs("codex", "hi", "gpt-5")).toEqual([
      "exec", "hi", "--json", "--skip-git-repo-check", "-m", "gpt-5",
    ]);
  });

  it("opencode uses run subcommand with --model", () => {
    expect(buildAgentArgs("opencode", "hi", "glm-4.7")).toEqual([
      "run", "hi", "--format", "json", "--thinking", "--model", "glm-4.7",
    ]);
  });

  // 没有这两个参数时 opencode 只输出格式化纯文本，思考与工具步骤全都拿不到
  // ——私聊里「看不到它想了什么」的根因。--format 是 1.19 才有的参数。
  it("opencode 强制开 JSON 事件流，思考流与过程流才有数据源", () => {
    const args = buildAgentArgs("opencode", "hi");
    expect(args).toEqual(["run", "hi", "--format", "json", "--thinking"]);
    expect(args).toContain("--format");
    expect(args[args.indexOf("--format") + 1]).toBe("json");
  });

  it("openclaw uses agent --local --json -m with --model", () => {
    expect(buildAgentArgs("openclaw", "hi", "m1")).toEqual(["agent", "--local", "--json", "-m", "hi", "--model", "m1"]);
  });

  it("openclaw 带会话键时用 --session-key 隔离会话（群聊不得读讨论组的上下文）", () => {
    expect(buildAgentArgs("openclaw", "hi", "m1", "cm-group-g1")).toEqual([
      "agent", "--local", "--json", "-m", "hi", "--session-key", "cm-group-g1", "--model", "m1",
    ]);
    // 不带键时参数形状不变（老行为、老测试）
    expect(buildAgentArgs("openclaw", "hi", "m1")).not.toContain("--session-key");
  });

  it("不支持会话键的 agent 忽略它（opencode/hermes 每轮本就是新会话）", () => {
    expect(buildAgentArgs("opencode", "hi", undefined, "cm-group-g1")).toEqual([
      "run", "hi", "--format", "json", "--thinking",
    ]);
    expect(buildAgentArgs("hermes", "hi", undefined, "cm-group-g1")).toEqual([
      "chat", "-q", "hi", "--oneshot", "--format", "stream-json",
    ]);
  });

  it("hermes 走 chat -q + 结构化事件流（-z 只有整包正文，拿不到工具过程）", () => {
    expect(buildAgentArgs("hermes", "hi", "m1")).toEqual([
      "chat", "-q", "hi", "--oneshot", "--format", "stream-json", "-m", "m1",
    ]);
  });

  // The panel stores `provider/model`; codex and hermes take the provider
  // separately, so the route is split rather than passed through. Verified
  // against the real CLIs: `codex -m quukk_direct/glm-5.2` is model_not_found.
  it("codex splits a route into -c model_provider + a bare -m", () => {
    expect(buildAgentArgs("codex", "hi", "quukk_direct/glm-5.2")).toEqual([
      "exec", "hi", "--json", "--skip-git-repo-check",
      "-c", "model_provider=quukk_direct", "-m", "glm-5.2",
    ]);
  });

  it("hermes passes the provider separately", () => {
    expect(buildAgentArgs("hermes", "hi", "quukk/glm-5.3")).toEqual([
      "chat", "-q", "hi", "--oneshot", "--format", "stream-json",
      "-m", "glm-5.3", "--provider", "quukk",
    ]);
  });

  it("openclaw keeps the whole route", () => {
    expect(buildAgentArgs("openclaw", "hi", "openai/glm-5.2")).toEqual([
      "agent", "--local", "--json", "-m", "hi", "--model", "openai/glm-5.2",
    ]);
  });

  it("omits model flags when no model is given", () => {
    expect(buildAgentArgs("claude", "hi")).toEqual(["-p", "hi", "--output-format", "stream-json", "--verbose"]);
    expect(buildAgentArgs("codex", "hi")).toEqual(["exec", "hi", "--json", "--skip-git-repo-check"]);
    expect(buildAgentArgs("opencode", "hi")).toEqual(["run", "hi", "--format", "json", "--thinking"]);
    expect(buildAgentArgs("openclaw", "hi")).toEqual(["agent", "--local", "--json", "-m", "hi"]);
    expect(buildAgentArgs("hermes", "hi")).toEqual([
      "chat", "-q", "hi", "--oneshot", "--format", "stream-json",
    ]);
  });

  // kimi 必须走 -p：不带参数直接 spawn 的是交互式 TUI，Windows 上拿不到
  // console 时会以 `No Windows console found` 退出（生产事故实证）。
  it("kimi 走 -p 非交互入口（裸 kimi 是 TUI，无 console 会直接退出）", () => {
    expect(buildAgentArgs("kimi", "hi")).toEqual(["-p", "hi", "--output-format", "text"]);
    // 不注入 --model：kimi 的 -m 只认它自己的模型别名，不认 provider/model 路由。
    expect(buildAgentArgs("kimi", "hi", "quukk/glm-5.2")).toEqual([
      "-p", "hi", "--output-format", "text",
    ]);
  });

  it("returns [] for agents without a strategy", () => {
    expect(buildAgentArgs("codebuddy", "hi")).toEqual([]);
    expect(buildAgentArgs("grok", "hi")).toEqual([]);
  });

  it("returns [] past the argv length cap (falls back to stdin)", () => {
    expect(buildAgentArgs("claude", "a".repeat(8001))).toEqual([]);
    expect(buildAgentArgs("codex", "a".repeat(8001), "gpt-5")).toEqual([]);
    expect(buildAgentArgs("claude", "a".repeat(8000))).toEqual([
      "-p", "a".repeat(8000), "--output-format", "stream-json", "--verbose",
    ]);
  });
});

describe("splitModelRoute / buildModelArgs", () => {
  it("splits only well-formed provider/model routes", () => {
    expect(splitModelRoute("quukk/glm-5.3")).toEqual({ provider: "quukk", model: "glm-5.3" });
    expect(splitModelRoute("quukk_direct/glm-5.2")).toEqual({
      provider: "quukk_direct",
      model: "glm-5.2",
    });
    expect(splitModelRoute("glm-5.3")).toBeNull();
    expect(splitModelRoute("a/b/c")).toBeNull();
    expect(splitModelRoute("/glm-5.3")).toBeNull();
    expect(splitModelRoute("quukk/")).toBeNull();
    expect(splitModelRoute("quukk/glm 5.3")).toBeNull();
  });

  it("omits flags without a model and defaults to --model", () => {
    expect(buildModelArgs("opencode", undefined)).toEqual([]);
    expect(buildModelArgs("opencode", "quukk/glm-5.3")).toEqual(["--model", "quukk/glm-5.3"]);
    expect(buildModelArgs("claude", "sonnet-4")).toEqual(["--model", "sonnet-4"]);
    // 无 argv 策略的 agent 不注入模型参数：它们不认 `--model`（kimi 走 stdin，
    // 收到未知参数只会 usage dump + exit 1），模型选择对它们本就无效。
    expect(buildModelArgs("kimi", "m1")).toEqual([]);
  });

  // 3a 的守卫必须逐 agent 钉住：既不能给不认 `--model` 的 agent 乱塞参数，
  // 也不能把本来支持 `--model` 的 agent 一起误伤（qwen 就是易回归点 ——
  // 它在设备端没有 argv 策略，但 `--model` 是真实支持的参数）。
  it("only renders model flags for agents that actually accept them", () => {
    // 无策略 → 不注入
    expect(buildModelArgs("kimi", "m1")).toEqual([]);
    expect(buildModelArgs("grok", "m1")).toEqual([]);
    // 有策略 → 按各家形状注入
    expect(buildModelArgs("codex", "p/m")).toEqual(["-c", "model_provider=p", "-m", "m"]);
    expect(buildModelArgs("opencode", "p/m")).toEqual(["--model", "p/m"]);
    expect(buildModelArgs("hermes", "p/m")).toEqual(["-m", "m", "--provider", "p"]);
    // 无策略但确实支持 --model：provider 段必须剥掉
    expect(buildModelArgs("qwen", "p/m")).toEqual(["--model", "m"]);
    // codebuddy 只留一个随机端口，模型走 CODEBUDDY_MODEL
    expect(buildModelArgs("codebuddy", "m1")).toEqual(["--port", "0"]);
    // 没选模型 → 什么都不加
    expect(buildModelArgs("claude", undefined)).toEqual([]);
  });

  it("strips the provider segment for qwen, which only takes the upstream slug", () => {
    expect(buildModelArgs("qwen", "quukk/glm-5.2")).toEqual(["--model", "glm-5.2"]);
    expect(buildModelArgs("qwen", "glm-5.2")).toEqual(["--model", "glm-5.2"]);
    expect(buildModelArgs("qwen", undefined)).toEqual([]);
  });

  it("never passes --model to codebuddy (it only exposes a free port)", () => {
    // codebuddy rejects any model id it does not own (`--model glm-5.2` →
    // "Authentication required"), so the model rides CODEBUDDY_MODEL instead.
    expect(buildModelArgs("codebuddy", "quukk/glm-5.2")).toEqual(["--port", "0"]);
    expect(buildModelArgs("codebuddy", undefined)).toEqual(["--port", "0"]);
  });
});

describe("agentRuntimeEnv", () => {
  const CONFIG = {
    endpoints: [
      {
        id: "quukk",
        name: "Quukk AI",
        baseUrl: "https://llmapi.quukk.com/v1",
        apiKey: "sk-test",
      },
    ],
    defaultModel: "quukk/glm-5.2",
  };

  it("points codebuddy at the configured gateway and passes the picked model", () => {
    const env = agentRuntimeEnv("codebuddy", "quukk/glm-5.3", { env: { PATH: "/bin" }, config: CONFIG });
    expect(env).toMatchObject({
      PATH: "/bin",
      CODEBUDDY_BASE_URL: "https://llmapi.quukk.com/v1",
      CODEBUDDY_AUTH_TOKEN: "sk-test",
      CODEBUDDY_MODEL: "glm-5.3",
    });
  });

  it("falls back to the configured default model when none was picked", () => {
    expect(agentRuntimeEnv("codebuddy", undefined, { env: {}, config: CONFIG }))
      .toMatchObject({ CODEBUDDY_MODEL: "glm-5.2" });
  });

  it("leaves the environment alone for agents that need no gateway switch", () => {
    expect(agentRuntimeEnv("qwen", "quukk/glm-5.2", { env: {}, config: CONFIG })).toBeNull();
    expect(agentRuntimeEnv("codebuddy", "quukk/glm-5.2", { env: {}, config: null })).toBeNull();
  });
});

// Real shapes observed from OpenClaw 2026.9.2 (`openclaw agent --local …`)
// on this machine.
const OPENCLAW_PLAIN_STDOUT = [
  "\x1b[33m[provider-transport-fetch]\x1b[39m \x1b[36m[model-fetch] start provider=openai api=openai-responses model=gpt-5.6-sol method=POST url=http://127.0.0.1:49830/v1/responses timeoutMs=undefined proxy=none policy=custom\x1b[39m",
  "\x1b[33m[provider-transport-fetch]\x1b[39m \x1b[36m[model-fetch] response status=200 elapsedMs=28 dispatcher=new contentType=text/event-stream\x1b[39m",
  "收到",
  "\x1b[33m[agents/agent-command]\x1b[39m \x1b[36m[agent] run 2b7c1e00-1234 ended with stopReason=stop\x1b[39m",
  "",
].join("\n");

const OPENCLAW_JSON_STDOUT = JSON.stringify({
  payloads: [{ text: "你好！有什么我可以帮你的吗？😊", mediaUrl: null }],
  meta: {
    durationMs: 7909,
    finalPromptText: "你好",
    finalAssistantVisibleText: "你好！有什么我可以帮你的吗？😊",
    finalAssistantRawText: "你好！有什么我可以帮你的吗？😊",
    stopReason: "stop",
  },
});

describe("stripAnsiCodes", () => {
  it("removes CSI color sequences", () => {
    expect(stripAnsiCodes("\x1b[33m[model-fetch]\x1b[39m text")).toBe("[model-fetch] text");
    expect(stripAnsiCodes("plain")).toBe("plain");
  });
});

describe("normalizeOpenclawOutput", () => {
  it("drops ANSI log lines and keeps the answer", () => {
    expect(normalizeOpenclawOutput(OPENCLAW_PLAIN_STDOUT)).toBe("收到");
  });

  it("preserves markdown links that look like bracket tags", () => {
    const md = "see [docs](https://example.com) for details";
    expect(normalizeOpenclawOutput(md)).toBe(md);
  });

  it("collapses blank runs and trims edges", () => {
    expect(normalizeOpenclawOutput("\n\nline1\n\n\n\nline2\n\n")).toBe("line1\n\nline2");
  });

  it("keeps multi-line answers intact", () => {
    const answer = "第一行\n第二行";
    expect(normalizeOpenclawOutput(`${answer}\n[agent] run x ended with stopReason=stop`)).toBe(answer);
  });
});

describe("parseOpenclawJsonOutput", () => {
  it("joins payload texts from the --json envelope", () => {
    expect(parseOpenclawJsonOutput(OPENCLAW_JSON_STDOUT)).toBe("你好！有什么我可以帮你的吗？😊");
  });

  it("joins multiple payloads with newlines", () => {
    const raw = JSON.stringify({ payloads: [{ text: "part1" }, { text: "part2" }, { mediaUrl: null }] });
    expect(parseOpenclawJsonOutput(raw)).toBe("part1\npart2");
  });

  it("falls back to meta.finalAssistantVisibleText when payloads are empty", () => {
    const raw = JSON.stringify({ payloads: [], meta: { finalAssistantVisibleText: "visible text" } });
    expect(parseOpenclawJsonOutput(raw)).toBe("visible text");
  });

  it("returns undefined for non-JSON output", () => {
    expect(parseOpenclawJsonOutput("收到")).toBeUndefined();
    expect(parseOpenclawJsonOutput("")).toBeUndefined();
  });

  it("returns undefined for JSON without recognizable fields", () => {
    expect(parseOpenclawJsonOutput(JSON.stringify({ foo: 1 }))).toBeUndefined();
  });
});

describe("extractOpenclawReply", () => {
  it("prefers the --json envelope", () => {
    expect(extractOpenclawReply(OPENCLAW_JSON_STDOUT)).toBe("你好！有什么我可以帮你的吗？😊");
  });

  it("normalizes plain stdout when no envelope is present", () => {
    expect(extractOpenclawReply(OPENCLAW_PLAIN_STDOUT)).toBe("收到");
  });

  it("falls back to raw trimmed text when extraction comes up empty", () => {
    expect(extractOpenclawReply("  \n raw fallback \n ")).toBe("raw fallback");
  });
});

describe("extractOpenclawEnvelopeDetails", () => {
  it("把 isReasoning 的 payload 抽成思考（不进正文）", () => {
    const raw = JSON.stringify({
      payloads: [
        { text: "先想想用户要什么", isReasoning: true },
        { text: "你好，有什么可以帮你？" },
      ],
    });
    expect(extractOpenclawEnvelopeDetails(raw).reasoning).toBe("先想想用户要什么");
    // 正文里绝不能混进思考
    expect(parseOpenclawJsonOutput(raw)).toBe("你好，有什么可以帮你？");
  });

  it("多种思考 payload 按顺序拼接", () => {
    const raw = JSON.stringify({
      payloads: [{ text: "第一段", isReasoning: true }, { text: "第二段", isReasoning: true }],
    });
    expect(extractOpenclawEnvelopeDetails(raw).reasoning).toBe("第一段\n\n第二段");
  });

  it("toolSummary 折成每一种工具一条汇总步骤", () => {
    const raw = JSON.stringify({
      payloads: [{ text: "已查完" }],
      toolSummary: { calls: 3, tools: ["web_search"], failures: 0, totalToolTimeMs: 1200 },
    });
    expect(extractOpenclawEnvelopeDetails(raw).tools).toEqual([
      { name: "web_search", calls: 3, failures: 0 },
    ]);
    const many = JSON.stringify({ toolSummary: { calls: 4, tools: ["web_search", "terminal"] } });
    expect(extractOpenclawEnvelopeDetails(many).tools).toEqual([
      { name: "web_search", calls: 1, failures: 0 },
      { name: "terminal", calls: 1, failures: 0 },
    ]);
  });

  it("没有思考/工具字段时返回空对象（老信封）", () => {
    expect(extractOpenclawEnvelopeDetails(JSON.stringify({ payloads: [{ text: "hi" }] }))).toEqual({});
    expect(extractOpenclawEnvelopeDetails("收到")).toEqual({});
    expect(extractOpenclawEnvelopeDetails("")).toEqual({});
  });
});

describe("emitsJsonEnvelope", () => {
  it("只把 openclaw 当作一次性 JSON 信封型 agent", () => {
    expect(emitsJsonEnvelope("openclaw")).toBe(true);
    expect(emitsJsonEnvelope("opencode")).toBe(false);
    expect(emitsJsonEnvelope("hermes")).toBe(false);
    expect(emitsJsonEnvelope("codex")).toBe(false);
    expect(emitsJsonEnvelope(undefined)).toBe(false);
  });
});

describe("openclawTurnHealth", () => {
  const envelope = (meta: unknown): string => JSON.stringify({ payloads: [], meta });

  it("把 aborted 判成没答完", () => {
    expect(openclawTurnHealth(envelope({ aborted: true })).incomplete).toBe(true);
  });

  it("把 finishReason=length 判成没答完（回答被截断）", () => {
    const health = openclawTurnHealth(envelope({ completion: { finishReason: "length" } }));
    expect(health.incomplete).toBe(true);
    expect(health.detail).toContain("length");
  });

  it("老信封没有 completion 时退到 stopReason", () => {
    expect(openclawTurnHealth(envelope({ stopReason: "content_filter" })).incomplete).toBe(true);
  });

  it("正常 stop 不算没答完", () => {
    expect(openclawTurnHealth(envelope({ aborted: false, completion: { finishReason: "stop" } })).incomplete).toBe(false);
  });

  it("非信封输出不做判断（绝不拿日志行猜结论）", () => {
    expect(openclawTurnHealth("plain stdout").incomplete).toBe(false);
    expect(openclawTurnHealth("{}").incomplete).toBe(false);
  });
});

describe("discoverAgents", () => {
  it("scans only extraPath when provided", async () => {
    const fixture = createEchoAgentScript("claude");
    try {
      const found = await discoverAgents({ extraPath: fixture.dir });
      expect(found.some((a) => a.name === "claude")).toBe(true);
      expect(found.every((a) => a.path.startsWith(fixture.dir))).toBe(true);
    } finally {
      fixture.cleanup();
    }
  });

  it("returns [] when extraPath has no known agent", async () => {
    const fixture = createEchoAgentScript("not-a-known-agent");
    try {
      expect(await discoverAgents({ extraPath: fixture.dir })).toEqual([]);
    } finally {
      fixture.cleanup();
    }
  });

  it("returns [] for an empty extraPath", async () => {
    expect(await discoverAgents({ extraPath: "" })).toEqual([]);
  });
});

describe("trimIncompleteUtf8Tail", () => {
  // "你" = E4 BD A0 (3-byte); "😀" = F0 9F 98 80 (4-byte).
  const cjk = Buffer.from("你", "utf8"); // e4 bd a0
  const emoji = Buffer.from("😀", "utf8"); // f0 9f 98 80
  const ascii = Buffer.from("a", "utf8");

  it("returns the buffer unchanged when it ends on a complete ASCII char", () => {
    const buf = Buffer.concat([ascii, cjk, ascii]);
    expect(trimIncompleteUtf8Tail(buf)).toEqual(buf);
  });

  it("returns an empty buffer for input that is only a partial sequence", () => {
    expect(trimIncompleteUtf8Tail(cjk.subarray(0, 1))).toEqual(Buffer.alloc(0));
    expect(trimIncompleteUtf8Tail(emoji.subarray(0, 1))).toEqual(Buffer.alloc(0));
  });

  it("trims a cut after the lead byte of a 3-byte char (E4)", () => {
    const buf = Buffer.concat([ascii, cjk.subarray(0, 1)]); // ...e4
    expect(trimIncompleteUtf8Tail(buf)).toEqual(ascii);
  });

  it("trims a cut after the 1st continuation byte of a 3-byte char (E4 BD)", () => {
    const buf = Buffer.concat([ascii, cjk.subarray(0, 2)]); // ...e4 bd
    expect(trimIncompleteUtf8Tail(buf)).toEqual(ascii);
  });

  it("trims a cut after the lead byte of a 4-byte char (F0)", () => {
    const buf = Buffer.concat([ascii, emoji.subarray(0, 1)]); // ...f0
    expect(trimIncompleteUtf8Tail(buf)).toEqual(ascii);
  });

  it("trims a cut after the 1st continuation byte of a 4-byte char (F0 9F)", () => {
    const buf = Buffer.concat([ascii, emoji.subarray(0, 2)]); // ...f0 9f
    expect(trimIncompleteUtf8Tail(buf)).toEqual(ascii);
  });

  it("trims a cut after the 2nd continuation byte of a 4-byte char (F0 9F 98)", () => {
    const buf = Buffer.concat([ascii, emoji.subarray(0, 3)]); // ...f0 9f 98
    expect(trimIncompleteUtf8Tail(buf)).toEqual(ascii);
  });

  it("keeps a complete multi-byte char at the end", () => {
    const buf = Buffer.concat([ascii, cjk]);
    expect(trimIncompleteUtf8Tail(buf)).toEqual(buf);
    const withEmoji = Buffer.concat([ascii, emoji]);
    expect(trimIncompleteUtf8Tail(withEmoji)).toEqual(withEmoji);
  });

  it("cuts the trailing continuation run when no lead byte exists within 4 bytes (invalid UTF-8)", () => {
    const garbage = Buffer.from([0x80, 0x80, 0x80, 0x80]); // bare continuations
    const buf = Buffer.concat([ascii, garbage]);
    expect(trimIncompleteUtf8Tail(buf)).toEqual(ascii);
  });
});

describe("runAgentTurn", () => {
  it("pipes prompt via stdin and resolves stdout", async () => {
    const fixture = createEchoAgentScript("claude");
    try {
      const out = await runAgentTurn({ execPath: fixture.agentPath, prompt: "hello agent", timeoutMs: 30_000 });
      expect(out).toContain("hello agent");
    } finally {
      fixture.cleanup();
    }
  });

  it("strategy agents receive the prompt as argv", async () => {
    // Echo agent prints its first argument (the argv prompt for strategy
    // agents) instead of stdin.
    const fixture = createEchoAgentScript("codex", { recordArgs: true, echoArg: 2 });
    try {
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "codex",
        prompt: "argv-prompt-here",
        timeoutMs: 30_000,
      });
      expect(out).toContain("argv-prompt-here");
      const args = fixture.recordedArgs();
      expect(args).toContain("exec");
      expect(args).toContain("argv-prompt-here");
    } finally {
      fixture.cleanup();
    }
  });

  it("strategy agents fall back to stdin past the argv length cap", async () => {
    // Long prompt -> buildAgentArgs returns [] -> stdin mode. The fixture
    // echoes stdin when no prompt arg is given.
    const fixture = createEchoAgentScript("codex", { recordArgs: true });
    try {
      const longPrompt = "x".repeat(8001);
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "codex",
        prompt: longPrompt,
        timeoutMs: 30_000,
      });
      expect(out).toContain("x".repeat(80));
      const args = fixture.recordedArgs();
      expect(args).not.toContain("exec");
    } finally {
      fixture.cleanup();
    }
  });

  it("unknown agents keep stdin mode", async () => {
    // kimi 现在有策略了，不能再拿它当「未知 agent」样本；用仍无策略的 grok。
    const fixture = createEchoAgentScript("grok", { recordArgs: true });
    try {
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "grok",
        prompt: "stdin-still-works",
        timeoutMs: 30_000,
      });
      expect(out).toContain("stdin-still-works");
      const args = fixture.recordedArgs();
      expect(args).not.toContain("-p");
      expect(args).not.toContain("run");
      expect(args).not.toContain("exec");
    } finally {
      fixture.cleanup();
    }
  });

  // 这条钉住生产事故的修复方向：kimi 必须以 -p 启动（argv 模式），
  // 而不是退化成「裸 kimi」——裸 kimi 是交互式 TUI，在 Windows 上拿不到
  // console 时会直接以 `No Windows console found` 退出。
  it("kimi 以 -p 启动并把 prompt 作为 argv 传入（不再是 stdin 兜底）", async () => {
    const fixture = createEchoAgentScript("kimi", { recordArgs: true });
    try {
      await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "kimi",
        prompt: "kimi-argv-prompt",
        timeoutMs: 30_000,
      });
      const args = fixture.recordedArgs();
      expect(args).toContain("-p");
      expect(args).toContain("kimi-argv-prompt");
      expect(args).toContain("--output-format");
    } finally {
      fixture.cleanup();
    }
  });

  it("passes --model for agents with an argv strategy", async () => {
    const fixture = createEchoAgentScript("claude", { recordArgs: true });
    try {
      await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "claude",
        prompt: "p",
        model: "sonnet-4",
        timeoutMs: 30_000,
      });
      const args = fixture.recordedArgs();
      expect(args).toContain("--model");
      expect(args).toContain("sonnet-4");
    } finally {
      fixture.cleanup();
    }
  });

  it("does not inject --model for an agent with no argv strategy", async () => {
    // 现场（2026-10-25）：给不认 `--model` 的 CLI 追加参数，等于让它更早退出
    // （usage dump + exit 1），用户看到的是「进程异常退出」。没有策略就不猜。
    const fixture = createEchoAgentScript("grok", { recordArgs: true });
    try {
      await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "grok",
        prompt: "p",
        model: "m1",
        timeoutMs: 30_000,
      });
      expect(fixture.recordedArgs()).not.toContain("--model");
    } finally {
      fixture.cleanup();
    }
  });

  it("omits --model when not provided", async () => {
    const fixture = createEchoAgentScript("claude", { recordArgs: true });
    try {
      await runAgentTurn({ execPath: fixture.agentPath, prompt: "p", timeoutMs: 30_000 });
      expect(fixture.recordedArgs()).not.toContain("--model");
    } finally {
      fixture.cleanup();
    }
  });

  it("rejects on timeout", async () => {
    const fixture = createEchoAgentScript("claude", { hangMs: 8000 });
    try {
      await expect(
        runAgentTurn({ execPath: fixture.agentPath, prompt: "p", timeoutMs: 500 }),
      ).rejects.toThrow(/timeout/i);
    } finally {
      fixture.cleanup();
    }
  });

  it("truncates stdout beyond maxOutputBytes", async () => {
    const fixture = createEchoAgentScript("claude", { bigOutputBytes: 200_000 });
    try {
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        prompt: "p",
        timeoutMs: 30_000,
        maxOutputBytes: 1024,
      });
      expect(out.length).toBeLessThanOrEqual(1024);
      expect(out).toBe("a".repeat(1024));
    } finally {
      fixture.cleanup();
    }
  });

  it("keeps CJK characters intact when a code point splits across stdout chunks", async () => {
    const fixture = createEchoAgentScript("claude", { cjkSplit: true, cjkRepeat: 100 });
    try {
      const chunks: string[] = [];
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        prompt: "p",
        timeoutMs: 30_000,
        onChunk: (c) => { chunks.push(c); },
      });
      const expected = "你好".repeat(100);
      expect(out).toBe(expected);
      expect(out).not.toContain("\uFFFD");
      expect(chunks.join("")).toBe(out);
    } finally {
      fixture.cleanup();
    }
  });

  it("truncates CJK output on a byte cap without corrupting the cut", async () => {
    // 100 CJK chars = 300 UTF-8 bytes; cap 100 bytes holds 33 complete chars
    // (99 bytes: 16 full 你好 pairs + one 你); the 34th char's dangling lead
    // byte must be dropped, never decoded to U+FFFD.
    const fixture = createEchoAgentScript("claude", { cjkSplit: true, cjkRepeat: 50 });
    try {
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        prompt: "p",
        timeoutMs: 30_000,
        maxOutputBytes: 100,
      });
      expect(Buffer.byteLength(out, "utf8")).toBeLessThanOrEqual(100);
      expect(out).toBe("你好".repeat(16) + "你");
      expect(out).not.toContain("\uFFFD");
    } finally {
      fixture.cleanup();
    }
  });

  // 线上现场（2026-10-20 10:54，运维助手 opencode）：跑到第 7 个工具时 stdout
  // 超过缓冲上限，旧实现在超限处直接 return —— 后半轮（含最终回答）整段丢掉，
  // 却仍然 resolve 出一个「completed」，前端于是把半句话当完整回答展示。
  // 现在缓冲满了只停止攒原始字节，解析继续，正文照样能拿到结尾。
  it("缓冲被截断后仍把后半轮的正文交出来，而不是只交前缀", async () => {
    const pad = JSON.stringify({ type: "reasoning", part: { id: "r1", type: "reasoning", text: "x".repeat(3_000) } });
    const answer = JSON.stringify({ type: "text", part: { id: "p1", type: "text", text: "最终答案：模型列表已拉到。" } });
    const fixture = createEchoAgentScript("opencode", { stdoutText: `${pad}\n${pad}\n${answer}\n` });
    try {
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "opencode",
        prompt: "列出可用模型列表",
        timeoutMs: 30_000,
        maxOutputBytes: 1024,
        onReasoningChunk: () => {},
      });
      expect(out).toBe("最终答案：模型列表已拉到。");
    } finally {
      fixture.cleanup();
    }
  });

  // 缓冲截断可以继续解析；但输出到硬上限就是这一轮已经不可信（agent 在喷日志），
  // 这时必须**失败**收尾，并把已产出的正文带出去，绝不能假装答完。
  it("输出超过硬上限时以 output_truncated 失败收尾，并带上已产出的正文", async () => {
    const pad = JSON.stringify({ type: "text", part: { id: "p1", type: "text", text: "半句话" } });
    const fixture = createEchoAgentScript("opencode", { stdoutText: pad.repeat(400) });
    try {
      await expect(runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "opencode",
        prompt: "p",
        timeoutMs: 30_000,
        maxOutputBytes: 512,
        hardOutputBytes: 2_048,
        onReasoningChunk: () => {},
      })).rejects.toMatchObject({ code: "output_truncated" });
    } finally {
      fixture.cleanup();
    }
  });

  // openclaw 信封自己说「这轮被中止 / 被截断」时，正文通常只剩半句前言。
  // 必须判失败（agent_incomplete），否则前端把它当完整回答展示。
  it("openclaw 信封说本轮被中止时判失败，并保留半截正文", async () => {
    const envelope = JSON.stringify({
      payloads: [{ text: "我先看看有哪些技能：", isReasoning: false }],
      meta: { aborted: true, completion: { finishReason: "aborted" }, finalAssistantVisibleText: "我先看看有哪些技能：" },
    });
    const fixture = createEchoAgentScript("openclaw", { stdoutText: envelope });
    try {
      await expect(runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "openclaw",
        prompt: "列出可用模型列表",
        timeoutMs: 30_000,
      })).rejects.toMatchObject({ code: "agent_incomplete", partialText: "我先看看有哪些技能：" });
    } finally {
      fixture.cleanup();
    }
  });

  it("openclaw 正常收尾（finishReason=stop）仍然算完成", async () => {
    const envelope = JSON.stringify({
      payloads: [{ text: "已经重启完成。" }],
      meta: { aborted: false, completion: { finishReason: "stop" }, finalAssistantVisibleText: "已经重启完成。" },
    });
    const fixture = createEchoAgentScript("openclaw", { stdoutText: envelope });
    try {
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "openclaw",
        prompt: "重启一下",
        timeoutMs: 30_000,
      });
      expect(out).toBe("已经重启完成。");
    } finally {
      fixture.cleanup();
    }
  });

  // 现场（2026-10-20 10:54，运维助手 opencode）：问「列出可用模型列表」，模型先
  // 说「已登录。拉取模型列表：」，接着 bash 报错、进程 exit 0，之后再没吐过一个
  // 字。opencode 自己的会话库可确认：该轮最后一条 part 是 bash(error)，其后既无
  // 文本也无新步骤。我们把 exit 0 当成「答完了」，前端就把这半句前言当完整回答。
  it("撞在失败的工具上就没下文时判 agent_incomplete", async () => {
    const lines = [
      JSON.stringify({ type: "text", part: { id: "p1", type: "text", text: "已登录。拉取模型列表：" } }),
      JSON.stringify({ type: "tool_use", part: { type: "tool", tool: "bash", callID: "c1", state: { status: "error", input: { command: "arkcli models list" } } } }),
    ].join("\n") + "\n";
    const fixture = createEchoAgentScript("opencode", { stdoutText: lines });
    try {
      await expect(runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "opencode",
        prompt: "列出可用模型列表",
        timeoutMs: 30_000,
        onReasoningChunk: () => {},
        onActivity: () => {},
      })).rejects.toMatchObject({
        code: "agent_incomplete",
        partialText: "已登录。拉取模型列表：",
        // failedStep 结构化带出失败命令：im.ts 靠它自动补跑一轮。
        failedStep: { label: "执行命令", command: "arkcli models list" },
      });
    } finally {
      fixture.cleanup();
    }
  });

  it("工具失败之后模型补了说明，就不算没答完", async () => {
    const lines = [
      JSON.stringify({ type: "tool_use", part: { type: "tool", tool: "bash", callID: "c1", state: { status: "error" } } }),
      JSON.stringify({ type: "text", part: { id: "p2", type: "text", text: "命令报了 ENOENT，这机器上没装 arkcli。" } }),
    ].join("\n") + "\n";
    const fixture = createEchoAgentScript("opencode", { stdoutText: lines });
    try {
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "opencode",
        prompt: "列出可用模型列表",
        timeoutMs: 30_000,
        onReasoningChunk: () => {},
        onActivity: () => {},
      });
      expect(out).toContain("没装 arkcli");
    } finally {
      fixture.cleanup();
    }
  });

  it("最后一步成功但没写结尾时不算没答完（不误报）", async () => {
    const lines = [
      JSON.stringify({ type: "text", part: { id: "p1", type: "text", text: "已重启。" } }),
      JSON.stringify({ type: "tool_use", part: { type: "tool", tool: "bash", callID: "c1", state: { status: "completed" } } }),
    ].join("\n") + "\n";
    const fixture = createEchoAgentScript("opencode", { stdoutText: lines });
    try {
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "opencode",
        prompt: "重启",
        timeoutMs: 30_000,
        onReasoningChunk: () => {},
        onActivity: () => {},
      });
      expect(out).toContain("已重启");
    } finally {
      fixture.cleanup();
    }
  });

  it("rejects immediately when the signal is already aborted", async () => {    const fixture = createEchoAgentScript("claude");
    try {
      const controller = new AbortController();
      controller.abort();
      await expect(
        runAgentTurn({ execPath: fixture.agentPath, prompt: "p", timeoutMs: 30_000, signal: controller.signal }),
      ).rejects.toThrow(/abort/i);
    } finally {
      fixture.cleanup();
    }
  });

  it("rejects with stderr context on nonzero exit", async () => {
    const fixture = createEchoAgentScript("claude", { failWith: { code: 3, message: "agent-boom" } });
    try {
      await expect(
        runAgentTurn({ execPath: fixture.agentPath, prompt: "p", timeoutMs: 30_000 }),
      ).rejects.toThrow(/agent-boom/);
    } finally {
      fixture.cleanup();
    }
  });

  // ── 事故核心回归（2026-10-25 私有设备私聊）──────────────────────────────
  // kimi 的登录横幅打在 **stdout** 上、stderr 为空，然后 exit 1。旧实现只拼
  // stderr，错误串是 "agent exited with code 1: "，前端于是说「进程异常退出，
  // 没有任何输出」，classifyTurnError 也归成 agent_failed。用户永远不知道该去
  // 登录/换 key —— 这条用例就是钉住「原因必须能被看见、被分类」。
  it("把 stdout 尾巴并进错误串，kimi 的登录横幅不再变成「没有任何输出」", async () => {
    const banner = [
      "Kimi Code CLI",
      "Session: abc",
      "Tip: send /login to use Kimi for Code",
    ].join("\n");
    const fixture = createStdoutFailureAgent("kimi", `${banner}\n`, 1);
    try {
      const error = await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "kimi",
        prompt: "hi",
        timeoutMs: 30_000,
      }).then(() => null, (err: Error) => err);

      expect(error).toBeInstanceOf(Error);
      const message = (error as Error).message;
      expect(message).toContain("agent exited with code 1");
      expect(message).toContain("[stdout tail]");
      expect(message).toContain("Tip: send /login to use Kimi for Code");
      // 分类器必须认得出这是鉴权/未登录，不是「未知崩溃」。
      expect(classifyTurnError(message)).toBe("model_auth_failed");
    } finally {
      fixture.cleanup();
    }
  });

  it("stdout 为空时不追加空的 [stdout tail] 段", async () => {
    const fixture = createEchoAgentScript("kimi", { failWith: { code: 1, message: "agent-boom" } });
    try {
      const error = await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "kimi",
        prompt: "p",
        timeoutMs: 30_000,
      }).then(() => null, (err: Error) => err);

      const message = (error as Error).message;
      expect(message).toContain("agent-boom");
      expect(message).not.toContain("[stdout tail]");
    } finally {
      fixture.cleanup();
    }
  });

  it("只带 stdout 末尾 800 字符，不把整份输出塞进错误串", async () => {
    // 洪峰场景：几 MB 的 stdout 不能整体进错误串（会撑爆日志与 wire 帧）。
    const stdout = `EARLY_MARKER${"x".repeat(4000)}\nTip: send /login to use Kimi for Code\n`;
    const fixture = createStdoutFailureAgent("kimi", stdout, 1);
    try {
      const error = await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "kimi",
        prompt: "hi",
        timeoutMs: 30_000,
      }).then(() => null, (err: Error) => err);

      const message = (error as Error).message;
      // 真正的结尾（含可分类的提示）必须还在
      expect(message).toContain("Tip: send /login to use Kimi for Code");
      // 头部早就不该在
      expect(message).not.toContain("EARLY_MARKER");
      // 尾巴 800 + 前缀措辞，整体远小于原始 stdout
      expect(message.length).toBeLessThan(1_000);
      expect(classifyTurnError(message)).toBe("model_auth_failed");
    } finally {
      fixture.cleanup();
    }
  });

  it("streams chunks through onChunk before resolving", async () => {
    const fixture = createEchoAgentScript("claude");
    try {
      let chunks = "";
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        prompt: "stream me",
        timeoutMs: 30_000,
        onChunk: (c) => { chunks += c; },
      });
      expect(out).toContain("stream me");
      expect(chunks).toContain("stream me");
    } finally {
      fixture.cleanup();
    }
  });

  // openclaw 的 stdout 是 `agent --local --json` 的完整信封（含输入提示词全文
  // 与执行 trace）。一旦把原始增量当正文回调出去，web 的打字气泡里就会出现
  // 一整坨 JSON；而终帧与兜底广播给的是 extractOpenclawReply 提取后的正文，
  // 两者内容不等，web 的 streamEchoIndex 按内容相等去重就失效，聊天里于是
  // 出现一对「内容重复」的气泡（一条 JSON、一条正文）。所以 openclaw 必须
  // 跳过增量回调，只靠终帧一次性给出正文。
  it("openclaw 的 --json 信封不进增量回调，终值仍是提取后的正文", async () => {
    const envelope = JSON.stringify({
      payloads: [{ text: "正文一号", mediaUrl: null }],
      meta: {
        finalPromptText: "[group chat] 请以「rc_node_x_openclaw」的身份发言，只输出发言正文。",
        finalAssistantVisibleText: "正文一号",
        finalAssistantRawText: "正文一号",
        replyInvalid: false,
        livenessState: "working",
        executionTrace: { winnerProvider: "openai", winnerModel: "gpt-5.6-sol" },
      },
    });
    const fixture = createEchoAgentScript("openclaw", { stdoutText: envelope });
    try {
      let chunks = "";
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        agentName: "openclaw",
        prompt: "p",
        timeoutMs: 30_000,
        onChunk: (c) => { chunks += c; },
      });
      // 信封一个字符都不许流进增量 —— 它就是气泡里那坨 JSON 的来源。
      expect(chunks).toBe("");
      // 但终值仍然要拿到提取后的正文，不能因此变成空回复。
      expect(out).toBe("正文一号");
    } finally {
      fixture.cleanup();
    }
  });

  it("rejects promptly when the signal aborts", async () => {
    const fixture = createEchoAgentScript("claude", { hangMs: 8000 });
    try {
      const controller = new AbortController();
      const pending = runAgentTurn({
        execPath: fixture.agentPath,
        prompt: "p",
        timeoutMs: 30_000,
        signal: controller.signal,
      });
      setTimeout(() => controller.abort(), 300);
      await expect(pending).rejects.toThrow(/abort/i);
    } finally {
      fixture.cleanup();
    }
  });
});

describe("Windows .cmd shim 穿透（cmd.exe 会截断多行提示词）", () => {
  it("能认出 npm shim 的真实目标：真 exe 直接跑，脚本交给同目录 node.exe", async () => {
    const { mkdtemp, mkdir, writeFile } = await import("node:fs/promises");
    const { tmpdir } = await import("node:os");
    const { join } = await import("node:path");
    const { existsSync } = await import("node:fs");
    const dir = await mkdtemp(join(tmpdir(), "cm-shim-"));

    // opencode 形态：目标本身就是 exe
    const binDir = join(dir, "node_modules", "pkg", "bin");
    await mkdir(binDir, { recursive: true });
    const exeTarget = join(binDir, "cli.exe");
    await writeFile(exeTarget, "");
    const exeShim = join(dir, "opencode.cmd");
    await writeFile(
      exeShim,
      ['@ECHO off', 'GOTO start', ':start', 'SETLOCAL', `"%dp0%\\node_modules\\pkg\\bin\\cli.exe"   %*`].join("\r\n"),
    );
    expect(resolveWindowsShim(exeShim)).toEqual({ file: exeTarget, prefixArgs: [] });

    // openclaw 形态：目标是 .mjs，且 shim 同目录有 node.exe
    const nodeExe = join(dir, "node.exe");
    await writeFile(nodeExe, "");
    const mjsTarget = join(dir, "node_modules", "oc", "openclaw.mjs");
    await mkdir(join(dir, "node_modules", "oc"), { recursive: true });
    await writeFile(mjsTarget, "");
    const mjsShim = join(dir, "openclaw.cmd");
    await writeFile(
      mjsShim,
      [
        '@ECHO off',
        'IF EXIST "%dp0%\\node.exe" (',
        '  SET "_prog=%dp0%\\node.exe"',
        ') ELSE (',
        '  SET "_prog=node"',
        ')',
        `endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & "%_prog%"  "%dp0%\\node_modules\\oc\\openclaw.mjs" %*`,
      ].join("\r\n"),
    );
    const resolved = resolveWindowsShim(mjsShim);
    expect(resolved?.file).toBe(nodeExe);
    expect(resolved?.prefixArgs).toEqual([mjsTarget]);
    expect(existsSync(resolved!.prefixArgs[0])).toBe(true);

    // 目标不存在 → 不硬来，返回 null（调用方退回 cmd.exe）
    const broken = join(dir, "broken.cmd");
    await writeFile(broken, '"%dp0%\\node_modules\\nope\\gone.exe" %*');
    expect(resolveWindowsShim(broken)).toBeNull();

    // 非 npm shim（认不出目标）→ null
    const weird = join(dir, "weird.cmd");
    await writeFile(weird, "@ECHO off\r\necho hello\r\n");
    expect(resolveWindowsShim(weird)).toBeNull();
  });

  it("buildSpawnInvocation：解析成功就不再经 cmd.exe；失败则退回 cmd.exe 并抹掉换行", async () => {
    const { mkdtemp, mkdir, writeFile } = await import("node:fs/promises");
    const { tmpdir } = await import("node:os");
    const { join } = await import("node:path");
    const dir = await mkdtemp(join(tmpdir(), "cm-spawn-"));
    const binDir = join(dir, "node_modules", "pkg", "bin");
    await mkdir(binDir, { recursive: true });
    const target = join(binDir, "cli.exe");
    await writeFile(target, "");
    const shim = join(dir, "pkg.cmd");
    await writeFile(shim, `"%dp0%\\node_modules\\pkg\\bin\\cli.exe"   %*`);

    const viaReal = buildSpawnInvocation(shim, ["run", "line1\nline2"]);
    expect(viaReal.file).toBe(target);
    expect(viaReal.args).toEqual(["run", "line1\nline2"]);

    const unresolvable = join(dir, "plain.cmd");
    await writeFile(unresolvable, "@ECHO off\r\necho hi\r\n");
    const viaCmd = buildSpawnInvocation(unresolvable, ["run", "line1\nline2"]);
    expect(viaCmd.file).toBe("cmd.exe");
    expect(viaCmd.args.every((a) => !a.includes("\n"))).toBe(true);
  });
})
