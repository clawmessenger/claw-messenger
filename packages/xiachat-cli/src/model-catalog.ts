import { stripAnsiCodes } from "./agents.js";
import {
  probeProviderModels,
  readAgentConfig,
  type AgentConfig,
  type ProviderEndpoint,
} from "./model-providers.js";

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
// agent. openclaw needs the `list` subcommand (`openclaw models` alone prints
// status, not the catalog).
//
// NOTE: none of these lists is complete on its own. openclaw's covers the
// provider's configured/OAuth catalogue but not the gateway's pass-through
// models, and codex/hermes have no usable listing command at all. Endpoint
// probing (see model-providers.ts) unions the agent's list with the live
// `/models` answer.
const AGENT_MODEL_LIST: Readonly<Record<string, ModelListSpec>> = {
  opencode: { argv: ["models"], timeoutMs: 30_000, maxOutputBytes: 256 * 1024 },
  openclaw: { argv: ["models", "list", "--json"], timeoutMs: 60_000, maxOutputBytes: 1024 * 1024 },
};

export function modelListSpec(agentName: string | undefined): ModelListSpec | null {
  if (!agentName) return null;
  return AGENT_MODEL_LIST[agentName] ?? null;
}

interface ProviderGroup {
  name: string;
  models: Map<string, string>;
}

type GroupedProviders = Map<string, ProviderGroup>;

function groupRoutes(
  routes: Iterable<string>,
  names: ReadonlyMap<string, string> = new Map(),
): GroupedProviders {
  const grouped: GroupedProviders = new Map();
  let scanned = 0;
  for (const route of routes) {
    if (++scanned > MAX_LINES_SCANNED) break;
    const match = ROUTE_RE.exec(route);
    if (!match) continue;
    const [, providerId, modelId] = match;
    let group = grouped.get(providerId);
    if (!group) {
      if (grouped.size >= MAX_PROVIDERS) continue;
      group = { name: providerId, models: new Map() };
      grouped.set(providerId, group);
    }
    if (!group.models.has(modelId) && group.models.size < MAX_MODELS_PER_PROVIDER) {
      group.models.set(modelId, names.get(route) ?? modelId);
    }
  }
  return grouped;
}

function finalizeCatalog(
  grouped: GroupedProviders,
  defaultModel: string | null,
): DiscussionModelCatalog {
  const providers = [...grouped.entries()]
    .filter(([, group]) => group.models.size > 0)
    .map(([id, group]) => ({
      id,
      name: group.name,
      models: [...group.models.entries()]
        .map(([modelId, name]) => ({ id: modelId, name }))
        .sort((a, b) => a.id.localeCompare(b.id)),
    }))
    .sort((a, b) => a.id.localeCompare(b.id));

  const wanted = defaultModel && ROUTE_RE.test(defaultModel) ? defaultModel : null;
  const inCatalog = wanted !== null
    && providers.some((provider) => provider.id === wanted.split("/")[0]
      && provider.models.some((model) => model.id === wanted.split("/")[1]));
  if (providers.length === 0 && !inCatalog) return EMPTY_MODEL_CATALOG;
  return { defaultModel: inCatalog ? wanted : null, providers };
}

// Folds an endpoint's `/models` answer into the provider group.
//
// This is a UNION, not a replacement, and that matters: an agent's own list
// and the endpoint enumerate different halves. openclaw's `models list` covers
// the provider's configured/OAuth catalogue (`openai/gpt-5.6-sol`, which the
// proxy does serve — verified), while `/models` lists the gateway's
// pass-through models (`glm-5.2`, `deepseek-v4-pro`, …) that openclaw does not
// know about. Both sets run, so both belong in the panel.
function mergeEndpointModels(
  grouped: GroupedProviders,
  endpoint: ProviderEndpoint,
  modelIds: readonly string[],
): number {
  const group = grouped.get(endpoint.id) ?? { name: endpoint.name ?? endpoint.id, models: new Map() };
  // A display name from the agent's own config (e.g. codex's
  // `name = "Quukk Direct"`) beats the raw id.
  if (endpoint.name) group.name = endpoint.name;
  let added = 0;
  for (const modelId of modelIds) {
    if (group.models.size >= MAX_MODELS_PER_PROVIDER) break;
    if (group.models.has(modelId)) continue;
    if (!ROUTE_RE.test(`${endpoint.id}/${modelId}`)) continue;
    group.models.set(modelId, modelId);
    added += 1;
  }
  grouped.set(endpoint.id, group);
  return added;
}

/** Groups flat `provider/model` routes into the web's catalog shape. */
export function catalogFromRoutes(
  routes: Iterable<string>,
  defaultModel: string | null = null,
  names: ReadonlyMap<string, string> = new Map(),
): DiscussionModelCatalog {
  return finalizeCatalog(groupRoutes(routes, names), defaultModel);
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
  // When the web client sent the request (epoch ms), as carried on the wire.
  // The dispatcher uses it to drop requests the web has already timed out on —
  // notably 融云's offline backlog, which is replayed in full on every
  // reconnect (the Node shim has no IndexDB, so the SDK cannot dedupe). Each
  // replay would otherwise fire dozens of responses at once and trip
  // RongCloud's SEND_FREQUENCY_TOO_FAST (20604), losing all of them.
  timestamp?: number;
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
  const timestamp = record.timestamp;
  if (timestamp !== undefined && (typeof timestamp !== "number" || !Number.isSafeInteger(timestamp))) {
    return null;
  }
  return {
    msg_type: "discussion_model_catalog_request",
    protocolVersion: 2,
    requestId,
    ...(timestamp !== undefined ? { timestamp } : {}),
  };
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
  // Reads the agent's own config (provider endpoints + configured default).
  // Defaults to the real readers; inject a stub to keep tests hermetic.
  readConfig?: (agentName: string | undefined) => AgentConfig | null;
  // Probes one OpenAI-compatible endpoint; `null` = unusable.
  probe?: (endpoint: ProviderEndpoint) => Promise<string[] | null>;
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

function groupsFromCatalog(catalog: DiscussionModelCatalog): GroupedProviders {
  const grouped: GroupedProviders = new Map();
  for (const provider of catalog.providers) {
    if (grouped.size >= MAX_PROVIDERS) break;
    grouped.set(provider.id, {
      name: provider.name,
      models: new Map(provider.models.map((model) => [model.id, model.name])),
    });
  }
  return grouped;
}

// The web aborts after 8s, while `openclaw models list` alone takes ~10s on a
// cold start, so a cache-warm path is required for anything beyond opencode.
export function createModelCatalogLoader(opts: ModelCatalogLoaderOpts): ModelCatalogLoader {
  const spec = modelListSpec(opts.agentName);
  const readConfig = opts.readConfig ?? ((name: string | undefined) => readAgentConfig(name));
  const probe = opts.probe ?? ((endpoint: ProviderEndpoint) => probeProviderModels(endpoint));
  const ttlMs = opts.ttlMs ?? DEFAULT_CACHE_TTL_MS;
  const now = opts.now ?? Date.now;
  let cached: DiscussionModelCatalog | null = null;
  let cachedAt = 0;
  let inflight: Promise<DiscussionModelCatalog> | null = null;

  const describe = (err: unknown): string => (err instanceof Error ? err.message : String(err));

  // Never rejects: a failing CLI command or an unreachable endpoint degrades to
  // whatever we already know rather than taking the panel down with it.
  const resolveCatalog = async (): Promise<DiscussionModelCatalog> => {
    const config = readConfig(opts.agentName);
    const grouped: GroupedProviders = new Map();
    let defaultModel = config?.defaultModel ?? null;

    if (spec) {
      const label = `${opts.agentName} ${spec.argv.join(" ")}`;
      try {
        const stdout = await opts.runCommand(spec.argv, spec.timeoutMs, spec.maxOutputBytes);
        const fromCli = parseModelCatalogOutput(opts.agentName, stdout);
        for (const [id, group] of groupsFromCatalog(fromCli)) grouped.set(id, group);
        if (!defaultModel) defaultModel = fromCli.defaultModel;
        opts.logger?.(`model catalog: ${label} -> ${fromCli.providers.length} provider(s)`);
      } catch (err) {
        opts.logger?.(`model catalog: ${label} failed: ${describe(err)}`);
      }
    }

    const endpoints = config?.endpoints ?? [];
    if (endpoints.length > 0) {
      const probed = await Promise.all(
        endpoints.map(async (endpoint) => ({ endpoint, ids: await probe(endpoint) })),
      );
      for (const { endpoint, ids } of probed) {
        if (!ids) {
          // A failed probe keeps the CLI's list: a transient outage must not
          // shrink the panel.
          opts.logger?.(
            `model catalog: ${opts.agentName} provider ${endpoint.id} unreachable at ${endpoint.baseUrl}; keeping the CLI list`,
          );
          continue;
        }
        const added = mergeEndpointModels(grouped, endpoint, ids);
        opts.logger?.(
          `model catalog: ${opts.agentName} provider ${endpoint.id} +${added} model(s) from ${endpoint.baseUrl}`,
        );
      }
    }

    return finalizeCatalog(grouped, defaultModel);
  };

  const refresh = (): Promise<DiscussionModelCatalog> => {
    if (inflight) return inflight;
    const startedAt = now();
    inflight = resolveCatalog()
      .then((catalog) => {
        cached = catalog;
        cachedAt = now();
        opts.logger?.(
          `model catalog: ${opts.agentName} -> ${catalog.providers.length} provider(s) in ${cachedAt - startedAt}ms`,
        );
        return catalog;
      })
      .catch((err: unknown) => {
        opts.logger?.(
          `model catalog refresh failed after ${now() - startedAt}ms: ${describe(err)}`,
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
