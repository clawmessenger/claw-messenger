import { readdirSync, statSync } from "node:fs";
import { extname, join, relative, sep } from "node:path";

// 「本轮 agent 产出了什么文件」的采集。
//
// 为什么需要：设备端的编码 agent 被要求「生成一个 html / 一份报告 / 一个表格」
// 时，产物落在它自己的工作目录里，stdout 往往只有一句「已生成」甚至一句话都
// 没有。此前的链路只把 stdout 当回复正文，于是用户看到的是零回复或一句空话，
// 文件本体永远拿不到。
//
// 做法：一轮开始前给工作目录拍一张「相对路径 → mtimeMs」的快照，一轮结束后
// 再拍一张并取差集（新增的 + mtime 变大的）。这比「只看扩展名」准得多，也不会
// 把目录里原有的旧文件当成新产出反复上报。

/** 一轮开始前的工作目录快照（相对路径 → mtimeMs）。 */
export type WorkdirSnapshot = Map<string, number>;

export interface ProducedArtifact {
  /** 绝对路径，交给上传函数读。 */
  path: string;
  /** 展示名（basename）。 */
  name: string;
  /** 字节数。 */
  size: number;
  /** 小写扩展名，带点。 */
  ext: string;
}

export interface CollectOptions {
  /** 递归深度上限（默认 4）。 */
  maxDepth?: number;
  /** 扫描到的条目上限，超过即放弃本轮采集（避免在超大目录上失速）。 */
  maxEntries?: number;
  /** 单文件体积上限，超过不采集（与服务端一致）。 */
  maxFileBytes?: number;
  /** 最多上报几个文件。 */
  maxFiles?: number;
}

const DEFAULTS = {
  maxDepth: 4,
  maxEntries: 4000,
  maxFileBytes: 25 << 20,
  maxFiles: 8,
} satisfies Required<CollectOptions>;

// 这些目录是工具链的产物，不是「用户要的文件」：node_modules 里可能一次
// 装出几万个文件，构建目录同理。命中即整棵剪掉，连遍历都不进。
const IGNORED_DIRS = new Set([
  "node_modules",
  ".git",
  ".hg",
  ".svn",
  "dist",
  "build",
  "out",
  "coverage",
  ".next",
  ".nuxt",
  ".turbo",
  ".cache",
  "__pycache__",
  "venv",
  ".venv",
  ".idea",
  ".vscode",
]);

// 值得上报的产物类型：文档、数据、图片、代码、压缩包。
const ARTIFACT_EXTENSIONS = new Set([
  ".html", ".htm", ".md", ".markdown", ".txt", ".csv", ".tsv", ".json",
  ".yaml", ".yml", ".xml", ".svg", ".pdf", ".ipynb",
  ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico",
  ".xlsx", ".xls", ".docx", ".doc", ".pptx", ".ppt",
  ".zip", ".gz", ".tar",
  ".py", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".css", ".scss",
  ".go", ".rs", ".java", ".kt", ".rb", ".php", ".c", ".cpp", ".h", ".hpp",
  ".sh", ".ps1", ".bat", ".sql", ".toml", ".ini", ".cfg", ".conf",
]);

// 能在右侧面板里直接预览的类型（其余只能下载）。
const PREVIEWABLE_EXTENSIONS = new Set([
  ".html", ".htm", ".md", ".markdown", ".txt", ".csv", ".tsv", ".json",
  ".yaml", ".yml", ".xml", ".svg", ".pdf",
  ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico",
]);

export function isPreviewableArtifact(name: string): boolean {
  return PREVIEWABLE_EXTENSIONS.has(extname(name).toLowerCase());
}

function shouldIgnoreDir(name: string): boolean {
  // 点目录一律不进：.git、.cache 之外还有各家 agent 自己的状态目录。
  return name.startsWith(".") || IGNORED_DIRS.has(name);
}

// CLI 自己写进工作目录的临时文件（html_revise 的上一版页面），不是 agent 的产出。
function isOwnScratchFile(name: string): boolean {
  return name.startsWith("cm-revise-");
}

/**
 * 拍一张工作目录快照。目录不存在 / 不可读时返回空表 —— 采集失败绝不能影响
 * 正常回合，只是这一轮没有产物可上报。
 */
export function snapshotWorkdir(root: string | undefined, options: CollectOptions = {}): WorkdirSnapshot {
  const snapshot: WorkdirSnapshot = new Map();
  if (!root) return snapshot;
  const opts = { ...DEFAULTS, ...options };
  walk(root, root, 0, opts, (rel, absolute) => {
    try {
      const stat = statSync(absolute);
      snapshot.set(rel, stat.mtimeMs);
    } catch {
      // 竞态删除：忽略。
    }
  }, { visited: 0, exhausted: false });
  return snapshot;
}

/**
 * 与本轮开始前的快照比对，返回本轮新产出/被改动的产物。
 * 结果按修改时间从新到旧排序，最多 maxFiles 个。
 */
export function collectProducedArtifacts(
  root: string | undefined,
  before: WorkdirSnapshot,
  options: CollectOptions = {},
): ProducedArtifact[] {
  if (!root) return [];
  const opts = { ...DEFAULTS, ...options };
  const candidates: Array<{ artifact: ProducedArtifact; mtimeMs: number }> = [];
  walk(root, root, 0, opts, (rel, absolute) => {
    if (candidates.length > opts.maxFiles * 8) return; // 已经够用了，别再扫
    const ext = extname(rel).toLowerCase();
    if (!ARTIFACT_EXTENSIONS.has(ext)) return;
    let stat;
    try {
      stat = statSync(absolute);
    } catch {
      return;
    }
    if (!stat.isFile() || stat.size === 0 || stat.size > opts.maxFileBytes) return;
    const previous = before.get(rel);
    // 新增（快照里没有）或本轮被改写（mtime 前进）才算产出。mtime 同秒的
    // 文件系统上留 1ms 容差，避免「刚写过但时间戳没动」被漏判成旧文件。
    if (previous !== undefined && stat.mtimeMs <= previous + 1) return;
    candidates.push({
      artifact: { path: absolute, name: rel.split(sep).pop() || rel, size: stat.size, ext },
      mtimeMs: stat.mtimeMs,
    });
  }, { visited: 0, exhausted: false });
  return candidates
    .sort((a, b) => b.mtimeMs - a.mtimeMs)
    .slice(0, opts.maxFiles)
    .map((item) => item.artifact);
}

type Visitor = (relPath: string, absolutePath: string) => void;

/** 遍历状态：条目预算跨整棵树共享，超出即停（超大目录不能把一轮拖住）。 */
interface WalkState {
  visited: number;
  exhausted: boolean;
}

function walk(
  root: string,
  current: string,
  depth: number,
  opts: Required<CollectOptions>,
  visit: Visitor,
  state: WalkState,
): void {
  if (depth > opts.maxDepth || state.exhausted) return;
  let entries;
  try {
    entries = readdirSync(current, { withFileTypes: true });
  } catch {
    return; // 不可读目录（权限/竞态删除）直接跳过。
  }
  for (const entry of entries) {
    if (state.exhausted) return;
    state.visited += 1;
    if (state.visited > opts.maxEntries) {
      state.exhausted = true;
      return;
    }
    if (entry.isDirectory()) {
      if (shouldIgnoreDir(entry.name)) continue;
      walk(root, join(current, entry.name), depth + 1, opts, visit, state);
      continue;
    }
    if (!entry.isFile()) continue;
    if (entry.name.startsWith(".") || isOwnScratchFile(entry.name)) continue;
    const absolute = join(current, entry.name);
    visit(relative(root, absolute), absolute);
  }
}
