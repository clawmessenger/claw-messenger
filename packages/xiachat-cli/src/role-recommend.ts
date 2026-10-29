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
/** 本地**拒收**闸门：role_prompt 超过这个长度就判定模型答非所问。
 *
 * 故意远大于产品口径（`ROLE_RECOMMEND_ROLE_PROMPT_CHARS` = 120 字）：它挡的是
 * 「模型把整篇分析当 role_prompt 塞回来」这类明显跑偏的应答，不是产品要求的
 * 产出尺寸。把这个值压到 120 会把模型正常的长回答判成错误回复，失败率反而升
 * 高 —— 产出尺寸只写在提示词里，校验闸门保持宽松。
 */
export const ROLE_RECOMMEND_MAX_PROMPT = 8000;
/** 产品口径：一轮推荐要几个角色（服务端下发的 max_roles 更小时以它为准）。 */
export const ROLE_RECOMMEND_TARGET_ROLES = 3;
/** 产品口径：每个 role_prompt 的字数上限（中文按字数算，标点也算一个字）。
 *
 * 只出现在提示词里，**不参与**本地校验（见 ROLE_RECOMMEND_MAX_PROMPT）。
 * 为什么钉这么短：role_prompt 是每轮发言都注入给 agent 的人设，一段人设越长，
 * 设备端一次完整 LLM 回合就越慢，讨论组「推荐角色」等十几秒到 100s+ 的主因之一
 * 就是产出量太大。
 */
export const ROLE_RECOMMEND_ROLE_PROMPT_CHARS = 120;

/**
 * 本轮实际要几个角色：产品口径 3 个，服务端下发的 max_roles 更小时按 max_roles
 * （一个 agent 只能演一个角色，候选设备不够时多要的角色没人演）。
 * max_roles 缺失/非法时调用方已在 parseRoleRecommendRequest 里回落到 3。
 */
export function roleRecommendTargetRoleCount(maxRoles: number): number {
  if (!Number.isFinite(maxRoles)) return ROLE_RECOMMEND_TARGET_ROLES;
  const bounded = Math.min(ROLE_RECOMMEND_MAX_ROLES, Math.max(1, Math.trunc(maxRoles)));
  return Math.min(ROLE_RECOMMEND_TARGET_ROLES, bounded);
}

// ---- 讨论风格 --------------------------------------------------------------
//
// C 端创建圆桌时先选「讨论风格」（clawmessenger-web/src/lib/
// roundtable-draft.ts 的 DISCUSSION_STYLES），但那一步只把风格压成 goal 里的
// 一个短语，模型看到五个风格给出的角色几乎是同一套（观察者/质疑者/总结者…），
// 五个风格的角色于是同名同调。
//
// 这里为每种风格钉死两样东西，让风格真正落到推荐结果上：
//   1. roleNames —— 该风格专属的角色骨架，角色名**不跨风格复用**，从源头
//      保证两种风格不会推荐同一批角色；
//   2. voice     —— 该风格的发言口吻，要求写进每个 role_prompt。role_prompt
//      就是讨论进行时注入给 agent 的角色人设（服务端
//      discussion_interactive.go 的 interactiveBuildPrompt 取
//      role.roleInstructions），所以口吻写进去就等于「聊天风格不一致」。
//
// 改这里要同时核对 C 端的 DISCUSSION_STYLES：id 与 goalPhrase 必须一一对应
// （goalPhrase 是 C 端 buildRoundtableGoal 拼进 goal 的原话，用于旧链路的兜底
// 识别）。两边对不上时，兜底识别会失效，风格只剩显式 style 参数一条路。
export type DiscussionStyle = "free" | "brainstorm" | "debate" | "review" | "consensus";

interface StyleProfile {
  /** 写给模型看的风格名。 */
  label: string;
  /** C 端 buildRoundtableGoal 里「以……的方式展开讨论」的原话。 */
  goalPhrase: string;
  /** 该风格专属的角色骨架，按「最核心在前」排列。 */
  roleNames: readonly string[];
  /** 该风格的发言口吻，必须被写进每个 role_prompt。 */
  voice: string;
}

const STYLE_PROFILES: Record<DiscussionStyle, StyleProfile> = {
  free: {
    label: "自由讨论 (free)",
    goalPhrase: "自由交流",
    roleNames: ["观察者", "提问者", "联想者", "综合者"],
    voice:
      "像熟人聊天：短句、口语，不用小标题也不用编号列表；一次只讲一个点，带上自己真实的感受或疑问。",
  },
  brainstorm: {
    label: "头脑风暴 (brainstorm)",
    goalPhrase: "头脑风暴发散共创",
    roleNames: ["创意发起人", "联想跳跃者", "逆向挑战者", "收敛整理者"],
    voice:
      "轻快跳跃，鼓励异想天开：多用「如果……会怎样」，先抛点子不急着评价，一次给 3-5 条短促的点子。",
  },
  debate: {
    label: "正反辩论 (debate)",
    goalPhrase: "正反两方交锋辩论",
    roleNames: ["正方主辩", "反方主辩", "证据审查员", "裁判"],
    voice:
      "针锋相对：先亮立场再论证，句式用「我反对……因为……」，必须引用数据或实例；不寒暄、不客套。",
  },
  review: {
    label: "专家评审 (review)",
    goalPhrase: "专家多角度评审",
    roleNames: ["领域专家", "风险审查员", "成本评审员", "合规评审员"],
    voice:
      "严谨书面：逐条编号陈述，每条给出「结论 + 依据 + 风险等级」；结尾必须给「通过 / 有条件通过 / 不通过」。",
  },
  consensus: {
    label: "决策共识 (consensus)",
    goalPhrase: "聚焦决策凝聚共识",
    roleNames: ["决策主持人", "方案提出者", "反对派", "落地执行者"],
    voice:
      "收敛务实：只谈取舍与行动，句式用「我建议……，代价是……」；结尾必须给出 1-2 条可执行动作。",
  },
};

/**
 * 认出这次推荐属于哪种讨论风格：显式 style 优先，其次从 goal 里回捞 C 端拼进去
 * 的风格短语（旧前端 / 旧服务端不下发 style，只能靠它）。都认不出时返回 null，
 * 此时提示词里不加风格段落，行为与加风格之前完全一致。
 */
export function detectDiscussionStyle(
  style: string | undefined,
  goal: string,
): DiscussionStyle | null {
  if (style !== undefined && Object.prototype.hasOwnProperty.call(STYLE_PROFILES, style)) {
    return style as DiscussionStyle;
  }
  for (const [id, profile] of Object.entries(STYLE_PROFILES) as [DiscussionStyle, StyleProfile][]) {
    if (goal.includes(`以${profile.goalPhrase}的方式`)) return id;
  }
  return null;
}


/** The admin waits at most this long for a recommendation, so a replay older
 * than this can no longer be delivered to anyone. */
export const STALE_ROLE_RECOMMEND_REQUEST_MS = 10 * 60 * 1000;

export interface RoleRecommendRequest {
  requestId: string;
  topic: string;
  goal: string;
  maxRoles: number;
  recommendationPrompt: string;
  /**
   * 讨论风格 id（free / brainstorm / debate / review / consensus）。C 端新版
   * 会显式下发；不认识的取值会被忽略，退回 goal 兜底识别。
   */
  style?: string;
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
  // 风格 id 只做「非空且不长」的粗校验；是不是已知风格由 detectDiscussionStyle
  // 决定 —— 这样服务端将来加风格也不必同步改这里。
  const rawStyle = params.style ?? params.discussion_style;
  const style =
    typeof rawStyle === "string" && rawStyle.trim().length > 0 && rawStyle.length <= 64
      ? rawStyle.trim()
      : undefined;
  return {
    requestId: cmd.request_id,
    topic: params.topic,
    goal: typeof params.goal === "string" ? params.goal : "",
    maxRoles: rawMaxRoles,
    recommendationPrompt:
      typeof params.recommendation_prompt === "string" ? params.recommendation_prompt : "",
    style,
    timestamp,
  };
}

/**
 * 风格段落：把该风格的角色骨架与发言口吻要求写进提示词。角色骨架保证「换个风格
 * 就换一批角色」，口吻要求保证「换个风格就换一种说话方式」（它最终落在
 * role_prompt 里，而 role_prompt 就是每轮发言时注入给 agent 的人设）。
 *
 * `targetRoles` 是**本轮实际要的角色数**（已经过 roleRecommendTargetRoleCount 夹逼），
 * 不是服务端下发的 max_roles：两处用不同的数字会让提示词自相矛盾 ——
 * 「Use exactly these role_name values」列了 4 个，下面又写「Return exactly 3 roles」。
 */
function buildStyleSection(profile: StyleProfile, targetRoles: number): string[] {
  const names = profile.roleNames.slice(0, targetRoles);
  const lines = [
    `Discussion style: ${profile.label}.`,
    "This style has its own cast. Do not fall back on a generic set of roles — a different style must produce different role names.",
    "",
    "Use exactly these role_name values, in this order:",
    ...names.map((name, index) => `${index + 1}. ${name}`),
  ];
  const spare = targetRoles - names.length;
  if (spare > 0) {
    lines.push(
      `This round wants ${spare} more participant(s) than the list provides: add up to ${spare} role(s) in the same spirit, and never reuse a name already listed.`,
    );
  }
  lines.push(
    "",
    "Write this style's speaking voice INTO every role_prompt, as an instruction that role must follow on every single turn:",
    profile.voice,
    "role_prompt is handed to that participant verbatim when it speaks, so the voice has to live there — it is what keeps two different styles from sounding alike.",
  );
  return lines;
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
  // 本轮实际要几个角色：产品口径 3 个与 max_roles 取小（见 roleRecommendTargetRoleCount）。
  const targetRoles = roleRecommendTargetRoleCount(request.maxRoles);
  const style = detectDiscussionStyle(request.style, request.goal);
  if (style !== null) {
    lines.push("", ...buildStyleSection(STYLE_PROFILES[style], targetRoles));
  }
  lines.push(
    `Return exactly ${targetRoles} role${targetRoles === 1 ? "" : "s"}.`,
    "",
    "Reply with ONLY a JSON object, no prose and no markdown fence:",
    '{"roles":[{"role_name":"...","role_prompt":"...","speaking_order":0}]}',
    "",
    "- role_name: a short distinct label (<= 80 characters)",
    `- role_prompt: ONE continuous paragraph of a natural persona, at most ${ROLE_RECOMMEND_ROLE_PROMPT_CHARS} Chinese characters (中文按字数算，标点也算一个字). 不写小标题、不分点、不举例、不换行 —— 就一段话，说清这个角色是谁、关心什么、发言时怎么做。`,
    "- speaking_order: 0-based, and across the roles exactly 0,1,2,… with no gaps or repeats",
    "",
    `Keep role_prompt at or under ${ROLE_RECOMMEND_ROLE_PROMPT_CHARS} characters on purpose: it is injected into that participant's context on every speaking turn, so a long one slows the whole roundtable down. 一段话写清人设即可，不要为了显得完整而展开。`,
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
