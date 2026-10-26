// 已答轮次台账：把「这一轮我已经答过」这件事落盘。
//
// 为什么需要它（2026-10-26 现场）：
// 服务端按序派发的 your_turn 带的是 `request_id`（讨论组里是 UUID）。设备**每次
// 重连**（重启 supervisor、换 exe、网络抖动）服务端都会把这个还没确认完毕的
// request_id 再派一次。而 run.ts 的 replay dedupe 只按融云 `messageUId` 去重 ——
// 重发是一条**新消息**（新 messageUId），于是每次都放行、每次都真跑一遍 agent。
//
// 观察到的样子：同一个讨论组同一轮，codex 的回答出现了三份，正文各不相同
// （276 / 326 / 282 字），即三次真执行。用户看到的就是「同一个角色说了三遍，
// 每遍内容还不一样」。
//
// 判据只认**成功收尾**的轮次（completed 终帧 + command_result 都发出去之后才
// 记账）。被中止、超时、失败的轮次不记账 —— 那种情况服务端重派是合理的，应该
// 真跑一遍。重派命中台账时只补帧、跑 agent 之前就返回：正文、终帧、command_result
// 都用上一轮的原物，服务端据此推进下一位，web 端按「已是终态 + 同 seq」幂等丢弃。
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";

/** 一轮成功收尾后要留的东西：够重放，不含工具原文（那部分重放没有意义）。 */
export interface AnsweredTurn {
  /** 记账时刻（ms）。用于淘汰最旧与诊断。 */
  at: number;
  /** 原帧的 stream_id：重放必须复用，换新的会让 web 端多出一条气泡。 */
  streamId: string;
  /** 原终帧的 seq：web 端对「已是终态 + 同 seq」直接丢弃。 */
  seq: number;
  /** 正文全文（超过上限截断，见 ANSWERED_TURN_MAX_TEXT_CHARS）。 */
  text: string;
  requesterId: string;
  conversationType: 1 | 3 | 4;
  conversationId: string;
}

export interface AnsweredTurnStore {
  /** 这一轮答过没有。命中即可只补帧、不跑 agent。 */
  get(requestId: string): AnsweredTurn | undefined;
  /** 记账；同一 requestId 重复记账以最后一次为准。 */
  remember(requestId: string, turn: AnsweredTurn): void;
  /** 立刻落盘（停机前调用，别把最后一批丢了）。 */
  flush(): void;
  readonly size: number;
}

export interface AnsweredTurnStoreOpts {
  /** 落盘位置。省略 -> 纯内存（测试用）。 */
  path?: string;
  maxEntries?: number;
  flushDelayMs?: number;
}

/** 50 条足够覆盖「重启后服务端重派的那一小批」，也不会让文件无限长。 */
export const ANSWERED_TURN_MAX_ENTRIES = 50;

/**
 * 单轮正文的留存上限。讨论组的回答通常几百到两千字，20000 字符远有余量；
 * 上限是为了让 50 条台账的文件大小有硬边界（最坏 1MB 量级）。
 */
export const ANSWERED_TURN_MAX_TEXT_CHARS = 20_000;

function isAnsweredTurn(v: unknown): v is AnsweredTurn {
  if (typeof v !== "object" || v === null) return false;
  const t = v as Record<string, unknown>;
  return typeof t.at === "number"
    && typeof t.streamId === "string"
    && typeof t.seq === "number"
    && typeof t.text === "string"
    && typeof t.requesterId === "string"
    && (t.conversationType === 1 || t.conversationType === 3 || t.conversationType === 4)
    && typeof t.conversationId === "string";
}

function readEntries(path: string | undefined): Array<[string, AnsweredTurn]> {
  if (!path) return [];
  let parsed: unknown;
  try {
    parsed = JSON.parse(readFileSync(path, "utf8"));
  } catch {
    // 缺失或损坏：从空开始。丢台账最多是多重跑一轮，绝不能因此不让设备跑。
    return [];
  }
  if (!Array.isArray(parsed)) return [];
  const out: Array<[string, AnsweredTurn]> = [];
  for (const item of parsed) {
    // 保序落盘格式：[[requestId, turn], ...]
    if (!Array.isArray(item) || item.length !== 2) continue;
    const [id, turn] = item as [unknown, unknown];
    if (typeof id !== "string" || !id || !isAnsweredTurn(turn)) continue;
    out.push([id, turn]);
  }
  return out;
}

export function createAnsweredTurnStore(
  opts: AnsweredTurnStoreOpts = {},
): AnsweredTurnStore {
  const { path, maxEntries = ANSWERED_TURN_MAX_ENTRIES, flushDelayMs = 1_000 } = opts;
  // Map 的插入顺序就是淘汰顺序：最旧的先走。
  const seen = new Map<string, AnsweredTurn>(readEntries(path));
  const evict = (): void => {
    while (seen.size > maxEntries) {
      const oldest = seen.keys().next().value;
      if (oldest === undefined) break;
      seen.delete(oldest);
    }
  };
  let timer: ReturnType<typeof setTimeout> | undefined;
  const flush = (): void => {
    if (timer) {
      clearTimeout(timer);
      timer = undefined;
    }
    if (!path) return;
    try {
      mkdirSync(dirname(path), { recursive: true });
      writeFileSync(path, JSON.stringify([...seen]), { mode: 0o600 });
    } catch (err) {
      // 尽力而为：写不进去只意味着下次重启可能重跑一轮，不该影响本轮的产出。
      console.error("answered-turns write failed:", err instanceof Error ? err.message : err);
    }
  };
  return {
    get(requestId) {
      if (!requestId) return undefined;
      return seen.get(requestId);
    },
    remember(requestId, turn) {
      if (!requestId) return;
      // 先删后插：重新记账的条目要排到队尾，避免被当成最旧的一条淘汰。
      seen.delete(requestId);
      seen.set(requestId, {
        ...turn,
        text: turn.text.length > ANSWERED_TURN_MAX_TEXT_CHARS
          ? turn.text.slice(0, ANSWERED_TURN_MAX_TEXT_CHARS)
          : turn.text,
      });
      evict();
      if (path && !timer) timer = setTimeout(flush, flushDelayMs);
    },
    flush,
    get size() {
      return seen.size;
    },
  };
}
