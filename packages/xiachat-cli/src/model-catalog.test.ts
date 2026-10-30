import { describe, expect, it, vi } from "vitest";
import {
  buildModelCatalogResponse,
  catalogFromRoutes,
  createModelCatalogLoader,
  EMPTY_MODEL_CATALOG,
  modelListSpec,
  parseModelCatalogOutput,
  parseModelCatalogRequest,
  parseModelListText,
  parseOpenclawModelListJson,
} from "./model-catalog.js";
import { MessageDispatcher, type InboundIMMessage } from "./im.js";

const OPENCODE_OUTPUT = [
  "opencode/big-pickle",
  "opencode/exo-free",
  "agent-plan/glm-5.3",
  "agent-plan/kimi-k3",
  "quukk/deepseek-v4-pro",
  "",
].join("\n");

// Real shape from OpenClaw 2026.9.2 (`openclaw models list`).
const OPENCLAW_TABLE = [
  "Config warnings: plugins.entries.pcmgr: plugin not found (stale config entry ignored)",
  "Model                                      Input      Ctx         Local Auth  Tags",
  "openai/gpt-5.6-sol                         -          -           yes   yes   default",
  "openai/gpt-5.6-mini                        -          -           yes   yes",
].join("\n");

const OPENCLAW_JSON = JSON.stringify({
  count: 2,
  models: [
    { key: "openai/gpt-5.6-sol", name: "GPT-5.6 Sol", tags: ["default"], local: true },
    { key: "openai/gpt-5.6-mini", name: "GPT-5.6 Mini", tags: [] },
  ],
});

describe("parseModelCatalogRequest", () => {
  it("accepts the web's v2 request", () => {
    const request = parseModelCatalogRequest(
      JSON.stringify({
        msg_type: "discussion_model_catalog_request",
        protocolVersion: 2,
        requestId: "req-1",
        timestamp: 1_700_000_000_000,
      }),
    );
    expect(request).toEqual({
      msg_type: "discussion_model_catalog_request",
      protocolVersion: 2,
      requestId: "req-1",
      timestamp: 1_700_000_000_000,
    });
  });

  it("carries an explicit refresh flag, and omits it otherwise", () => {
    const base = {
      msg_type: "discussion_model_catalog_request",
      protocolVersion: 2,
      requestId: "req-9",
    };
    // 老服务端不发这个键：解析结果里不该凭空多出一个 false，否则响应/日志/diff
    // 都会与历史记录不一致。
    expect(parseModelCatalogRequest(JSON.stringify(base))?.refresh).toBeUndefined();
    expect(parseModelCatalogRequest(JSON.stringify({ ...base, refresh: true }))?.refresh).toBe(true);
    // 非布尔一律当没提：一个 "yes" 不该让设备跳过缓存判定。
    expect(parseModelCatalogRequest(JSON.stringify({ ...base, refresh: "yes" }))?.refresh).toBeUndefined();
  });

  it("accepts a request without a timestamp and rejects a malformed one", () => {
    expect(parseModelCatalogRequest(JSON.stringify({
      msg_type: "discussion_model_catalog_request", protocolVersion: 2, requestId: "req-2",
    }))).toEqual({
      msg_type: "discussion_model_catalog_request", protocolVersion: 2, requestId: "req-2",
    });
    expect(parseModelCatalogRequest(JSON.stringify({
      msg_type: "discussion_model_catalog_request", protocolVersion: 2, requestId: "req-3", timestamp: "soon",
    }))).toBeNull();
    expect(parseModelCatalogRequest(JSON.stringify({
      msg_type: "discussion_model_catalog_request", protocolVersion: 2, requestId: "req-4", timestamp: 1.5,
    }))).toBeNull();
  });

  it("rejects other commands, wrong versions, bad ids and junk", () => {
    expect(parseModelCatalogRequest("not json")).toBeNull();
    expect(parseModelCatalogRequest(JSON.stringify({ request_id: "r", service: "discussion", action: "your_turn" }))).toBeNull();
    expect(parseModelCatalogRequest(JSON.stringify({
      msg_type: "discussion_model_catalog_request", protocolVersion: 3, requestId: "r",
    }))).toBeNull();
    expect(parseModelCatalogRequest(JSON.stringify({
      msg_type: "discussion_model_catalog_request", protocolVersion: 2, requestId: "",
    }))).toBeNull();
    expect(parseModelCatalogRequest(JSON.stringify({
      msg_type: "discussion_model_catalog_request", protocolVersion: 2, requestId: "x".repeat(129),
    }))).toBeNull();
  });
});

describe("catalogFromRoutes", () => {
  it("groups by provider, sorts and dedupes", () => {
    const catalog = catalogFromRoutes(["b/two", "a/one", "b/two", "a/one", "b/one"]);
    expect(catalog.providers.map((p) => p.id)).toEqual(["a", "b"]);
    expect(catalog.providers[0].models.map((m) => m.id)).toEqual(["one"]);
    expect(catalog.providers[1].models.map((m) => m.id)).toEqual(["one", "two"]);
  });

  it("drops malformed routes and unreachable defaults", () => {
    const catalog = catalogFromRoutes(["no-slash", "a/b/c", " / ", "ok/fine"], "missing/model");
    expect(catalog.providers).toEqual([
      { id: "ok", name: "ok", models: [{ id: "fine", name: "fine" }] },
    ]);
    expect(catalog.defaultModel).toBeNull();
  });

  it("keeps a default that is present in the catalog", () => {
    expect(catalogFromRoutes(["a/one", "a/two"], "a/two").defaultModel).toBe("a/two");
  });
});

describe("parseModelListText", () => {
  it("reads a flat provider/model list (opencode)", () => {
    const catalog = parseModelListText(OPENCODE_OUTPUT);
    expect(catalog.providers.map((p) => p.id)).toEqual(["agent-plan", "opencode", "quukk"]);
    expect(catalog.providers[1].models).toEqual([
      { id: "big-pickle", name: "big-pickle" },
      { id: "exo-free", name: "exo-free" },
    ]);
    expect(catalog.defaultModel).toBeNull();
  });

  it("reads the openclaw table including its default tag", () => {
    const catalog = parseModelListText(OPENCLAW_TABLE);
    expect(catalog.defaultModel).toBe("openai/gpt-5.6-sol");
    expect(catalog.providers).toEqual([
      {
        id: "openai",
        name: "openai",
        models: [
          { id: "gpt-5.6-mini", name: "gpt-5.6-mini" },
          { id: "gpt-5.6-sol", name: "gpt-5.6-sol" },
        ],
      },
    ]);
  });

  it("reads a labelled Default line", () => {
    const catalog = parseModelListText("Default       : openai/gpt-5.6-sol\nProvider      : openai");
    expect(catalog.defaultModel).toBe("openai/gpt-5.6-sol");
  });

  it("strips ANSI colour codes", () => {
    const catalog = parseModelListText("\u001b[32mopencode/big-pickle\u001b[0m");
    expect(catalog.providers[0].models[0].id).toBe("big-pickle");
  });
});

describe("parseOpenclawModelListJson", () => {
  it("parses the JSON envelope and keeps display names", () => {
    const catalog = parseOpenclawModelListJson(OPENCLAW_JSON);
    expect(catalog?.defaultModel).toBe("openai/gpt-5.6-sol");
    expect(catalog?.providers[0].models).toEqual([
      { id: "gpt-5.6-mini", name: "GPT-5.6 Mini" },
      { id: "gpt-5.6-sol", name: "GPT-5.6 Sol" },
    ]);
  });

  it("skips leading stderr noise before the JSON body", () => {
    const catalog = parseOpenclawModelListJson(`Config warnings: something\n${OPENCLAW_JSON}`);
    expect(catalog?.providers.length).toBe(1);
  });

  it("returns null for non-JSON output", () => {
    expect(parseOpenclawModelListJson("opencode/big-pickle")).toBeNull();
    expect(parseOpenclawModelListJson("{")).toBeNull();
  });
});

describe("parseModelCatalogOutput", () => {
  it("prefers openclaw JSON, falls back to the table", () => {
    expect(parseModelCatalogOutput("openclaw", OPENCLAW_JSON).defaultModel).toBe("openai/gpt-5.6-sol");
    expect(parseModelCatalogOutput("openclaw", OPENCLAW_TABLE).defaultModel).toBe("openai/gpt-5.6-sol");
    expect(parseModelCatalogOutput("opencode", OPENCODE_OUTPUT).providers.length).toBe(3);
  });
});

describe("buildModelCatalogResponse", () => {
  it("emits exactly the keys the web normalizer accepts", () => {
    const request = { msg_type: "discussion_model_catalog_request" as const, protocolVersion: 2 as const, requestId: "req-1" };
    const body = JSON.parse(
      buildModelCatalogResponse(request, catalogFromRoutes(["a/one"]), () => 42),
    );
    expect(Object.keys(body).sort()).toEqual([
      "defaultModel", "msg_type", "protocolVersion", "providers", "requestId", "timestamp",
    ]);
    expect(body).toEqual({
      msg_type: "discussion_model_catalog_response",
      protocolVersion: 2,
      requestId: "req-1",
      defaultModel: null,
      providers: [{ id: "a", name: "a", models: [{ id: "one", name: "one" }] }],
      timestamp: 42,
    });
  });
});

describe("modelListSpec", () => {
  it("knows the agents with a provider/model catalog command", () => {
    expect(modelListSpec("opencode")?.argv).toEqual(["models"]);
    expect(modelListSpec("openclaw")?.argv).toEqual(["models", "list", "--json"]);
    expect(modelListSpec("codex")).toBeNull();
    expect(modelListSpec(undefined)).toBeNull();
  });
});

describe("createModelCatalogLoader", () => {
  // `readConfig: () => null` 不是可选项：opencode 现在有配置解析器，不注入就会
  // 读**跑测试那台机器**的 ~/.config/opencode/opencode.json，测试结果取决于
  // 开发者的 home（CI 与本机结论不一致）。
  const noConfig = () => null;

  it("runs the agent command once and then serves the cache", async () => {
    const runCommand = vi.fn().mockResolvedValue(OPENCODE_OUTPUT);
    const loader = createModelCatalogLoader({ agentName: "opencode", runCommand, readConfig: noConfig });

    const first = await loader.load();
    expect(runCommand).toHaveBeenCalledTimes(1);
    expect(runCommand.mock.calls[0][0]).toEqual(["models"]);

    const second = await loader.load();
    expect(second).toBe(first);
    expect(runCommand).toHaveBeenCalledTimes(1);
  });

  it("serves a warm cache without refetching until the ttl expires", async () => {
    let clock = 1_000;
    const runCommand = vi.fn().mockResolvedValue(OPENCODE_OUTPUT);
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand,
      readConfig: noConfig,
      ttlMs: 60_000,
      now: () => clock,
    });

    await loader.load();
    expect(runCommand).toHaveBeenCalledTimes(1);

    clock += 30_000;
    await loader.load();
    expect(runCommand).toHaveBeenCalledTimes(1);

    clock += 60_000;
    await loader.load();
    expect(runCommand).toHaveBeenCalledTimes(2);
  });

  // 管理端「设备改了模型但后台不更新」的根因就在这里：老实现是 cache-first +
  // 5 分钟 TTL 的后台刷新，而且**这一轮仍返回旧的那份**，所以要么看到 5 分钟前的
  // 清单，要么过期后第一次请求还是旧的（刷新在后台跑），得点第二次。
  it("re-enumerates before answering once the cache outlives maxStaleMs", async () => {
    let clock = 1_000;
    let output = OPENCODE_OUTPUT;
    const runCommand = vi.fn().mockImplementation(async () => output);
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand,
      readConfig: noConfig,
      maxStaleMs: 20_000,
      now: () => clock,
    });

    const first = await loader.load();
    expect(first.providers.map((provider) => provider.id)).toEqual(["agent-plan", "opencode", "quukk"]);

    // 设备上新增了一个供应商。缓存过期后，**这一轮**就必须看到它。
    output = `${OPENCODE_OUTPUT}newco/new-model\n`;
    clock += 21_000;
    const second = await loader.load();
    expect(second.providers.map((provider) => provider.id)).toContain("newco");
    expect(runCommand).toHaveBeenCalledTimes(2);
  });

  it("still serves a fresh cache without re-enumerating", async () => {
    let clock = 1_000;
    const runCommand = vi.fn().mockResolvedValue(OPENCODE_OUTPUT);
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand,
      readConfig: noConfig,
      maxStaleMs: 20_000,
      now: () => clock,
    });

    await loader.load();
    clock += 5_000;
    await loader.load();
    expect(runCommand).toHaveBeenCalledTimes(1);
  });

  it("re-enumerates on demand even when the cache is fresh (force)", async () => {
    let output = OPENCODE_OUTPUT;
    const runCommand = vi.fn().mockImplementation(async () => output);
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand,
      readConfig: noConfig,
      maxStaleMs: 20_000,
    });

    await loader.load();
    output = `${OPENCODE_OUTPUT}newco/new-model\n`;
    const forced = await loader.load({ force: true });
    expect(forced.providers.map((provider) => provider.id)).toContain("newco");
    expect(runCommand).toHaveBeenCalledTimes(2);
  });

  it("keeps the previous catalog when the awaited re-enumeration fails", async () => {
    let clock = 1_000;
    const runCommand = vi
      .fn()
      .mockImplementationOnce(async () => OPENCODE_OUTPUT)
      .mockRejectedValueOnce(new Error("opencode models timed out"));
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand,
      readConfig: noConfig,
      maxStaleMs: 20_000,
      now: () => clock,
    });

    const first = await loader.load();
    expect(first.providers).toHaveLength(3);

    clock += 21_000;
    const second = await loader.load();
    // 一次失败的重枚举不能把已知目录冲成空的：宁可给旧清单，也不能让面板突然空掉。
    expect(second.providers.map((provider) => provider.id)).toEqual(["agent-plan", "opencode", "quukk"]);
  });

  it("never rejects and stays empty for agents without a list command", async () => {    const runCommand = vi.fn();
    const loader = createModelCatalogLoader({
      agentName: "codex",
      runCommand,
      readConfig: () => null,
    });
    await expect(loader.load()).resolves.toBe(EMPTY_MODEL_CATALOG);
    expect(runCommand).not.toHaveBeenCalled();
  });

  it("falls back to the empty catalog when the command fails", async () => {
    const runCommand = vi.fn().mockRejectedValue(new Error("boom"));
    const logger = vi.fn();
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand,
      readConfig: noConfig,
      logger,
    });
    await expect(loader.load()).resolves.toBe(EMPTY_MODEL_CATALOG);
    expect(logger).toHaveBeenCalledWith(expect.stringContaining("boom"));
  });

  it("deduplicates concurrent requests", async () => {
    const runCommand = vi.fn().mockImplementation(() => new Promise((resolve) => setTimeout(() => resolve(OPENCODE_OUTPUT), 5)));
    const loader = createModelCatalogLoader({ agentName: "opencode", runCommand, readConfig: noConfig });
    const [first, second] = await Promise.all([loader.load(), loader.load()]);
    expect(runCommand).toHaveBeenCalledTimes(1);
    expect(first).toBe(second);
  });
});

describe("createModelCatalogLoader with provider endpoints", () => {
  // openclaw's own list is the *built-in* openai catalogue, which is fiction
  // for a provider that points at a local gateway. Only one entry here so the
  // replacement is easy to see.
  const BOGUS_OPENCLAW_LIST = JSON.stringify({
    count: 1,
    models: [{ key: "openai/gpt-5.6-sol", name: "GPT-5.6 Sol", tags: ["default"] }],
  });
  const OPENCLAW_ENDPOINT = { id: "openai", baseUrl: "http://127.0.0.1:49830/v1" };

  it("unions the CLI list with the endpoint's live models", async () => {
    const runCommand = vi.fn().mockResolvedValue(BOGUS_OPENCLAW_LIST);
    const loader = createModelCatalogLoader({
      agentName: "openclaw",
      runCommand,
      readConfig: () => ({ endpoints: [OPENCLAW_ENDPOINT], defaultModel: null }),
      probe: async () => ["glm-5.2", "deepseek-v4-pro", "kimi-k2.7-code"],
    });

    const catalog = await loader.load();
    // The CLI's configured/OAuth entry survives (with its display name) and
    // the endpoint's pass-through models are added alongside it.
    expect(catalog.providers).toEqual([
      {
        id: "openai",
        name: "openai",
        models: [
          { id: "deepseek-v4-pro", name: "deepseek-v4-pro" },
          { id: "glm-5.2", name: "glm-5.2" },
          { id: "gpt-5.6-sol", name: "GPT-5.6 Sol" },
          { id: "kimi-k2.7-code", name: "kimi-k2.7-code" },
        ],
      },
    ]);
  });

  it("lets the agent's configured display name win over the raw provider id", async () => {
    const loader = createModelCatalogLoader({
      agentName: "openclaw",
      runCommand: vi.fn().mockResolvedValue(BOGUS_OPENCLAW_LIST),
      readConfig: () => ({
        endpoints: [{ ...OPENCLAW_ENDPOINT, name: "Local Gateway" }],
        defaultModel: null,
      }),
      probe: async () => ["glm-5.2"],
    });

    const catalog = await loader.load();
    expect(catalog.providers[0].name).toBe("Local Gateway");
  });

  it("serves codex/hermes purely from their configured endpoint", async () => {
    const runCommand = vi.fn();
    const loader = createModelCatalogLoader({
      agentName: "codex",
      runCommand,
      readConfig: () => ({
        endpoints: [{ id: "quukk_direct", name: "Quukk Direct", baseUrl: "http://127.0.0.1:49830/v1" }],
        defaultModel: "quukk_direct/glm-5.2",
      }),
      probe: async () => ["glm-5.2", "k3"],
    });

    const catalog = await loader.load();
    expect(runCommand).not.toHaveBeenCalled();
    expect(catalog.defaultModel).toBe("quukk_direct/glm-5.2");
    expect(catalog.providers).toEqual([
      {
        id: "quukk_direct",
        name: "Quukk Direct",
        models: [
          { id: "glm-5.2", name: "glm-5.2" },
          { id: "k3", name: "k3" },
        ],
      },
    ]);
  });

  it("keeps the CLI list when the endpoint is unreachable", async () => {
    const loader = createModelCatalogLoader({
      agentName: "openclaw",
      runCommand: vi.fn().mockResolvedValue(BOGUS_OPENCLAW_LIST),
      readConfig: () => ({ endpoints: [OPENCLAW_ENDPOINT], defaultModel: null }),
      probe: async () => null,
    });

    const catalog = await loader.load();
    expect(catalog.providers[0].models.map((m) => m.id)).toEqual(["gpt-5.6-sol"]);
  });

  it("keeps the CLI list when the endpoint answers with no usable ids", async () => {
    const loader = createModelCatalogLoader({
      agentName: "openclaw",
      runCommand: vi.fn().mockResolvedValue(BOGUS_OPENCLAW_LIST),
      readConfig: () => ({ endpoints: [OPENCLAW_ENDPOINT], defaultModel: null }),
      probe: async () => [],
    });

    const catalog = await loader.load();
    expect(catalog.providers[0].models.map((m) => m.id)).toEqual(["gpt-5.6-sol"]);
  });

  it("drops endpoint ids that cannot form a provider/model route", async () => {
    const loader = createModelCatalogLoader({
      agentName: "codex",
      runCommand: vi.fn(),
      readConfig: () => ({ endpoints: [{ id: "p", baseUrl: "http://127.0.0.1:1/v1" }], defaultModel: null }),
      probe: async () => ["good", "bad model", "has/slash", "", "also-good"],
    });

    const catalog = await loader.load();
    expect(catalog.providers[0].models.map((m) => m.id)).toEqual(["also-good", "good"]);
  });

  it("still yields the empty catalog when only an unusable endpoint exists", async () => {
    const loader = createModelCatalogLoader({
      agentName: "hermes",
      runCommand: vi.fn(),
      readConfig: () => ({
        endpoints: [{ id: "quukk", baseUrl: "https://llmapi.quukk.com/v1" }],
        defaultModel: "quukk/glm-5.3",
      }),
      probe: async () => null,
    });

    await expect(loader.load()).resolves.toBe(EMPTY_MODEL_CATALOG);
  });

  it("discards a configured default that is not in the catalog", async () => {
    const loader = createModelCatalogLoader({
      agentName: "codex",
      runCommand: vi.fn(),
      readConfig: () => ({
        endpoints: [{ id: "quukk_direct", baseUrl: "http://127.0.0.1:1/v1" }],
        defaultModel: "quukk_direct/not-served",
      }),
      probe: async () => ["glm-5.2"],
    });

    const catalog = await loader.load();
    expect(catalog.providers[0].models.map((m) => m.id)).toEqual(["glm-5.2"]);
    expect(catalog.defaultModel).toBeNull();
  });
});

// 配置文件里**显式声明**的模型清单（opencode 的 `provider.<id>.models`）。
// 这是本轮的根因修复：现场「私有设备拿不到模型目录 + 对话报进程异常退出」，
// 因为目录只能靠 `opencode models`（3-5s 冷启，可能超时/失败）和端点 probe
// （最多 8s 空等）两条网络路径 —— 而 web 只等 8s。
describe("createModelCatalogLoader with config-declared models", () => {
  const QUUKK_ENDPOINT = { id: "quukk", baseUrl: "https://llmapi.example.com/v1" };

  it("serves the declared models without probing the declared endpoint", async () => {
    const probe = vi.fn(async () => {
      throw new Error("declared providers must not be probed");
    });
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand: vi.fn().mockResolvedValue(""),
      readConfig: () => ({
        endpoints: [QUUKK_ENDPOINT],
        defaultModel: "quukk/glm-5.2",
        declaredModels: { quukk: ["glm-5.2", "k3"] },
      }),
      probe,
    });

    const catalog = await loader.load();
    // 声明即事实：再 probe 一遍只可能拿到同一份清单的子集，白等最多 8s。
    expect(probe).not.toHaveBeenCalled();
    expect(catalog.providers).toEqual([
      {
        id: "quukk",
        name: "quukk",
        models: [
          { id: "glm-5.2", name: "glm-5.2" },
          { id: "k3", name: "k3" },
        ],
      },
    ]);
    expect(catalog.defaultModel).toBe("quukk/glm-5.2");
  });

  it("probes only the endpoints that declare no models of their own", async () => {
    const probe = vi.fn(async (endpoint: { id: string }) => [`${endpoint.id}-model`]);
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand: vi.fn().mockResolvedValue(""),
      readConfig: () => ({
        endpoints: [QUUKK_ENDPOINT, { id: "other", baseUrl: "https://other.example.com/v1" }],
        defaultModel: null,
        declaredModels: { quukk: ["glm-5.2"] },
      }),
      probe,
    });

    const catalog = await loader.load();
    expect(probe).toHaveBeenCalledTimes(1);
    expect(probe).toHaveBeenCalledWith(expect.objectContaining({ id: "other" }));
    expect(catalog.providers.map((provider) => provider.id)).toEqual(["other", "quukk"]);
    expect(catalog.providers[1].models.map((model) => model.id)).toEqual(["glm-5.2"]);
  });

  // 「配置声明了清单就别再问 CLI」这条捷径在 1.3.10 线上闯过祸：一台 opencode
  // 节点的 opencode.json 里只声明了 1 个模型，而 `opencode models` 能列出 93 条、
  // 覆盖整个 `opencode/*`，却一次都没被执行 —— 面板上只剩 `custom (1)`，用户看到
  // 的是「供应商只有一个」。声明是**下限**，CLI 的并集必须进来。
  it("unions the CLI list into the declared models instead of skipping the command", async () => {
    const runCommand = vi.fn().mockResolvedValue(
      "quukk/glm-5.2\nquukk/beam-free\nopencode/claude-sonnet-5\nopencode/gpt-5\n",
    );
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand,
      readConfig: () => ({
        endpoints: [],
        defaultModel: "quukk/glm-5.2",
        declaredModels: { quukk: ["glm-5.2", "glm-5.3", "k3"] },
      }),
    });

    const catalog = await loader.load();
    expect(runCommand).toHaveBeenCalledTimes(1);
    expect(catalog.providers.map((provider) => provider.id)).toEqual(["opencode", "quukk"]);
    // 声明的 3 个一个都不能少（CLI 只回报了其中 1 个），也不会被 CLI 那份覆盖。
    expect(catalog.providers[1].models.map((model) => model.id)).toEqual([
      "beam-free", "glm-5.2", "glm-5.3", "k3",
    ]);
    expect(catalog.defaultModel).toBe("quukk/glm-5.2");
  });

  // 预算语义：CLI 再慢也不能把**这一次请求**拖过面板的 8s 窗口。超时就先交声明
  // 的那份；命令在后台跑完，结果补进缓存，下一次请求就是完整的。
  it("serves the declared models when the list command overruns the budget", async () => {
    let release: ((stdout: string) => void) | undefined;
    const runCommand = vi.fn(
      () => new Promise<string>((resolve) => {
        release = resolve;
      }),
    );
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand,
      cliBudgetMs: 20,
      readConfig: () => ({
        endpoints: [],
        defaultModel: "quukk/glm-5.2",
        declaredModels: { quukk: ["glm-5.2"] },
      }),
    });

    const startedAt = Date.now();
    const first = await loader.load();
    expect(Date.now() - startedAt).toBeLessThan(1_000);
    expect(first.providers).toEqual([
      { id: "quukk", name: "quukk", models: [{ id: "glm-5.2", name: "glm-5.2" }] },
    ]);

    release?.("opencode/claude-sonnet-5\nquukk/glm-5.2\n");
    await new Promise((resolve) => setTimeout(resolve, 20));

    // 缓存已被晚到的结果补齐，且没有再起一次子进程。
    const second = await loader.load();
    expect(second.providers.map((provider) => provider.id)).toEqual(["opencode", "quukk"]);
    expect(runCommand).toHaveBeenCalledTimes(1);
  });

  // TTL 到期的那次后台刷新如果撞上冷启（枚举超预算），绝不能把已缓存的完整目录
  // 缩小成配置里那几条 —— 否则用户刷新页面，模型反而变少。
  it("never lets an overrun refresh shrink a catalog that is already cached", async () => {
    let calls = 0;
    const runCommand = vi.fn(() => {
      calls += 1;
      if (calls === 1) return Promise.resolve("opencode/claude-sonnet-5\nquukk/glm-5.2\n");
      return new Promise<string>(() => {
        /* 第二次起永不返回，模拟冷启卡住 */
      });
    });
    let clock = 0;
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand,
      cliBudgetMs: 20,
      ttlMs: 1_000,
      now: () => clock,
      readConfig: () => ({
        endpoints: [],
        defaultModel: null,
        declaredModels: { quukk: ["glm-5.2"] },
      }),
    });

    const first = await loader.load();
    expect(first.providers.map((provider) => provider.id)).toEqual(["opencode", "quukk"]);

    clock = 5_000; // TTL 过期 → 后台刷新
    await loader.load();
    await new Promise((resolve) => setTimeout(resolve, 60));

    const third = await loader.load();
    expect(third.providers.map((provider) => provider.id)).toEqual(["opencode", "quukk"]);
  });

  // 配置是用户手写的，provider key 里带空格完全可能。wire 上的 id 必须能拼成
  // `provider/model` 路由（与 groupRoutes / mergeEndpointModels 同一把尺子），
  // 否则前端既存不下也回传不了。
  it("drops declared providers whose key cannot form a route", async () => {
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand: vi.fn().mockResolvedValue(""),
      readConfig: () => ({
        endpoints: [],
        defaultModel: null,
        declaredModels: { "my provider": ["glm-5.2"] },
      }),
    });

    await expect(loader.load()).resolves.toBe(EMPTY_MODEL_CATALOG);
  });

  it("drops declared slugs that cannot form a route", async () => {
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand: vi.fn().mockResolvedValue(""),
      readConfig: () => ({
        endpoints: [],
        defaultModel: null,
        declaredModels: { p: ["bad model", "has/slash", "", "good"] },
      }),
    });

    const catalog = await loader.load();
    expect(catalog.providers.map((provider) => provider.id)).toEqual(["p"]);
    expect(catalog.providers[0].models.map((model) => model.id)).toEqual(["good"]);
  });

  it("keeps every valid declared slug, in order", async () => {
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand: vi.fn().mockResolvedValue(""),
      readConfig: () => ({
        endpoints: [],
        defaultModel: "quukk/k3",
        declaredModels: { quukk: ["glm-5.2", "k3"] },
      }),
    });

    const catalog = await loader.load();
    expect(catalog.providers[0].models.map((model) => model.id)).toEqual(["glm-5.2", "k3"]);
    expect(catalog.defaultModel).toBe("quukk/k3");
  });

  it("caps a single declared provider at 500 models", async () => {
    // MAX_MODELS_PER_PROVIDER（model-catalog.ts:40），与 probe/CLI 路径同一个上限：
    // 一份手写错的配置不能把整帧撑爆。
    const slugs = Array.from({ length: 600 }, (_, i) => `m${i}`);
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand: vi.fn().mockResolvedValue(""),
      readConfig: () => ({ endpoints: [], defaultModel: null, declaredModels: { p: slugs } }),
    });

    const catalog = await loader.load();
    expect(catalog.providers[0].models).toHaveLength(500);
    expect(catalog.providers[0].models.map((model) => model.id)).toContain("m0");
    expect(catalog.providers[0].models.map((model) => model.id)).toContain("m499");
    expect(catalog.providers[0].models.map((model) => model.id)).not.toContain("m500");
  });

  it("discards a declared default that the declaration itself does not contain", async () => {
    const loader = createModelCatalogLoader({
      agentName: "opencode",
      runCommand: vi.fn().mockResolvedValue(""),
      readConfig: () => ({
        endpoints: [],
        defaultModel: "quukk/not-declared",
        declaredModels: { quukk: ["glm-5.2"] },
      }),
    });

    const catalog = await loader.load();
    expect(catalog.providers[0].models.map((model) => model.id)).toEqual(["glm-5.2"]);
    expect(catalog.defaultModel).toBeNull();
  });
});

describe("MessageDispatcher model catalog request", () => {
  function inbound(content: unknown): InboundIMMessage {
    return {
      objectName: "command",
      fromUserId: "web-user",
      toUserId: "node-user",
      targetId: "node-user",
      conversationType: 1,
      content: JSON.stringify(content),
    };
  }

  const turns = { runTurn: async () => "unused" };

  it("replies with the catalog to the requester", async () => {
    const sent: Array<{ to: string; objectName: string; content: string }> = [];
    const dispatcher = new MessageDispatcher({
      send: async (to, objectName, content) => {
        sent.push({ to, objectName, content });
      },
      loadModelCatalog: async () => catalogFromRoutes(["opencode/big-pickle"]),
    });

    await dispatcher.handle(
      inbound({
        msg_type: "discussion_model_catalog_request",
        protocolVersion: 2,
        requestId: "req-9",
        // Must be a live timestamp: the dispatcher drops requests the web has
        // already timed out on (see handleModelCatalogRequest).
        timestamp: Date.now(),
      }),
      turns,
    );

    expect(sent).toHaveLength(1);
    expect(sent[0].to).toBe("web-user");
    expect(sent[0].objectName).toBe("command");
    const body = JSON.parse(sent[0].content);
    expect(body.msg_type).toBe("discussion_model_catalog_response");
    expect(body.requestId).toBe("req-9");
    expect(body.providers[0].id).toBe("opencode");
    expect(typeof body.timestamp).toBe("number");
    expect(Number.isSafeInteger(body.timestamp)).toBe(true);
  });

  it("still answers an empty catalog when no loader is wired", async () => {
    const sent: string[] = [];
    const dispatcher = new MessageDispatcher({
      send: async (_to, _objectName, content) => {
        sent.push(content);
      },
    });

    await dispatcher.handle(
      inbound({
        msg_type: "discussion_model_catalog_request",
        protocolVersion: 2,
        requestId: "req-10",
      }),
      turns,
    );

    expect(JSON.parse(sent[0])).toMatchObject({
      msg_type: "discussion_model_catalog_response",
      requestId: "req-10",
      defaultModel: null,
      providers: [],
    });
  });

  it("survives a loader failure and still answers", async () => {
    const sent: string[] = [];
    const dispatcher = new MessageDispatcher({
      send: async (_to, _objectName, content) => {
        sent.push(content);
      },
      loadModelCatalog: async () => {
        throw new Error("device exploded");
      },
    });

    await dispatcher.handle(
      inbound({
        msg_type: "discussion_model_catalog_request",
        protocolVersion: 2,
        requestId: "req-11",
      }),
      turns,
    );

    expect(JSON.parse(sent[0]).providers).toEqual([]);
  });
});
