import { stripAnsiCodes } from "./agents.js";

// Protocol v2 model-catalog responder for the web's 设备管理 → 默认模型 panel.
//
// clawmessenger-web sends a private "command" message
//   { msg_type: "discussion_model_catalog_request", protocolVersion: 2,
//     requestId: <uuid>, timestamp: <ms> }
// to the node's RongCloud user and waits 8s (15s when the node looks offline)
// for a "discussion_model_catalog_response" from that same user. Only the
// OpenClaw plugin (oepnclaw-clawmessenger) implemented that responder, so nodes
// driven by this CLI (opencode / codex / hermes / ...) never answered and the
// panel stayed on "正在获取设备模型目录…".
//
// Wire shape must stay byte-compatible with the web's normalizeCatalog
// (clawmessenger-web/src/services/node-model-catalog.ts): exactly the keys
// msg_type/protocolVersion/requestId/defaultModel/providers/timestamp, each
// provider exactly {id,name,models}, each model exactly {id,name}.

export interface DiscussionModelCatalog {
  defaultModel: string | null;
  providers: Array<{
    id: string;
    name: string;
    models: Array<{ id: string; name: string }>;
  }>;
}

export const EMPTY_MODEL_CATALOG: DiscussionModelCatalog = Object.freeze({
  defaultModel: null,
  providers: [],
}) as DiscussionModelCatalog;

const MAX_PROVIDERS = 64;
const MAX_MODELS_PER_PROVIDER = 500;
const MAX_LINES_SCANNED = 32_000;

// A `provider/model` route: matches the web's segment() rules (non-empty, <=128
// chars, no whitespace, exactly one slash).
const ROUTE_RE = /^([A-Za-z0-9][A-Za-z0-9._-]{0,127})\/([A-Za-z0-9][A-Za-z0-9._:-]{0,127})$/;

export interface ModelListSpec {
  argv: string[];
  timeoutMs: number;
  maxOutputBytes: number;
}

// Agents whose CLI can enumerate models as `provider/model` routes — the only
// shape the web's provider+model dropdowns can persist and hand back to the
// agent as `--model <provider>/<model>`. openclaw needs the `list` subcommand
// (`openclaw models` alone prints status, not the catalog).
const AGENT_MODEL_LIST: Readonly<Record<string, ModelListSpec>> = {
  opencode: { argv: ["models"], timeoutMs: 30_000, maxOutputBytes: 256 * 1024 },
  openclaw: { argv: ["models", "list", "--json"], timeoutMs: 60_000, maxOutputBytes: 1024 * 1024 },
};

export function modelListSpec(agentName: string | undefined): ModelListSpec | null {
  if (!agentName) return null;
  return AGENT_MODEL_LIST[agentName] ?? null;
}

/** Groups flat `provider/model` routes into the web's catalog shape. */
export function catalogFromRoutes(
  routes: Iterable<string>,
  defaultModel: string | null = null,
  names: ReadonlyMap<string, string> = new Map(),
): DiscussionModelCatalog {
  const grouped = new Map<string, Map<string, string>>();
  let scanned = 0;
  for (const route of routes) {
    if (++scanned > MAX_LINES_SCANNED) break;
    const match = ROUTE_RE.exec(route);
    if (!match) continue;
    const [, providerId, modelId] = match;
    let models = grouped.get(providerId);
    if (!models) {
      if (grouped.size >= MAX_PROVIDERS) continue;
      models = new Map<string, string>();
      grouped.set(providerId, models);
    }
    if (!models.has(modelId) && models.size < MAX_MODELS_PER_PROVIDER) {
      models.set(modelId, names.get(route) ?? modelId);
    }
  }
  const providers = [...grouped.entries()]
    .map(([id, models]) => ({
      id,
      name: id,
      models: [...models.entries()]
        .map(([modelId, name]) => ({ id: modelId, name }))
        .sort((a, b) => a.id.localeCompare(b.id)),
    }))
    .sort((a, b) => a.id.localeCompare(b.id));

  const wanted = defaultModel && ROUTE_RE.test(defaultModel) ? defaultModel : null;
  const inCatalog = wanted !== null
    && providers.some((provider) => provider.id === wanted.split("/")[0]
      && provider.models.some((model) => model.id === wanted.split("/")[1]));
  return { defaultModel: inCatalog ? wanted : null, providers };
}

// Text output shapes handled here:
//   opencode models        -> one `provider/model` per line
//   openclaw models        -> `Default       : openai/gpt-5.6-sol`
//   openclaw models list   -> a table row tagged `default` in its last column
export function parseModelListText(stdout: string): DiscussionModelCatalog {
  const routes: string[] = [];
  let defaultModel: string | null = null;
  const lines = stripAnsiCodes(stdout).split(/\r?\n/).slice(0, MAX_LINES_SCANNED);
  for (const rawLine of lines) {
    const line = rawLine.trim();
    if (!line) continue;
    const labelled = /^(?:default|默认模型|节点默认)\s*[:：]\s*(\S+)/i.exec(line);
    if (labelled) {
      const candidate = labelled[1].replace(/[.,;]+$/, "");
      if (ROUTE_RE.test(candidate)) {
        defaultModel = candidate;
        // The default must also be selectable, so it joins the route list.
        routes.push(candidate);
      }
      continue;
    }
    let lineRoute: string | null = null;
    for (const token of line.split(/[\s,|]+/)) {
      if (!ROUTE_RE.test(token)) continue;
      routes.push(token);
      lineRoute = token;
    }
    // openclaw's table marks the current default with a trailing `default` tag.
    if (lineRoute && /(^|[\s|])default([\s|]|$)/.test(line)) defaultModel = lineRoute;
  }
  return catalogFromRoutes(routes, defaultModel);
}

// `openclaw models list --json` -> { count, models: [{ key, name, tags: [...] }] }
export function parseOpenclawModelListJson(stdout: string): DiscussionModelCatalog | null {
  const start = stdout.indexOf("{");
  if (start < 0) return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(stdout.slice(start));
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const models = (parsed as { models?: unknown }).models;
  if (!Array.isArray(models)) return null;

  const routes: string[] = [];
  const names = new Map<string, string>();
  let defaultModel: string | null = null;
  for (const entry of models) {
    if (typeof entry !== "object" || entry === null) continue;
    const key = (entry as { key?: unknown }).key;
    if (typeof key !== "string" || !ROUTE_RE.test(key)) continue;
    routes.push(key);
    const name = (entry as { name?: unknown }).name;
    if (typeof name === "string" && name.length > 0 && name.length <= 500) names.set(key, name);
    const tags = (entry as { tags?: unknown }).tags;
    if (Array.isArray(tags) && tags.includes("default")) defaultModel = key;
  }
  return catalogFromRoutes(routes, defaultModel, names);
}

export function parseModelCatalogOutput(
  agentName: string | undefined,
  stdout: string,
): DiscussionModelCatalog {
  if (agentName === "openclaw") {
    const json = parseOpenclawModelListJson(stdout);
    if (json && json.providers.length > 0) return json;
  }
  return parseModelListText(stdout);
}

export interface ModelCatalogRequest {
  msg_type: "discussion_model_catalog_request";
  protocolVersion: 2;
  requestId: string;
}

export function parseModelCatalogRequest(content: string): ModelCatalogRequest | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(content);
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const record = parsed as Record<string, unknown>;
  if (record.msg_type !== "discussion_model_catalog_request") return null;
  if (record.protocolVersion !== 2) return null;
  const requestId = record.requestId;
  if (typeof requestId !== "string" || requestId.length < 1 || requestId.length > 128) return null;
  return { msg_type: "discussion_model_catalog_request", protocolVersion: 2, requestId };
}

export function buildModelCatalogResponse(
  request: ModelCatalogRequest,
  catalog: DiscussionModelCatalog,
  clock: () => number = Date.now,
): string {
  return JSON.stringify({
    msg_type: "discussion_model_catalog_response",
    protocolVersion: 2,
    requestId: request.requestId,
    defaultModel: catalog.defaultModel,
    providers: catalog.providers,
    timestamp: clock(),
  });
}

export interface ModelCatalogLoaderOpts {
  agentName?: string;
  runCommand: (argv: string[], timeoutMs: number, maxOutputBytes: number) => Promise<string>;
  logger?: (message: string) => void;
  /** How long a cached catalog is served before a background refresh kicks in. */
  ttlMs?: number;
  now?: () => number;
}

export interface ModelCatalogLoader {
  /** Catalog for the current request; never rejects. */
  load(): Promise<DiscussionModelCatalog>;
  /** Kick off a background refresh so the first panel open is served warm. */
  warm(): void;
}

const DEFAULT_CACHE_TTL_MS = 5 * 60_000;

// The web aborts after 8s, while `openclaw models list` alone takes ~10s on a
// cold start, so a cache-warm path is required for anything beyond opencode.
export function createModelCatalogLoader(opts: ModelCatalogLoaderOpts): ModelCatalogLoader {
  const spec = modelListSpec(opts.agentName);
  const ttlMs = opts.ttlMs ?? DEFAULT_CACHE_TTL_MS;
  const now = opts.now ?? Date.now;
  let cached: DiscussionModelCatalog | null = null;
  let cachedAt = 0;
  let inflight: Promise<DiscussionModelCatalog> | null = null;

  const refresh = (): Promise<DiscussionModelCatalog> => {
    if (inflight) return inflight;
    if (!spec) {
      cached = EMPTY_MODEL_CATALOG;
      cachedAt = now();
      return Promise.resolve(cached);
    }
    inflight = opts
      .runCommand(spec.argv, spec.timeoutMs, spec.maxOutputBytes)
      .then((stdout) => {
        cached = parseModelCatalogOutput(opts.agentName, stdout);
        cachedAt = now();
        return cached;
      })
      .catch((err: unknown) => {
        opts.logger?.(
          `model catalog refresh failed: ${err instanceof Error ? err.message : String(err)}`,
        );
        return cached ?? EMPTY_MODEL_CATALOG;
      })
      .finally(() => {
        inflight = null;
      });
    return inflight;
  };

  return {
    async load() {
      if (cached) {
        const served = cached;
        if (now() - cachedAt >= ttlMs) void refresh();
        return served;
      }
      return refresh();
    },
    warm() {
      void refresh();
    },
  };
}
