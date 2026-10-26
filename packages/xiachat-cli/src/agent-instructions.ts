import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

// 设备端向 agent 注入**输出形态约束**的唯一入口。
//
// 起因（2026-10-26 现场）：用户让 codex「写一段关于长江的说明，正好 800 字，
// 分 5 段」，正文第一段以
//   「我先核对每段字数，确保五段合计正好 800 字。这里没有安装 Python，我改用
//     PowerShell 直接按字符计数并微调文本。长江是中国第一大河……」
// 开头 —— 模型为了满足「正好 800 字」这个硬约束，把执行过程写进了正文。
// 用户看到的是「过程自述 + 成品」，而过程属于思考区（reasoning），不该出现在
// 正文里。
//
// 为什么不去改 prompt：私聊的 prompt 就是用户原话（im.ts 的 RC:TxtMsg 分支直接
// 透传 text），往里塞一句「不要叙述执行过程」会污染用户输入，agent 还可能复述
// 它。约束要落在**指令层**，不是**用户消息层**。
//
// 为什么是 AGENTS.md：codex 会把 `$CODEX_HOME/AGENTS.md`（默认
// `~/.codex/AGENTS.md`）作为全局指令注入上下文。0.144.6 实测有效 —— 写入
// 「Always end every final answer with the literal token [AGTEST]」后，
// `codex exec` 的推理里明确引用了这条 AGENTS.md 指令（它与「只回答一个字」
// 冲突时选择了后者，说明它读到并被当成指令，而不是被忽略）。
//
// 注入必须**幂等且不破坏用户内容**：用户自己也可能在用 AGENTS.md。所以用一对
// 标记注释圈定我们这一段，只替换标记之间的内容，前后文原样保留；文件不存在才
// 创建。任何一步失败都只返回 false，绝不让一轮对话因为写不了指令文件而失败。

export const CODEX_OUTPUT_STYLE_BEGIN = "<!-- clawmessenger:output-style:begin -->";
export const CODEX_OUTPUT_STYLE_END = "<!-- clawmessenger:output-style:end -->";

export const CODEX_OUTPUT_STYLE_BODY = [
  CODEX_OUTPUT_STYLE_BEGIN,
  "## 输出风格（由 ClawMessenger 设备端维护，可整段删除）",
  "",
  "- 正文只写成品内容。不要在正文里叙述你的执行过程、环境检查、工具选择或实现思路，",
  "  例如「我先核对字数」「这里没有安装 Python，我改用 PowerShell 直接按字符计数」。",
  "  这些属于思考，不是回答。",
  "- 遇到字数、格式之类的硬性要求，直接给出符合要求的结果，不要汇报你是如何凑够的。",
  "- 不要复述或总结本节内容。",
  CODEX_OUTPUT_STYLE_END,
].join("\n");

/** codex 的全局指令文件路径。`CODEX_HOME` 优先（codex 自己也认这个变量）。 */
export function codexAgentsPath(env: NodeJS.ProcessEnv = process.env, home?: string): string {
  const codexHome = env.CODEX_HOME?.trim();
  if (codexHome) return join(codexHome, "AGENTS.md");
  return join(home ?? homedir(), ".codex", "AGENTS.md");
}

/**
 * 把输出风格段合并进已有内容。
 *
 * - 没有标记段：追加到末尾（用户原有内容一个字不动）
 * - 已有标记段：原地替换（前后文保留），用于升级文案而不重复堆叠
 */
export function mergeCodexOutputStyle(existing: string | null | undefined): string {
  const base = existing ?? "";
  const begin = base.indexOf(CODEX_OUTPUT_STYLE_BEGIN);
  const end = base.indexOf(CODEX_OUTPUT_STYLE_END);
  if (begin >= 0 && end > begin) {
    const tail = base.slice(end + CODEX_OUTPUT_STYLE_END.length);
    return `${base.slice(0, begin)}${CODEX_OUTPUT_STYLE_BODY}${tail}`;
  }
  const trimmed = base.replace(/\s+$/, "");
  return trimmed ? `${trimmed}\n\n${CODEX_OUTPUT_STYLE_BODY}\n` : `${CODEX_OUTPUT_STYLE_BODY}\n`;
}

export interface EnsureCodexOutputStyleOpts {
  env?: NodeJS.ProcessEnv;
  home?: string;
  /** 测试注入；返回 null 表示文件不存在。 */
  readFile?: (path: string) => string | null;
  writeFile?: (path: string, content: string) => void;
}

/**
 * 确保 `$CODEX_HOME/AGENTS.md` 含输出风格段。
 *
 * @returns true = 本次写了文件（新建或有更新）；false = 已是最新，或写入失败。
 */
export function ensureCodexOutputStyle(opts: EnsureCodexOutputStyleOpts = {}): boolean {
  const env = opts.env ?? process.env;
  const path = codexAgentsPath(env, opts.home);
  const readFile = opts.readFile
    ?? ((p: string): string | null => {
      try {
        return readFileSync(p, "utf8");
      } catch {
        return null;
      }
    });
  const writeFile = opts.writeFile
    ?? ((p: string, content: string): void => {
      mkdirSync(dirname(p), { recursive: true });
      writeFileSync(p, content, "utf8");
    });

  try {
    const existing = readFile(path);
    const merged = mergeCodexOutputStyle(existing);
    // 内容一致就不写：这函数每个 codex 轮次都会跑，避免无谓的 mtime 抖动。
    if (existing === merged) return false;
    writeFile(path, merged);
    return true;
  } catch {
    // 指令注入是增强项：目录只读、权限不足、磁盘满都不该影响这一轮回答。
    return false;
  }
}
