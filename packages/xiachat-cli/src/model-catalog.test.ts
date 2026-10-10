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
    });
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
  it("runs the agent command once and then serves the cache", async () => {
    const runCommand = vi.fn().mockResolvedValue(OPENCODE_OUTPUT);
    const loader = createModelCatalogLoader({ agentName: "opencode", runCommand });

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

  it("never rejects and stays empty for agents without a list command", async () => {
    const runCommand = vi.fn();
    const loader = createModelCatalogLoader({ agentName: "codex", runCommand });
    await expect(loader.load()).resolves.toBe(EMPTY_MODEL_CATALOG);
    expect(runCommand).not.toHaveBeenCalled();
  });

  it("falls back to the empty catalog when the command fails", async () => {
    const runCommand = vi.fn().mockRejectedValue(new Error("boom"));
    const logger = vi.fn();
    const loader = createModelCatalogLoader({ agentName: "opencode", runCommand, logger });
    await expect(loader.load()).resolves.toBe(EMPTY_MODEL_CATALOG);
    expect(logger).toHaveBeenCalledWith(expect.stringContaining("boom"));
  });

  it("deduplicates concurrent requests", async () => {
    const runCommand = vi.fn().mockImplementation(() => new Promise((resolve) => setTimeout(() => resolve(OPENCODE_OUTPUT), 5)));
    const loader = createModelCatalogLoader({ agentName: "opencode", runCommand });
    const [first, second] = await Promise.all([loader.load(), loader.load()]);
    expect(runCommand).toHaveBeenCalledTimes(1);
    expect(first).toBe(second);
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
        timestamp: 1,
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
