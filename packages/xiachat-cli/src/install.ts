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

/** True when running as a single-file SEA executable (not `node bundle.js`). */
export function isSeaExecutable(argv1: string | undefined): boolean {
  return !argv1 || !argv1.endsWith(".js");
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
          unlinkSync(tmp);
        }
        copied = true;
      } catch (err) {
        // Target may be locked by a running instance; PATH is still ensured.
        process.stderr.write(
          `clawmessenger: could not self-install to ${paths.target}: ${err instanceof Error ? err.message : String(err)}\n`,
        );
      }
    }
    const pathAdded = platform === "win32" ? ensureWindowsPath(paths.dir) : ensureUnixPath(paths.dir);
    return { copied, pathAdded, dir: paths.dir, target: paths.target };
  } catch {
    return null;
  }
}
