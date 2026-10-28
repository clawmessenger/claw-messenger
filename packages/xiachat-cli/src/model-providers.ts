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
  /** 配置文件里**显式声明**的模型清单（provider id → slugs）。
   *  有它时不必 probe 端点：省掉 8s 超时窗口，且离线可用。 */
  declaredModels?: Record<string, string[]>;
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

// Gateways list every model they front — embedding, rerank, TTS, vision-only
// endpoints included (quukk returns ~200 entries, a third of them unusable for
// a chat turn). Selecting one of those in 设备管理 → 默认模型 makes every later
// turn fail, so the probe drops them. Agent CLIs' own `models list` output is
// not filtered: those are models the agent itself vouches for.
const NON_CHAT_MODEL_RE =
  /(embed|rerank|bge|whisper|\btts\b|\basr\b|moderation|dall-e|stable-diffusion|inpaint|-vl\b|\bvl-|vl-chat|vision-only)/i;

function isChatModel(id: string): boolean {
  return !NON_CHAT_MODEL_RE.test(id);
}

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
      if (id !== null && isChatModel(id)) ids.push(id);
      return;
    }
    if (typeof entry !== "object" || entry === null) return;
    const record = entry as Record<string, unknown>;
    const id = asString(record.id ?? record.key ?? record.model);
    if (id !== null && isChatModel(id)) ids.push(id);
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

// Derives the provider id the web panel shows from the endpoint host, so a
// gateway like `llmapi.quukk.com` presents as `quukk` instead of a generic
// `openai` bucket. Unusable hosts (bare `com`, `localhost`) fall back.
function providerIdFromBaseUrl(baseUrl: string, fallback: string): string {
  let host: string;
  try {
    host = new URL(baseUrl).hostname;
  } catch {
    return fallback;
  }
  const labels = host
    .split(".")
    .filter((label) => label !== "" && !/^(www|api|llmapi|gateway|openai|v1)$/i.test(label));
  const first = labels[0];
  if (!first || /^(com|cn|net|org|io|ai|local|localhost)$/i.test(first)) return fallback;
  const slug = first.replace(/[^A-Za-z0-9._-]/g, "");
  return slug === "" ? fallback : slug;
}

// `~/.qwen/settings.json` — qwen-code 的鉴权走 `security.auth.selectedType`，
// 端点与密钥放在 `env` 里（实测 `OPENAI_BASE_URL` / `OPENAI_API_KEY` /
// `OPENAI_MODEL` 三项生效；qwen 自己没有可枚举模型目录的子命令）。
export function parseQwenConfig(text: string): AgentConfig {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { endpoints: [], defaultModel: null };
  }
  if (typeof parsed !== "object" || parsed === null) {
    return { endpoints: [], defaultModel: null };
  }
  const record = parsed as Record<string, unknown>;
  const env = (typeof record.env === "object" && record.env !== null
    ? record.env
    : {}) as Record<string, unknown>;
  const baseUrl = asString(env.OPENAI_BASE_URL ?? env.OPENAI_API_BASE);
  if (!baseUrl) return { endpoints: [], defaultModel: null };
  const apiKey = asString(env.OPENAI_API_KEY ?? env.OPENAI_KEY);
  const id = providerIdFromBaseUrl(baseUrl, "openai");
  const modelField = record.model;
  const model = asString(env.OPENAI_MODEL ?? env.QWEN_MODEL)
    ?? (typeof modelField === "object" && modelField !== null
      ? asString((modelField as Record<string, unknown>).name)
      : asString(modelField));
  return {
    endpoints: [{ id, baseUrl, ...(apiKey ? { apiKey } : {}) }],
    defaultModel: model ? `${id}/${model}` : null,
  };
}

// `~/.codebuddy/clawmessenger-provider.json` — codebuddy 自己的 settings.json
// 只认 `custom-local:<m>`（走 WorkBuddy 托管通道，有周/月限额），且这个 build
// 丢掉了 ANTHROPIC_BASE_URL 支持，唯一可直连自建网关的入口是环境变量
// `CODEBUDDY_BASE_URL`（OpenAI 兼容，实测 `/v1/chat/completions`）。
// 所以端点写在这份独立文件里：model-providers 用它 probe 模型目录，
// agents.ts 用同一份文件给子进程注入 CODEBUDDY_* 环境变量。
//
//   { "provider": "quukk", "baseUrl": "https://llmapi.quukk.com/v1",
//     "apiKey": "sk-…", "defaultModel": "glm-5.2" }
export function parseCodebuddyProviderConfig(text: string): AgentConfig {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { endpoints: [], defaultModel: null };
  }
  if (typeof parsed !== "object" || parsed === null) {
    return { endpoints: [], defaultModel: null };
  }
  const record = parsed as Record<string, unknown>;
  const baseUrl = asString(record.baseUrl ?? record.base_url);
  if (!baseUrl) return { endpoints: [], defaultModel: null };
  const apiKey = asString(record.apiKey ?? record.api_key);
  const id = asString(record.provider) ?? providerIdFromBaseUrl(baseUrl, "codebuddy");
  const name = asString(record.name);
  const model = asString(record.defaultModel ?? record.default_model ?? record.model);
  return {
    endpoints: [{ id, ...(name ? { name } : {}), baseUrl, ...(apiKey ? { apiKey } : {}) }],
    defaultModel: model ? `${id}/${model}` : null,
  };
}

// `~/.config/opencode/opencode.json`（或 `.jsonc`）—— opencode 的配置天生允许
// 注释，真实的 `.jsonc` 就带 `//`，所以先剥注释再交给 JSON.parse。剥注释时
// **必须跳过字符串字面量内部**：`"baseURL": "https://…"` 里的 `//` 一旦被当成
// 行注释，整个 baseURL 连同后面半份配置都会被吞掉（provider 全都消失）。
export function stripJsonComments(text: string): string {
  let out = "";
  let inString = false;
  let quote = "";
  for (let i = 0; i < text.length; i++) {
    const char = text[i];
    if (inString) {
      out += char;
      if (char === "\\") {
        // 转义序列整体照抄，`\"` 不能被当成字符串结束。
        if (i + 1 < text.length) {
          out += text[i + 1];
          i += 1;
        }
        continue;
      }
      if (char === quote) inString = false;
      continue;
    }
    if (char === '"' || char === "'") {
      inString = true;
      quote = char;
      out += char;
      continue;
    }
    if (char === "/" && text[i + 1] === "/") {
      while (i < text.length && text[i] !== "\n") i += 1;
      // 用换行替换注释，避免把前后两个 token 粘在一起。
      out += "\n";
      continue;
    }
    if (char === "/" && text[i + 1] === "*") {
      i += 2;
      while (i < text.length && !(text[i] === "*" && text[i + 1] === "/")) i += 1;
      i += 1; // 落在 `/` 上，for 的 i += 1 越过它
      continue;
    }
    out += char;
  }
  return out;
}

// `~/.config/opencode/opencode.json` —— opencode 与上面几个 agent 不同：它把
// **模型清单本身**写进配置（`provider.<id>.models` 的 key 就是 slug），所以
// 这里除了端点，还能直接交出 `declaredModels`：离线、瞬时、不赌 8s 窗口。
//
//   { "model": "quukk/glm-5.2",            ← 默认路由，本来就是 provider/model
//     "provider": {
//       "quukk": { "name": "Quukk AI",
//                  "options": { "baseURL": "https://…/v1", "apiKey": "sk-…" },
//                  "models": { "glm-5.2": {…}, "k3": {…} } } } }
//
// 注意 key 的大小写：baseURL / apiKey 是驼峰，别写成 baseUrl。
export function parseOpencodeConfig(text: string): AgentConfig {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    try {
      parsed = JSON.parse(stripJsonComments(text));
    } catch {
      return { endpoints: [], defaultModel: null };
    }
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return { endpoints: [], defaultModel: null };
  }
  const record = parsed as Record<string, unknown>;
  const endpoints: ProviderEndpoint[] = [];
  const declaredModels: Record<string, string[]> = {};

  const providers = record.provider;
  if (typeof providers === "object" && providers !== null && !Array.isArray(providers)) {
    for (const [rawId, value] of Object.entries(providers as Record<string, unknown>)) {
      // provider 的 key 就是 id（`provider.<id>.name` 只是展示名）。
      const id = asString(rawId);
      if (!id) continue;
      if (typeof value !== "object" || value === null) continue;
      const provider = value as Record<string, unknown>;
      const options = (typeof provider.options === "object" && provider.options !== null
        ? provider.options
        : {}) as Record<string, unknown>;
      const name = asString(provider.name);
      const baseUrl = asString(options.baseURL ?? options.baseUrl ?? options.base_url);
      const apiKey = asString(options.apiKey ?? options.api_key);
      if (baseUrl) {
        endpoints.push({ id, ...(name ? { name } : {}), baseUrl, ...(apiKey ? { apiKey } : {}) });
      }
      const models = provider.models;
      if (typeof models !== "object" || models === null || Array.isArray(models)) continue;
      const slugs: string[] = [];
      for (const rawSlug of Object.keys(models as Record<string, unknown>)) {
        const slug = asString(rawSlug);
        if (!slug || slugs.includes(slug)) continue;
        slugs.push(slug);
      }
      if (slugs.length > 0) declaredModels[id] = slugs;
    }
  }

  return {
    endpoints,
    // 顶层 `model` 本来就是 `provider/model` 路由，原样返回，不要重新拼。
    defaultModel: asString(record.model),
    ...(Object.keys(declaredModels).length > 0 ? { declaredModels } : {}),
  };
}

// --- per-agent wiring ------------------------------------------------------

const AGENT_CONFIG_PARSERS: Readonly<Record<string, (text: string) => AgentConfig>> = {
  openclaw: parseOpenclawConfig,
  codex: parseCodexConfig,
  hermes: parseHermesConfig,
  qwen: parseQwenConfig,
  codebuddy: parseCodebuddyProviderConfig,
  opencode: parseOpencodeConfig,
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
  if (agentName === "qwen") {
    const candidates: string[] = [];
    if (env.QWEN_HOME) candidates.push(join(env.QWEN_HOME, "settings.json"));
    candidates.push(join(home, ".qwen", "settings.json"));
    return candidates;
  }
  if (agentName === "codebuddy") {
    // CLAWMESSENGER_CODEBUDDY_PROVIDER lets a machine point at a second
    // gateway without editing the shared profile.
    const candidates: string[] = [];
    if (env.CLAWMESSENGER_CODEBUDDY_PROVIDER) {
      candidates.push(env.CLAWMESSENGER_CODEBUDDY_PROVIDER);
    }
    candidates.push(join(home, ".codebuddy", "clawmessenger-provider.json"));
    return candidates;
  }
  if (agentName === "opencode") {
    // opencode 自己按 XDG 找配置；Windows 上 XDG_CONFIG_HOME 通常缺失，所以
    // 无条件再补一条 ~/.config（opencode 自身也是这么回落的）。
    const candidates: string[] = [];
    if (env.CLAWMESSENGER_OPENCODE_CONFIG) candidates.push(env.CLAWMESSENGER_OPENCODE_CONFIG);
    const xdg = env.XDG_CONFIG_HOME;
    if (xdg) {
      candidates.push(join(xdg, "opencode", "opencode.json"));
      candidates.push(join(xdg, "opencode", "opencode.jsonc"));
    }
    candidates.push(join(home, ".config", "opencode", "opencode.json"));
    candidates.push(join(home, ".config", "opencode", "opencode.jsonc"));
    return candidates;
  }
  return [];
}

/**
 * 找出 opencode 实际会读的那份配置文件（第一个存在的候选）。
 *
 * 只给「派生设备端精简配置」用 —— 原文件必须原样不动：里面可能有用户自己
 * 维护的 plugin / MCP，删掉会改变他交互使用 opencode 的体验。
 */
export function discoverOpencodeConfigPath(opts: DiscoverOpts = {}): string | null {
  const home = opts.home ?? homedir();
  const env = opts.env ?? process.env;
  const readFile = opts.readFile ?? defaultReadFile;
  for (const path of configPaths("opencode", home, env)) {
    try {
      readFile(path);
      return path;
    } catch {
      continue;
    }
  }
  return null;
}

export interface DeviceOpencodeConfigOpts {
  /**
   * 「文件读写禁止」限制态（见 agent-permissions.ts fsRestriction）。
   *
   * 为什么把键写进配置而不是加命令行参数：opencode 没有「禁用工具」的 CLI 参数，
   * 工具授权只在配置里。写入两套键是为了同时覆盖 1.x 与 2.x：
   *   · `permission`（1.x 起就有，2.x 仍接受，schema 里标 deprecated 但保留）
   *     —— `edit` / `bash` 等工具级的 allow|ask|deny；
   *   · `tools`（更老的版本只认这个）—— 工具名 → 布尔开关。
   * 两套都写、语义一致，任一版本生效即可，不必猜版本。
   */
  fsRestricted?: boolean;
  /**
   * 追加到 `instructions` 的行为规范文件路径（不存在就不传）。
   *
   * opencode 的 `instructions` 是「补充环境指令」的路径数组（schema:
   * `instructions: string[]`），正好是「指令层」而不是用户消息层。
   */
  instructions?: string[];
}

// 文件读写限制态要关掉的工具。`read`/`glob`/`grep`/`list` 是读，`edit`/`write`/
// `patch` 是写；`bash` 既能读也能写，一并关掉 —— 否则「禁了文件工具但留一个
// shell」等于没禁。`task`（子 agent）也关：子 agent 会重新拿到全套工具。
const FS_DENY_PERMISSION: Readonly<Record<string, string>> = {
  read: "deny",
  edit: "deny",
  glob: "deny",
  grep: "deny",
  list: "deny",
  bash: "deny",
  task: "deny",
  external_directory: "deny",
  webfetch: "deny",
  websearch: "deny",
};

const FS_DENY_TOOLS: Readonly<Record<string, boolean>> = {
  read: false,
  write: false,
  edit: false,
  patch: false,
  bash: false,
  glob: false,
  grep: false,
  list: false,
  webfetch: false,
};

/**
 * 从用户的 opencode 配置派生出「设备端专用」精简版：去掉 `plugin` 与 `mcp`，
 * 再按权限快照写入限制/指令。
 *
 * 为什么要这么做（2026-10-26 实测）：那两段里的 `superpowers@git+https://…`
 * 插件与 `uvx --from git+https://…` 的 MCP，会让 `opencode run` 在
 * `message=init` 上卡七十秒以上、零输出 —— 设备端每轮都要重新付这笔启动
 * 开销，用户看到的就是「先想很久」。provider / model / instructions /
 * permission 全部保留，所以模型与人格不变。
 *
 * 返回 null 表示这份配置读不懂（带 BOM、非对象、JSON 破损）——调用方这时
 * 应该什么都不做，让 opencode 用原配置，别把它弄成「配置没了」。
 */
export function buildDeviceOpencodeConfig(
  rawText: string,
  opts: DeviceOpencodeConfigOpts = {},
): { text: string; dropped: string[] } | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(stripJsonComments(rawText.replace(/^\uFEFF/, "")));
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return null;
  const source = parsed as Record<string, unknown>;
  const out: Record<string, unknown> = { ...source };
  const dropped: string[] = [];
  for (const key of ["plugin", "mcp"]) {
    if (key in out) {
      delete out[key];
      dropped.push(key);
    }
  }
  if (opts.instructions && opts.instructions.length > 0) {
    // 追加而不是覆盖：用户自己的 instructions 必须原样保留。
    const existing = Array.isArray(out.instructions)
      ? out.instructions.filter((item): item is string => typeof item === "string")
      : [];
    out.instructions = [...new Set([...existing, ...opts.instructions])];
  }
  if (opts.fsRestricted) {
    const permission = (out.permission && typeof out.permission === "object" && !Array.isArray(out.permission))
      ? { ...(out.permission as Record<string, unknown>) }
      : {};
    out.permission = { ...permission, ...FS_DENY_PERMISSION };
    const tools = (out.tools && typeof out.tools === "object" && !Array.isArray(out.tools))
      ? { ...(out.tools as Record<string, unknown>) }
      : {};
    out.tools = { ...tools, ...FS_DENY_TOOLS };
  }
  return { text: JSON.stringify(out, null, 2), dropped };
}


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
