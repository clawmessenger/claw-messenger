/**
 * 入口与命令的「暂存落点」一致性守卫（源码级，运行时不可测）。
 *
 * 为什么只能读源码：`bin.ts` 是进程入口，顶层就带副作用（交换暂存的 exe、
 * 把自身复制到 ~/.local/bin）。在测试里 import 它，会真的去动跑测试那台机器的
 * home 目录 —— 这不是「难测」，是「不该测」。
 *
 * 要锁住的回归（2026-10-28 现场事故）：
 *   暂存升级的落点必须是 `upgradeTargetPath`（= 正在运行的自己 / execPath），
 *   不能退回按平台猜的 `installPaths`。猜错时的现象极具欺骗性 ——
 *   systemd 的 unit 指向 /usr/local/bin、installPaths 指向 ~/.local/bin，
 *   于是暂存文件落在**没人运行的那份**旁边：`update` 兴高采烈地报
 *   「staged 1.3.x」，每次启动却去另一个目录找暂存、永远找不到，
 *   版本纹丝不动；而下一次 `update` 又会去改同一个错文件，问题自己不会暴露。
 */
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const read = (rel: string): string => readFileSync(new URL(rel, import.meta.url), "utf8");

/** 取 `start` 之后、`end` 之前的那一段；找不到 start 直接抛，避免静默通过。 */
function sliceBetween(src: string, start: string, end: string): string {
  const i = src.indexOf(start);
  if (i < 0) throw new Error(`anchor not found: ${start}`);
  const j = src.indexOf(end, i + start.length);
  return j < 0 ? src.slice(i) : src.slice(i, j);
}

describe("暂存升级的落点必须与正在运行的自己一致", () => {
  it("bin.ts 的启动兜底交换走 upgradeTargetPath，不用 installPaths 猜路径", () => {
    const src = read("./bin.ts");
    expect(src).toContain("upgradeTargetPath(");
    // 注释里提到 installPaths 是允许的（那是在解释为什么不能用它），
    // 但不允许出现调用。所以匹配「带左括号的调用」而不是裸标识符。
    expect(/installPaths\s*\(/.test(src)).toBe(false);
  });

  it("update 命令与启动交换指向同一个判定函数", () => {
    const src = read("./main.ts");
    const updateBlock = sliceBetween(src, '.command("update")', '.command("__relaunch"');
    expect(updateBlock).toContain("upgradeTargetPath(");
    expect(/installPaths\s*\(/.test(updateBlock)).toBe(false);
  });
});
