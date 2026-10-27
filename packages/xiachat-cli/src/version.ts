/**
 * 版本号的单一来源。
 *
 * 背景：SEA 打包后读不到外部 package.json（main.ts 的 .version() 注释里写过），
 * 所以历史上版本号是硬编码字符串、与 package.json 手抄一份。手抄的后果很具体：
 * 管理端「exe 版本」列与升级判断都以这个值为准，抄错了设备就会对着新版报旧版
 * （永远显示「可升级」）或者对着旧版报新版（永远升不上去）。
 *
 * 因此：
 *   1. 构建期由 scripts/build-exe.mjs 通过 esbuild `--define` 注入
 *      __CLAWMESSENGER_VERSION__（等价于 Go 的 -ldflags -X：发布流水线在打 tag
 *      时把 tag 版本带进来，不依赖人改文件）；
 *   2. 没有注入时（tsc 直跑、vitest、开发机）退回下面这个字面量；
 *   3. version.test.ts 断言这里的字面量与 package.json 的 version 一致 ——
 *      这条断言就是「兜底不许撒谎」的护栏。
 */

declare const __CLAWMESSENGER_VERSION__: string | undefined;

/** 与 packages/xiachat-cli/package.json 的 version 手工同步（有测试盯着）。 */
export const CLAWMESSENGER_VERSION_FALLBACK = "1.3.5";

/** 本进程的 CLI 版本：构建期注入优先，否则用兜底字面量。 */
export function cliVersion(): string {
  // typeof 守卫：tsc/vitest 下这个标识符根本不存在，直接引用会 ReferenceError。
  if (typeof __CLAWMESSENGER_VERSION__ === "string" && __CLAWMESSENGER_VERSION__.trim()) {
    return __CLAWMESSENGER_VERSION__.trim();
  }
  return CLAWMESSENGER_VERSION_FALLBACK;
}
