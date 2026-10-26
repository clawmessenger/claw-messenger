import { join } from "node:path";
import { describe, expect, it, vi } from "vitest";
import {
  modelsUrl,
  parseCodebuddyProviderConfig,
  parseCodexConfig,
  parseHermesConfig,
  parseModelIds,
  parseOpenclawConfig,
  parseOpencodeConfig,
  parseQwenConfig,
  probeProviderModels,
  readAgentConfig,
} from "./model-providers.js";

function fakeResponse(body: string, ok = true): Response {
  return { ok, status: ok ? 200 : 500, text: async () => body } as unknown as Response;
}

describe("parseModelIds", () => {
  it("reads the OpenAI {data:[{id}]} envelope", () => {
    expect(parseModelIds(JSON.stringify({ data: [{ id: "glm-5.2" }, { id: "k3" }] }))).toEqual([
      "glm-5.2",
      "k3",
    ]);
  });

  it("reads openclaw-style {models:[{key}]} and plain arrays", () => {
    expect(parseModelIds(JSON.stringify({ models: [{ key: "openai/gpt-5" }] }))).toEqual([
      "openai/gpt-5",
    ]);
    expect(parseModelIds(JSON.stringify(["a", "b"]))).toEqual(["a", "b"]);
    expect(parseModelIds(JSON.stringify([{ id: "x" }, "y", { nope: 1 }, 7]))).toEqual(["x", "y"]);
  });

  it("returns null when the body is not a model list", () => {
    expect(parseModelIds("not json")).toBeNull();
    expect(parseModelIds(JSON.stringify({ error: "nope" }))).toBeNull();
    expect(parseModelIds(JSON.stringify("glm-5.2"))).toBeNull();
  });

  it("drops blank ids", () => {
    expect(parseModelIds(JSON.stringify({ data: [{ id: "  " }, { id: " ok " }] }))).toEqual(["ok"]);
  });

  it("drops models a chat turn cannot use (embedding/rerank/tts/vl)", () => {
    const body = JSON.stringify({
      data: [
        { id: "glm-5.2" },
        { id: "BGE-m3" },
        { id: "Text-Embedding-V4-A" },
        { id: "Qwen3-Rerank-A" },
        { id: "Qwen-VL-Chat" },
        { id: "kimi-k2.7-code" },
        { id: "MiniMax-M3-A" },
        { id: "whisper-large-v3" },
      ],
    });
    expect(parseModelIds(body)).toEqual(["glm-5.2", "kimi-k2.7-code", "MiniMax-M3-A"]);
  });
});

describe("probeProviderModels", () => {
  it("GETs {base}/models with the configured bearer token", async () => {
    const fetchImpl = vi.fn().mockResolvedValue(fakeResponse(JSON.stringify({ data: [{ id: "k3" }] })));
    const ids = await probeProviderModels(
      { id: "quukk", baseUrl: "https://llmapi.quukk.com/v1/", apiKey: "sk-1" },
      { fetchImpl: fetchImpl as unknown as typeof fetch },
    );
    expect(ids).toEqual(["k3"]);
    expect(modelsUrl("https://llmapi.quukk.com/v1/")).toBe("https://llmapi.quukk.com/v1/models");
    const [url, init] = fetchImpl.mock.calls[0];
    expect(url).toBe("https://llmapi.quukk.com/v1/models");
    expect((init as RequestInit).headers).toMatchObject({ authorization: "Bearer sk-1" });
  });

  it("never rejects: HTTP errors and throws both yield null", async () => {
    const notOk = vi.fn().mockResolvedValue(fakeResponse("", false));
    expect(await probeProviderModels(
      { id: "p", baseUrl: "http://127.0.0.1:1/v1" },
      { fetchImpl: notOk as unknown as typeof fetch },
    )).toBeNull();

    const boom = vi.fn().mockRejectedValue(new Error("ECONNREFUSED"));
    expect(await probeProviderModels(
      { id: "p", baseUrl: "http://127.0.0.1:1/v1" },
      { fetchImpl: boom as unknown as typeof fetch },
    )).toBeNull();
  });
});

describe("parseOpenclawConfig", () => {
  it("reads models.providers baseUrl/apiKey", () => {
    const config = parseOpenclawConfig(JSON.stringify({
      models: { providers: { openai: { baseUrl: "http://127.0.0.1:49830/v1", apiKey: "sk-local-proxy" } } },
    }));
    expect(config.endpoints).toEqual([
      { id: "openai", baseUrl: "http://127.0.0.1:49830/v1", apiKey: "sk-local-proxy" },
    ]);
    // openclaw keeps its default in the model list, not in the config file.
    expect(config.defaultModel).toBeNull();
  });

  it("honours a models.default route and skips providers without a baseUrl", () => {
    const config = parseOpenclawConfig(JSON.stringify({
      models: { default: "openai/glm-5.2", providers: { broken: { apiKey: "x" } } },
    }));
    expect(config.endpoints).toEqual([]);
    expect(config.defaultModel).toBe("openai/glm-5.2");
  });

  it("returns nothing for junk", () => {
    expect(parseOpenclawConfig("not json").endpoints).toEqual([]);
    expect(parseOpenclawConfig(JSON.stringify({})).endpoints).toEqual([]);
  });
});

describe("parseCodexConfig", () => {
  const TOML = [
    'model = "glm-5.2"',
    'model_provider = "quukk_direct"',
    'model_reasoning_effort = "medium"',
    "",
    '[model_providers.quukk_direct]',
    'name = "Quukk Direct"',
    'base_url = "http://127.0.0.1:49830/v1"   # local gateway',
    'wire_api = "responses"',
    "",
    '[marketplaces.openai-bundled]',
    "source_type = 'local'",
    "",
  ].join("\n");

  it("reads the active provider and the table it points at", () => {
    const config = parseCodexConfig(TOML);
    expect(config.endpoints).toEqual([
      { id: "quukk_direct", name: "Quukk Direct", baseUrl: "http://127.0.0.1:49830/v1" },
    ]);
    expect(config.defaultModel).toBe("quukk_direct/glm-5.2");
  });

  it("ignores model_providers-named keys and a hash inside a quoted url", () => {
    const config = parseCodexConfig('base_url = "http://h/v1"\nmodel_providers = "x"\n');
    expect(config.endpoints).toEqual([]);
    expect(config.defaultModel).toBeNull();
  });

  it("keeps every declared provider, not just the active one", () => {
    const config = parseCodexConfig([
      '[model_providers."a.b"]',
      'base_url = "http://a/v1"',
      "[model_providers.second]",
      'base_url = "http://b/v1"',
      'api_key = "k"',
    ].join("\n"));
    expect(config.endpoints.map((e) => e.id)).toEqual(["a.b", "second"]);
    expect(config.endpoints[1].apiKey).toBe("k");
  });
});

describe("parseHermesConfig", () => {
  const YAML = [
    "onboarding:",
    "  seen:",
    "    openclaw_residue_cleanup: true",
    "custom_providers:",
    "  - name: quukk",
    "    base_url: https://llmapi.quukk.com/v1",
    "    api_key: sk-Txc",
    "    api_mode: chat_completions",
    "    model: glm-5.3",
    "model:",
    "  default: glm-5.3",
    "  base_url: https://llmapi.quukk.com/v1",
    "  provider: quukk",
    "",
  ].join("\n");

  it("reads custom_providers and the default provider/model", () => {
    const config = parseHermesConfig(YAML);
    expect(config.endpoints).toEqual([
      { id: "quukk", name: "quukk", baseUrl: "https://llmapi.quukk.com/v1", apiKey: "sk-Txc" },
    ]);
    expect(config.defaultModel).toBe("quukk/glm-5.3");
  });

  it("returns nothing when there is no endpoint to probe", () => {
    const config = parseHermesConfig("model:\n  default: glm-5.3\n");
    expect(config.endpoints).toEqual([]);
    expect(config.defaultModel).toBeNull();
  });
});

describe("parseQwenConfig", () => {
  const SETTINGS = JSON.stringify({
    security: { auth: { selectedType: "openai" } },
    env: {
      OPENAI_API_KEY: "sk-Txc",
      OPENAI_BASE_URL: "https://llmapi.quukk.com/v1",
      OPENAI_MODEL: "glm-5.2",
    },
    model: { name: "glm-5.2" },
  });

  it("reads the OpenAI-compatible endpoint from settings.env", () => {
    const config = parseQwenConfig(SETTINGS);
    expect(config.endpoints).toEqual([
      { id: "quukk", baseUrl: "https://llmapi.quukk.com/v1", apiKey: "sk-Txc" },
    ]);
    expect(config.defaultModel).toBe("quukk/glm-5.2");
  });

  it("returns nothing when the endpoint is missing or the file is not JSON", () => {
    expect(parseQwenConfig(JSON.stringify({ env: { OPENAI_MODEL: "glm-5.2" } })).endpoints).toEqual([]);
    expect(parseQwenConfig("{not json")).toEqual({ endpoints: [], defaultModel: null });
  });
});

describe("parseCodebuddyProviderConfig", () => {
  const PROVIDER = JSON.stringify({
    provider: "quukk",
    name: "Quukk AI",
    baseUrl: "https://llmapi.quukk.com/v1",
    apiKey: "sk-Txc",
    defaultModel: "glm-5.2",
  });

  it("reads the self-hosted gateway described in clawmessenger-provider.json", () => {
    const config = parseCodebuddyProviderConfig(PROVIDER);
    expect(config.endpoints).toEqual([
      { id: "quukk", name: "Quukk AI", baseUrl: "https://llmapi.quukk.com/v1", apiKey: "sk-Txc" },
    ]);
    expect(config.defaultModel).toBe("quukk/glm-5.2");
  });

  it("derives the provider id from the host when it is not written out", () => {
    const config = parseCodebuddyProviderConfig(JSON.stringify({
      baseUrl: "https://llmapi.quukk.com/v1",
    }));
    expect(config.endpoints[0]?.id).toBe("quukk");
    expect(config.defaultModel).toBeNull();
  });

  it("returns nothing without a baseUrl", () => {
    expect(parseCodebuddyProviderConfig(JSON.stringify({ apiKey: "sk" }))).toEqual({
      endpoints: [],
      defaultModel: null,
    });
    expect(parseCodebuddyProviderConfig("nope")).toEqual({ endpoints: [], defaultModel: null });
  });
});

// 固定 fixture（形状取自真实的 ~/.config/opencode/opencode.json，但**不是**
// 读本机文件）：opencode 把模型清单本身写在配置里，所以这一份既是端点来源，
// 也是 declaredModels 来源。测试不许碰跑测试那台机器的 home。
const OPENCODE_JSON = JSON.stringify({
  model: "quukk/glm-5.2",
  provider: {
    quukk: {
      name: "Quukk AI",
      options: { baseURL: "https://llmapi.example.com/v1", apiKey: "sk-test" },
      models: { "glm-5.2": {}, "glm-5.3": {}, k3: {} },
    },
    "empty-provider": {
      name: "No Models",
      options: { baseURL: "https://b.example.com/v1" },
    },
    "no-url": { name: "No URL", models: { x: {} } },
  },
});

describe("parseOpencodeConfig", () => {
  it("reads endpoints and the models each provider declares", () => {
    const config = parseOpencodeConfig(OPENCODE_JSON);
    // `provider` 的 key 就是 id；`options.baseURL` 决定它是不是可 probe 的端点。
    expect(config.endpoints).toEqual([
      {
        id: "quukk",
        name: "Quukk AI",
        baseUrl: "https://llmapi.example.com/v1",
        apiKey: "sk-test",
      },
      // 没有 models 但有 baseURL：端点仍然要报（目录靠 probe 补）。
      { id: "empty-provider", name: "No Models", baseUrl: "https://b.example.com/v1" },
    ]);
    // 没有 baseURL 的 provider 不是一个可连的端点。
    expect(config.endpoints.map((e) => e.id)).not.toContain("no-url");
  });

  it("declares models for every provider, including ones we cannot probe", () => {
    const config = parseOpencodeConfig(OPENCODE_JSON);
    // 顺序按配置书写顺序，不去重排序 —— 目录层自己会排。
    expect(config.declaredModels?.quukk).toEqual(["glm-5.2", "glm-5.3", "k3"]);
    // 没写 models 的 provider 不产生空清单（空清单会白白触发一次 probe）。
    expect(config.declaredModels?.["empty-provider"]).toBeUndefined();
    // 无 baseURL ≠ 不可用：opencode 的内置 provider 走 SDK 默认端点（如
    // `opencode/*-free`），模型是真能跑的，只是我们**无法 probe** 它。
    // 剔掉等于替用户隐藏可选项，所以它照样进 declaredModels（只是不进 endpoints）。
    expect(config.declaredModels?.["no-url"]).toEqual(["x"]);
    expect(config.endpoints.map((endpoint) => endpoint.id)).not.toContain("no-url");
  });

  it("returns the top-level model verbatim instead of re-assembling it", () => {
    // 顶层 model 本来就是 `provider/slug` 路由；再拼一次会得到 `quukk/quukk/...`。
    expect(parseOpencodeConfig(OPENCODE_JSON).defaultModel).toBe("quukk/glm-5.2");
  });

  it("leaves declaredModels out entirely when no provider declares any", () => {
    const config = parseOpencodeConfig(JSON.stringify({
      provider: { p: { options: { baseURL: "https://p.example.com/v1" } } },
    }));
    expect(config.endpoints.map((e) => e.id)).toEqual(["p"]);
    expect("declaredModels" in config).toBe(false);
    expect(config.defaultModel).toBeNull();
  });

  // opencode 的配置允许注释（真实 .jsonc 就带）。剥注释最容易写错的地方是
  // **字符串里的 `//`**：`"baseURL": "https://a//b"` 一旦被当成行注释，baseURL
  // 连同后面半份配置都会被吞掉，provider 全部消失。
  it("strips // and /* */ comments but never a // inside a string", () => {
    const jsonc = [
      "// opencode 配置支持注释",
      "{",
      '  "model": "quukk/glm-5.2",   // 行尾注释',
      '  "provider": {',
      '    "quukk": {',
      '      "name": "Quukk AI",',
      '      /* 块注释：端点与密钥',
      "         跨两行 */",
      '      "options": { "baseURL": "https://a//b", "apiKey": "sk-test" },',
      '      "models": { "glm-5.2": {}, "k3": {} }',
      "    },",
      '    "after": { "name": "After", "options": { "baseURL": "https://after.example.com/v1" } }',
      "  }",
      "}",
    ].join("\n");
    const config = parseOpencodeConfig(jsonc);
    expect(config.endpoints).toEqual([
      {
        id: "quukk",
        name: "Quukk AI",
        // 双斜杠必须原样保留，不能被当成注释的起点。
        baseUrl: "https://a//b",
        apiKey: "sk-test",
      },
      // 注释之后声明的 provider 也必须还在：注释若吞掉换行，后面的 token 会粘连。
      { id: "after", name: "After", baseUrl: "https://after.example.com/v1" },
    ]);
    expect(config.declaredModels).toEqual({ quukk: ["glm-5.2", "k3"] });
    expect(config.defaultModel).toBe("quukk/glm-5.2");
  });

  it("returns an empty config (never throws) for unusable input", () => {
    for (const text of ["", "not json", "[]", "null"]) {
      expect(parseOpencodeConfig(text)).toEqual({ endpoints: [], defaultModel: null });
    }
  });
});

describe("readAgentConfig", () => {
  const OPENCLAW_JSON = JSON.stringify({
    models: { providers: { openai: { baseUrl: "http://127.0.0.1:49830/v1" } } },
  });

  it("reads openclaw.json from the agent's config directory", () => {
    const home = join("/", "home", "u");
    const path = join(home, ".openclaw", "openclaw.json");
    const readFile = vi.fn((p: string) => {
      if (p === path) return OPENCLAW_JSON;
      throw new Error("ENOENT");
    });
    const config = readAgentConfig("openclaw", { home, env: {}, readFile });
    expect(config?.endpoints).toHaveLength(1);
    expect(readFile).toHaveBeenCalledWith(path);
  });

  it("prefers CODEX_HOME and returns null when the file is missing", () => {
    const home = join("/", "home", "u");
    const codexHome = join("/", "opt", "codex");
    const path = join(codexHome, "config.toml");
    const readFile = vi.fn((p: string) => {
      if (p === path) return 'model = "glm-5.2"\nmodel_provider = "quukk_direct"\n';
      throw new Error("ENOENT");
    });
    const config = readAgentConfig("codex", { home, env: { CODEX_HOME: codexHome }, readFile });
    expect(config?.defaultModel).toBe("quukk_direct/glm-5.2");
    expect(readAgentConfig("codex", { home, env: {}, readFile: () => { throw new Error("ENOENT"); } })).toBeNull();
  });

  it("reads opencode.json from ~/.config/opencode", () => {
    const home = join("/", "home", "u");
    const path = join(home, ".config", "opencode", "opencode.json");
    const readFile = vi.fn((p: string) => {
      if (p === path) return OPENCODE_JSON;
      throw new Error("ENOENT");
    });
    const config = readAgentConfig("opencode", { home, env: {}, readFile });
    expect(config?.endpoints.map((endpoint) => endpoint.id)).toEqual(["quukk", "empty-provider"]);
    expect(config?.defaultModel).toBe("quukk/glm-5.2");
    expect(config?.declaredModels?.quukk).toEqual(["glm-5.2", "glm-5.3", "k3"]);
    expect(readFile).toHaveBeenCalledWith(path);
  });

  it("tries XDG_CONFIG_HOME and the .jsonc fallback before giving up", () => {
    const home = join("/", "home", "u");
    const xdg = join("/", "xdg");
    const jsoncPath = join(xdg, "opencode", "opencode.jsonc");
    const readFile = vi.fn((p: string) => {
      if (p === jsoncPath) return `// jsonc\n${OPENCODE_JSON}`;
      throw new Error("ENOENT");
    });
    const config = readAgentConfig("opencode", { home, env: { XDG_CONFIG_HOME: xdg }, readFile });
    expect(config?.defaultModel).toBe("quukk/glm-5.2");
    // 先试 XDG 的 .json，再试 .jsonc —— 顺序固定，不能反过来先吃 .json。
    expect(readFile.mock.calls.map(([p]) => p)).toEqual([
      join(xdg, "opencode", "opencode.json"),
      jsoncPath,
    ]);
  });

  it("returns null when no opencode config exists", () => {
    const home = join("/", "home", "u");
    expect(readAgentConfig("opencode", {
      home,
      env: {},
      readFile: () => { throw new Error("ENOENT"); },
    })).toBeNull();
  });

  it("has nothing for agents with no config reader", () => {
    // kimi 没有配置解析器（本机无字段样本，不猜）——不读任何文件，直接 null。
    const readFile = vi.fn(() => { throw new Error("should not read"); });
    expect(readAgentConfig("kimi", { home: "/home/u", env: {}, readFile })).toBeNull();
    expect(readFile).not.toHaveBeenCalled();
    expect(readAgentConfig(undefined, {})).toBeNull();
  });
});
