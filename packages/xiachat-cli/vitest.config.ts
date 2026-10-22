import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.ts"],
    // 默认 5s 对「spawn 子进程模拟 agent」的用例太紧：Windows + 杀软/并发跑时
    // 经常踩线（单独跑只挂 1 个，并行跑挂 4 个，纯抖动）。放到 20s。
    testTimeout: 20_000,
  },
});
