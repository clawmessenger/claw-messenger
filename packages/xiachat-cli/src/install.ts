/**
 * Self-install: when the single-file SEA executable runs for the first time
 * (typically straight from the Downloads folder), copy itself to a per-user
 * install directory and add that directory to the user's PATH — no admin
 * rights required. Idempotent: running the already-installed copy, or running
 * again from anywhere, is a quiet no-op.
 *
 *   Windows:  %LOCALAPPDATA%\clawmessenger\clawmessenger.exe  (HKCU\Environment\Path)
 *   Linux/macOS:  ~/.local/bin/clawmessenger                   (~/.bashrc / ~/.zshrc)
 */

import { spawnSync } from "node:child_process";
import { copyFileSync, existsSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

export interface InstallPaths {
  dir: string;
  exe: string;
  target: string;
}

/** Per-user install directory and executable name for a platform. */
export function installPaths(platform: string, homedir: string, localAppData?: string): InstallPaths {
  if (platform === "win32") {
    const base = localAppData || path.win32.join(homedir, "AppData", "Local");
    const dir = path.win32.join(base, "clawmessenger");
    return { dir, exe: "clawmessenger.exe", target: path.win32.join(dir, "clawmessenger.exe") };
  }
  const dir = path.posix.join(homedir, ".local", "bin");
  return { dir, exe: "clawmessenger", target: path.posix.join(dir, "clawmessenger") };
}

/**
 * True when running as a single-file SEA executable (not `node bundle.js`).
 *
 * `.cjs` / `.mjs` count as script entrypoints too: the SEA entry bundle that
 * `scripts/build-exe.mjs` emits is `dist/clawmessenger.sea.cjs`, and the release
 * pipeline runs it with plain `node ... --version` to check the baked-in
 * version. Judging that run as "the exe" is not a harmless over-detection — it
 * makes a version check heal/swap a real install directory and copy the running
 * `node` binary (100+ MB) over `~/.local/bin/clawmessenger`.
 */
export function isSeaExecutable(argv1: string | undefined): boolean {
  if (!argv1) return true;
  return !/\.(c|m)?js$/i.test(argv1);
}

/**
 * 一次自更新该覆盖哪个文件。
 *
 * 单文件 exe 跑起来时，答案只有一个：**正在运行的自己**（`process.execPath`）。
 * 按平台去猜安装路径会在 Linux 上直接猜错 —— systemd 的 unit 可能指向
 * `/usr/local/bin/clawmessenger`，而 `installPaths` 给的是 `~/.local/bin/...`。
 * 换错的后果不是「升级没生效」这么简单：unit 重启后仍跑旧版，而那份没人用的
 * 副本变成了新版，于是「页面显示的版本 / 磁盘上的文件 / 实际在跑的东西」三方
 * 对不上，且下一次升级又会去改同一个错文件。
 *
 * 只有在用 node 跑脚本（非 SEA）时才退回安装路径 —— 那时 `process.execPath`
 * 是 node 本体，覆盖它等于把开发机的 node 换成一个 CLI 二进制。
 */
export function upgradeTargetPath(
  platform: string,
  argv1: string | undefined,
  execPath: string,
  homedir: string,
  localAppData?: string,
): string {
  if (isSeaExecutable(argv1)) return execPath;
  return installPaths(platform, homedir, localAppData).target;
}

/** Case-insensitive on Windows, exact elsewhere. */
export function samePath(a: string, b: string, platform: string): boolean {
  const p = platform === "win32" ? path.win32 : path.posix;
  const ra = p.resolve(a);
  const rb = p.resolve(b);
  return platform === "win32" ? ra.toLowerCase() === rb.toLowerCase() : ra === rb;
}

/** Extract the Path value from `reg query HKCU\Environment /v Path` output. */
export function parseRegPathQuery(stdout: string): string {
  const m = stdout.match(/Path\s+REG_(?:EXPAND_)?SZ\s+([^\r\n]*)/i);
  const value = (m?.[1] ?? "").trim();
  // Empty values are echoed as "(value not set)" or similar; treat as absent.
  if (!value || value.startsWith("(")) return "";
  return value;
}

/** Append `entry` to a `;`-separated PATH value unless already present. */
export function nextPathValue(current: string, entry: string, platform: string): string {
  const sep = platform === "win32" ? ";" : ":";
  const norm = (s: string): string => (platform === "win32" ? s.trim().toLowerCase() : s.trim());
  const segments = current.split(sep).map((s) => s.trim()).filter(Boolean);
  if (segments.some((s) => norm(s) === norm(entry))) return current;
  return segments.length ? segments.join(sep) + sep + entry : entry;
}

const RC_PATH_LINE = 'export PATH="$HOME/.local/bin:$PATH"';

function ensureUnixPath(dir: string): boolean {
  // Already visible in this shell's PATH (e.g. ~/.profile default) — nothing to do.
  const inPath = (process.env.PATH ?? "").split(path.delimiter).includes(dir);
  if (inPath) return false;
  let changed = false;
  const home = os.homedir();
  for (const rc of [".bashrc", ".zshrc"]) {
    const rcPath = path.join(home, rc);
    try {
      if (existsSync(rcPath)) {
        const content = readFileSync(rcPath, "utf8");
        if (content.includes(".local/bin")) continue;
        writeFileSync(rcPath, content.replace(/\s*$/, "") + "\n" + RC_PATH_LINE + "\n", "utf8");
      } else if (rc === ".bashrc") {
        writeFileSync(rcPath, RC_PATH_LINE + "\n", "utf8");
      } else {
        continue; // no zshrc — don't create one
      }
      changed = true;
    } catch {
      // best effort
    }
  }
  return changed;
}

function ensureWindowsPath(dir: string): boolean {
  // Prefer the expandable literal when LOCALAPPDATA is the standard base.
  const literal = process.env.LOCALAPPDATA ? "%LOCALAPPDATA%\\clawmessenger" : dir;
  const query = spawnSync("reg", ["query", "HKCU\\Environment", "/v", "Path"], { encoding: "utf8" });
  const current = query.status === 0 ? parseRegPathQuery(query.stdout) : "";
  const next = nextPathValue(current, literal, "win32");
  if (next === current) return false;
  const add = spawnSync("reg", [
    "add", "HKCU\\Environment", "/v", "Path", "/t", "REG_EXPAND_SZ", "/d", next, "/f",
  ]);
  return add.status === 0;
}

export interface InstallResult {
  /** The binary was copied to the install directory this run. */
  copied: boolean;
  /** The user's PATH configuration was modified this run. */
  pathAdded: boolean;
  dir: string;
  target: string;
}

/**
 * Best-effort self-install. Returns null when not applicable (dev `node`
 * mode) or on unexpected failure — never throws, never blocks normal CLI
 * operation.
 */
export function ensureInstalled(): InstallResult | null {
  try {
    if (!isSeaExecutable(process.argv[1])) return null;
    const platform = process.platform;
    const paths = installPaths(platform, os.homedir());
    mkdirSync(paths.dir, { recursive: true });
    let copied = false;
    if (!samePath(process.execPath, paths.target, platform)) {
      const tmp = `${paths.target}.tmp-${process.pid}`;
      try {
        copyFileSync(process.execPath, tmp);
        try {
          renameSync(tmp, paths.target); // atomic-ish replace of an old install
        } catch {
          copyFileSync(process.execPath, paths.target);
        }
        copied = true;
      } catch (err) {
        // Target may be locked by a running instance; PATH is still ensured.
        process.stderr.write(
          `clawmessenger: could not self-install to ${paths.target}: ${err instanceof Error ? err.message : String(err)}\n`,
        );
      } finally {
        // A successful rename consumes tmp; every other exit path — including a
        // fallback copy that throws EBUSY on a locked target — must not leave a
        // full-size stray file next to the install.
        try {
          unlinkSync(tmp);
        } catch {
          // already renamed into place, or never created
        }
      }
    }
    const pathAdded = platform === "win32" ? ensureWindowsPath(paths.dir) : ensureUnixPath(paths.dir);
    return { copied, pathAdded, dir: paths.dir, target: paths.target };
  } catch {
    return null;
  }
}
