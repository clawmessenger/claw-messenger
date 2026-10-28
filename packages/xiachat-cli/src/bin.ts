import * as os from "node:os";
import { buildProgram, commanderExit } from "./main.js";
import { Keystore } from "./keystore.js";
import { XiachatApi } from "./api.js";
import { defaultKeystorePath } from "./config.js";
import { ensureInstalled, isSeaExecutable, upgradeTargetPath, type InstallResult } from "./install.js";
import { applyStagedUpdate, cleanupStaleBackups, healInstall } from "./self-update.js";

// A staged update — downloaded and checksum-verified by `clawmessenger update`
// on an earlier run — is swapped in here, before anything else starts. The
// current process keeps executing the image it already mapped; every process
// started afterwards (including the agents the supervisor spawns) runs the new
// version. Renaming the live exe is what makes this possible on Windows, where
// an in-place overwrite is refused with EBUSY.
let stagedVersion: string | null = null;
try {
  if (isSeaExecutable(process.argv[1])) {
    // 落点必须是**正在运行的自己**（`process.execPath`），不能按平台猜 ——
    // 与手动 `update`、服务端下发升级共用 upgradeTargetPath 这一个判定。
    // 按 installPaths 猜会在 Linux 上直接猜错：systemd 的 unit 可能指向
    // /usr/local/bin，而 installPaths 给的是 ~/.local/bin，于是暂存文件落在
    // 没人运行的那份旁边，每次启动都「什么也没有」，升级静默失效。判定细节
    // 与理由见 install.ts 的 upgradeTargetPath。
    const target = upgradeTargetPath(
      process.platform,
      process.argv[1],
      process.execPath,
      os.homedir(),
    );
    // 自愈先于交换：升级的交换是两步 rename（旧 → .old-<ts>、新 → target），
    // 中间 target 会短暂不存在。断电停在那一步之后，登录自启的 VBS 会找不到
    // exe 而静默失败 —— 设备就此离线。这里（以及 __relaunch）把 target 补回来。
    const healed = healInstall(target);
    if (healed.healed) {
      process.stderr.write(
        `clawmessenger: restored ${target} from ${healed.staged ?? healed.from ?? "a backup"}\n`,
      );
    }
    const applied = applyStagedUpdate(target, process.platform);
    if (applied.applied) stagedVersion = applied.version ?? null;
    cleanupStaleBackups(target);
  }
} catch {
  // Update bookkeeping must never block normal operation.
}

// Single-file exe: best-effort per-user install (copy + PATH) on first run.
// Quiet no-op when already installed or running under `node` in dev.
let installNote: InstallResult | null = null;
try {
  installNote = ensureInstalled();
} catch {
  installNote = null;
}

const program = buildProgram({
  keystore: new Keystore(defaultKeystorePath()),
  apiFactory: (serverUrl) => new XiachatApi(serverUrl),
  stdout: process.stdout,
});

program.parseAsync(process.argv).catch((err: unknown) => {
  // `.exitOverride()` 让 commander 用抛异常代替 process.exit（测试要在进程内 parse）。
  // 生产入口必须把这类异常翻译回**正确的**退出码：`--version` / `--help` 是成功，
  // 不是错误（否则 `clawmessenger --version` 打印版本却 exit 1，发布流水线的版本
  // 校验永远失败，而且版本号会被重复打到 stderr）。
  const translated = commanderExit(err);
  if (translated) {
    if (translated.printMessage) console.error(err instanceof Error ? err.message : err);
    process.exitCode = translated.exitCode;
    return;
  }
  console.error(err instanceof Error ? err.message : err);
  process.exitCode = 1;
});

// After the command finishes (or when commander prints help and the parse
// promise resolves), tell the user where the binary now lives.
//
// 这两条通知走 **stderr**：stdout 只留给命令本身的输出。发布流水线里
// `actual="$("<exe>" --version | tail -n1)"` 要拿最后一行当版本号，而首次运行时
// 这两条通知恰好打在版本号之后 —— 留在 stdout 就会让流水线读到 "Open a NEW
// terminal window…" 并据此判定「产物版本不对」，把一个正确的包拦下来。
if (installNote && (installNote.copied || installNote.pathAdded)) {
  const what = [installNote.copied ? "copied itself to " + installNote.target : null,
    installNote.pathAdded ? "added it to your user PATH" : null].filter(Boolean).join(" and ");
  process.stderr.write(
    `\nclawmessenger: ${what}.\n` +
    `Open a NEW terminal window and run \`clawmessenger\` from anywhere.\n`,
  );
}

if (stagedVersion) {
  process.stderr.write(
    `\nclawmessenger: update ${stagedVersion} is now on disk; ` +
    `it takes effect the next time the supervisor starts.\n`,
  );
}
