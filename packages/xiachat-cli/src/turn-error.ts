// 本轮失败分类：把 agent 进程的失败原因（`agent exited with code N: <stderr 尾巴>`）
// 归一成一个**稳定、可枚举、可传输**的短码，随 chat_stream 失败终帧的
// error_code 发给 web，由前端翻成一句用户看得懂的话。
//
// 为什么必须归一成短码、不能直接透传 stderr：
//   1. web 端 parseChatStreamEvent 把 error_code 当**标识符**校验（无控制字符、
//      无首尾空白、≤256 字节）。stderr 里一个换行就让整帧判为无效被丢弃 ——
//      前端于是只剩「正在思考」永远转圈，这正是「模型额度用完也一直转圈」的
//      根因之一（0.2.37 之前还把 errMsg.slice(0,120) 直接塞进 error_code）。
//   2. 同一种失败在各家运行时措辞不同（hermes/opencode 走 quukk 上游，
//      claude 走 Anthropic，openclaw 自成一派），前端不该认识二十种错误串。
//
// 判定顺序按「用户能采取的动作」排：额度/余额（换模型或充值）> 鉴权（换 key）
// > 超时（重试）> 限流（等一会）> 兜底。1308 必须先于 429 判 —— 上游把
// 5 小时额度耗尽也回 429，只是 body 里带 [1308]。

/**
 * 失败终帧上的 error_code 取值。**只增不改**：web 端按字面量分支渲染文案，
 * 改一个已发布的值等于让线上旧前端丢文案。新增语义就新增枚举。
 */
export type TurnErrorCode =
  /** 模型额度用完（5 小时/每日限额、配额上限）：等额度重置或换模型。 */
  | "model_quota_exhausted"
  /** 账户余额不足：需要充值，换模型没用。 */
  | "model_balance_exhausted"
  /** 鉴权失败（key 无效/过期/无权访问该模型）：去设备端换 key。 */
  | "model_auth_failed"
  /** 被限流（429 且不是额度耗尽）：稍后重试即可。 */
  | "model_rate_limited"
  /** 本轮跑超时被我们自己掐掉。 */
  | "turn_timeout"
  /** 会话队列满（1 在执行 + 1 排队），这条消息被直接丢弃。
   *  由 run.ts 的 SessionQueue 在忙碌时发出，不经过 classifyTurnError。 */
  | "agent_busy"
  /** 其余一切失败（进程崩溃、输出解析失败、未知错误）。 */
  | "agent_failed";

const QUOTA_PATTERNS: readonly RegExp[] = [
  // quukk / 智谱系上游的 5 小时额度：`HTTP 429: [1308][已达到 5 小时的使用上限…]`
  /\[1308\]/,
  /使用上限/,
  /额度(?:已|不足|用完|用尽|耗尽)/,
  /配额(?:已|不足|用完|用尽|耗尽|超出)/,
  /insufficient_?quota/i,
  /exceeded your current quota/i,
  /out of credits/i,
];

const BALANCE_PATTERNS: readonly RegExp[] = [
  // 402 + insufficient balance：换模型也许还有别的余额，但当前这个模型付不起。
  /insufficient balance/i,
  /余额不足/,
  /欠费/,
  /\b402\b/,
  /payment required/i,
];

const AUTH_PATTERNS: readonly RegExp[] = [
  /invalid api key/i,
  /incorrect api key/i,
  /invalid[_ ]?x-api-key/i,
  /unauthorized/i,
  /\b401\b/,
  /authentication(?:_error| failed| error)/i,
  /no such model/i,
  /model not found/i,
];

// 只认**我们自己**的超时措辞（agents.ts 的 `agent turn timeout after Nms`）。
// 不认泛化的 "timed out"：agent 的 stderr 里工具级超时很常见，那属于「这一轮
// 跑了但某步失败」，不该被说成「本轮执行超时」。
const TIMEOUT_PATTERNS: readonly RegExp[] = [
  /agent turn timeout/i,
  /turn timeout after/i,
];

const RATE_LIMIT_PATTERNS: readonly RegExp[] = [
  /\b429\b/,
  /rate[_ -]?limit(?:ed)?/i,
  /too many requests/i,
];

function matchesAny(text: string, patterns: readonly RegExp[]): boolean {
  return patterns.some((pattern) => pattern.test(text));
}

/**
 * 把一轮失败的原始错误串归类。**永不抛错、永不返回空**：分类只影响文案，
 * 不能因为它自己出错就把失败终帧也弄丢。
 */
export function classifyTurnError(raw: string): TurnErrorCode {
  const text = (raw ?? "").slice(0, 4000);
  // 用户主动停止（chat_stop）会把 agent 进程杀掉，错误串长得像崩溃。它不该
  // 被归成「模型额度用完」—— 调用方另有 aborted 分支，这里只是别误报。
  if (/agent turn aborted/i.test(text)) return "agent_failed";
  if (matchesAny(text, QUOTA_PATTERNS)) return "model_quota_exhausted";
  if (matchesAny(text, BALANCE_PATTERNS)) return "model_balance_exhausted";
  if (matchesAny(text, AUTH_PATTERNS)) return "model_auth_failed";
  if (matchesAny(text, TIMEOUT_PATTERNS)) return "turn_timeout";
  if (matchesAny(text, RATE_LIMIT_PATTERNS)) return "model_rate_limited";
  return "agent_failed";
}

/**
 * 一行给人看的中文说明，仅用于设备端日志（web 端有自己的一份文案表，
 * 两边按 TurnErrorCode 对齐而不是按字符串对齐）。
 */
export function describeTurnError(code: TurnErrorCode): string {
  switch (code) {
    case "model_quota_exhausted": return "模型额度已用完";
    case "model_balance_exhausted": return "模型账户余额不足";
    case "model_auth_failed": return "模型鉴权失败（key 无效/无权限）";
    case "model_rate_limited": return "模型请求被限流";
    case "turn_timeout": return "本轮执行超时";
    case "agent_busy": return "上一个任务还在执行，本条消息被丢弃";
    case "agent_failed": return "agent 执行失败";
  }
}
