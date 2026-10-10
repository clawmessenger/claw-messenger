import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

// Provider-endpoint discovery for the 设备管理 → 默认模型 panel.
//
// The web only understands `provider/model` routes, so every node must report
// pairs its agent CLI will actually accept. No single source enumerates all of
// them:
//
//   * openclaw's `models list` only reports the provider's *configured* /
//     OAuth catalogue. When that provider is an OpenAI-compatible gateway
//     fronting several upstreams, the gateway's pass-through models are absent
//     from it — verified on a real machine: `openclaw models list` never
//     mentions glm/deepseek/kimi, yet the endpoint serves them and openclaw
//     runs `openai/glm-4.5` successfully.
//   * codex's `debug models` returns bare slugs with no provider prefix (and
//     they are not all runnable — `gpt-6-astra` is rejected by its ChatGPT
//     account), and hermes has no non-interactive listing command at all.
//
// What every agent does have is a configured OpenAI-compatible endpoint
// (`baseUrl` / `base_url` in its own config file), and `GET {base}/models` is
// the authoritative answer for what that endpoint serves. So we read each
// agent's config, collect its endpoints, probe them, and let model-catalog.ts
// union the result with whatever the agent's own CLI reported.

export interface ProviderEndpoint {
  id: string;
  // Human label for the panel's provider dropdown; falls back to `id`.
  name?: string;
  baseUrl: string;
  apiKey?: string;
}

export interface AgentConfig {
  endpoints: ProviderEndpoint[];
  // The agent's own configured default, as a `provider/model` route.
  defaultModel: string | null;
}

export interface DiscoverOpts {
  home?: string;
  env?: NodeJS.ProcessEnv;
  readFile?: (path: string) => string;
}

export interface ProbeOpts {
  fetchImpl?: typeof fetch;
  timeoutMs?: number;
}

const DEFAULT_PROBE_TIMEOUT_MS = 8_000;
const MAX_MODELS = 500;

function defaultReadFile(path: string): string {
  return readFileSync(path, "utf8");
}

function asString(value: unknown): string | null {
  return typeof value === "string" && value.trim() !== "" ? value.trim() : null;
}

// --- probing ---------------------------------------------------------------

// `GET {base}/models` — tolerate the several envelopes seen in the wild:
// OpenAI's `{data:[{id}]}`, openclaw-style `{models:[{key}]}`, and plain
// arrays. `null` means "not a model list", so callers can tell an unusable
// response from a legitimately empty one.
export function parseModelIds(body: string): string[] | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(body);
  } catch {
    return null;
  }
  const ids: string[] = [];
  const pushEntry = (entry: unknown): void => {
    if (ids.length >= MAX_MODELS) return;
    if (typeof entry === "string") {
      const id = asString(entry);
      if (id !== null) ids.push(id);
      return;
    }
    if (typeof entry !== "object" || entry === null) return;
    const record = entry as Record<string, unknown>;
    const id = asString(record.id ?? record.key ?? record.model);
    if (id !== null) ids.push(id);
  };

  if (Array.isArray(parsed)) {
    for (const entry of parsed) pushEntry(entry);
    return ids;
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const record = parsed as Record<string, unknown>;
  const list = Array.isArray(record.data)
    ? record.data
    : Array.isArray(record.models)
      ? record.models
      : null;
  if (!list) return null;
  for (const entry of list) pushEntry(entry);
  return ids;
}

export function modelsUrl(baseUrl: string): string {
  return `${baseUrl.replace(/\/+$/, "")}/models`;
}

/** Probes one OpenAI-compatible endpoint. Never rejects; `null` = unusable. */
export async function probeProviderModels(
  endpoint: ProviderEndpoint,
  opts: ProbeOpts = {},
): Promise<string[] | null> {
  const fetchImpl = opts.fetchImpl ?? globalThis.fetch;
  if (typeof fetchImpl !== "function") return null;
  const headers: Record<string, string> = { accept: "application/json" };
  if (endpoint.apiKey) headers.authorization = `Bearer ${endpoint.apiKey}`;
  try {
    const response = await fetchImpl(modelsUrl(endpoint.baseUrl), {
      method: "GET",
      headers,
      signal: AbortSignal.timeout(opts.timeoutMs ?? DEFAULT_PROBE_TIMEOUT_MS),
    });
    if (!response.ok) return null;
    return parseModelIds(await response.text());
  } catch {
    return null;
  }
}

// --- minimal config readers ------------------------------------------------
// Only the handful of keys we consume is read; a full TOML/YAML parser would
// be overkill and would have to be bundled into the SEA binary.

// Drops a trailing `# comment`, ignoring hashes inside quoted strings.
function stripTomlComment(line: string): string {
  let quote: '"' | "'" | null = null;
  for (let i = 0; i < line.length; i++) {
    const char = line[i];
    if (quote) {
      if (char === quote && line[i - 1] !== "\\") quote = null;
      continue;
    }
    if (char === '"' || char === "'") quote = char;
    else if (char === "#") return line.slice(0, i);
  }
  return line;
}

function unquote(value: string): string | null {
  const trimmed = value.trim();
  const quoted = /^(['"])([\s\S]*)\1$/.exec(trimmed);
  if (quoted) return quoted[2].trim() === "" ? null : quoted[2];
  return /^[A-Za-z0-9_./:@-]+$/.test(trimmed) ? trimmed : null;
}

// `~/.codex/config.toml`: `model_provider = "<id>"`, `model = "<slug>"`, and
// `[model_providers.<id>]` tables carrying `base_url` / `api_key` / `name`.
export function parseCodexConfig(text: string): AgentConfig {
  const endpoints: ProviderEndpoint[] = [];
  let activeProvider: string | null = null;
  let model: string | null = null;
  let table: string | null = null;
  let current: Partial<ProviderEndpoint> & { id?: string } = {};

  const flush = (): void => {
    if (current.id && asString(current.baseUrl)) {
      endpoints.push({
        id: current.id,
        ...(current.name ? { name: current.name } : {}),
        baseUrl: current.baseUrl as string,
        ...(current.apiKey ? { apiKey: current.apiKey } : {}),
      });
    }
    current = {};
  };

  for (const rawLine of text.split(/\r?\n/)) {
    const line = stripTomlComment(rawLine).trim();
    if (!line) continue;
    const header = /^\[([^\]]+)\]$/.exec(line);
    if (header) {
      flush();
      table = header[1];
      const provider = /^model_providers\.(?:"([^"]+)"|([^.]+))$/.exec(table);
      if (provider) current.id = provider[1] ?? provider[2];
      continue;
    }
    const kv = /^([A-Za-z0-9_.-]+)\s*=\s*(.+)$/.exec(line);
    if (!kv) continue;
    const value = unquote(kv[2]);
    if (table === null) {
      if (kv[1] === "model_provider") activeProvider = value;
      else if (kv[1] === "model") model = value;
      continue;
    }
    if (!current.id) continue;
    if (kv[1] === "base_url" || kv[1] === "baseUrl") current.baseUrl = value ?? undefined;
    else if (kv[1] === "name") current.name = value ?? undefined;
    else if (kv[1] === "api_key" || kv[1] === "apiKey") current.apiKey = value ?? undefined;
  }
  flush();

  const provider = activeProvider ?? endpoints[0]?.id ?? null;
  return {
    endpoints,
    defaultModel: provider && model ? `${provider}/${model}` : null,
  };
}

// `~/AppData/Local/hermes/config.yaml` (or `~/.hermes/config.yaml`):
//   custom_providers:
//     - name: quukk
//       base_url: https://llmapi.quukk.com/v1
//       api_key: sk-...
//   model:
//     default: glm-5.3
//     provider: quukk
export function parseHermesConfig(text: string): AgentConfig {
  const endpoints: ProviderEndpoint[] = [];
  let current: Partial<ProviderEndpoint> & { id?: string } = {};
  let provider: string | null = null;
  let model: string | null = null;
  let section: "custom_providers" | "model" | null = null;
  let sectionIndent = -1;

  const flush = (): void => {
    if (current.id && asString(current.baseUrl)) {
      endpoints.push({
        id: current.id,
        ...(current.name ? { name: current.name } : {}),
        baseUrl: current.baseUrl as string,
        ...(current.apiKey ? { apiKey: current.apiKey } : {}),
      });
    }
    current = {};
  };

  for (const rawLine of text.split(/\r?\n/)) {
    if (rawLine.trim() === "" || rawLine.trimStart().startsWith("#")) continue;
    const indent = rawLine.length - rawLine.trimStart().length;
    const line = rawLine.trim();
    if (section !== null && indent <= sectionIndent) {
      flush();
      section = null;
    }
    if (section === null) {
      if (/^custom_providers\s*:/.test(line)) {
        section = "custom_providers";
        sectionIndent = indent;
      } else if (/^model\s*:/.test(line)) {
        section = "model";
        sectionIndent = indent;
      }
      continue;
    }
    const item = /^-\s*(.*)$/.exec(line);
    if (section === "custom_providers") {
      if (item) {
        flush();
        const kv = /^([A-Za-z0-9_.-]+)\s*:\s*(.*)$/.exec(item[1]);
        if (kv) applyHermesEntry(current, kv[1], kv[2]);
        continue;
      }
      const kv = /^([A-Za-z0-9_.-]+)\s*:\s*(.*)$/.exec(line);
      if (kv) applyHermesEntry(current, kv[1], kv[2]);
      continue;
    }
    const kv = /^([A-Za-z0-9_.-]+)\s*:\s*(.*)$/.exec(line);
    if (!kv) continue;
    if (kv[1] === "provider") provider = unquote(kv[2]);
    else if (kv[1] === "default") model = unquote(kv[2]);
  }
  flush();

  return {
    endpoints,
    defaultModel: provider && model ? `${provider}/${model}` : null,
  };
}

function applyHermesEntry(
  target: Partial<ProviderEndpoint> & { id?: string },
  key: string,
  rawValue: string,
): void {
  const value = unquote(rawValue);
  if (!value) return;
  if (key === "name") {
    // hermes' `--provider <name>` uses this same slug, so it doubles as the id.
    target.id = value;
    target.name = value;
  } else if (key === "base_url" || key === "baseUrl") target.baseUrl = value;
  else if (key === "api_key" || key === "apiKey") target.apiKey = value;
}

// `~/.openclaw/openclaw.json`: models.providers.<id>.{baseUrl,apiKey,name}.
// The configured default lives in the model list (`tags: ["default"]`), not
// here, so `defaultModel` stays null and the loader fills it from the CLI.
export function parseOpenclawConfig(text: string): AgentConfig {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { endpoints: [], defaultModel: null };
  }
  if (typeof parsed !== "object" || parsed === null) {
    return { endpoints: [], defaultModel: null };
  }
  const providers = (parsed as { models?: { providers?: unknown } }).models?.providers;
  if (typeof providers !== "object" || providers === null || Array.isArray(providers)) {
    return { endpoints: [], defaultModel: null };
  }
  const endpoints: ProviderEndpoint[] = [];
  for (const [id, value] of Object.entries(providers as Record<string, unknown>)) {
    if (typeof value !== "object" || value === null) continue;
    const record = value as Record<string, unknown>;
    const baseUrl = asString(record.baseUrl ?? record.base_url);
    if (!baseUrl) continue;
    const name = asString(record.name);
    const apiKey = asString(record.apiKey ?? record.api_key);
    endpoints.push({ id, ...(name ? { name } : {}), baseUrl, ...(apiKey ? { apiKey } : {}) });
  }
  const configuredDefault = asString(
    (parsed as { models?: { default?: unknown } }).models?.default,
  );
  return {
    endpoints,
    defaultModel: configuredDefault && configuredDefault.includes("/") ? configuredDefault : null,
  };
}

// --- per-agent wiring ------------------------------------------------------

const AGENT_CONFIG_PARSERS: Readonly<Record<string, (text: string) => AgentConfig>> = {
  openclaw: parseOpenclawConfig,
  codex: parseCodexConfig,
  hermes: parseHermesConfig,
};

function configPaths(agentName: string, home: string, env: NodeJS.ProcessEnv): string[] {
  if (agentName === "openclaw") return [join(home, ".openclaw", "openclaw.json")];
  if (agentName === "codex") {
    const codexHome = env.CODEX_HOME ?? join(home, ".codex");
    return [join(codexHome, "config.toml")];
  }
  if (agentName === "hermes") {
    const candidates: string[] = [];
    if (env.HERMES_CONFIG) candidates.push(env.HERMES_CONFIG);
    if (env.HERMES_HOME) candidates.push(join(env.HERMES_HOME, "config.yaml"));
    if (env.LOCALAPPDATA) candidates.push(join(env.LOCALAPPDATA, "hermes", "config.yaml"));
    candidates.push(join(home, ".hermes", "config.yaml"));
    candidates.push(join(home, ".config", "hermes", "config.yaml"));
    return candidates;
  }
  return [];
}

/**
 * Reads the agent's own config. `null` means "no readable config", which is
 * not an error — it just means we fall back to the agent's CLI output.
 */
export function readAgentConfig(
  agentName: string | undefined,
  opts: DiscoverOpts = {},
): AgentConfig | null {
  if (!agentName) return null;
  const parser = AGENT_CONFIG_PARSERS[agentName];
  if (!parser) return null;
  const home = opts.home ?? homedir();
  const env = opts.env ?? process.env;
  const readFile = opts.readFile ?? defaultReadFile;
  for (const path of configPaths(agentName, home, env)) {
    let text: string;
    try {
      text = readFile(path);
    } catch {
      continue;
    }
    try {
      const config = parser(text);
      return { ...config, endpoints: dedupeEndpoints(config.endpoints) };
    } catch {
      return null;
    }
  }
  return null;
}

/** Endpoints an agent's own config points at (empty when none/unreadable). */
export function discoverProviderEndpoints(
  agentName: string | undefined,
  opts: DiscoverOpts = {},
): ProviderEndpoint[] {
  return readAgentConfig(agentName, opts)?.endpoints ?? [];
}

/** The agent's configured default route, if it has one. */
export function discoverDefaultModel(
  agentName: string | undefined,
  opts: DiscoverOpts = {},
): string | null {
  return readAgentConfig(agentName, opts)?.defaultModel ?? null;
}

function dedupeEndpoints(endpoints: ProviderEndpoint[]): ProviderEndpoint[] {
  const seen = new Set<string>();
  const out: ProviderEndpoint[] = [];
  for (const endpoint of endpoints) {
    const key = `${endpoint.id}\u0000${endpoint.baseUrl}`;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push(endpoint);
  }
  return out;
}
