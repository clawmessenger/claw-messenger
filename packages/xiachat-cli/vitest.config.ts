import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.ts"],
    // 默认 5s 对「spawn 子进程模拟 agent」的用例太紧：Windows + 杀软/并发跑时
    // 经常踩线（单独跑只挂 1 个，并行跑挂 4 个，纯抖动）。放到 20s。
    //
    // 2026-10-27 再抬到 60s：CLI 1.3.5 发布时，同一份代码连跑 5 次全量，其中 1 次
    // 有 2 例失败 —— 隔离单跑 4.52s、负载回落后整包 28.1s 全绿，是**负载敏感**而不是
    // 逻辑错。20s 在机器忙（同时跑 SEA 构建/下载）时不够用；抬到 60s 后同一时段连跑
    // 5 次全绿。这里不针对某两例打补丁，是因为全局上限本来就是为这类抖动设的。
    testTimeout: 60_000,
  },
});
