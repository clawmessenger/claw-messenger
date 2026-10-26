// Build a self-contained executable via Node SEA (single executable application).
//
// Toolchain note: esbuild and postject are NOT devDependencies of this package
// (they could not be added offline, ERR_PNPM_NO_OFFLINE_META). Both binaries
// already exist in the workspace root node_modules/.bin (hoisted there by other
// packages), so we locate them by walking up instead of declaring them.
//
// Usage:
//   node scripts/build-exe.mjs                          # dist/clawmessenger.exe from src/bin.ts
//   node scripts/build-exe.mjs --entry <file> --name <name>
import { spawnSync } from "node:child_process";
import { copyFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const pkgRoot = resolve(here, "..");

const binExt = process.platform === "win32" ? ".CMD" : "";
function findTool(name) {
  let dir = pkgRoot;
  for (;;) {
    const candidate = join(dir, "node_modules", ".bin", name + binExt);
    if (existsSync(candidate)) return candidate;
    const parent = dirname(dir);
    if (parent === dir) {
      throw new Error(`${name} not found in any ancestor node_modules/.bin — run from the pnpm workspace`);
    }
    dir = parent;
  }
}

const args = process.argv.slice(2);
function opt(name, fallback) {
  const i = args.indexOf(name);
  return i >= 0 && i + 1 < args.length ? args[i + 1] : fallback;
}

const entry = resolve(opt("--entry", join(pkgRoot, "src", "bin.ts")));
const exeName = opt("--name", "clawmessenger");
const isWindows = process.platform === "win32";

const distDir = join(pkgRoot, "dist");
mkdirSync(distDir, { recursive: true });
const exePath = join(distDir, isWindows ? `${exeName}.exe` : exeName);

// 1. Bundle to a single CJS file (SEA entry must be CommonJS).
//
// 版本号在这里注入（等价于 Go 的 -ldflags -X）：`--version` 显式指定，否则取
// 环境变量 CLAWMESSENGER_VERSION，最后退回 package.json。发布流水线在 tag 上
// 传 CLAWMESSENGER_VERSION，于是 exe 里编进去的版本就是 tag 的版本 —— 不需要
// 有人记得手改 src/version.ts（那正是「上报的版本和实际装的不一致」的来源）。
//
// 流水线传进来的可能是 `clawmessenger-v1.3.0`（tag）而不是 `1.3.0`，这里统一
// 归一成 x.y.z 并**拒绝**任何不是 x.y.z 的输入：一个编错版本的包会让设备每次
// 「升级成功」都被自己判成 restart_failed，而管理端看不出原因（审查报告 S1）。
const pkg = JSON.parse(readFileSync(join(pkgRoot, "package.json"), "utf8"));
const rawVersion = (opt("--version", process.env.CLAWMESSENGER_VERSION || pkg.version) || pkg.version).trim();
const versionMatch = rawVersion.match(/(\d+\.\d+\.\d+)/);
const injectedVersion = versionMatch ? versionMatch[1] : "";
if (!injectedVersion) {
  throw new Error(
    `cannot resolve a version from ${JSON.stringify(rawVersion)}: ` +
      "pass --version x.y.z, set CLAWMESSENGER_VERSION=x.y.z, or set package.json#version",
  );
}
if (injectedVersion !== rawVersion) {
  console.log(`version ${rawVersion} normalized to ${injectedVersion}`);
}
const outCjs = join(distDir, `${exeName}.sea.cjs`);
const esbuild = findTool("esbuild");
const bundle = spawnSync(
  esbuild,
  [
    entry,
    "--bundle",
    "--platform=node",
    "--format=cjs",
    "--target=node20",
    `--define:__CLAWMESSENGER_VERSION__=${JSON.stringify(JSON.stringify(injectedVersion))}`,
    `--outfile=${outCjs}`,
    "--log-level=warning",
  ],
  { stdio: "inherit", shell: isWindows },
);
if (bundle.status !== 0) throw new Error(`esbuild failed (exit ${bundle.status})`);

// 2. SEA config → blob.
const blobPath = join(distDir, `${exeName}.sea.blob`);
const configPath = join(distDir, `${exeName}.sea-config.json`);
writeFileSync(
  configPath,
  JSON.stringify(
    {
      main: outCjs,
      output: blobPath,
      disableExperimentalSEAWarning: true,
      useSnapshot: false,
      useCodeCache: true,
    },
    null,
    2,
  ),
);
const sea = spawnSync(process.execPath, ["--experimental-sea-config", configPath], { stdio: "inherit" });
if (sea.status !== 0) throw new Error(`node --experimental-sea-config failed (exit ${sea.status})`);

// 3. Copy the current node binary and inject the blob.
rmSync(exePath, { force: true });
copyFileSync(process.execPath, exePath);
const postject = findTool("postject");
const inject = spawnSync(
  postject,
  [exePath, "NODE_SEA_BLOB", blobPath, "--sentinel-fuse", "NODE_SEA_FUSE_fce680ab2cc467b6e072b8b5df1996b2"],
  { stdio: "inherit", shell: isWindows },
);
if (inject.status !== 0) throw new Error(`postject failed (exit ${inject.status})`);

console.log(`built ${exePath} (clawmessenger ${injectedVersion})`);
