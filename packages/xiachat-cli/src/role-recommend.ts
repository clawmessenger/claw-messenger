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
 *
 * max_roles **缺失**时调用方已在 parseRoleRecommendRequest 里回落到 3；而 max_roles
 * **非法**（非整数、越界）不是回落 —— 那条命令在 parseRoleRecommendRequest 里整条
 * 拒收，根本走不到这里。函数里的 !Number.isFinite 分支只是给导出的公共函数兜底
 * （直接传 NaN/Infinity 时不至于算出 NaN 个角色）。
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
  /**
   * 这一轮要用的模型 route（`provider/slug`），来自主持节点在后台配的
   * 「讨论主持 AI 模型」。
   *
   * 空 = 不指定，走设备默认模型。这个字段是推荐速度的主要杠杆：推荐是一次
   * 短输出任务（3 段 120 字的人设 + 一层 JSON），用旗舰模型要二三十秒，换一个
   * 快模型通常 2-3 秒 —— 链路本身并没有那么慢。
   */
  model?: string;
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
  // 模型 route 只做「非空且不长」的粗校验：认不认识由设备端 agent 自己决定，
  // 传一个它不认识的 route 会让这一轮直接失败 —— 所以服务端只在确实配了模型时
  // 才下发这个键，缺省（空串/缺字段）一律走节点默认模型。
  const rawModel = params.model;
  const model =
    typeof rawModel === "string" && rawModel.trim().length > 0 && rawModel.length <= 200
      ? rawModel.trim()
      : undefined;
  return {
    requestId: cmd.request_id,
    topic: params.topic,
    goal: typeof params.goal === "string" ? params.goal : "",
    maxRoles: rawMaxRoles,
    recommendationPrompt:
      typeof params.recommendation_prompt === "string" ? params.recommendation_prompt : "",
    style,
    model,
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
 * 解析失败的稳定短标识 —— 它同时出现在设备端日志（`reason=`）和服务端回执的
 * `payload.detail.reason` 里，所以取值只能加不能改：线上排查靠它认现场。
 */
export type RoleRecommendParseFailureReason =
  | "empty-output"
  | "no-json"
  | "roles-not-array"
  | "roles-empty"
  | "roles-too-many"
  | "role-not-object"
  | "role-name-invalid"
  | "role-name-duplicate"
  | "role-prompt-invalid"
  | "speaking-order-not-integer"
  | "speaking-order-not-permutation";

/**
 * 就地修正的记录值。模型常犯的两种「良性偏差」不该整份判死，修好之后仍要留痕，
 * 否则下一轮排查无法区分「模型答对了」与「我们替模型圆过去了」。
 */
export type RoleRecommendRepair = "speaking_order-1-based" | "roles-truncated";

export type RoleRecommendParseResult =
  | { ok: true; roles: RecommendedRole[]; repairs: RoleRecommendRepair[] }
  | { ok: false; reason: RoleRecommendParseFailureReason };

/** 失败时随应答带回设备的原文片段长度（服务端日志同样按它截断）。 */
export const ROLE_RECOMMEND_SAMPLE_CHARS = 400;

/**
 * Extracts and validates the roles from an agent's raw stdout. Returns null when
 * the answer cannot be trusted, which the caller reports as an error rather
 * than forwarding.
 *
 * 只保留 `roles | null` 这个老签名给「不关心失败原因」的调用方与老测试；
 * 需要知道**为什么**失败时用 parseRoleRecommendationOutputDetailed。
 */
export function parseRoleRecommendationOutput(
  output: string,
  maxRoles: number,
): RecommendedRole[] | null {
  const result = parseRoleRecommendationOutputDetailed(output, maxRoles);
  return result.ok ? result.roles : null;
}

/**
 * 解析 + 校验，并把失败原因与就地修正分开报出来。
 *
 * 为什么需要它：过去这一层把「模型答非所问」「JSON 被散文切坏」「角色数超了」
 * 「speaking_order 从 1 开始」全部压成 null，现场只留一行 `could not parse`，
 * 24s～84s 的失败跑完等于白跑。现在失败必须自证。
 */
export function parseRoleRecommendationOutputDetailed(
  output: string,
  maxRoles: number,
): RoleRecommendParseResult {
  if (typeof output !== "string" || !output.trim()) return { ok: false, reason: "empty-output" };
  // 取「roles 真的是数组的那个对象」，而不是「第一个能解析的对象」：思考里的
  // 示例对象常常又完整又更长，按长度取会把示例当答案（reason 变成 roles-not-array）。
  //
  // 判据收到 Array.isArray 而不是 `"roles" in value`：推理模型的示例里
  // `{"roles":"..."}` / `{"roles":{}}` 这种占位写法很常见，只认键名会把假示例
  // 当成真答案。收紧后：真答案（数组）一定赢过假示例；而单一对象只有
  // `{"roles":"nope"}` 时 prefer 全部落空 → 走 fallback 返回该对象 →
  // 后面照旧报 roles-not-array，诊断语义不变。
  const parsed = extractJsonObject(
    output,
    (value) => typeof value === "object" && value !== null
      && Array.isArray((value as { roles?: unknown }).roles),
  );
  if (parsed === null) return { ok: false, reason: "no-json" };
  const rawRoles = (parsed as { roles?: unknown }).roles;
  if (!Array.isArray(rawRoles)) return { ok: false, reason: "roles-not-array" };
  if (rawRoles.length === 0) return { ok: false, reason: "roles-empty" };

  // 逐条看字段：一条坏就整份不收，但要能指认坏在哪一条的哪个字段。
  const names = new Set<string>();
  const normalized: RecommendedRole[] = [];
  for (const item of rawRoles) {
    if (typeof item !== "object" || item === null || Array.isArray(item)) {
      return { ok: false, reason: "role-not-object" };
    }
    const role = item as Record<string, unknown>;

    const name = trimmedText(role.role_name, ROLE_RECOMMEND_MAX_NAME);
    if (!name) return { ok: false, reason: "role-name-invalid" };
    if (names.has(name)) return { ok: false, reason: "role-name-duplicate" };
    const prompt = trimmedText(role.role_prompt, ROLE_RECOMMEND_MAX_PROMPT);
    if (!prompt) return { ok: false, reason: "role-prompt-invalid" };
    const order = integerValue(role.speaking_order);
    if (order === null) return { ok: false, reason: "speaking-order-not-integer" };

    names.add(name);
    normalized.push({ role_name: name, role_prompt: prompt, speaking_order: order });
  }

  const repairs: RoleRecommendRepair[] = [];
  let kept = normalized;
  const limit = roleRecommendAcceptLimit(maxRoles);
  if (kept.length > limit) {
    // 提示词要 N 个、模型给了 N+1：按 speaking_order 升序保留前 limit 个。
    // 只有截断后仍能通过校验才算修好 —— 截断不清一个坏答案。
    kept = [...kept].sort((a, b) => a.speaking_order - b.speaking_order).slice(0, limit);
    repairs.push("roles-truncated");
  }

  const truncated = kept.length < normalized.length;
  if (!acceptSpeakingOrder(kept, repairs)) {
    // 已经进入截断分支时统一报 roles-too-many：角色数本身就是错的，再报排列
    // 错误会把排查方向带偏（服务端两个码都是 INVALID，设备端日志才是判据）。
    return { ok: false, reason: truncated ? "roles-too-many" : "speaking-order-not-permutation" };
  }
  return {
    ok: true,
    roles: kept.sort((a, b) => a.speaking_order - b.speaking_order),
    repairs,
  };
}

/**
 * 校验 speaking_order 是否恰好是 0..n-1 的排列；1-based（1..n）时先整体减 1
 * 再收下（模型按「第 1 个/第 2 个」数数，这是最高频的良性偏差）。
 */
function acceptSpeakingOrder(roles: RecommendedRole[], repairs: RoleRecommendRepair[]): boolean {
  const sorted = roles.map((role) => role.speaking_order).sort((a, b) => a - b);
  if (isPermutationFrom(sorted, 0)) return true;
  if (!isPermutationFrom(sorted, 1)) return false;
  for (const role of roles) role.speaking_order -= 1;
  repairs.push("speaking_order-1-based");
  return true;
}

function isPermutationFrom(sortedOrders: readonly number[], base: number): boolean {
  for (let index = 0; index < sortedOrders.length; index += 1) {
    if (sortedOrders[index] !== base + index) return false;
  }
  return true;
}

/**
 * 本地收受闸门的上限：服务端下发的 max_roles 已经过 1..8 的校验，这里的夹逼只是
 * 给导出的公共函数兜底（直接传 0/NaN/Infinity 时不至于算出 0 个角色）。
 */
function roleRecommendAcceptLimit(maxRoles: number): number {
  if (!Number.isFinite(maxRoles)) return ROLE_RECOMMEND_MAX_ROLES;
  const bounded = Math.trunc(maxRoles);
  if (bounded < 1) return 1;
  return Math.min(bounded, ROLE_RECOMMEND_MAX_ROLES);
}

/**
 * 失败现场的单行化片段：取前 ROLE_RECOMMEND_SAMPLE_CHARS 个字符，把换行压成
 * `\n` 字面量。原样多行输出会把 journalctl / 应用日志一行撑成几百行，反而看不见
 * 别的现场。
 */
export function roleRecommendationOutputSample(output: string): string {
  if (typeof output !== "string") return "";
  return output
    .slice(0, ROLE_RECOMMEND_SAMPLE_CHARS)
    .replace(/\r\n?/g, "\n")
    .replace(/\n/g, "\\n");
}

/** The success reply: roles ride in payload.roles, kind "text". */
export function buildRoleRecommendationResponse(
  requestId: string,
  roles: RecommendedRole[],
): string {
  return encodeCommandResult(requestId, "text", { roles });
}

/** The failure reply. The server maps the message to a public error code. */
export function buildRoleRecommendationError(
  requestId: string,
  message: string,
  detail?: { reason: string; sample: string },
): string {
  // `error` 的文本一个字都不能改：服务端 mapNodeError 靠它分类（含 "timeout"
  // 才判 TIMEOUT，其余一律 INVALID）。自证信息只走 detail，服务端只记日志、
  // 不参与任何判定。
  return encodeCommandResult(
    requestId,
    "error",
    detail ? { error: message, detail } : { error: message },
  );
}

/**
 * Pulls a JSON object out of model output: a bare object, a fenced block, or an
 * object embedded in surrounding prose. Returns null when none parses.
 *
 * 第三条路径（内嵌）不能用「第一个 `{` 到最后一个 `}`」：推理型模型的散文/思考
 * 里常带花括号（举例、空对象、更外层的包装），那样切出来的区间横跨两段 JSON，
 * JSON.parse 必失败 —— 现场表现就是「同一台设备同一个模型偶发无法解析」。
 * 改为按字符串/转义感知的配对括号枚举每一个平衡区间，从长到短试。
 *
 * `prefer` 是可选的「哪种对象才算数」判据，调用方注入，提取器本身不认识业务
 * 字段。为什么需要它：推理模型的思考里常写一个**完整可解析**的示例对象
 * （`{"analysis":"…400 字…"}`），长度往往超过真正的答案；只按长度取第一个可
 * 解析的，就会拿到示例而不是答案。有 prefer 时优先返回第一个满足它的结果，
 * 一个都不满足才退回老行为（第一个可解析的），因此不传 prefer 时行为逐字节
 * 不变。
 */
export function extractJsonObject(
  output: string,
  prefer?: (value: unknown) => boolean,
): unknown {
  const text = output.trim();
  if (!text) return null;
  // 先通过 prefer 的先返回；否则按优先级顺序记下「第一个可解析的」当兜底。
  const fallbacks: unknown[] = [];
  const accept = (parsed: unknown): boolean => {
    if (parsed === null) return false;
    if (prefer === undefined || prefer(parsed)) return true;
    fallbacks.push(parsed);
    return false;
  };

  const direct = tryParse(text);
  if (accept(direct)) return direct;

  const fence = /```(?:json)?\s*([\s\S]*?)```/i.exec(text);
  if (fence) {
    const fenced = tryParse(fence[1].trim());
    if (accept(fenced)) return fenced;
  }

  // 从长到短：最外层平衡区间优先（它最可能是一整份应答），被包围的碎片随后。
  for (const candidate of balancedObjectCandidates(text)) {
    const parsed = tryParse(candidate);
    if (accept(parsed)) return parsed;
  }
  return fallbacks.length > 0 ? fallbacks[0] : null;
}

/**
 * 枚举 text 里所有**平衡**的 `{…}` 区间，按长度降序返回（等长保持原文顺序，
 * sort 稳定）。
 *
 * 单趟栈扫描：一次遍历就拿到所有区间的起止，整体 O(n + k log k)。逐个 `{` 各
 * 扫到末尾是 O(n×括号数) —— 模型卡住时反复吐 `{` 是真实退化形态，64KB 输出
 * 能把它拖到秒级。
 *
 * 为什么要字符串感知：role_prompt 里出现 `}` 或 `\"` 是常态，按裸字符数括号会
 * 在字符串中间判定区间结束。
 */
function balancedObjectCandidates(text: string): string[] {
  const spans: Array<{ start: number; end: number }> = [];
  const openStack: number[] = [];
  let inString = false;
  let escaped = false;
  for (let index = 0; index < text.length; index += 1) {
    const char = text[index];
    if (inString) {
      if (escaped) {
        escaped = false;
      } else if (char === "\\") {
        escaped = true;
      } else if (char === '"') {
        inString = false;
      }
      continue;
    }
    if (char === '"') {
      inString = true;
      continue;
    }
    if (char === "{") {
      openStack.push(index);
      continue;
    }
    if (char === "}") {
      const start = openStack.pop();
      // 没有配对的开括号（`}` 多出来的那种）直接丢掉，不能当成区间起点。
      if (start !== undefined) spans.push({ start, end: index });
    }
  }
  // 按区间长度降序；sort 稳定，等长时保留「先闭合者在前」的原文顺序。
  spans.sort((a, b) => (b.end - b.start) - (a.end - a.start));
  return spans.map((span) => text.slice(span.start, span.end + 1));
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
