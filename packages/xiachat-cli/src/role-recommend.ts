import { encodeCommandResult, type CommandContent } from "./protocol.js";

// Protocol v1 role-recommendation responder for the admin 「系统 → 讨论主持 AI →
// 测试角色推荐」 button.
//
// The server sends this node a `command` message on the shared envelope:
//   { request_id, service: "discussion", action: "recommend_roles",
//     params: { topic, goal, max_roles, recommendation_prompt, config_version,
//               protocol_version: "v1", timestamp } }
// and waits (up to the configured host timeout) for a command_result carrying
// `payload.roles`. The Go side re-validates everything (see
// rongcloud.NodeRequestBroker.parseRecommendedRoles), so this module's own
// validation exists only to turn a bad model answer into an honest error reply
// instead of shipping garbage that will be rejected downstream.
//
// Nothing about the local machine leaves the device: the reply carries only role
// names, prompts and speaking order.

export const ROLE_RECOMMEND_ACTION = "recommend_roles";
export const ROLE_RECOMMEND_PROTOCOL_VERSION = "v1";

export const ROLE_RECOMMEND_MAX_ROLES = 8;
export const ROLE_RECOMMEND_MAX_NAME = 80;
export const ROLE_RECOMMEND_MAX_PROMPT = 8000;

/** The admin waits at most this long for a recommendation, so a replay older
 * than this can no longer be delivered to anyone. */
export const STALE_ROLE_RECOMMEND_REQUEST_MS = 10 * 60 * 1000;

export interface RoleRecommendRequest {
  requestId: string;
  topic: string;
  goal: string;
  maxRoles: number;
  recommendationPrompt: string;
  timestamp?: number;
}

export interface RecommendedRole {
  role_name: string;
  role_prompt: string;
  speaking_order: number;
}

/**
 * Strictly parses the inbound request; `null` means "not ours / not valid". The
 * caller has already matched service/action, so this only has to reject a
 * malformed param set.
 */
export function parseRoleRecommendRequest(cmd: CommandContent): RoleRecommendRequest | null {
  const params = (cmd.params ?? {}) as Record<string, unknown>;
  if (typeof params.topic !== "string" || !params.topic.trim()) return null;
  const rawMaxRoles = params.max_roles ?? 3;
  if (typeof rawMaxRoles !== "number" || !Number.isInteger(rawMaxRoles)) return null;
  if (rawMaxRoles < 1 || rawMaxRoles > ROLE_RECOMMEND_MAX_ROLES) return null;
  const timestamp =
    typeof params.timestamp === "number" && Number.isSafeInteger(params.timestamp)
      ? params.timestamp
      : undefined;
  return {
    requestId: cmd.request_id,
    topic: params.topic,
    goal: typeof params.goal === "string" ? params.goal : "",
    maxRoles: rawMaxRoles,
    recommendationPrompt:
      typeof params.recommendation_prompt === "string" ? params.recommendation_prompt : "",
    timestamp,
  };
}

/**
 * Builds the prompt handed to the local agent. The admin's own
 * `recommendation_prompt` is the operator's instruction and leads, so a
 * deployment can tune the phrasing without shipping a new CLI; the framing and
 * the output contract are appended by us and cannot be overridden.
 */
export function buildRoleRecommendPrompt(request: RoleRecommendRequest): string {
  const lines: string[] = [];
  if (request.recommendationPrompt.trim()) {
    lines.push(request.recommendationPrompt.trim(), "");
  }
  lines.push(
    "Propose the discussion roles for a multi-agent roundtable.",
    "",
    `Topic: ${request.topic}`,
  );
  if (request.goal.trim()) {
    lines.push(`Goal: ${request.goal.trim()}`);
  }
  lines.push(
    `Return between 1 and ${request.maxRoles} roles.`,
    "",
    "Reply with ONLY a JSON object, no prose and no markdown fence:",
    '{"roles":[{"role_name":"...","role_prompt":"...","speaking_order":0}]}',
    "",
    "- role_name: a short distinct label (<= 80 characters)",
    "- role_prompt: the instructions for that role (<= 8000 characters)",
    "- speaking_order: 0-based, and across the roles exactly 0,1,2,… with no gaps or repeats",
  );
  return lines.join("\n");
}

/**
 * Extracts and validates the roles from an agent's raw stdout. Returns null when
 * the answer cannot be trusted, which the caller reports as an error rather
 * than forwarding.
 */
export function parseRoleRecommendationOutput(
  output: string,
  maxRoles: number,
): RecommendedRole[] | null {
  const parsed = extractJsonObject(output);
  if (parsed === null) return null;
  const roles = (parsed as { roles?: unknown }).roles;
  if (!Array.isArray(roles) || roles.length === 0 || roles.length > maxRoles) return null;

  const names = new Set<string>();
  const orders: number[] = [];
  const normalized: RecommendedRole[] = [];

  for (const item of roles) {
    if (typeof item !== "object" || item === null) return null;
    const role = item as Record<string, unknown>;

    const name = trimmedText(role.role_name, ROLE_RECOMMEND_MAX_NAME);
    if (!name || names.has(name)) return null;
    const prompt = trimmedText(role.role_prompt, ROLE_RECOMMEND_MAX_PROMPT);
    if (!prompt) return null;
    const order = integerValue(role.speaking_order);
    if (order === null) return null;

    names.add(name);
    orders.push(order);
    normalized.push({ role_name: name, role_prompt: prompt, speaking_order: order });
  }

  const sorted = [...orders].sort((a, b) => a - b);
  for (let i = 0; i < sorted.length; i += 1) {
    if (sorted[i] !== i) return null;
  }
  return normalized.sort((a, b) => a.speaking_order - b.speaking_order);
}

/** The success reply: roles ride in payload.roles, kind "text". */
export function buildRoleRecommendationResponse(
  requestId: string,
  roles: RecommendedRole[],
): string {
  return encodeCommandResult(requestId, "text", { roles });
}

/** The failure reply. The server maps the message to a public error code. */
export function buildRoleRecommendationError(requestId: string, message: string): string {
  return encodeCommandResult(requestId, "error", { error: message });
}

/**
 * Pulls a JSON object out of model output: a bare object, a fenced block, or an
 * object embedded in surrounding prose. Returns null when none parses.
 */
export function extractJsonObject(output: string): unknown {
  const text = output.trim();
  if (!text) return null;
  const direct = tryParse(text);
  if (direct !== null) return direct;

  const fence = /```(?:json)?\s*([\s\S]*?)```/i.exec(text);
  if (fence) {
    const fenced = tryParse(fence[1].trim());
    if (fenced !== null) return fenced;
  }

  const start = text.indexOf("{");
  const end = text.lastIndexOf("}");
  if (start === -1 || end <= start) return null;
  return tryParse(text.slice(start, end + 1));
}

function tryParse(candidate: string): unknown {
  try {
    const parsed: unknown = JSON.parse(candidate);
    return typeof parsed === "object" && parsed !== null ? parsed : null;
  } catch {
    return null;
  }
}

function trimmedText(value: unknown, maximum: number): string | null {
  if (typeof value !== "string") return null;
  const text = value.trim();
  if (!text || [...text].length > maximum) return null;
  return text;
}

function integerValue(value: unknown): number | null {
  if (typeof value === "number" && Number.isInteger(value)) return value;
  if (typeof value === "string" && /^-?\d+$/.test(value.trim())) return Number(value.trim());
  return null;
}
