import { join } from "node:path";
import { describe, expect, it, vi } from "vitest";
import {
  modelsUrl,
  parseCodexConfig,
  parseHermesConfig,
  parseModelIds,
  parseOpenclawConfig,
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

  it("has nothing for agents with no config reader", () => {
    expect(readAgentConfig("opencode", { home: "/home/u", env: {} })).toBeNull();
    expect(readAgentConfig(undefined, {})).toBeNull();
  });
});
