import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
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
  DIRECT_CONNECT_TIMEOUT_MS,
  DIRECT_TRANSFER_TIMEOUT_MS,
  findStaged,
  healInstall,
  mirrorTimeoutPlan,
  parseSha256Sums,
  PROXY_MIRROR_TIMEOUT_MS,
  stageUpdate,
  stageUpdateFromMirrors,
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

// ---- 断点自愈（升级交换两步 rename 中间掉电的恢复） -------------------------
describe("healInstall", () => {
  it("restores the target from the newest backup when the swap was interrupted", () => {
    // 交换的第一步已经做完、第二步没做：target 不见了，只剩 .old-<ts>。
    rmSync(target, { force: true });
    writeFileSync(`${target}.old-1000`, "OLD-1000");
    writeFileSync(`${target}.old-2000`, "OLD-2000");

    const healed = healInstall(target);

    expect(healed.healed).toBe(true);
    expect(readFileSync(target, "utf8")).toBe("OLD-2000");
  });

  it("prefers a staged file over a backup (it is the version we meant to install)", () => {
    rmSync(target, { force: true });
    writeFileSync(`${target}.old-1000`, "OLD");
    writeFileSync(stagedPath(target, "1.3.0"), "NEW");

    const healed = healInstall(target);

    expect(healed.healed).toBe(true);
    expect(readFileSync(target, "utf8")).toBe("NEW");
    // 暂存件被消费掉了，不应该再被 applyStagedUpdate 交换一次。
    expect(findStaged(target)).toHaveLength(0);
  });

  it("does nothing when the install is intact", () => {
    expect(healInstall(target)).toEqual({ healed: false });
    expect(readFileSync(target, "utf8")).toBe("OLD-BINARY");
  });

  it("reports failure when there is nothing to restore", () => {
    rmSync(target, { force: true });
    expect(healInstall(target)).toEqual({ healed: false });
    expect(existsSync(target)).toBe(false);
  });

  it("ignores an empty staged file and falls back to the backup", () => {
    // 0 字节暂存件（磁盘写满 / 写入被截断）rename 成 target 之后设备就再也起不来
    // 了 —— 而旁边就躺着一份能用的备份。宁可回滚到旧版，也不能装一个空文件。
    rmSync(target, { force: true });
    writeFileSync(`${target}.old-1000`, "OLD-WORKING");
    writeFileSync(stagedPath(target, "1.3.0"), "");

    const healed = healInstall(target);

    expect(healed.healed).toBe(true);
    expect(readFileSync(target, "utf8")).toBe("OLD-WORKING");
  });
});

// ---- 按 mirrors 下载（GitHub 直连失败 -> 服务端代理） ----------------------
describe("stageUpdateFromMirrors", () => {
  const binary = Buffer.from("BRAND-NEW-EXE");
  const sum = (buf: Buffer): string => createHash("sha256").update(buf).digest("hex");

  const mirrorFetch = (handlers: Record<string, Buffer | Error>) =>
    (async (url: string) => {
      const handler = handlers[url];
      if (!handler) throw new Error(`unexpected url ${url}`);
      if (handler instanceof Error) throw handler;
      return { ok: true, status: 200, arrayBuffer: async () => handler } as never;
    }) as never;

  it("falls back to the server proxy and resolves its relative path against serverUrl", async () => {
    const plan = {
      version: "1.3.0",
      assetName: "clawmessenger-windows-x64.exe",
      sha256: sum(binary),
      mirrors: [
        "https://github.com/clawmessenger/claw-messenger/releases/download/clawmessenger-v1.3.0/clawmessenger-windows-x64.exe",
        "/api/agent/upgrade/download?version=1.3.0&asset=clawmessenger-windows-x64.exe&token=t",
      ],
    };
    const fetchImpl = mirrorFetch({
      [plan.mirrors[0]]: new Error("connect ETIMEDOUT"),
      [`https://srv.example.com${plan.mirrors[1]}`]: binary,
    });

    const staged = await stageUpdateFromMirrors(plan, target, "https://srv.example.com", fetchImpl as never);

    expect(staged.version).toBe("1.3.0");
    expect(staged.source).toBe(`https://srv.example.com${plan.mirrors[1]}`);
    expect(readFileSync(staged.path, "utf8")).toBe("BRAND-NEW-EXE");
  });

  it("rejects bytes whose sha256 does not match and leaves nothing staged", async () => {
    const plan = {
      version: "1.3.0",
      assetName: "clawmessenger-windows-x64.exe",
      sha256: sum(Buffer.from("what-we-expected")),
      mirrors: ["https://example.test/binary"],
    };
    const fetchImpl = mirrorFetch({ "https://example.test/binary": binary });

    await expect(
      stageUpdateFromMirrors(plan, target, "https://srv.example.com", fetchImpl as never),
    ).rejects.toThrow(/checksum mismatch/);
    expect(findStaged(target)).toHaveLength(0);
  });

  it("refuses to install when the server supplied no checksum", async () => {
    const plan = {
      version: "1.3.0",
      assetName: "clawmessenger-windows-x64.exe",
      sha256: "",
      mirrors: ["https://example.test/binary"],
    };
    const fetchImpl = mirrorFetch({ "https://example.test/binary": binary });

    await expect(
      stageUpdateFromMirrors(plan, target, "https://srv.example.com", fetchImpl as never),
    ).rejects.toThrow(/no checksum supplied/);
  });

  // 超时必须按**阶段**分开，而不是给整个下载一个数字。两个方向都会出事：
  //   · 全程套 15 秒 —— 88MB 要在 15 秒内传完（≥47Mbps），企业出口到 GitHub 通常
  //     只有 1–10MB/s，于是「服务端没外网时直连是唯一活路」这条兜底必然失败；
  //   · 全程套 15 分钟 —— 受限出口打到 GitHub 是黑洞，页面在「下载中」躺一刻钟。
  // 代理则只有一个总墙钟，且必须长于服务端回源（先落盘再发）的 10 分钟。
  it("budgets the direct mirror in two phases and the server proxy as one long wall clock", () => {
    expect(DIRECT_CONNECT_TIMEOUT_MS).toBe(15_000);
    expect(DIRECT_TRANSFER_TIMEOUT_MS).toBe(600_000);
    expect(PROXY_MIRROR_TIMEOUT_MS).toBe(900_000);

    const direct = mirrorTimeoutPlan(
      "https://github.com/clawmessenger/claw-messenger/releases/download/x/y.exe",
    );
    expect(direct.connectMs).toBe(DIRECT_CONNECT_TIMEOUT_MS);
    expect(direct.totalMs).toBe(DIRECT_TRANSFER_TIMEOUT_MS);
    // 传输预算必须比连接预算大得多，否则「连接 15 秒」会顺带把 body 也掐掉。
    expect(direct.totalMs).toBeGreaterThan(direct.connectMs * 10);

    // 服务端代理是相对路径：设备用自己 serverUrl 拼，仍然按「代理」的预算走，
    // 且比服务端 client.Timeout（10 分钟）长 —— 否则冷缓存时服务端还在拉，
    // 设备先把自己掐了。
    const proxy = mirrorTimeoutPlan("/api/agent/upgrade/download?version=1.3.0&asset=x&token=t");
    expect(proxy.connectMs).toBe(PROXY_MIRROR_TIMEOUT_MS);
    expect(proxy.totalMs).toBe(PROXY_MIRROR_TIMEOUT_MS);
    expect(proxy.totalMs).toBeGreaterThan(10 * 60_000);
  });

  // 受限出口的现场：直连那一跳既不拒绝也不返回，唯一让它结束的办法是我们自己
  // 掐断。掐断的异常必须计入 failures 并继续下一个源，否则整次升级就停在「下载
  // 中」，服务端那边什么都看不到。
  it("aborts a black-holed direct mirror and falls back to the proxy", async () => {
    const plan = {
      version: "1.3.0",
      assetName: "clawmessenger-windows-x64.exe",
      sha256: sum(binary),
      mirrors: [
        "https://github.com/clawmessenger/claw-messenger/releases/download/clawmessenger-v1.3.0/clawmessenger-windows-x64.exe",
        "/api/agent/upgrade/download?version=1.3.0&asset=clawmessenger-windows-x64.exe&token=t",
      ],
    };
    const seen: { url: string; signal?: AbortSignal }[] = [];
    const fetchImpl = (async (url: string, init?: { signal?: AbortSignal }) => {
      seen.push({ url, signal: init?.signal });
      if (url === plan.mirrors[0]) {
        // 与实现里 `controller.abort()` 触发的形状一致：name=AbortError。
        // 注意别跟 `AbortSignal.timeout()` 搞混 —— 后者抛的是 name=TimeoutError
        // （实测 Node 20+/22 都是），所以实现不能按 name 判断「是不是超时」，
        // 只能按我们自己记的阶段判断（见 stageUpdateFromMirrors 的 catch）。
        const abort = new Error("This operation was aborted");
        abort.name = "AbortError";
        throw abort;
      }
      return { ok: true, status: 200, arrayBuffer: async () => binary } as never;
    }) as never;

    const staged = await stageUpdateFromMirrors(plan, target, "https://srv.example.com", fetchImpl as never);

    expect(staged.version).toBe("1.3.0");
    expect(staged.source).toBe(`https://srv.example.com${plan.mirrors[1]}`);
    expect(readFileSync(staged.path, "utf8")).toBe("BRAND-NEW-EXE");
    // 两个源都必须拿到 signal —— 少了它，超时只是注释里的一句话。
    expect(seen.map((call) => call.url)).toEqual([
      plan.mirrors[0],
      `https://srv.example.com${plan.mirrors[1]}`,
    ]);
    for (const call of seen) expect(call.signal).toBeInstanceOf(AbortSignal);
  });

  // 每一个源都失败时，文案必须点名**每个源**、**失败发生在哪一阶段**、以及**是不是
  // 我们掐的**：设备日志里只留一句 "fetch failed" 的话，运维分不清是连不上（换出口）
  // 还是传不完（预热缓存 / 等更久）。
  //
  // 这条钉的是「不是超时」那一支：ECONNREFUSED / DNS / TLS 这类对端立刻失败的错误，
  // 绝不能写成 "(connect timeout 15000ms)" —— 那会把运维引去调一个根本没参与的数字。
  it("attributes a real failure to its phase instead of calling it a timeout", async () => {
    const plan = {
      version: "1.3.0",
      assetName: "clawmessenger-windows-x64.exe",
      sha256: sum(binary),
      mirrors: [
        "/api/agent/upgrade/download?version=1.3.0&asset=x&token=t",
        "https://example.test/binary",
      ],
    };
    const fetchImpl = (async (url: string) => {
      if (url.includes("/api/agent/upgrade/download")) {
        // 代理：头返回了，body 传输中断（连接被对端 reset）。
        return {
          ok: true,
          status: 200,
          arrayBuffer: async () => { throw new Error("socket hang up"); },
        } as never;
      }
      // 直连：连一个没人监听的端口，立刻 ECONNREFUSED。
      throw new Error("fetch failed");
    }) as never;

    await expect(
      stageUpdateFromMirrors(plan, target, "https://srv.example.com", fetchImpl as never),
    ).rejects.toThrow(
      /socket hang up \(transfer failed\)[\s\S]*fetch failed \(connect failed\)/,
    );
    expect(findStaged(target)).toHaveLength(0);
  });

  // 两阶段的**接线**必须被钉住：头一到就清 connect 定时器，之后 body 只受总预算
  // 约束。上面那条用例里的 fetch 是立刻抛的，真实定时器根本没参与，删掉
  // `clearTimeout(connectTimer)` 它照样绿 —— 所以这里用假时钟真推进。
  it("clears the connect budget once headers arrive and only the total budget can abort the body", async () => {
    vi.useFakeTimers();
    try {
      const plan = {
        version: "1.3.0",
        assetName: "clawmessenger-windows-x64.exe",
        sha256: sum(binary),
        mirrors: ["https://example.test/binary"],
      };
      const fetchImpl = (async (_url: string, init?: { signal?: AbortSignal }) => ({
        ok: true,
        status: 200,
        arrayBuffer: () => new Promise<ArrayBuffer>((_resolve, reject) => {
          init?.signal?.addEventListener("abort", () => {
            const err = new Error("This operation was aborted");
            err.name = "TimeoutError";
            reject(err);
          });
        }),
      })) as never;

      let settled: string | undefined;
      void stageUpdateFromMirrors(plan, target, "https://srv.example.com", fetchImpl as never).then(
        () => { settled = "resolved"; },
        (err: Error) => { settled = err.message; },
      );

      // 先空转一次让「头已到达」的续体落地：`await fetchImpl` 的 resumption 是一个
      // 微任务，它才会执行 `clearTimeout(connectTimer)`。假时钟推进（哪怕 0ms）
      // 会先把定时器调到点再 flush 微任务，所以直接 `advanceTimersByTimeAsync(20_000)`
      // 会让 15s 的连接定时器抢在续体之前触发 —— 那不是接线的问题，是测试自己没给
      // 续体执行机会（见下方变异自证：清掉 clearTimeout 这条仍会红）。
      await vi.advanceTimersByTimeAsync(0);
      // 推过连接预算（15s）：头早就回来了，这条不能掐任何东西。
      await vi.advanceTimersByTimeAsync(20_000);
      expect(settled).toBeUndefined();

      // 推过总预算（600s）：这才该 abort，而且归因必须是 transfer。
      await vi.advanceTimersByTimeAsync(600_000);
      expect(settled).toMatch(/transfer timeout 600000ms/);
      // 反向自证：如果 connect 定时器没被清掉，上面那条 20s 处就会以
      // "(connect timeout 15000ms)" 落定。
      expect(settled).not.toMatch(/connect timeout/);
    } finally {
      vi.useRealTimers();
    }
  });
});
