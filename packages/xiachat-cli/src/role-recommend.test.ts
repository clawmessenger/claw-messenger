import { describe, expect, it } from "vitest";
import {
  buildRoleRecommendPrompt,
  buildRoleRecommendationError,
  buildRoleRecommendationResponse,
  detectDiscussionStyle,
  parseRoleRecommendRequest,
  parseRoleRecommendationOutput,
  ROLE_RECOMMEND_MAX_PROMPT,
  ROLE_RECOMMEND_ROLE_PROMPT_CHARS,
  ROLE_RECOMMEND_TARGET_ROLES,
  roleRecommendTargetRoleCount,
  STALE_ROLE_RECOMMEND_REQUEST_MS,
  type RoleRecommendRequest,
} from "./role-recommend.js";

describe("parseRoleRecommendRequest", () => {
  const base = {
    request_id: "roles_1",
    service: "discussion",
    action: "recommend_roles",
    params: { topic: "AI 与就业", goal: "", max_roles: 3, timestamp: 1_700_000_000_000 },
  };

  it("accepts a well-formed request", () => {
    const parsed = parseRoleRecommendRequest(base);
    expect(parsed).toEqual({
      requestId: "roles_1",
      topic: "AI 与就业",
      goal: "",
      maxRoles: 3,
      recommendationPrompt: "",
      timestamp: 1_700_000_000_000,
    });
  });

  it("defaults max_roles to 3 and goal/prompt to empty", () => {
    const parsed = parseRoleRecommendRequest({
      ...base,
      params: { topic: "t" },
    });
    expect(parsed?.maxRoles).toBe(3);
    expect(parsed?.goal).toBe("");
    expect(parsed?.recommendationPrompt).toBe("");
    expect(parsed?.timestamp).toBeUndefined();
  });

  // model 只在服务端确实配了模型时下发。缺失/空串必须落成 undefined，否则
  // im.ts 会把一个空串当「显式指定模型」传给 agent，`--model ""` 直接让这一轮
  // 起不来 —— 表现是「角色推荐忽然全红」，比用默认模型慢严重得多。
  it("takes the model route when present and drops blanks", () => {
    expect(parseRoleRecommendRequest({
      ...base,
      params: { ...base.params, model: "anthropic/claude-haiku-5-5" },
    })?.model).toBe("anthropic/claude-haiku-5-5");
    expect(parseRoleRecommendRequest({
      ...base,
      params: { ...base.params, model: "   " },
    })?.model).toBeUndefined();
    expect(parseRoleRecommendRequest({
      ...base,
      params: { ...base.params, model: 42 },
    })?.model).toBeUndefined();
    // 超长串当脏数据丢掉：让它进 argv 只会得到一个必然失败的回合。
    expect(parseRoleRecommendRequest({
      ...base,
      params: { ...base.params, model: "x".repeat(201) },
    })?.model).toBeUndefined();
  });

  it("rejects a missing/blank topic", () => {
    expect(parseRoleRecommendRequest({ ...base, params: { max_roles: 3 } })).toBeNull();
    expect(parseRoleRecommendRequest({ ...base, params: { topic: "   " } })).toBeNull();
  });

  it("rejects an out-of-range or non-integer max_roles", () => {
    expect(parseRoleRecommendRequest({ ...base, params: { topic: "t", max_roles: 0 } })).toBeNull();
    expect(parseRoleRecommendRequest({ ...base, params: { topic: "t", max_roles: 9 } })).toBeNull();
    expect(parseRoleRecommendRequest({ ...base, params: { topic: "t", max_roles: 2.5 } })).toBeNull();
    expect(parseRoleRecommendRequest({ ...base, params: { topic: "t", max_roles: "3" } })).toBeNull();
  });

  it("carries an explicit discussion style through, defaulting to undefined", () => {
    // 旧请求（没有 style）必须保持 undefined —— 提示词才会退回旧形态。
    expect(parseRoleRecommendRequest(base)?.style).toBeUndefined();

    const camel = parseRoleRecommendRequest({ ...base, params: { ...base.params, style: "debate" } });
    expect(camel?.style).toBe("debate");

    // 服务端用 snake_case 下发时同样认。
    const snake = parseRoleRecommendRequest({ ...base, params: { ...base.params, discussion_style: "review" } });
    expect(snake?.style).toBe("review");

    // 空白或过长的取值不当作风格。
    expect(parseRoleRecommendRequest({ ...base, params: { ...base.params, style: "   " } })?.style).toBeUndefined();
    expect(parseRoleRecommendRequest({ ...base, params: { ...base.params, style: "x".repeat(65) } })?.style).toBeUndefined();
  });
});

describe("buildRoleRecommendPrompt", () => {
  it("leads with the operator prompt and always states the output contract", () => {
    const prompt = buildRoleRecommendPrompt({
      requestId: "r",
      topic: "AI 与就业",
      goal: "找平衡",
      maxRoles: 4,
      recommendationPrompt: "用中文，角色要具体",
    });
    expect(prompt.startsWith("用中文，角色要具体")).toBe(true);
    expect(prompt).toContain("Topic: AI 与就业");
    expect(prompt).toContain("Goal: 找平衡");
    // 产品口径 3 个角色：max_roles=4 时目标仍是 3（min(3, max_roles)）。
    expect(prompt).toContain("Return exactly 3 roles.");
    expect(prompt).not.toContain("Return exactly 4 roles.");
    expect(prompt).toContain('"speaking_order"');
  });

  it("omits the goal line when there is no goal", () => {
    const prompt = buildRoleRecommendPrompt({
      requestId: "r",
      topic: "t",
      goal: "",
      maxRoles: 2,
      recommendationPrompt: "",
    });
    expect(prompt).not.toContain("Goal:");
  });

  it("pins role_prompt to one short paragraph in the prompt only", () => {
    const prompt = buildRoleRecommendPrompt({
      requestId: "r",
      topic: "t",
      goal: "",
      maxRoles: 3,
      recommendationPrompt: "",
    });
    expect(prompt).toContain(String(ROLE_RECOMMEND_ROLE_PROMPT_CHARS));
    expect(prompt).toContain("不写小标题、不分点、不举例");
    // 提示词里的尺寸口径与本地拒收闸门是两件事：闸门必须依旧宽松，
    // 否则模型正常的长回答会被判成「错误回复」。
    expect(ROLE_RECOMMEND_MAX_PROMPT).toBe(8000);
    expect(ROLE_RECOMMEND_MAX_PROMPT).toBeGreaterThan(ROLE_RECOMMEND_ROLE_PROMPT_CHARS);
  });
});

// 提示词里的角色数 = min(产品口径 3, 服务端下发的 max_roles)。
describe("roleRecommendTargetRoleCount", () => {
  it("takes the smaller of the product target and max_roles", () => {
    expect(ROLE_RECOMMEND_TARGET_ROLES).toBe(3);
    expect(roleRecommendTargetRoleCount(1)).toBe(1);
    expect(roleRecommendTargetRoleCount(2)).toBe(2);
    expect(roleRecommendTargetRoleCount(3)).toBe(3);
    expect(roleRecommendTargetRoleCount(4)).toBe(3);
    expect(roleRecommendTargetRoleCount(8)).toBe(3);
    // 非法/缺省不该产出 0 或 NaN 个角色。
    expect(roleRecommendTargetRoleCount(Number.NaN)).toBe(3);
    expect(roleRecommendTargetRoleCount(0)).toBe(1);
  });

  it("asks for max_roles roles when the pool is smaller than the target", () => {
    const two = buildRoleRecommendPrompt({
      requestId: "r",
      topic: "t",
      goal: "",
      maxRoles: 2,
      recommendationPrompt: "",
    });
    expect(two).toContain("Return exactly 2 roles.");
    // 校验闸门跟着 max_roles 走（不是产品口径）：少于 equal 不拦。
    expect(parseRoleRecommendationOutput(JSON.stringify({
      roles: [
        { role_name: "A", role_prompt: "p", speaking_order: 0 },
        { role_name: "B", role_prompt: "p", speaking_order: 1 },
      ],
    }), 2)).not.toBeNull();
  });
});

describe("parseRoleRecommendationOutput", () => {
  const roles = [
    { role_name: "主持人", role_prompt: "引导讨论", speaking_order: 0 },
    { role_name: "质疑者", role_prompt: "提出反例", speaking_order: 1 },
  ];

  it("parses a bare JSON object", () => {
    expect(parseRoleRecommendationOutput(JSON.stringify({ roles }), 3)).toEqual(roles);
  });

  it("parses a fenced JSON block", () => {
    const output = "Here you go:\n```json\n" + JSON.stringify({ roles }) + "\n```";
    expect(parseRoleRecommendationOutput(output, 3)).toEqual(roles);
  });

  it("parses an object embedded in surrounding prose", () => {
    const output = "Sure! " + JSON.stringify({ roles }) + " done.";
    expect(parseRoleRecommendationOutput(output, 3)).toEqual(roles);
  });

  it("accepts numeric strings for speaking_order and re-sorts", () => {
    const shuffled = [
      { role_name: "B", role_prompt: "p", speaking_order: "1" },
      { role_name: "A", role_prompt: "p", speaking_order: "0" },
    ];
    expect(parseRoleRecommendationOutput(JSON.stringify({ roles: shuffled }), 2)).toEqual([
      { role_name: "A", role_prompt: "p", speaking_order: 0 },
      { role_name: "B", role_prompt: "p", speaking_order: 1 },
    ]);
  });

  it("rejects a non-permutation speaking order", () => {
    const dup = [
      { role_name: "A", role_prompt: "p", speaking_order: 0 },
      { role_name: "B", role_prompt: "p", speaking_order: 0 },
    ];
    expect(parseRoleRecommendationOutput(JSON.stringify({ roles: dup }), 3)).toBeNull();
    const gap = [{ role_name: "A", role_prompt: "p", speaking_order: 3 }];
    expect(parseRoleRecommendationOutput(JSON.stringify({ roles: gap }), 3)).toBeNull();
  });

  it("rejects duplicate names, empty fields and too many roles", () => {
    const dupName = [
      { role_name: "A", role_prompt: "p", speaking_order: 0 },
      { role_name: "A", role_prompt: "p", speaking_order: 1 },
    ];
    expect(parseRoleRecommendationOutput(JSON.stringify({ roles: dupName }), 3)).toBeNull();
    const emptyPrompt = [{ role_name: "A", role_prompt: "  ", speaking_order: 0 }];
    expect(parseRoleRecommendationOutput(JSON.stringify({ roles: emptyPrompt }), 3)).toBeNull();
    expect(parseRoleRecommendationOutput(JSON.stringify({ roles }), 1)).toBeNull();
  });

  it("returns null for prose with no JSON at all", () => {
    expect(parseRoleRecommendationOutput("I cannot help with that.", 3)).toBeNull();
  });
});

describe("reply builders", () => {
  it("wraps roles as a text command_result", () => {
    const reply = JSON.parse(
      buildRoleRecommendationResponse("roles_1", [
        { role_name: "A", role_prompt: "p", speaking_order: 0 },
      ]),
    );
    expect(reply).toEqual({
      request_id: "roles_1",
      msg_type: "command_result",
      payload: { kind: "text", roles: [{ role_name: "A", role_prompt: "p", speaking_order: 0 }] },
    });
  });

  it("wraps a failure as an error command_result", () => {
    const reply = JSON.parse(buildRoleRecommendationError("roles_1", "boom"));
    expect(reply).toEqual({
      request_id: "roles_1",
      msg_type: "command_result",
      payload: { kind: "error", error: "boom" },
    });
  });

  it("keeps the staleness window at least the max host timeout (600s)", () => {
    expect(STALE_ROLE_RECOMMEND_REQUEST_MS).toBeGreaterThanOrEqual(600_000);
  });
});

// 讨论风格必须真的改变推荐结果：换风格就换一批角色名，也换一种说话方式。
describe("discussion style steering", () => {
  const styleIds = ["free", "brainstorm", "debate", "review", "consensus"] as const;

  const promptFor = (style: string | undefined, overrides: Partial<RoleRecommendRequest> = {}) =>
    buildRoleRecommendPrompt({
      requestId: "r",
      topic: "AI 与就业",
      goal: "",
      maxRoles: 4,
      recommendationPrompt: "",
      style,
      ...overrides,
    });

  it("resolves the style from an explicit id first, then from the goal phrase", () => {
    expect(detectDiscussionStyle("debate", "")).toBe("debate");
    // 旧链路（服务端/前端都没带 style）只能靠 C 端拼进 goal 的原话。
    expect(detectDiscussionStyle(undefined, "围绕“X”，以专家多角度评审的方式展开讨论，形成可执行的共识。")).toBe("review");
    // 不认识的显式取值要让位给 goal，而不是直接把风格丢掉。
    expect(detectDiscussionStyle("nonsense", "围绕“X”，以聚焦决策凝聚共识的方式展开讨论。")).toBe("consensus");
    expect(detectDiscussionStyle(undefined, "没有风格短语的 goal")).toBeNull();
    expect(detectDiscussionStyle("nonsense", "")).toBeNull();
  });

  it("gives every style its own cast — no role name is shared across styles", () => {
    const owner = new Map<string, string>();
    for (const id of styleIds) {
      const names = [...promptFor(id, { maxRoles: 8 }).matchAll(/^\d+\. (.+)$/gm)].map((m) => m[1]);
      // 产品口径 3 个角色：max_roles 再大也只列前三名（见 roleRecommendTargetRoleCount）。
      expect(names.length, `${id} 应给出 ${ROLE_RECOMMEND_TARGET_ROLES} 个角色`).toBe(ROLE_RECOMMEND_TARGET_ROLES);
      for (const name of names) {
        expect(owner.has(name), `角色「${name}」在 ${id} 与 ${owner.get(name)} 之间重复`).toBe(false);
        owner.set(name, id);
      }
    }
    expect(owner.size).toBe(styleIds.length * ROLE_RECOMMEND_TARGET_ROLES);
  });

  it("writes each style's own speaking voice into the prompt", () => {
    const marker: Record<(typeof styleIds)[number], string> = {
      free: "像熟人聊天",
      brainstorm: "异想天开",
      debate: "我反对",
      review: "风险等级",
      consensus: "代价是",
    };
    for (const id of styleIds) {
      const prompt = promptFor(id);
      expect(prompt, `${id} 缺少风格段落`).toContain(`Discussion style: `);
      expect(prompt, `${id} 缺少口吻要求`).toContain(marker[id]);
      // 口吻必须落在 role_prompt 上（role_prompt 才是每轮注入的人设）。
      expect(prompt).toContain("role_prompt");
    }
  });

  it("caps the cast at max_roles and asks for the shortfall", () => {
    // 候选设备只有 2 台：只能要 2 个角色（一个 agent 演一个角色）。
    const tight = promptFor("debate", { maxRoles: 2 });
    expect(tight).toContain("1. 正方主辩");
    expect(tight).toContain("2. 反方主辩");
    expect(tight).not.toContain("3. 证据审查员");
    expect(tight).not.toContain("add up to");
    expect(tight).toContain("Return exactly 2 roles.");

    // 候选再宽也只按产品口径 3 个走，第 4 个角色（裁判）不进提示词。
    const roomy = promptFor("debate", { maxRoles: 8 });
    expect(roomy).toContain("3. 证据审查员");
    expect(roomy).not.toContain("4. 裁判");
    expect(roomy).not.toContain("add up to");
    expect(roomy).toContain("Return exactly 3 roles.");
  });

  it("matches the explicit style with the goal-only fallback", () => {
    const goal = "围绕“AI 与就业”，以正反两方交锋辩论的方式展开讨论，形成可执行的共识、分歧清单和后续行动。";
    expect(promptFor(undefined, { goal })).toBe(promptFor("debate", { goal }));
  });

  it("leaves the prompt exactly as before when no style is recognisable", () => {
    const prompt = buildRoleRecommendPrompt({
      requestId: "r",
      topic: "t",
      goal: "找平衡",
      maxRoles: 3,
      recommendationPrompt: "",
    });
    expect(prompt).not.toContain("Discussion style:");
    expect(prompt).toContain("Return exactly 3 roles.");
  });
});
