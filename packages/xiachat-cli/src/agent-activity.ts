// agent-activity.ts —— 过程流（activity）：把一轮 agent 里「调了什么工具」
// 落成可上屏的步骤列表，与思考流（reasoning）一起构成用户眼里的「过程」。
//
// 数据源：claude（--output-format stream-json --verbose）、codex（exec --json）
// 与 opencode（run --format json），三者都是逐行 JSON 事件流，工具事件与正文
// 分道。其余 agent 没有结构化输出，**不做猜测式解析** —— 猜错的代价是把日志
// 噪声写进用户气泡，比「没有过程」更糟。它们天然降级为只有正文 + 思考。
//
// 语义与 chat_stream 的 text/reasoning 一致：**全量快照**。每一行事件消化后
// 调用方拿到的是当轮完整的步骤数组（不是增量），丢帧无害。

export type ActivityKind = "search" | "fetch" | "command" | "file" | "tool";
export type ActivityStatus = "running" | "done" | "failed";

export interface ActivityStep {
  /** 一轮内稳定的步骤 id（claude 的 tool_use id / codex 的 item id）。 */
  id: string;
  kind: ActivityKind;
  /** 展示名（已本地化，如「联网搜索」）。 */
  label: string;
  /** 参数摘要：搜索词 / 命令 / 文件路径，过长会被裁剪。 */
  detail?: string;
  status: ActivityStatus;
  /** 该步开始时间（epoch ms）。web 用它算每步耗时；历史消息里也保留。 */
  startedAt?: number;
  /** 该步进入终态的时间（epoch ms）。running 时缺省。 */
  endedAt?: number;
}

// 一帧里最多带多少步：快照是全量的，步骤多了会把帧顶爆（进而走 wire 分块）。
// 工具调用密集时用户要的是「在做什么」，不是完整审计日志，超出即丢弃后续步骤。
export const ACTIVITY_MAX_STEPS = 20;
// 单条摘要的字符上限（含省略号）。
const DETAIL_MAX_CHARS = 120;

// 已知工具 → { 类别, 中文名 }。键不区分大小写（各 agent 拼写不一）。
// 未命中的按原名作为 tool 展示 —— 至少用户能看到「调了什么」。
const TOOL_LABELS: Readonly<Record<string, { kind: ActivityKind; label: string }>> = {
  websearch: { kind: "search", label: "联网搜索" },
  web_search: { kind: "search", label: "联网搜索" },
  search: { kind: "search", label: "联网搜索" },
  webfetch: { kind: "fetch", label: "抓取网页" },
  web_fetch: { kind: "fetch", label: "抓取网页" },
  fetch: { kind: "fetch", label: "抓取网页" },
  bash: { kind: "command", label: "执行命令" },
  command_execution: { kind: "command", label: "执行命令" },
  exec_command: { kind: "command", label: "执行命令" },
  read: { kind: "file", label: "读取文件" },
  write: { kind: "file", label: "写入文件" },
  edit: { kind: "file", label: "编辑文件" },
  multiedit: { kind: "file", label: "编辑文件" },
  notebookedit: { kind: "file", label: "编辑文件" },
  file_change: { kind: "file", label: "修改文件" },
  glob: { kind: "file", label: "查找文件" },
  ls: { kind: "file", label: "列出目录" },
  list: { kind: "file", label: "列出目录" },
  grep: { kind: "file", label: "搜索内容" },
  patch: { kind: "file", label: "编辑文件" },
  task: { kind: "tool", label: "子任务" },
  todowrite: { kind: "tool", label: "更新待办" },
  todo_list: { kind: "tool", label: "更新待办" },
  mcp_tool_call: { kind: "tool", label: "MCP 工具" },
};

/** 工具名 → 展示类别与中文名；未登记的工具按原名展示。 */
export function classifyTool(name: string): { kind: ActivityKind; label: string } {
  const key = name.trim().toLowerCase();
  return TOOL_LABELS[key] ?? { kind: "tool", label: name.trim() || "工具" };
}

// 摘要字段的取值顺序：越靠前越能说明「在干什么」。
const DETAIL_KEYS = [
  "query", "command", "cmd", "file_path", "filePath", "path",
  "url", "pattern", "description", "prompt", "text", "input",
] as const;

function clip(value: string): string {
  const oneLine = value.replace(/\s+/g, " ").trim();
  return oneLine.length <= DETAIL_MAX_CHARS
    ? oneLine
    : `${oneLine.slice(0, DETAIL_MAX_CHARS - 1)}…`;
}

/**
 * 从一个工具事件里摘出一句人话摘要。输入形状各家不一（claude 的 input 对象、
 * codex 的 item 本体），这里只挑第一个像样的字符串字段。
 */
export function summarizeToolDetail(input: unknown): string | undefined {
  if (typeof input === "string") {
    return input.trim() ? clip(input) : undefined;
  }
  if (!input || typeof input !== "object") return undefined;
  const rec = input as Record<string, unknown>;
  for (const key of DETAIL_KEYS) {
    const value = rec[key];
    if (typeof value === "string" && value.trim()) return clip(value);
    if (Array.isArray(value) && value.length > 0 && typeof value[0] === "string") {
      return clip(value[0]);
    }
  }
  // codex 的 file_change 把路径放在 changes[].path 里。
  const changes = rec.changes;
  if (Array.isArray(changes)) {
    const paths = changes
      .map((c) => (typeof c === "object" && c !== null
        ? (c as Record<string, unknown>).path : undefined))
      .filter((p): p is string => typeof p === "string" && p.length > 0);
    if (paths.length > 0) return clip(paths.join("、"));
  }
  return undefined;
}

// --- JSONL 行解析 ----------------------------------------------------------

function parseLine(line: string): Record<string, unknown> | null {
  const trimmed = line.trim();
  if (!trimmed.startsWith("{")) return null;
  try {
    const parsed: unknown = JSON.parse(trimmed);
    return parsed && typeof parsed === "object" && !Array.isArray(parsed)
      ? parsed as Record<string, unknown>
      : null;
  } catch {
    return null;
  }
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return value && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : null;
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

// 这两个 item 类型不是工具调用：codex 的思考与正文，交给 reasoning/text 通道。
const CODEX_NON_TOOL_ITEMS = new Set(["reasoning", "agent_message", "error"]);

export interface ActivityTracker {
  /** 消化一行 JSONL；返回 true 表示步骤列表发生变化（值得重发一帧）。 */
  push(line: string): boolean;
  /** 把仍标记为 running 的步骤收尾（一轮正常结束 / 异常结束都调）。 */
  finish(): boolean;
  /** 当前全量快照。 */
  snapshot(): ActivityStep[];
}

/**
 * 一轮一个 tracker：claude 的 content_block index 与 tool_use id 只在轮内有
 * 意义，跨轮复用会串。
 */
export function createActivityTracker(): ActivityTracker {
  const steps: ActivityStep[] = [];
  const byId = new Map<string, ActivityStep>();
  // claude content_block_start 给 id，content_block_stop 只给 index —— 需要
  // 这张表才能在 stop 时找到该收尾哪一步。
  const idByIndex = new Map<number, string>();

  const upsert = (id: string, next: Omit<ActivityStep, "id">): boolean => {
    const now = Date.now();
    const existing = byId.get(id);
    if (existing) {
      // 进终态的那一刻打结束时间；调用方不必关心时间戳，避免各处重复实现。
      const enteringTerminal = existing.status === "running" && next.status !== "running";
      const merged: ActivityStep = {
        ...existing,
        ...next,
        // 已有摘要不补空：收尾事件往往不带参数，不能把摘要抹掉。
        detail: next.detail ?? existing.detail,
        // 首见时间只认第一次，后续重复上报不能把它刷新（否则每步耗时恒为 0）。
        startedAt: existing.startedAt ?? now,
        endedAt: enteringTerminal ? now : existing.endedAt,
      };
      if (
        merged.kind === existing.kind && merged.label === existing.label
        && merged.detail === existing.detail && merged.status === existing.status
      ) {
        return false;
      }
      byId.set(id, merged);
      const at = steps.findIndex((s) => s.id === id);
      if (at >= 0) steps[at] = merged;
      return true;
    }
    if (steps.length >= ACTIVITY_MAX_STEPS) return false;
    const step: ActivityStep = { id, ...next, startedAt: now };
    byId.set(id, step);
    steps.push(step);
    return true;
  };

  const markDone = (id: string, status: ActivityStatus): boolean => {
    const existing = byId.get(id);
    if (!existing) return false;
    return upsert(id, { kind: existing.kind, label: existing.label, status });
  };

  const finish = (): boolean => {
    let changed = false;
    // 走 upsert 而不是就地改 status：终态的 endedAt 由 upsert 统一打。
    for (const step of [...steps]) {
      if (step.status !== "running") continue;
      if (upsert(step.id, { kind: step.kind, label: step.label, status: "done" })) changed = true;
    }
    return changed;
  };

  const push = (line: string): boolean => {
    const event = parseLine(line);
    if (!event) return false;
    const type = asString(event.type);

    // 三个 agent 的「本轮结束」事件：把悬着的 running 收掉。
    if (type === "result" || type === "turn.completed" || type === "step_finish") return finish();

    // --- claude：stream_event 里逐个 content block 上报 ---
    if (type === "stream_event") {
      const inner = asRecord(event.event);
      if (!inner) return false;
      const innerType = asString(inner.type);
      const block = asRecord(inner.content_block);
      if (innerType === "content_block_start" && block) {
        const blockType = asString(block.type);
        const index = typeof inner.index === "number" ? inner.index : -1;
        if (blockType === "tool_use") {
          const name = asString(block.name);
          const id = asString(block.id) || (index >= 0 ? `cb-${index}` : `cb-${steps.length}`);
          if (index >= 0) idByIndex.set(index, id);
          const cls = classifyTool(name);
          return upsert(id, {
            kind: cls.kind,
            label: cls.label,
            detail: summarizeToolDetail(block.input),
            status: "running",
          });
        }
        // 工具结果块开始：说明上一步已经跑完。
        if (blockType === "tool_result" && index > 0) {
          const id = idByIndex.get(index - 1);
          if (id) return markDone(id, "done");
        }
        return false;
      }
      if (innerType === "content_block_stop") {
        const index = typeof inner.index === "number" ? inner.index : -1;
        const id = index >= 0 ? idByIndex.get(index) : undefined;
        return id ? markDone(id, "done") : false;
      }
      return false;
    }

    // --- claude：整段 assistant 消息，带完整 tool_use（input 已补全）---
    if (type === "assistant") {
      const message = asRecord(event.message);
      const content = message?.content;
      if (!Array.isArray(content)) return false;
      let changed = false;
      content.forEach((item, index) => {
        const block = asRecord(item);
        if (!block || asString(block.type) !== "tool_use") return;
        const name = asString(block.name);
        const id = asString(block.id) || `cb-${index}`;
        const cls = classifyTool(name);
        if (upsert(id, {
          kind: cls.kind,
          label: cls.label,
          detail: summarizeToolDetail(block.input),
          status: "running",
        })) changed = true;
      });
      return changed;
    }

    // --- claude：tool_result 回灌（user 消息）---
    if (type === "user") {
      const message = asRecord(event.message);
      const content = message?.content;
      if (!Array.isArray(content)) return false;
      let changed = false;
      for (const item of content) {
        const block = asRecord(item);
        if (!block || asString(block.type) !== "tool_result") continue;
        const id = asString(block.tool_use_id);
        if (!id) continue;
        if (markDone(id, block.is_error === true ? "failed" : "done")) changed = true;
      }
      return changed;
    }

    // --- codex：item.started / item.completed ---
    if (type === "item.started" || type === "item.completed") {
      const item = asRecord(event.item);
      if (!item) return false;
      const itemType = asString(item.type);
      if (!itemType || CODEX_NON_TOOL_ITEMS.has(itemType)) return false;
      const id = asString(item.id) || `${itemType}-${steps.length}`;
      const cls = classifyTool(asString(item.tool) || itemType);
      const failed = itemType === "error" || asRecord(item.error) !== null;
      return upsert(id, {
        kind: cls.kind,
        label: cls.label,
        detail: summarizeToolDetail(item),
        status: type === "item.started" ? "running" : (failed ? "failed" : "done"),
      });
    }

    // --- opencode：run --format json 的 tool_use 事件 ---
    // 形状：{type:"tool_use", part:{type:"tool", tool:"bash", callID:"call_…",
    //        state:{status:"completed"|"running"|"error", input:{…}, title:"…"}}}
    // 一次工具调用可能先以 running 上报、完成后再上报一次（同一 callID）。
    if (type === "tool_use") {
      const part = asRecord(event.part);
      if (!part || asString(part.type) !== "tool") return false;
      const tool = asString(part.tool) || "tool";
      const id = asString(part.callID) || asString(part.id) || `${tool}-${steps.length}`;
      const state = asRecord(part.state);
      const status = asString(state?.status);
      const failed = status === "error" || status === "failed";
      const cls = classifyTool(tool);
      return upsert(id, {
        kind: cls.kind,
        label: cls.label,
        // 摘要优先取调用参数（bash 的 command、read 的 filePath…），退到 state.title。
        detail: summarizeToolDetail(state?.input) ?? summarizeToolDetail(state?.title),
        status: failed ? "failed" : (status === "running" || status === "pending" ? "running" : "done"),
      });
    }

    // opencode 每步结束：这一步里没再更新的工具视为已完成，别让气泡一直转圈
    // （上面的 step_finish 分支已经收尾，这里不再重复）。

    return false;
  };

  return {
    push,
    finish,
    snapshot: () => steps.map((step) => ({ ...step })),
  };
}
