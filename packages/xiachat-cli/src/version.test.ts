import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { CLAWMESSENGER_VERSION_FALLBACK, cliVersion } from "./version.js";

// 版本号会被上报给服务端当作「这台机器装的是哪一版」，管理端的「exe 版本」列与
// 「能否升级」全看它。所以这里的断言不是形式主义：字面量与 package.json 一旦
// 漂移，设备就会永远显示「可升级」或者永远升不上去，而且没有任何报错。
describe("cli version", () => {
  it("keeps the fallback in lockstep with package.json", () => {
    const pkg = JSON.parse(
      readFileSync(new URL("../package.json", import.meta.url), "utf8"),
    ) as { version: string };
    expect(CLAWMESSENGER_VERSION_FALLBACK).toBe(pkg.version);
  });

  it("uses the fallback when the build injected nothing (tsc/vitest/dev)", () => {
    expect(cliVersion()).toBe(CLAWMESSENGER_VERSION_FALLBACK);
  });
});
