/**
 * Self-update for the single-file SEA executable.
 *
 * Why it works this way: Windows refuses to overwrite an executable that is
 * currently mapped by a running process (EBUSY), which is exactly why the old
 * best-effort copy in install.ts could never replace a live install. What
 * Windows *does* allow is renaming a running executable within the same volume.
 *
 * So the update is split into two halves:
 *
 *   1. stage  — download the release asset next to the install as
 *               `<target>.new-<version>`, verify it against SHA256SUMS.
 *   2. apply  — on the *next* process start, rename the live target aside to
 *               `<target>.old-<ts>` and rename the staged file into its place.
 *
 * The running process keeps executing the old image (already mapped), and the
 * next start picks up the new one. Nothing is deleted before the swap succeeds,
 * so a failure at any point leaves the install intact.
 */

import { createHash } from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  renameSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import * as path from "node:path";

/** GitHub repo that publishes the CLI releases. */
export const RELEASES_REPO = "clawmessenger/claw-messenger";

const STAGED_SUFFIX = ".new-";
const BACKUP_SUFFIX = ".old-";

/** Overridable so tests (and mirrors) can point at another endpoint. */
export function updateApiBase(): string {
  const custom = process.env.CLAWMESSENGER_UPDATE_API?.trim();
  return (custom || "https://api.github.com").replace(/\/+$/, "");
}

/**
 * Where release *assets* are served from. Asset downloads go through GitHub's
 * CDN and are not rate limited, which is what the fallback path relies on.
 */
export function updateWebBase(): string {
  const custom = process.env.CLAWMESSENGER_UPDATE_WEB?.trim();
  return (custom || "https://github.com").replace(/\/+$/, "");
}

/**
 * Optional token for the API lookup. Anonymous GitHub API calls are capped at
 * 60 requests/hour per IP, which a shared/NAT egress burns through quickly —
 * a token raises the limit, and the CDN fallback covers the rest.
 */
export function updateToken(): string {
  return (process.env.CLAWMESSENGER_UPDATE_TOKEN ?? process.env.GITHUB_TOKEN ?? "").trim();
}

/** Every asset name the release matrix publishes (besides SHA256SUMS). */
const RELEASE_ASSET_NAMES = [
  "clawmessenger-windows-x64.exe",
  "clawmessenger-windows-x64-daemon-scripts.zip",
  "clawmessenger-linux-x64",
  "clawmessenger-linux-arm64",
  "clawmessenger-linux-daemon-scripts.tar.gz",
  "clawmessenger-darwin-arm64",
  "SHA256SUMS",
];

export interface ReleaseAsset {
  name: string;
  url: string;
  size: number;
}

export interface ReleaseInfo {
  version: string;
  tag: string;
  assets: ReleaseAsset[];
  /**
   * `api`  — resolved through the GitHub REST API.
   * `cdn`  — the API was unavailable (typically 403 rate limit); the tag was
   *          recovered from the `releases/latest/download/...` redirect.
   */
  source?: "api" | "cdn";
}

/** `clawmessenger-v1.2.3` -> `1.2.3`; leaves odd tags untouched. */
export function versionFromTag(tag: string): string {
  const semver = tag.match(/(\d+\.\d+\.\d+)/);
  if (semver) return semver[1];
  return tag.replace(/^clawmessenger-v?/, "");
}

/** Compare dotted versions, ignoring any `-prerelease` suffix. -1 | 0 | 1. */
export function compareVersions(a: string, b: string): number {
  const split = (v: string): number[] =>
    v.split("-")[0].split(".").map((n) => Number.parseInt(n, 10) || 0);
  const pa = split(a);
  const pb = split(b);
  for (let i = 0; i < 3; i += 1) {
    const x = pa[i] ?? 0;
    const y = pb[i] ?? 0;
    if (x !== y) return x > y ? 1 : -1;
  }
  return 0;
}

/**
 * Release asset name for the running platform. Mirrors the CI release matrix;
 * returns "" for combinations the matrix does not build.
 */
export function assetNameForPlatform(platform: string, arch: string): string {
  if (platform === "win32") return arch === "x64" ? "clawmessenger-windows-x64.exe" : "";
  if (platform === "darwin") return arch === "arm64" ? "clawmessenger-darwin-arm64" : "";
  if (platform === "linux") {
    if (arch === "arm64") return "clawmessenger-linux-arm64";
    if (arch === "x64") return "clawmessenger-linux-x64";
  }
  return "";
}

type FetchLike = (url: string, init?: {
  headers?: Record<string, string>;
  redirect?: "follow" | "manual" | "error";
}) => Promise<{
  ok: boolean;
  status: number;
  headers?: { get(name: string): string | null };
  json: () => Promise<unknown>;
  arrayBuffer: () => Promise<ArrayBuffer>;
  text?: () => Promise<string>;
}>;

function apiHeaders(): Record<string, string> {
  const headers: Record<string, string> = {
    "User-Agent": "clawmessenger-cli",
    Accept: "application/vnd.github+json",
  };
  const token = updateToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  return headers;
}

/**
 * Recover the latest tag without touching the REST API: `releases/latest/download`
 * answers with a 302 whose Location embeds the tag. Costs no API quota.
 */
export async function resolveLatestTagViaCdn(
  fetchImpl: FetchLike = fetch as unknown as FetchLike,
  probeAsset = "SHA256SUMS",
): Promise<string> {
  const res = await fetchImpl(
    `${updateWebBase()}/${RELEASES_REPO}/releases/latest/download/${probeAsset}`,
    { headers: { "User-Agent": "clawmessenger-cli" }, redirect: "manual" },
  );
  const location = res.headers?.get?.("location") ?? "";
  const tag = location.match(/\/releases\/download\/([^/]+)\//)?.[1] ?? "";
  if (!tag) throw new Error(`could not resolve the latest tag from the release CDN (HTTP ${res.status})`);
  return tag;
}

/**
 * Look up the latest release. Tries the API first, then falls back to the
 * unauthenticated CDN redirect so a rate-limited or token-less machine can
 * still update.
 */
export async function fetchLatestRelease(
  fetchImpl: FetchLike = fetch as unknown as FetchLike,
  platform: string = process.platform,
  arch: string = process.arch,
): Promise<ReleaseInfo> {
  let apiFailure = "";
  try {
    const res = await fetchImpl(`${updateApiBase()}/repos/${RELEASES_REPO}/releases/latest`, {
      headers: apiHeaders(),
    });
    if (res.ok) {
      const body = (await res.json()) as {
        tag_name?: string;
        assets?: { name?: string; browser_download_url?: string; size?: number }[];
      };
      const assets: ReleaseAsset[] = (body.assets ?? [])
        .filter((a) => typeof a.name === "string" && typeof a.browser_download_url === "string")
        .map((a) => ({ name: a.name as string, url: a.browser_download_url as string, size: a.size ?? 0 }));
      const tag = body.tag_name ?? "";
      return { version: versionFromTag(tag), tag, assets, source: "api" };
    }
    apiFailure = `HTTP ${res.status}`;
  } catch (err) {
    apiFailure = err instanceof Error ? err.message : String(err);
  }

  // API unavailable — the CDN path needs no quota and no token.
  const assetName = assetNameForPlatform(platform, arch);
  let tag: string;
  try {
    tag = await resolveLatestTagViaCdn(fetchImpl, assetName || "SHA256SUMS");
  } catch (err) {
    const fallback = err instanceof Error ? err.message : String(err);
    throw new Error(`release lookup failed: API ${apiFailure}; ${fallback}`);
  }
  const assets: ReleaseAsset[] = RELEASE_ASSET_NAMES.map((name) => ({
    name,
    url: `${updateWebBase()}/${RELEASES_REPO}/releases/download/${tag}/${name}`,
    size: 0,
  }));
  return { version: versionFromTag(tag), tag, assets, source: "cdn" };
}

/** Parse `SHA256SUMS` (`<hex>  <name>` per line) into a name -> hex map. */
export function parseSha256Sums(text: string): Map<string, string> {
  const sums = new Map<string, string>();
  for (const line of text.split(/\r?\n/)) {
    const m = line.trim().match(/^([0-9a-f]{64})\s+\*?(.+)$/i);
    if (m) sums.set(m[2].trim(), m[1].toLowerCase());
  }
  return sums;
}

export function sha256File(file: string): string {
  return createHash("sha256").update(readFileSync(file)).digest("hex");
}

export function stagedPath(target: string, version: string): string {
  return `${target}${STAGED_SUFFIX}${version}`;
}

/** Staged updates sitting next to the install, newest version first. */
export function findStaged(target: string): { file: string; version: string }[] {
  const dir = path.dirname(target);
  const base = path.basename(target);
  let entries: string[] = [];
  try {
    entries = readdirSync(dir);
  } catch {
    return [];
  }
  return entries
    .filter((f) => f.startsWith(`${base}${STAGED_SUFFIX}`))
    .map((f) => ({ file: path.join(dir, f), version: f.slice(`${base}${STAGED_SUFFIX}`.length) }))
    .filter((s) => s.version.length > 0)
    .sort((a, b) => compareVersions(b.version, a.version));
}

export interface StageResult {
  version: string;
  asset: string;
  path: string;
  bytes: number;
}

/**
 * Download and verify a release asset, then leave it staged next to the
 * install. The live executable is never touched here.
 */
export async function stageUpdate(
  release: ReleaseInfo,
  target: string,
  platform: string,
  arch: string,
  fetchImpl: FetchLike = fetch as unknown as FetchLike,
): Promise<StageResult> {
  const assetName = assetNameForPlatform(platform, arch);
  if (!assetName) throw new Error(`no release asset for ${platform}/${arch}`);

  const asset = release.assets.find((a) => a.name === assetName);
  if (!asset) throw new Error(`release ${release.tag} has no asset named ${assetName}`);

  const sumsAsset = release.assets.find((a) => a.name === "SHA256SUMS");
  if (!sumsAsset) throw new Error(`release ${release.tag} has no SHA256SUMS; refusing to install unverified binary`);

  const sumsRes = await fetchImpl(sumsAsset.url, { headers: { "User-Agent": "clawmessenger-cli" } });
  if (!sumsRes.ok) throw new Error(`SHA256SUMS download failed: HTTP ${sumsRes.status}`);
  const sums = parseSha256Sums(Buffer.from(await sumsRes.arrayBuffer()).toString("utf8"));
  const expected = sums.get(assetName);
  if (!expected) throw new Error(`SHA256SUMS does not list ${assetName}`);

  const binRes = await fetchImpl(asset.url, { headers: { "User-Agent": "clawmessenger-cli" } });
  if (!binRes.ok) throw new Error(`download failed: HTTP ${binRes.status}`);
  const bytes = Buffer.from(await binRes.arrayBuffer());

  const actual = createHash("sha256").update(bytes).digest("hex");
  if (actual !== expected) {
    throw new Error(`checksum mismatch for ${assetName}: expected ${expected}, got ${actual}`);
  }

  const dest = stagedPath(target, release.version);
  mkdirSync(path.dirname(dest), { recursive: true });
  // Write to a partial name first so a crash mid-write cannot look like a
  // valid staged update.
  const partial = `${dest}.part`;
  writeFileSync(partial, bytes);
  renameSync(partial, dest);
  if (platform !== "win32") {
    try {
      chmodSync(dest, 0o755);
    } catch {
      // best effort; the apply step retries the mode
    }
  }
  return { version: release.version, asset: assetName, path: dest, bytes: bytes.length };
}

export interface ApplyResult {
  applied: boolean;
  version?: string;
  reason?: string;
}

/**
 * Swap a staged update into place. Safe to call on every start: no-op when
 * nothing is staged. The previous binary is kept as `<target>.old-<ts>` until a
 * later start cleans it up.
 */
export function applyStagedUpdate(target: string, platform: string): ApplyResult {
  const staged = findStaged(target)[0];
  if (!staged) return { applied: false };

  const backup = `${target}${BACKUP_SUFFIX}${Date.now()}`;
  let movedAside = false;
  try {
    // Renaming a running executable is allowed on Windows (same volume); the
    // process keeps executing the old image it already mapped.
    if (existsSync(target)) {
      renameSync(target, backup);
      movedAside = true;
    }
    renameSync(staged.file, target);
    if (platform !== "win32") {
      try {
        chmodSync(target, 0o755);
      } catch {
        // best effort
      }
    }
    return { applied: true, version: staged.version };
  } catch (err) {
    // Put the old binary back so the install is never left without an exe.
    if (movedAside && !existsSync(target)) {
      try {
        renameSync(backup, target);
      } catch {
        // nothing more we can do; surface the original failure
      }
    }
    try {
      if (existsSync(staged.file)) unlinkSync(staged.file);
    } catch {
      // keep it; a later start may succeed
    }
    return { applied: false, reason: err instanceof Error ? err.message : String(err) };
  }
}

/**
 * Delete stale `<target>.old-*` backups, keeping the most recent one. The
 * currently running image cannot be deleted on Windows, so it simply survives
 * until a later start.
 */
export function cleanupStaleBackups(target: string, keep = 1): string[] {
  const dir = path.dirname(target);
  const base = path.basename(target);
  let entries: string[] = [];
  try {
    entries = readdirSync(dir);
  } catch {
    return [];
  }
  const olds = entries
    .filter((f) => f.startsWith(`${base}${BACKUP_SUFFIX}`))
    .sort()
    .reverse()
    .slice(keep);
  const removed: string[] = [];
  for (const f of olds) {
    try {
      unlinkSync(path.join(dir, f));
      removed.push(f);
    } catch {
      // locked by a running process — retried on a later start
    }
  }
  return removed;
}

export interface UpdatePlan {
  currentVersion: string;
  latestVersion: string;
  updateAvailable: boolean;
  assetName: string;
  release: ReleaseInfo;
}

/** Look up the latest release and decide whether it is newer than `current`. */
export async function checkForUpdate(
  currentVersion: string,
  platform: string,
  arch: string,
  fetchImpl: FetchLike = fetch as unknown as FetchLike,
): Promise<UpdatePlan> {
  const release = await fetchLatestRelease(fetchImpl, platform, arch);
  return {
    currentVersion,
    latestVersion: release.version,
    updateAvailable: compareVersions(release.version, currentVersion) > 0,
    assetName: assetNameForPlatform(platform, arch),
    release,
  };
}
