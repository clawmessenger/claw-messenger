import { join } from "node:path";
import { describe, expect, it, vi } from "vitest";
import {
  CODEX_OUTPUT_STYLE_BEGIN,
  CODEX_OUTPUT_STYLE_END,
  codexAgentsPath,
  ensureCodexOutputStyle,
  mergeCodexOutputStyle,
} from "./agent-instructions.js";

describe("codexAgentsPath", () => {
  it("CODEX_HOME 优先于用户目录（codex 自己也认这个变量）", () => {
    expect(codexAgentsPath({ CODEX_HOME: "/custom/codex" }, "/home/u")).toBe(
      join("/custom/codex", "AGENTS.md"),
    );
  });

  it("没有 CODEX_HOME 时落在 ~/.codex/AGENTS.md", () => {
    expect(codexAgentsPath({}, "/home/u")).toBe(join("/home/u", ".codex", "AGENTS.md"));
  });

  it("空白 CODEX_HOME 视同未设置", () => {
    expect(codexAgentsPath({ CODEX_HOME: "   " }, "/home/u")).toBe(join("/home/u", ".codex", "AGENTS.md"));
  });
});

describe("mergeCodexOutputStyle", () => {
  it("空文件只写我们的段", () => {
    const merged = mergeCodexOutputStyle(null);
    expect(merged.startsWith(CODEX_OUTPUT_STYLE_BEGIN)).toBe(true);
    expect(merged.trimEnd().endsWith(CODEX_OUTPUT_STYLE_END)).toBe(true);
  });

  it("已有用户内容时追加，原内容一个字不动", () => {
    const merged = mergeCodexOutputStyle("# 我的规则\n\n- 用中文回答\n");
    expect(merged.startsWith("# 我的规则\n\n- 用中文回答")).toBe(true);
    expect(merged).toContain(CODEX_OUTPUT_STYLE_BEGIN);
    // 用户内容与标记段之间留一空行，且有结尾换行
    expect(merged).toContain("用中文回答\n\n<!-- clawmessenger:output-style:begin -->");
    expect(merged.endsWith("\n")).toBe(true);
  });

  it("已有标记段时原地替换，不重复堆叠", () => {
    const stale = [
      "# 我的规则",
      CODEX_OUTPUT_STYLE_BEGIN,
      "旧文案",
      CODEX_OUTPUT_STYLE_END,
      "## 尾部说明",
    ].join("\n");
    const merged = mergeCodexOutputStyle(stale);
    expect(merged.match(new RegExp(CODEX_OUTPUT_STYLE_BEGIN, "g"))).toHaveLength(1);
    expect(merged.match(new RegExp(CODEX_OUTPUT_STYLE_END, "g"))).toHaveLength(1);
    expect(merged).not.toContain("旧文案");
    expect(merged).toContain("正文只写成品内容");
    // 前后文都保留
    expect(merged.startsWith("# 我的规则")).toBe(true);
    expect(merged).toContain("## 尾部说明");
  });

  it("幂等：合并两次结果相同", () => {
    const once = mergeCodexOutputStyle("# mine\n");
    expect(mergeCodexOutputStyle(once)).toBe(once);
  });
});

describe("ensureCodexOutputStyle", () => {
  it("文件不存在时创建并写入", () => {
    const writeFile = vi.fn();
    const changed = ensureCodexOutputStyle({
      env: { CODEX_HOME: "/x" },
      readFile: () => null,
      writeFile,
    });
    expect(changed).toBe(true);
    expect(writeFile).toHaveBeenCalledTimes(1);
    expect(writeFile.mock.calls[0][0]).toBe(join("/x", "AGENTS.md"));
    expect(writeFile.mock.calls[0][1]).toContain("正文只写成品内容");
  });

  it("已是最新时不写（每个 codex 轮次都会调用，避免 mtime 抖动）", () => {
    const writeFile = vi.fn();
    const current = mergeCodexOutputStyle("# mine\n");
    const changed = ensureCodexOutputStyle({
      env: { CODEX_HOME: "/x" },
      readFile: () => current,
      writeFile,
    });
    expect(changed).toBe(false);
    expect(writeFile).not.toHaveBeenCalled();
  });

  it("无标记段的旧文件会被补上并保留原内容", () => {
    const writeFile = vi.fn();
    const changed = ensureCodexOutputStyle({
      env: { CODEX_HOME: "/x" },
      readFile: () => "# mine\n",
      writeFile,
    });
    expect(changed).toBe(true);
    expect(writeFile.mock.calls[0][1]).toContain("# mine");
    expect(writeFile.mock.calls[0][1]).toContain(CODEX_OUTPUT_STYLE_BEGIN);
  });

  it("写入失败不抛错（指令注入不能拖垮一轮对话）", () => {
    const changed = ensureCodexOutputStyle({
      env: { CODEX_HOME: "/x" },
      readFile: () => null,
      writeFile: () => {
        throw new Error("EACCES");
      },
    });
    expect(changed).toBe(false);
  });
});
