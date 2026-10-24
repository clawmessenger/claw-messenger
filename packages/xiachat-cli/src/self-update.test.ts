import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  applyStagedUpdate,
  assetNameForPlatform,
  checkForUpdate,
  cleanupStaleBackups,
  compareVersions,
  findStaged,
  parseSha256Sums,
  stageUpdate,
  stagedPath,
  versionFromTag,
  type ReleaseInfo,
} from "./self-update.js";

let dir: string;
let target: string;

beforeEach(() => {
  dir = mkdtempSync(path.join(os.tmpdir(), "cm-update-"));
  target = path.join(dir, "clawmessenger.exe");
  writeFileSync(target, "OLD-BINARY");
});

afterEach(() => {
  rmSync(dir, { recursive: true, force: true });
});

/** Backups parked next to the install by applyStagedUpdate. */
function readBackups(): string[] {
  if (!existsSync(dir)) return [];
  return readdirSync(dir).filter((f) => f.startsWith("clawmessenger.exe.old-"));
}

const sha256 = (buf: Buffer): string => createHash("sha256").update(buf).digest("hex");

/** Minimal fetch stub: JSON for the API call, raw bytes for asset downloads. */
function fetchStub(opts: {
  release: unknown;
  files: Record<string, Buffer>;
  sums?: string;
  apiStatus?: number;
}) {
  return (async (url: string) => {
    if (url.endsWith("/releases/latest")) {
      const status = opts.apiStatus ?? 200;
      return {
        ok: status === 200,
        status,
        json: async () => opts.release,
        arrayBuffer: async () => new ArrayBuffer(0),
      };
    }
    const name = url.split("/").pop() ?? "";
    if (name === "SHA256SUMS") {
      const body = Buffer.from(opts.sums ?? "");
      return { ok: true, status: 200, json: async () => ({}), arrayBuffer: async () => body.buffer.slice(body.byteOffset, body.byteOffset + body.byteLength) as ArrayBuffer };
    }
    const file = opts.files[name];
    if (!file) {
      return { ok: false, status: 404, json: async () => ({}), arrayBuffer: async () => new ArrayBuffer(0) };
    }
    return { ok: true, status: 200, json: async () => ({}), arrayBuffer: async () => file.buffer.slice(file.byteOffset, file.byteOffset + file.byteLength) as ArrayBuffer };
  }) as never;
}

describe("version helpers", () => {
  it("extracts a semver from the release tag", () => {
    expect(versionFromTag("clawmessenger-v1.2.3")).toBe("1.2.3");
    expect(versionFromTag("v0.2.54")).toBe("0.2.54");
  });

  it("compares dotted versions numerically, not lexically", () => {
    expect(compareVersions("1.10.0", "1.9.9")).toBe(1);
    expect(compareVersions("1.0.0", "1.0.0")).toBe(0);
    expect(compareVersions("0.2.52", "1.0.0")).toBe(-1);
    expect(compareVersions("2.0.0-rc1", "2.0.0")).toBe(0);
  });

  it("maps platform/arch onto the CI release matrix", () => {
    expect(assetNameForPlatform("win32", "x64")).toBe("clawmessenger-windows-x64.exe");
    expect(assetNameForPlatform("linux", "arm64")).toBe("clawmessenger-linux-arm64");
    expect(assetNameForPlatform("darwin", "arm64")).toBe("clawmessenger-darwin-arm64");
    // The matrix does not build these; refusing beats downloading a wrong binary.
    expect(assetNameForPlatform("win32", "arm64")).toBe("");
    expect(assetNameForPlatform("darwin", "x64")).toBe("");
  });

  it("parses SHA256SUMS regardless of alignment or binary markers", () => {
    const a = "a".repeat(64);
    const b = "b".repeat(64);
    const sums = parseSha256Sums(`${a}  file-one\n${b} *file-two\n\nnot-a-line\n`);
    expect(sums.get("file-one")).toBe(a);
    expect(sums.get("file-two")).toBe(b);
    expect(sums.size).toBe(2);
  });
});

describe("stageUpdate", () => {
  const NEW = Buffer.from("NEW-BINARY");
  const release: ReleaseInfo = {
    version: "1.1.0",
    tag: "clawmessenger-v1.1.0",
    assets: [
      { name: "clawmessenger-windows-x64.exe", url: "https://example.test/clawmessenger-windows-x64.exe", size: NEW.length },
      { name: "SHA256SUMS", url: "https://example.test/SHA256SUMS", size: 100 },
    ],
  };

  it("stages a verified download next to the install and leaves the live exe alone", async () => {
    const result = await stageUpdate(
      release,
      target,
      "win32",
      "x64",
      fetchStub({
        release,
        files: { "clawmessenger-windows-x64.exe": NEW },
        sums: `${sha256(NEW)}  clawmessenger-windows-x64.exe\n`,
      }),
    );

    expect(result.version).toBe("1.1.0");
    expect(result.path).toBe(stagedPath(target, "1.1.0"));
    expect(readFileSync(result.path)).toEqual(NEW);
    // Critical: staging must not touch the running install.
    expect(readFileSync(target, "utf8")).toBe("OLD-BINARY");
    expect(existsSync(`${result.path}.part`)).toBe(false);
  });

  it("rejects a checksum mismatch and leaves nothing behind", async () => {
    await expect(
      stageUpdate(
        release,
        target,
        "win32",
        "x64",
        fetchStub({
          release,
          files: { "clawmessenger-windows-x64.exe": NEW },
          sums: `${"0".repeat(64)}  clawmessenger-windows-x64.exe\n`,
        }),
      ),
    ).rejects.toThrow(/checksum mismatch/);

    expect(findStaged(target)).toHaveLength(0);
    expect(readFileSync(target, "utf8")).toBe("OLD-BINARY");
  });

  it("refuses to install when the release ships no SHA256SUMS", async () => {
    const noSums: ReleaseInfo = { ...release, assets: [release.assets[0]] };
    await expect(
      stageUpdate(noSums, target, "win32", "x64", fetchStub({ release: noSums, files: { "clawmessenger-windows-x64.exe": NEW } })),
    ).rejects.toThrow(/SHA256SUMS/);
    expect(findStaged(target)).toHaveLength(0);
  });

  it("refuses an architecture the release matrix does not build", async () => {
    await expect(
      stageUpdate(release, target, "win32", "arm64", fetchStub({ release, files: {} })),
    ).rejects.toThrow(/no release asset/);
  });
});

describe("applyStagedUpdate", () => {
  it("is a no-op when nothing is staged", () => {
    expect(applyStagedUpdate(target, "win32")).toEqual({ applied: false });
    expect(readFileSync(target, "utf8")).toBe("OLD-BINARY");
  });

  it("swaps the staged file into place and keeps the old binary as a backup", () => {
    const staged = stagedPath(target, "1.1.0");
    writeFileSync(staged, "NEW-BINARY");

    const result = applyStagedUpdate(target, "win32");

    expect(result.applied).toBe(true);
    expect(result.version).toBe("1.1.0");
    expect(readFileSync(target, "utf8")).toBe("NEW-BINARY");
    expect(existsSync(staged)).toBe(false);
    // The previous image is parked, not deleted — the running process still maps it.
    const backups = readBackups();
    expect(backups).toHaveLength(1);
    expect(readFileSync(path.join(dir, backups[0]), "utf8")).toBe("OLD-BINARY");
  });

  it("prefers the highest staged version when several are present", () => {
    writeFileSync(stagedPath(target, "1.1.0"), "NEW-110");
    writeFileSync(stagedPath(target, "1.2.0"), "NEW-120");

    expect(applyStagedUpdate(target, "win32").version).toBe("1.2.0");
    expect(readFileSync(target, "utf8")).toBe("NEW-120");
  });

  it("creates the install when it does not exist yet", () => {
    rmSync(target);
    writeFileSync(stagedPath(target, "1.1.0"), "NEW-BINARY");

    expect(applyStagedUpdate(target, "win32").applied).toBe(true);
    expect(readFileSync(target, "utf8")).toBe("NEW-BINARY");
  });
});

describe("cleanupStaleBackups", () => {
  it("keeps only the newest backup", () => {
    mkdirSync(dir, { recursive: true });
    for (const ts of ["1000", "2000", "3000"]) {
      writeFileSync(`${target}.old-${ts}`, `old-${ts}`);
    }

    const removed = cleanupStaleBackups(target, 1);

    expect(removed.sort()).toEqual(["clawmessenger.exe.old-1000", "clawmessenger.exe.old-2000"]);
    expect(readBackups()).toEqual(["clawmessenger.exe.old-3000"]);
  });
});

describe("checkForUpdate", () => {
  it("flags a newer release and reports the asset for this platform", async () => {
    const plan = await checkForUpdate(
      "1.0.0",
      "win32",
      "x64",
      fetchStub({
        release: { tag_name: "clawmessenger-v1.1.0", assets: [{ name: "clawmessenger-windows-x64.exe", browser_download_url: "https://example.test/x", size: 1 }] },
        files: {},
      }),
    );

    expect(plan.latestVersion).toBe("1.1.0");
    expect(plan.updateAvailable).toBe(true);
    expect(plan.assetName).toBe("clawmessenger-windows-x64.exe");
  });

  it("does not flag the same version, or an older one", async () => {
    const same = await checkForUpdate("1.1.0", "win32", "x64", fetchStub({ release: { tag_name: "clawmessenger-v1.1.0", assets: [] }, files: {} }));
    expect(same.updateAvailable).toBe(false);

    const older = await checkForUpdate("1.2.0", "win32", "x64", fetchStub({ release: { tag_name: "clawmessenger-v1.1.0", assets: [] }, files: {} }));
    expect(older.updateAvailable).toBe(false);
  });

  it("surfaces an API failure instead of silently reporting up-to-date", async () => {
    await expect(
      checkForUpdate("1.0.0", "win32", "x64", fetchStub({ release: {}, files: {}, apiStatus: 503 })),
    ).rejects.toThrow(/HTTP 503/);
  });
});

/**
 * The API path is quota-limited (60 req/hour per IP anonymously), which a
 * shared egress IP burns through instantly. These tests pin the fallback: the
 * tag is recovered from the `releases/latest/download` redirect, and the
 * checksum gate stays exactly as strict.
 */
function cdnStub(opts: {
  apiStatus: number;
  tag?: string;
  files?: Record<string, Buffer>;
  sums?: string;
  captured?: { url: string; headers?: Record<string, string>; redirect?: string }[];
}) {
  return (async (url: string, init?: { headers?: Record<string, string>; redirect?: string }) => {
    opts.captured?.push({ url, headers: init?.headers, redirect: init?.redirect });
    const empty = async () => new ArrayBuffer(0);
    if (url.endsWith("/releases/latest")) {
      const ok = opts.apiStatus === 200;
      return {
        ok,
        status: opts.apiStatus,
        headers: { get: () => null },
        json: async () => (ok ? { tag_name: opts.tag ?? "", assets: [] } : {}),
        arrayBuffer: empty,
      };
    }
    if (url.includes("/releases/latest/download/")) {
      const loc = opts.tag
        ? `https://github.com/clawmessenger/claw-messenger/releases/download/${opts.tag}/SHA256SUMS`
        : "";
      return {
        ok: false,
        status: 302,
        headers: { get: (n: string) => (n.toLowerCase() === "location" ? loc : null) },
        json: async () => ({}),
        arrayBuffer: empty,
      };
    }
    const name = url.split("/").pop() ?? "";
    if (name === "SHA256SUMS") {
      const body = Buffer.from(opts.sums ?? "");
      return { ok: true, status: 200, json: async () => ({}), arrayBuffer: async () => body.buffer.slice(body.byteOffset, body.byteOffset + body.byteLength) as ArrayBuffer };
    }
    const file = opts.files?.[name];
    if (!file) return { ok: false, status: 404, json: async () => ({}), arrayBuffer: empty };
    return { ok: true, status: 200, json: async () => ({}), arrayBuffer: async () => file.buffer.slice(file.byteOffset, file.byteOffset + file.byteLength) as ArrayBuffer };
  }) as never;
}

describe("release lookup fallback", () => {
  const saved = { ...process.env };
  afterEach(() => {
    delete process.env.GITHUB_TOKEN;
    delete process.env.CLAWMESSENGER_UPDATE_TOKEN;
    process.env = { ...saved };
  });

  it("recovers the version from the CDN when the API is rate limited", async () => {
    const plan = await checkForUpdate(
      "1.1.0", "win32", "x64",
      cdnStub({ apiStatus: 403, tag: "clawmessenger-v1.2.0" }),
    );
    expect(plan.release.source).toBe("cdn");
    expect(plan.release.tag).toBe("clawmessenger-v1.2.0");
    expect(plan.latestVersion).toBe("1.2.0");
    expect(plan.updateAvailable).toBe(true);
  });

  it("builds asset URLs from the recovered tag", async () => {
    const plan = await checkForUpdate(
      "1.1.0", "win32", "x64",
      cdnStub({ apiStatus: 403, tag: "clawmessenger-v1.2.0" }),
    );
    const exe = plan.release.assets.find((a) => a.name === plan.assetName);
    expect(exe?.url).toBe(
      "https://github.com/clawmessenger/claw-messenger/releases/download/clawmessenger-v1.2.0/clawmessenger-windows-x64.exe",
    );
    expect(plan.release.assets.some((a) => a.name === "SHA256SUMS")).toBe(true);
  });

  it("never touches the CDN when the API answers", async () => {
    const captured: { url: string }[] = [];
    const plan = await checkForUpdate(
      "1.1.0", "win32", "x64",
      fetchStub({ release: { tag_name: "clawmessenger-v1.2.0", assets: [] }, files: {} }),
    );
    expect(plan.release.source).toBe("api");
    expect(captured).toHaveLength(0);
  });

  it("sends the configured token to the API", async () => {
    process.env.CLAWMESSENGER_UPDATE_TOKEN = "t0ken-abc";
    const captured: { url: string; headers?: Record<string, string> }[] = [];
    await checkForUpdate("1.1.0", "win32", "x64", cdnStub({ apiStatus: 200, tag: "clawmessenger-v1.2.0", captured }));
    const apiCall = captured.find((c) => c.url.endsWith("/releases/latest"));
    expect(apiCall?.headers?.Authorization).toBe("Bearer t0ken-abc");
  });

  it("still rejects a checksum mismatch on the CDN path", async () => {
    const bin = Buffer.from("REAL-BINARY");
    const plan = await checkForUpdate(
      "1.1.0", "win32", "x64",
      cdnStub({ apiStatus: 403, tag: "clawmessenger-v1.2.0", files: { "clawmessenger-windows-x64.exe": bin } }),
    );
    await expect(
      stageUpdate(plan.release, target, "win32", "x64",
        cdnStub({
          apiStatus: 403, tag: "clawmessenger-v1.2.0",
          files: { "clawmessenger-windows-x64.exe": bin },
          sums: `${"0".repeat(64)}  clawmessenger-windows-x64.exe\n`,
        })),
    ).rejects.toThrow(/checksum mismatch/i);
    expect(existsSync(stagedPath(target, "1.2.0"))).toBe(false);
  });

  it("stages a verified download on the CDN path", async () => {
    const bin = Buffer.from("REAL-BINARY");
    const plan = await checkForUpdate(
      "1.1.0", "win32", "x64",
      cdnStub({ apiStatus: 403, tag: "clawmessenger-v1.2.0", files: { "clawmessenger-windows-x64.exe": bin } }),
    );
    const stub = cdnStub({
      apiStatus: 403, tag: "clawmessenger-v1.2.0",
      files: { "clawmessenger-windows-x64.exe": bin },
      sums: `${sha256(bin)}  clawmessenger-windows-x64.exe\n`,
    });
    const staged = await stageUpdate(plan.release, target, "win32", "x64", stub);
    expect(staged.version).toBe("1.2.0");
    expect(readFileSync(stagedPath(target, "1.2.0"))).toEqual(bin);
  });

  it("names both failures when the CDN cannot resolve a tag either", async () => {
    await expect(
      checkForUpdate("1.1.0", "win32", "x64", cdnStub({ apiStatus: 403 })),
    ).rejects.toThrow(/API HTTP 403[\s\S]*latest tag/i);
  });
});
