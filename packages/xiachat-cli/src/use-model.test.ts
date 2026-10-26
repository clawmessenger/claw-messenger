// @vitest-environment node
import { describe, expect, it } from "vitest";
import { formatUseModelAck, parseUseModelCommand } from "./use-model.js";

describe("parseUseModelCommand", () => {
  it("parses a full provider/model route", () => {
    expect(parseUseModelCommand("/use-model quukk/glm-5.3")).toEqual({
      kind: "set",
      route: "quukk/glm-5.3",
    });
  });

  it("trims the surrounding whitespace the web may leave in", () => {
    expect(parseUseModelCommand("  /use-model   quukk/glm-5.3  ")).toEqual({
      kind: "set",
      route: "quukk/glm-5.3",
    });
  });

  it("reads a bare command as a status query", () => {
    expect(parseUseModelCommand("/use-model")).toEqual({ kind: "query" });
    expect(parseUseModelCommand("/use-model    ")).toEqual({ kind: "query" });
  });

  it("reads reset words as 'go back to the node default'", () => {
    for (const word of ["default", "reset", "clear", "none", "auto", "-", "off", "DEFAULT"]) {
      expect(parseUseModelCommand(`/use-model ${word}`)).toEqual({ kind: "clear" });
    }
  });

  it("flags a bare slug as invalid instead of silently ignoring it", () => {
    // 裸 slug 在 agents.ts buildModelArgs 里三种 agent 三种解释（codex 配
    // provider、hermes 拆 --provider、qwen 只吃裸名）。拒收并教格式，比让它
    // 落到某个 agent 手里变成「模型不存在」好。
    expect(parseUseModelCommand("/use-model glm-5.3")).toEqual({ kind: "invalid", arg: "glm-5.3" });
    expect(parseUseModelCommand("/use-model quukk/")).toEqual({ kind: "invalid", arg: "quukk/" });
  });

  it("leaves anything that is not this command to the agent", () => {
    const notCommands = [
      "use-model quukk/glm-5.3", // 不带斜杠：用户真的在聊 use-model
      "/use-models quukk/glm-5.3", // 另一个词
      "/use-modelx",
      "/model quukk/glm-5.3",
      "帮我 /use-model 一下",
      "",
    ];
    for (const text of notCommands) {
      expect(parseUseModelCommand(text)).toBeNull();
    }
  });
});

describe("formatUseModelAck", () => {
  it("confirms a switch and scopes it to this user", () => {
    const ack = formatUseModelAck({ kind: "set", route: "quukk/glm-5.3" }, "quukk/glm-5.3");
    expect(ack).toContain("quukk/glm-5.3");
    expect(ack).toContain("仅对你");
  });

  it("confirms the reset", () => {
    const ack = formatUseModelAck({ kind: "clear" }, undefined);
    expect(ack).toContain("节点默认模型");
  });

  it("teaches the format for an invalid route", () => {
    const ack = formatUseModelAck({ kind: "invalid", arg: "glm-5.3" }, undefined);
    expect(ack).toContain("<provider>/<model>");
    expect(ack).toContain("glm-5.3");
  });

  it("reports the current model, or the default when none is set", () => {
    expect(formatUseModelAck({ kind: "query" }, "quukk/glm-5.3")).toContain("当前模型：quukk/glm-5.3");
    const fallback = formatUseModelAck({ kind: "query" }, undefined);
    expect(fallback).toContain("节点默认模型");
    expect(fallback).toContain("/use-model <provider>/<model>");
  });
});
