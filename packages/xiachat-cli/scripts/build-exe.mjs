// Build a self-contained executable via Node SEA (single executable application).
//
// Toolchain note: esbuild and postject are NOT devDependencies of this package
// (they could not be added offline, ERR_PNPM_NO_OFFLINE_META). Both already
// exist in the workspace root node_modules (hoisted there by other packages),
// so we locate their JS entrypoints by walking up instead of declaring them.
//
// 为什么不走 node_modules/.bin：Windows 上 `.bin/esbuild` 是 `.cmd` 批处理，必须
// `shell: true` 才能起，而一旦过 shell 参数就会被重新解析 —— 这正好是 1.3.0 首次
// 发布时那个 bug 的土壤：define 的参数被双重 JSON 编码成
// `--define:__CLAWMESSENGER_VERSION__="\"1.3.0\""`，POSIX 上无 shell 原样交给
// esbuild，注入进二进制的字符串**自带引号**（`clawmessenger --version` 打印
// `"1.3.0"`，报给服务端的版本也跟着带引号）；Windows 上 cmd 吃掉一层转义，
// 反而「碰巧正确」。用 process.execPath 直接跑 JS 入口，两个平台拿到完全相同的
// argv，没有第二套解析器可以偏离。
//
// Usage:
//   node scripts/build-exe.mjs                          # dist/clawmessenger.exe from src/bin.ts
//   node scripts/build-exe.mjs --entry <file> --name <name>
import { spawnSync } from "node:child_process";
import {
  closeSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  openSync,
  readFileSync,
  readSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const pkgRoot = resolve(here, "..");

/**
 * 失败时必须留下 `::error::` 注解。这个脚本跑在发布流水线里，失败时 GitHub 只给
 * 一句 "Process completed with exit code 1."，真正的异常堆栈在日志里 —— 而日志要
 * 登录才能看。注解不需要。
 */
function fail(message) {
  console.log(`::error::build-exe: ${message.replace(/%/g, "%25").replace(/\r?\n/g, "%0A")}`);
  throw new Error(message);
}

/** Resolve a package-internal file (e.g. `esbuild/bin/esbuild`) up the workspace. */
function findModuleFile(relative) {
  const tried = [];
  let dir = pkgRoot;
  for (;;) {
    const candidate = join(dir, "node_modules", relative);
    tried.push(candidate);
    if (existsSync(candidate)) return candidate;
    const parent = dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  // `.npmrc` 里是 shamefully-hoist=true，所以上面这条路径应当命中；真命中不了
  // 就把试过的路径全写进注解，别让人再去猜 pnpm 把包装到哪了。
  fail(`cannot find node_modules/${relative}; tried: ${tried.join(" | ")}`);
}

/** Last `n` lines of a captured stream, for the annotation. */
function tail(text, n = 15) {
  const lines = text.split(/\r?\n/).filter((line) => line.trim() !== "");
  return lines.slice(-n).join(" ⏎ ") || "<empty>";
}

/**
 * 工具入口是「要交给 node 跑的脚本」还是「本机可执行文件」？
 *
 * 不能按平台猜：`node_modules/esbuild/bin/esbuild` 在 Windows 上是 JS shim，在
 * Linux 上是**原生 ELF 可执行文件**。用 node 去跑 ELF 得到的报错是
 * `SyntaxError: Invalid or unexpected token` 加上一行 `\x7fELF` —— v1.3.0 第三次
 * 构建失败就是这么来的（Windows 通过、三个 POSIX 全挂）。
 *
 * 看文件头即可：ELF / PE(MZ) / Mach-O 直接执行，其余（shebang 或普通脚本）交给 node。
 */
function isNativeBinary(file) {
  let fd;
  try {
    fd = openSync(file, "r");
    const head = Buffer.alloc(4);
    if (readSync(fd, head, 0, 4, 0) < 4) return false;
    if (head[0] === 0x7f && head[1] === 0x45 && head[2] === 0x4c && head[3] === 0x46) return true; // ELF
    if (head[0] === 0x4d && head[1] === 0x5a) return true; // PE (MZ)
    const magic = head.readUInt32BE(0);
    return [0xfeedface, 0xfeedfacf, 0xcefaedfe, 0xcffaedfe, 0xcafebabe, 0xbebafeca].includes(magic);
  } catch {
    return false; // 读不出头就当脚本，交由 node 报出真正的错
  } finally {
    if (fd !== undefined) closeSync(fd);
  }
}

/** Spawn a build tool whichever way the platform ships it — never through a shell. */
function runTool(label, toolPath, toolArgs) {
  return isNativeBinary(toolPath)
    ? run(label, toolPath, toolArgs)
    : run(label, process.execPath, [toolPath, ...toolArgs]);
}
/**
 * Run a build tool and turn **every** failure mode into an annotation.
 *
 * 三个理由：
 *   1. `spawnSync` 对「起不来」只把 error 挂在返回值上，而 esbuild 自己的 JS shim
 *      在子进程失败时是直接 throw 的 —— 那样就绕过了状态检查。
 *   2. 工具的输出（esbuild 到底在抱怨什么）默认只进日志，而日志要登录才能看。
 *      所以 stdio 一律落到文件，失败时把末尾几行塞进注解。
 *   3. 输出接**文件**而不是管道：接文件时子进程的 stdout 是同步写，接管道是异步
 *      写，判断依据不该取决于采集方式。
 */
function run(label, file, commandArgs) {
  const out = join(distDir, `.${label}.out`);
  const err = join(distDir, `.${label}.err`);
  let result;
  try {
    const outFd = openSync(out, "w");
    const errFd = openSync(err, "w");
    try {
      result = spawnSync(file, commandArgs, { stdio: ["ignore", outFd, errFd] });
    } finally {
      closeSync(outFd);
      closeSync(errFd);
    }
  } catch (err2) {
    fail(`${label} threw: ${err2 instanceof Error ? err2.message : String(err2)}`);
  }
  const stdout = (existsSync(out) ? readFileSync(out, "utf8") : "").trim();
  const stderr = (existsSync(err) ? readFileSync(err, "utf8") : "").trim();
  rmSync(out, { force: true });
  rmSync(err, { force: true });
  // 成功也把工具输出打出来：日志里仍能看到 esbuild 的 warning / postject 的注入信息。
  if (stdout) console.log(stdout);
  if (stderr) console.error(stderr);
  if (result.error) fail(`${label} could not be started: ${result.error.message}`);
  if (result.status !== 0) {
    fail(
      `${label} failed (exit ${result.status ?? "signal"}): ${file} ${commandArgs[0] ?? ""} — ` +
        `stdout: ${tail(stdout)} | stderr: ${tail(stderr)}`,
    );
  }
  return result;
}

/**
 * Best-effort run: log what happened and continue. 用在「失败了也无所谓」的步骤上
 * （比如给未签名的二进制去签名，本身就会失败），**不能**复用 run()：那会打出一条
 * `::error::` 注解，把一个正常构建标成有错误。
 */
function runOptional(label, file, commandArgs) {
  let result;
  try {
    result = spawnSync(file, commandArgs, { stdio: "inherit" });
  } catch (err) {
    console.log(`note: ${label} could not run: ${err instanceof Error ? err.message : String(err)}`);
    return null;
  }
  if (result.error) console.log(`note: ${label} could not run: ${result.error.message}`);
  else if (result.status !== 0) console.log(`note: ${label} exited ${result.status} (ignored)`);
  return result;
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
  fail(
    `cannot resolve a version from ${JSON.stringify(rawVersion)}: ` +
      "pass --version x.y.z, set CLAWMESSENGER_VERSION=x.y.z, or set package.json#version",
  );
}
if (injectedVersion !== rawVersion) {
  console.log(`version ${rawVersion} normalized to ${injectedVersion}`);
}
const outCjs = join(distDir, `${exeName}.sea.cjs`);
const esbuildCli = findModuleFile(join("esbuild", "bin", "esbuild"));
console.log(
  `building ${exeName} ${injectedVersion} on ${process.platform}/${process.arch} ` +
    `with node ${process.version} (esbuild: ${esbuildCli})`,
);
// define 的值**只能编码一次**：JSON.stringify 一次得到的 `"1.3.0"`（带引号的
// JSON 字符串字面量）正是 esbuild 期望的形式。编码两次会得到 `"\"1.3.0\""`，
// 注入进二进制的字符串自带引号 —— 见文件头关于 1.3.0 首次发布的注释。
const defineArg = `--define:__CLAWMESSENGER_VERSION__=${JSON.stringify(injectedVersion)}`;
runTool("esbuild", esbuildCli, [
  entry,
  "--bundle",
  "--platform=node",
  "--format=cjs",
  "--target=node20",
  defineArg,
  `--outfile=${outCjs}`,
  "--log-level=warning",
]);

// 1b. 自检：把刚打出的 bundle 当普通 node 脚本跑一次，确认它答出来的版本**逐字符**
// 等于注入值。上面那个双重编码的 bug 靠这一步在构建期就暴露，而不是等到发版时由
// 流水线的闸门发现（代价是两轮 CI）。
//
// 只警告、不阻断：权威在发布闸门（Gate 1 跑的就是同一个 bundle，Gate 2 跑最终产物），
// 这里是「尽早说话」而不是第二个把关人 —— 一个因环境原因跑不起来的自检不该拦住
// 一个好版本。失败原因写进 `::warning::`，注解里直接能读。
//
// 用文件接住子进程的 stdout，而不是默认管道：接文件时 process.stdout 是同步写、
// 接管道是异步写，判断依据不该取决于采集方式。
const probeOut = join(distDir, `${exeName}.version-probe.out`);
const probeErr = join(distDir, `${exeName}.version-probe.err`);
let probe = { status: null, error: null };
try {
  const probeOutFd = openSync(probeOut, "w");
  const probeErrFd = openSync(probeErr, "w");
  try {
    probe = spawnSync(process.execPath, [outCjs, "--version"], {
      stdio: ["ignore", probeOutFd, probeErrFd],
    });
  } finally {
    closeSync(probeOutFd);
    closeSync(probeErrFd);
  }
} catch (err) {
  probe = { status: null, error: err };
}
const probed = (existsSync(probeOut) ? readFileSync(probeOut, "utf8") : "").trim();
const probeStderr = (existsSync(probeErr) ? readFileSync(probeErr, "utf8") : "").trim();
rmSync(probeOut, { force: true });
rmSync(probeErr, { force: true });
console.log(`bundle self-check: --version -> ${JSON.stringify(probed)} (exit ${probe.status})`);
if (probe.error || probe.status !== 0 || probed !== injectedVersion) {
  const detail =
    `${probe.error ? `could not start (${probe.error.message})` : `exit ${probe.status}`}` +
    `, stdout ${JSON.stringify(probed)}, injected ${JSON.stringify(injectedVersion)}, stderr: ${tail(probeStderr, 10)}`;
  console.log(`::warning::build-exe: the bundle self-check disagrees — ${detail.replace(/%/g, "%25").replace(/\r?\n/g, "%0A")}`);
}

// 2 + 3. SEA config → blob → 注入到一份 node 副本里（macOS 还需签名）。
//
// macOS 上必须处理签名：官方 Node 二进制是带签名的，注入 blob 会改掉文件内容，签名
// 随之失效 —— 系统不报「签名无效」，而是直接把进程杀掉（实测 exit 139 / SIGSEGV，
// stdout 和 stderr 全空）。所以先去掉原签名，注入后再做 ad-hoc 签名（Node SEA 文档
// 里 macOS 的标准步骤）。
//
// 但只做签名并不足以保证产物能跑（v1.3.0 第 7 次构建就在这一步仍然 139）。所以
// macOS 上这里做的是「构建 + 实测 + 回退」：注入后先测一次（未签名），签名后再测一次，
// 还挂就关掉 useCodeCache 重来一遍，每一步的结果都以 `::warning::` 写进注解 ——
// 一次 CI 就能看到整张对照表，不用一轮一轮试。
//
// 顺带一个结论：v1.1.0 的 darwin-arm64 大概一直是坏的。那时流水线没有「跑一遍产物」
// 这道闸门，从来没人执行过它。
const blobPath = join(distDir, `${exeName}.sea.blob`);
const configPath = join(distDir, `${exeName}.sea-config.json`);
const isMac = process.platform === "darwin";
const postjectCli = findModuleFile(join("postject", "dist", "cli.js"));

function writeSeaConfig(useCodeCache) {
  writeFileSync(
    configPath,
    JSON.stringify(
      {
        main: outCjs,
        output: blobPath,
        disableExperimentalSEAWarning: true,
        useSnapshot: false,
        useCodeCache,
      },
      null,
      2,
    ),
  );
}

/**
 * 跑一个二进制，结果打到日志；不抛异常、也不判断「对不对」。
 *
 * 一定要把 `signal` 单独报出来：被信号杀掉的进程 `status` 是 null，只打 exit 会变成
 * 「exit null」——而 macOS 上「签名无效」恰恰表现为被信号杀掉（SIGSEGV / SIGKILL），
 * 空 stdout 空 stderr，看起来像什么都没发生。这行信息是把「崩在哪」和「崩成什么样」
 * 分开的关键。
 */
function probeBinary(label, binary, binArgs) {
  const out = join(distDir, `.exec-probe-${label}.out`);
  const err = join(distDir, `.exec-probe-${label}.err`);
  let result = { status: null, signal: null };
  try {
    const outFd = openSync(out, "w");
    const errFd = openSync(err, "w");
    try {
      result = spawnSync(binary, binArgs, { stdio: ["ignore", outFd, errFd] });
    } finally {
      closeSync(outFd);
      closeSync(errFd);
    }
  } catch (e) {
    result = { status: null, signal: null, error: e };
  }
  const stdout = (existsSync(out) ? readFileSync(out, "utf8") : "").trim();
  const stderr = (existsSync(err) ? readFileSync(err, "utf8") : "").trim();
  rmSync(out, { force: true });
  rmSync(err, { force: true });
  const status = result.error ? `error(${result.error.message})` : result.status;
  const how = result.signal ? `signal ${result.signal}` : `exit ${status}`;
  console.log(`probe [${label}]: ${how}, stdout ${JSON.stringify(stdout)}`);
  return { status, signal: result.signal ?? null, how, stdout, stderr };
}

/** 产物自检：必须答出注入的那个版本，且正常退出。 */
function probeExecutable(label) {
  const p = probeBinary(label, exePath, ["--version"]);
  const ok = p.signal === null && String(p.status) === "0" && p.stdout === injectedVersion;
  console.log(`exec probe [${label}]: ${p.how}, stdout ${JSON.stringify(p.stdout)}${ok ? "" : " <<< NOT OK"}`);
  if (!ok) {
    const detail = `exec probe [${label}] — ${exeName} --version ${p.how}, stdout ${JSON.stringify(p.stdout)}, stderr: ${tail(p.stderr, 8)}`;
    console.log(`::warning::build-exe: ${detail.replace(/%/g, "%25").replace(/\r?\n/g, "%0A")}`);
  }
  return { ...p, ok };
}

/** 写 config → 生成 blob → 复制 node → [mac 去签名] → 注入 → [mac 签名] */
function buildExecutable(useCodeCache) {
  writeSeaConfig(useCodeCache);
  run("sea-blob", process.execPath, ["--experimental-sea-config", configPath]);
  rmSync(exePath, { force: true });
  copyFileSync(process.execPath, exePath);
  if (isMac) {
    // 未签名的二进制会让这一步失败，不算错误。
    runOptional("codesign-remove", "codesign", ["--remove-signature", exePath]);
  }
  runTool("postject", postjectCli, [
    exePath,
    "NODE_SEA_BLOB",
    blobPath,
    "--sentinel-fuse",
    "NODE_SEA_FUSE_fce680ab2cc467b6e072b8b5df1996b2",
    // postject 的 Mach-O 段名默认是 `__POSTJECT`，而 Node 官方 SEA 文档给的是
    // `--macho-segment-name NODE_SEA`。这是唯一一处只影响 darwin 的偏差 ——
    // 而 darwin 也正是四个平台里唯一一个「四个版本从头到尾没人跑起来过」的平台。
    // 段名不对的时候 node 依然能靠 sentinel fuse 判定自己是 SEA，却按错误的布局去
    // 解那一节，于是启动阶段直接段错误、stdout/stderr 全空（正是实测现象）。
    ...(isMac ? ["--macho-segment-name", "NODE_SEA"] : []),
  ]);
  if (isMac) {
    const unsigned = probeExecutable("unsigned");
    // 这一步必须成功：签不上的产物在任何 Mac 上都起不来。
    run("codesign-sign", "codesign", ["--sign", "-", "--force", exePath]);
    const info = spawnSync("codesign", ["-dv", "--verbose=2", exePath], { encoding: "utf8" });
    console.log(`codesign: ${(info.stderr ?? "").trim().split(/\r?\n/).slice(0, 4).join(" | ")}`);
    return { unsigned, signed: probeExecutable("signed") };
  }
  return { signed: probeExecutable("final") };
}

// macOS 上先做一次「分水岭」实验：**原样复制一份 node**跑不跑得起来。
//
// 三种注入结果（未签名 / 已签名 / 关 code cache）如果全挂，光看它们分不清是
// 「runner 环境本身有问题」还是「注入 + 签名把二进制弄坏了」。而一份纯 node 副本
// 是这两者的天然分界：它能跑 ⇒ 环境没问题，锅在注入/签名；它不能跑 ⇒ 这个 runner
// 上复制出来的 node 就不该被 torch，后面的结论全都不算数。
//
// 顺手把「去签名后」和「重新 ad-hoc 签名后」也各测一次，于是这一段本身就是一张
// 完整的对照表：copy / copy-unsigned / copy-resigned。
if (isMac) {
  const baseline = join(distDir, ".node-baseline");
  copyFileSync(process.execPath, baseline);
  const b0 = probeBinary("node-copy", baseline, ["--version"]);
  runOptional("baseline-remove", "codesign", ["--remove-signature", baseline]);
  const b1 = probeBinary("node-copy-unsigned", baseline, ["--version"]);
  const signBaseline = spawnSync("codesign", ["--sign", "-", "--force", baseline], { encoding: "utf8" });
  const b2 = probeBinary("node-copy-resigned", baseline, ["--version"]);
  rmSync(baseline, { force: true });
  console.log(
    `baseline table: copy=${b0.how}/${JSON.stringify(b0.stdout)} ` +
      `unsigned=${b1.how}/${JSON.stringify(b1.stdout)} ` +
      `resigned=${b2.how}/${JSON.stringify(b2.stdout)} (resign exit ${signBaseline.status})`,
  );
  if (b1.signal || b2.signal) {
    // 连一份原样的 node 副本都起不来，那问题不在 SEA 层；别让后面几轮的失败掩盖这件事。
    console.log(
      `::warning::build-exe: 连未注入的 node 副本在这台 runner 上都跑不起来 ` +
        `(unsigned ${b1.how}, resigned ${b2.how}) — 后续 SEA 结论不可信`,
    );
  }
}

let attempt = buildExecutable(true);
if (isMac && !attempt.signed.ok) {
  // 代码缓存对签名后的镜像最敏感，先试这一项 —— 它只是启动期的优化，关掉不影响功能。
  console.log("::warning::build-exe: macOS 产物在签名后仍无法运行，关闭 useCodeCache 重建一次");
  attempt = buildExecutable(false);
  console.log(`exec probe [noCodeCache]: ${attempt.signed.how}, ok=${attempt.signed.ok}`);
}

// macOS 上仍然跑不起来就不要发这个包：一个「下下来就段错误」的资产比没有资产更糟。
if (isMac && !attempt.signed.ok) {
  fail(
    `the macOS executable still fails to run after signing (${attempt.signed.how}, ` +
      `stdout ${JSON.stringify(attempt.signed.stdout)}, stderr: ${tail(attempt.signed.stderr, 8)}); ` +
      `refusing to publish a binary that cannot start`,
  );
}

console.log(`built ${exePath} (clawmessenger ${injectedVersion})`);
