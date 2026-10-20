// @vitest-environment node
import { describe, expect, it } from "vitest";
import { buildAutoContinuationPrompt, classifyTurnError, describeTurnError, TurnIncompleteError } from "./turn-error.js";

/** web 端 validChatIdentifier 的同款约束：无控制/格式字符、首尾无空白、≤256。 */
function isWireSafe(code: string): boolean {
  return code.length > 0 && code.length <= 256 && code.trim() === code && !/[\p{Cc}\p{Cf}]/u.test(code);
}

describe("classifyTurnError", () => {
  it("把 quukk 的 5 小时额度耗尽认成额度用完（429 不能先判成限流）", () => {
    // 线上 hermes 的真实 stderr（2026-10-19 18:13）
    const raw = "agent exited with code 1: HTTP 429: [1308][已达到 5 小时的使用上限。"
      + "您的限额将在 2026-10-19 19:40:03 重置。]\n  at request (llmapi.quukk.com:443)";
    expect(classifyTurnError(raw)).toBe("model_quota_exhausted");
  });

  it("各家措辞的额度问题都归到同一码", () => {
    const cases = [
      "Error: insufficient_quota: You exceeded your current quota, please check your plan",
      "OpenAI 400: 配额已用尽",
      "上游返回：模型额度已用完",
      "HTTP 429 rate_limit_exceeded: out of credits",
    ];
    for (const raw of cases) expect(classifyTurnError(raw)).toBe("model_quota_exhausted");
  });

  it("402 / 余额不足是「余额」而不是「额度」：补救方式不同", () => {
    const cases = [
      "openai error 402: Insufficient Balance",
      "HTTP 402 payment required",
      "账户余额不足，请充值",
    ];
    for (const raw of cases) expect(classifyTurnError(raw)).toBe("model_balance_exhausted");
  });

  it("鉴权与超时各归各码", () => {
    expect(classifyTurnError("401 Unauthorized: Invalid API key")).toBe("model_auth_failed");
    expect(classifyTurnError("{\"error\":{\"message\":\"no such model glm-9\"}}")).toBe("model_auth_failed");
    expect(classifyTurnError("agent turn timeout after 300000ms")).toBe("turn_timeout");
  });

  it("纯限流（429 但不含额度语义）", () => {
    expect(classifyTurnError("HTTP 429 Too Many Requests")).toBe("model_rate_limited");
  });

  it("用户主动停止不该被说成额度问题", () => {
    expect(classifyTurnError("agent turn aborted")).toBe("agent_failed");
  });

  it("空串/垃圾输入兜底成 agent_failed，永不抛错", () => {
    expect(classifyTurnError("")).toBe("agent_failed");
    expect(classifyTurnError(undefined as unknown as string)).toBe("agent_failed");
    expect(classifyTurnError("\u0000\u0007")).toBe("agent_failed");
  });

  it("每个码都能翻成一句中文，且都能安全过 web 端标识符校验", () => {
    const codes = [
      classifyTurnError("[1308] 使用上限"),
      classifyTurnError("insufficient balance"),
      classifyTurnError("401"),
      classifyTurnError("429 too many requests"),
      classifyTurnError("agent turn timeout after 1ms"),
      classifyTurnError("boom"),
    ];
    for (const code of codes) {
      expect(isWireSafe(code)).toBe(true);
      expect(describeTurnError(code).length).toBeGreaterThan(0);
    }
  });
});

describe("buildAutoContinuationPrompt（撞失败工具后的自动补跑提示词）", () => {
  const err = new TurnIncompleteError(
    "agent_incomplete",
    "……",
    "turn ended on a failed 执行命令 with no follow-up text",
    { label: "执行命令", command: "Get-Content x -Tail 50", output: "Get-Content: command not found" },
  );

  it("带上原请求、失败步骤、命令与错误输出", () => {
    const prompt = buildAutoContinuationPrompt("设备上线", err);
    expect(prompt).toContain("设备上线");
    expect(prompt).toContain("执行命令");
    expect(prompt).toContain("Get-Content x -Tail 50");
    expect(prompt).toContain("command not found");
    // 必须提醒 shell 是 git-bash —— 否则补跑会再犯同一个错。
    expect(prompt).toContain("git-bash");
  });

  it("没有 failedStep 时也能给出通用提示（不崩）", () => {
    const bare = new TurnIncompleteError("agent_incomplete", "半句", "aborted");
    const prompt = buildAutoContinuationPrompt("原问题", bare);
    expect(prompt).toContain("原问题");
    expect(prompt).toContain("没有答完");
  });
});
