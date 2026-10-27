import { existsSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

// 设备端的「绑定权限」快照：服务端在 10s 轮询（POST /api/claw/device/control）
// 里下发每个 agent 的四个准入开关、文件读写开关与行为规范。设备端把这一份
// 快照落盘，agent 子进程按需读取 —— 私聊闸门与文件读写闸门都在这里判定。
//
// 为什么要落盘而不是只放内存：轮询跑在 supervisor 进程里，真正执行 agent 的是
// 它拉起的**子进程**（`clawmessenger run --agent X`），两者不共享内存。文件是
// 唯一不需要再加一条 IPC 通道的传递方式，也顺带让「手敲 run」的单进程模式能
// 读到同一份快照。
//
// 为什么按 nodeId 索引：一次绑定 = 一个 agent = 一个 rongcloud_node 行，nodeId
// 是它在服务端与设备端都唯一稳定的 id（agent 名在一台设备上理论上可重复绑定，
// 例如同一台机器上两个 opencode？—— 服务端按 agent 名绑定，故退化为按 agent
// 名兜底）。轮询结果进快照、判定时查快照，**不在消息热路径上现查**。
//
// 兼容性（关键）：所有字段都是「明确 false 才算关闭」。旧服务端不返回这些字段
// → 快照里是 undefined → 一律放行，行为与升级前完全一致。这一条是为「服务端
// 先上线、CLI 后升级」和「CLI 先升级、服务端没升级」两侧都不出新故障。

/** 一个 agent 节点在服务端配置里的准入与能力开关。 */
export interface AgentPermissionEntry {
  agent: string;
  nodeId: string;
  /**
   * 三个会话准入开关 + 文件读写开关。
   *
   * undefined = 服务端没下发（旧服务端），语义是「不限制」；只有 **false**
   * 才是关闭。用可选布尔而不是默认 false，是为了让「没下发」与「显式关闭」
   * 在日志与测试里可区分 —— 前者不该被当成一次有效的关闭。
   */
  allowPrivate?: boolean;
  allowGroup?: boolean;
  allowDiscussion?: boolean;
  allowChatroom?: boolean;
  allowFs?: boolean;
  /** 服务端已拼好的生效值（用户没改时是平台默认模板）。空串 = 不下发。 */
  behaviorPrompt: string;
  /** control 行的版本号；仅用于排障，不参与任何判定。 */
  version: number;
}

export interface PermissionSnapshot {
  /** 认领这台设备的用户（claw IM user_id）。空串 = 未认领。 */
  ownerUserId: string;
  /**
   * 响应里是否**真的带了** `ownerUserId` 字段。
   *
   * 没有它就分不清「未认领」与「旧服务端没下发」：前者要按 fail-open 处理，
   * 后者必须保留上一份（否则服务端一滚回旧版本，认领关系就被抹成未认领）。
   */
  ownerUserIdPresent?: boolean;
  agents: AgentPermissionEntry[];
  /** 设备本地写入时间（ms），仅用于排障。 */
  updatedAt: number;
}

/** 覆盖快照文件位置（测试与多实例隔离用）。 */
export const PERMISSION_FILE_ENV = "CLAWMESSENGER_AGENT_PERMISSIONS_FILE";

/**
 * 快照在内存里的最长保鲜期。轮询周期是 10s，取 5s 让「用户在界面关掉私聊」
 * 最多 5s 后在本进程生效；同时把「每条私聊消息都读一次盘」降成「最多每 5s
 * 读一次」。文件 mtime/size 变了会立刻重读，不靠 TTL 兜。
 */
export const PERMISSION_CACHE_TTL_MS = 5_000;

export function permissionCachePath(
  env: NodeJS.ProcessEnv = process.env,
  home: string = homedir(),
): string {
  const override = env[PERMISSION_FILE_ENV]?.trim();
  if (override) return override;
  return join(home, ".clawmessenger", "agent-permissions.json");
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function asBool(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined;
}

function asNumber(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

/**
 * 把 `POST /api/claw/device/control` 的响应（FROZEN CONTRACT）解析成快照。
 *
 * 容忍两种信封：claw 的 `{code,message,data:{...}}` 与顶层直出。字段同时认
 * camelCase 与 snake_case —— 服务端 Go 结构体的 JSON tag 一旦改风格，设备端
 * 不该整份失效。
 *
 * @returns null = 响应里没有 agents 数组（旧服务端 / 非预期形状），调用方应
 *          按「没有快照」处理，即全部放行。
 */
export function parseControlPermissions(raw: unknown, now: number = Date.now()): PermissionSnapshot | null {
  if (!raw || typeof raw !== "object") return null;
  const body = raw as Record<string, unknown>;
  const data = (body.data && typeof body.data === "object" && !Array.isArray(body.data)
    ? body.data
    : body) as Record<string, unknown>;
  const list = data.agents;
  if (!Array.isArray(list)) return null;

  const agents: AgentPermissionEntry[] = [];
  for (const item of list) {
    if (!item || typeof item !== "object") continue;
    const row = item as Record<string, unknown>;
    const agent = asString(row.agent);
    const nodeId = asString(row.nodeId) || asString(row.node_id);
    if (!agent && !nodeId) continue;
    agents.push({
      agent,
      nodeId,
      allowPrivate: asBool(row.allowPrivate) ?? asBool(row.allow_private),
      allowGroup: asBool(row.allowGroup) ?? asBool(row.allow_group),
      allowDiscussion: asBool(row.allowDiscussion) ?? asBool(row.allow_discussion),
      allowChatroom: asBool(row.allowChatroom) ?? asBool(row.allow_chatroom),
      allowFs: asBool(row.allowFs) ?? asBool(row.allow_fs),
      behaviorPrompt: asString(row.behaviorPrompt) || asString(row.behavior_prompt),
      version: asNumber(row.version),
    });
  }
  const ownerRaw = data.ownerUserId ?? data.owner_user_id;
  return {
    ownerUserId: asString(ownerRaw),
    ownerUserIdPresent: typeof ownerRaw === "string",
    agents,
    updatedAt: now,
  };
}

/** 解析落盘快照；形状不对就当没有（绝不因为一个坏文件把闸门判成关闭）。 */
export function parsePermissionFile(text: string, now: number = Date.now()): PermissionSnapshot | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return null;
  }
  if (!parsed || typeof parsed !== "object") return null;
  const body = parsed as Record<string, unknown>;
  if (!Array.isArray(body.agents)) return null;
  const snapshot = parseControlPermissions({ agents: body.agents, ownerUserId: body.ownerUserId }, now);
  return snapshot;
}

interface CacheEntry {
  path: string;
  stamp: string;
  at: number;
  snapshot: PermissionSnapshot | null;
}

let cache: CacheEntry | null = null;

/** 测试用：清掉内存缓存，让下一次读取一定打盘。 */
export function clearPermissionCache(): void {
  cache = null;
}

/**
 * 读快照（带缓存）。
 *
 * - 5s 内命中内存直接返回；
 * - 过期后 stat 一次：mtime+size 没变就只续期，不重读内容；
 * - 文件不存在 / 读不出 / 形状不对 → null（= 没有快照，调用方放行）。
 */
export function loadPermissionSnapshot(opts: {
  path?: string;
  env?: NodeJS.ProcessEnv;
  home?: string;
  now?: number;
} = {}): PermissionSnapshot | null {
  const path = opts.path ?? permissionCachePath(opts.env ?? process.env, opts.home);
  const now = opts.now ?? Date.now();
  if (cache && cache.path === path && now - cache.at < PERMISSION_CACHE_TTL_MS) {
    return cache.snapshot;
  }
  let stamp = "";
  let snapshot: PermissionSnapshot | null = null;
  try {
    const st = statSync(path);
    stamp = `${st.mtimeMs}:${st.size}`;
    if (cache && cache.path === path && cache.stamp === stamp) {
      cache = { path, stamp, at: now, snapshot: cache.snapshot };
      return cache.snapshot;
    }
    snapshot = parsePermissionFile(readFileSync(path, "utf8"), now);
  } catch {
    snapshot = null;
  }
  cache = { path, stamp, at: now, snapshot };
  return snapshot;
}

/**
 * 落盘快照。返回写入路径，失败返回 null（调用方只记日志 —— 写不了快照不该让
 * 轮询本身失败，最坏结果是闸门继续用上一份）。
 */
export function writePermissionSnapshot(
  snapshot: PermissionSnapshot,
  opts: { path?: string; env?: NodeJS.ProcessEnv; home?: string } = {},
): string | null {
  const path = opts.path ?? permissionCachePath(opts.env ?? process.env, opts.home);
  try {
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, `${JSON.stringify(snapshot, null, 2)}\n`, "utf8");
    cache = null;
    return path;
  } catch {
    return null;
  }
}

/**
 * 权限签名：supervisor 用它判断「这一轮轮询的结果和上一轮是不是同一份」。
 * 只有签变了才重算配置，避免每 10s 都把派生配置重写一遍。
 *
 * 刻意**不含** updatedAt 与 desiredState：前者每次轮询都变，后者只影响启停，
 * 与权限/配置无关。
 */
export function permissionSignature(snapshot: PermissionSnapshot | null): string {
  if (!snapshot) return "none";
  const agents = snapshot.agents
    .map((a) => [
      a.agent,
      a.nodeId,
      a.allowPrivate ?? null,
      a.allowGroup ?? null,
      a.allowDiscussion ?? null,
      a.allowChatroom ?? null,
      a.allowFs ?? null,
      a.behaviorPrompt,
      a.version,
    ])
    .sort((left, right) => String(left[1] ?? left[0]).localeCompare(String(right[1] ?? right[0])));
  return JSON.stringify({ owner: snapshot.ownerUserId, agents });
}

/**
 * 把本轮轮询结果合并进上一份快照（审查 S4）。
 *
 * 为什么需要合并：旧服务端（或服务端回滚到旧版本）的响应里**没有权限字段**。
 * 直接覆盖会把这些字段写成 undefined，等于「一次回滚把所有限制悄悄打开」。
 * 规则：
 *  - **只有明确出现 true/false 才更新**该字段，缺失就沿用上一份；
 *  - `ownerUserId` 同理 —— 字段整体缺失时沿用上一份（否则认领关系被抹成未认领，
 *    私聊闸门会从「只允许主人」退回「所有人可用」）；字段存在但为空串 =
 *    服务端明确说未认领，按空串生效；
 *  - `behaviorPrompt` 只在「整行都没有任何权限字段」（旧服务端形状）时沿用，
 *    否则以服务端下发的为准（用户清空内容时服务端会下发空串，此时要真的清空）。
 */
export function mergePermissionSnapshot(
  previous: PermissionSnapshot | null,
  next: PermissionSnapshot,
): PermissionSnapshot {
  const ownerUserId = next.ownerUserIdPresent === true || next.ownerUserId !== ""
    ? next.ownerUserId
    : previous?.ownerUserId ?? "";
  if (!previous) return { ...next, ownerUserId, ownerUserIdPresent: true };

  const agents = next.agents.map((entry) => {
    const prior = findPermissionEntry(previous, { nodeId: entry.nodeId, agent: entry.agent });
    if (!prior) return entry;
    // 旧服务端形状：这一行一个权限字段都没有 → 整行沿用上一份的权限值。
    const legacy = entry.allowPrivate === undefined
      && entry.allowGroup === undefined
      && entry.allowDiscussion === undefined
      && entry.allowChatroom === undefined
      && entry.allowFs === undefined;
    const merged: AgentPermissionEntry = {
      ...entry,
      behaviorPrompt: legacy && !entry.behaviorPrompt ? prior.behaviorPrompt : entry.behaviorPrompt,
    };
    if (entry.allowPrivate !== undefined) merged.allowPrivate = entry.allowPrivate;
    else if (prior.allowPrivate !== undefined) merged.allowPrivate = prior.allowPrivate;
    if (entry.allowGroup !== undefined) merged.allowGroup = entry.allowGroup;
    else if (prior.allowGroup !== undefined) merged.allowGroup = prior.allowGroup;
    if (entry.allowDiscussion !== undefined) merged.allowDiscussion = entry.allowDiscussion;
    else if (prior.allowDiscussion !== undefined) merged.allowDiscussion = prior.allowDiscussion;
    if (entry.allowChatroom !== undefined) merged.allowChatroom = entry.allowChatroom;
    else if (prior.allowChatroom !== undefined) merged.allowChatroom = prior.allowChatroom;
    if (entry.allowFs !== undefined) merged.allowFs = entry.allowFs;
    else if (prior.allowFs !== undefined) merged.allowFs = prior.allowFs;
    return merged;
  });
  return { ownerUserId, ownerUserIdPresent: true, agents, updatedAt: next.updatedAt };
}

/** 查这个节点（或这个 agent 名）的配置。nodeId 优先，agent 名兜底。 */
export function findPermissionEntry(
  snapshot: PermissionSnapshot | null,
  query: { nodeId?: string; agent?: string },
): AgentPermissionEntry | null {
  if (!snapshot) return null;
  const nodeId = query.nodeId?.trim();
  if (nodeId) {
    const hit = snapshot.agents.find((a) => a.nodeId && a.nodeId === nodeId);
    if (hit) return hit;
  }
  const agent = query.agent?.trim();
  if (agent) {
    const hit = snapshot.agents.find((a) => a.agent && a.agent === agent);
    if (hit) return hit;
  }
  return null;
}

/**
 * 私聊闸门的判定结果。`reason` 是给日志用的：四条分支必须能在日志里分开，
 * 否则「为什么这条私聊没回」永远只能靠猜。
 */
export type PrivateGateReason =
  /** 服务端没关，或根本没下发这个字段 */
  | "unrestricted"
  /** 关闭，但发信人就是认领者本人 */
  | "owner"
  /** 关闭且发信人不是主人 → 不执行 */
  | "denied-stranger"
  /** 关闭，但设备未认领（没有主人可判）→ 唯一允许的 fail-open */
  | "unclaimed-fail-open"
  /** 没有权限快照（旧服务端 / 从未轮询成功 / 独立 run）→ 放行 */
  | "no-snapshot"
  /**
   * 有快照，但里面没有这个节点 → **拒绝**（审查 S3）。
   *
   * 方向必须与「默认全关」一致：有快照说明服务端已经在下发权限，只是没给这个
   * 节点写行（绑定关系不同步、服务端漏写）。这时放行等于「查不到就给你开口子」，
   * 比拒绝危险得多 —— 服务端加一个 agent、设备端就白送一个无限制入口。
   * 注意与 `no-snapshot` 区分：那是「整份都没有」（旧服务端），已裁决放行。
   */
  | "no-agent-entry";

export interface PrivateGateDecision {
  allow: boolean;
  reason: PrivateGateReason;
  entry?: AgentPermissionEntry;
}

/**
 * 私聊是否允许执行。
 *
 * 服务端**没有**私聊派发点（私聊是设备端收到 RC:TxtMsg 直接跑），所以「关掉
 * 私聊」只能在这一层执行。
 *
 * fail-open 点只有两个，且都必须能说清理由：
 *   1. `no-snapshot`：整份快照都没有（旧服务端 / 尚未轮询 / 独立 run）——
 *      已由队长裁决放行，否则升级瞬间所有设备私聊哑掉；
 *   2. `unclaimed-fail-open`：`ownerUserId` 为空，无法判定谁是主人。
 * 除这两条外一律「判不了就拒绝」：`no-agent-entry` 是有快照但没这个节点，
 * 说明服务端已在管权限，只是没给这一行 —— 放行等于凭空开一个无限制入口（审查 S3）。
 */
export function decidePrivateChat(
  snapshot: PermissionSnapshot | null,
  query: { fromUserId: string; nodeId?: string; agent?: string },
): PrivateGateDecision {
  if (!snapshot) return { allow: true, reason: "no-snapshot" };
  const entry = findPermissionEntry(snapshot, query);
  if (!entry) return { allow: false, reason: "no-agent-entry" };
  if (entry.allowPrivate !== false) return { allow: true, reason: "unrestricted", entry };
  const owner = snapshot.ownerUserId.trim();
  if (!owner) return { allow: true, reason: "unclaimed-fail-open", entry };
  if (query.fromUserId && query.fromUserId === owner) return { allow: true, reason: "owner", entry };
  return { allow: false, reason: "denied-stranger", entry };
}

export interface FsRestriction {
  restricted: boolean;
  /** 触发限制的 agent（用于日志：说清楚是谁把整机拖进了限制态）。 */
  because: string[];
  /** 实际参与判定的 agent（排除运维豁免者），日志里打出来便于运维排查。 */
  participating: string[];
  /** 本次判定被豁免（运维 agent）：它既不受限，也不参与「有一个人关就都不给」。 */
  exempt: boolean;
}

/**
 * 不参与文件读写闸门的 agent。
 *
 * 只有内置运维助手 `ops`（见 ops.ts 的 `OPS_AGENT_NAME`；这里不 import，避免
 * agents.ts ↔ ops.ts 的循环依赖）。为什么必须豁免：
 *
 * 新建绑定的默认值就是 `allowFs=false`，而运维助手的全部价值就是「替用户修
 * 环境」—— 禁掉读写等于新设备一上线运维就废。它不接受外部派活（没有权限
 * 区块、只服务本机主人），也不该被「某个对话 agent 关了读写」连坐。
 *
 * 代价是已知的：同一台设备上「受限的对话 agent + 运维 agent」并存时，运维的
 * 修复能力不会被降级（这是本豁免的目的），但整机配置仍是按 agent 一份，
 * 未来若要做真正的隔离需要 per-agent 配置目录 —— 见报告 §10 known issues。
 */
export const FS_GATE_EXEMPT_AGENTS: readonly string[] = ["ops"];

/** 名字形态：`ops`、`ops@<agentRC>`（per-agent 运维子进程）。 */
export function fsGateExempt(agent: string | undefined): boolean {
  const name = (agent ?? "").trim().toLowerCase();
  if (!name) return false;
  return FS_GATE_EXEMPT_AGENTS.some((exempt) => name === exempt || name.startsWith(`${exempt}@`));
}

/**
 * 整机是否进「文件只读」限制态。
 *
 * 派生配置是**整机一份**（opencode 的配置路径固定），没法按 agent 分开表达，
 * 所以取「有一个人关，就都不给」—— 已知不完美，但方向是安全的那一侧。
 *
 * 两条与运维相关的修正（队长 2026-10-27 裁决）：
 *   · 运维 agent（`ops`）自己**不**受限 —— 否则新建绑定（默认 allowFs=false）
 *     一上线运维就废；
 *   · 运维 agent 的 `allowFs` **不参与**这个判定 —— 它没有权限区块、也不接受
 *     外部派活，它的 false 不该连坐对话 agent。
 * 两者都会在 `participating` / `exempt` 里如实反映，供日志与排障使用。
 */
export function fsRestriction(
  snapshot: PermissionSnapshot | null,
  agent?: string,
): FsRestriction {
  const none: FsRestriction = { restricted: false, because: [], participating: [], exempt: false };
  if (!snapshot) return none;
  if (fsGateExempt(agent)) return { ...none, exempt: true };
  const participants = snapshot.agents.filter((entry) => !fsGateExempt(entry.agent));
  const participating = participants.map((entry) => entry.agent || entry.nodeId || "?");
  const off = participants.filter((entry) => entry.allowFs === false);
  if (off.length === 0) return { restricted: false, because: [], participating, exempt: false };
  return {
    restricted: true,
    because: off.map((entry) => entry.agent || entry.nodeId || "?"),
    participating,
    exempt: false,
  };
}

/** 这个 agent 的行为规范（服务端已拼好生效值）。空串 = 不下发。 */
export function behaviorPromptFor(
  snapshot: PermissionSnapshot | null,
  query: { nodeId?: string; agent?: string },
): string {
  const entry = findPermissionEntry(snapshot, query);
  return entry?.behaviorPrompt?.trim() ?? "";
}

/** 文件名安全化：agent 名会进路径，按不可信输入处理。 */
export function sanitizeAgentId(agent: string | undefined): string {
  const raw = (agent ?? "").trim();
  if (!raw) return "_";
  return raw.replace(/[^A-Za-z0-9_-]/g, "_");
}

/** opencode 行为规范落盘位置（per-agent：指令文件必须和被注入的 agent 对应）。 */
export function opencodeBehaviorPath(agent: string | undefined, home: string = homedir()): string {
  return join(home, ".clawmessenger", "agent-behavior", `${sanitizeAgentId(agent)}.md`);
}

/**
 * 写（或清掉）opencode 的行为规范文件，返回可放进 `instructions` 的路径。
 *
 * - 有内容：写入并返回路径
 * - 空内容：删掉旧文件并返回 null —— 否则「用户清空了行为规范」之后，配置里
 *   仍指着上一版文本，表现为「改了没生效」。
 */
export function writeOpencodeBehaviorFile(
  agent: string | undefined,
  prompt: string,
  home: string = homedir(),
): string | null {
  const path = opencodeBehaviorPath(agent, home);
  const text = prompt.trim();
  try {
    if (!text) {
      if (existsSync(path)) rmSync(path, { force: true });
      return null;
    }
    mkdirSync(dirname(path), { recursive: true });
    const body = [
      "# 行为规范（由 ClawMessenger 设备端维护，可整份删除）",
      "",
      text,
      "",
    ].join("\n");
    const existing = existsSync(path) ? readFileSync(path, "utf8") : null;
    if (existing !== body) writeFileSync(path, body, "utf8");
    return path;
  } catch {
    // 指令层注入是增强项：目录只读、磁盘满都不该让这一轮失败。
    return null;
  }
}
