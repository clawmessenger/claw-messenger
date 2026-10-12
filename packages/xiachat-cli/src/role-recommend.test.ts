import { describe, expect, it } from "vitest";
import {
  buildRoleRecommendPrompt,
  buildRoleRecommendationError,
  buildRoleRecommendationResponse,
  parseRoleRecommendRequest,
  parseRoleRecommendationOutput,
  STALE_ROLE_RECOMMEND_REQUEST_MS,
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
    expect(prompt).toContain("Return between 1 and 4 roles.");
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
