import * as fs from "node:fs";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi, type MockInstance } from "vitest";
import {
  ensureInstalled,
  installPaths,
  isSeaExecutable,
  nextPathValue,
  parseRegPathQuery,
  samePath,
  upgradeTargetPath,
} from "./install.js";

// Self-install tests need a virtual filesystem: the assertions are about what
// is left on disk after each failure path (in particular: no 88 MB stray tmp
// file next to the install), not about which fs function was called.
// `disk` is the source of truth — copyFileSync creates, unlinkSync removes,
// renameSync moves. Everything else in this file is pure and unaffected.
vi.mock("node:fs", () => {
  const disk = new Set<string>();
  const copyFailure = new Map<string, Error>();
  const control = { renameError: null as Error | null };

  const copyFileSync = vi.fn((_src: string, dest: string) => {
    const err = copyFailure.get(dest);
    if (err) throw err;
    disk.add(dest);
  });
  const renameSync = vi.fn((from: string, to: string) => {
    if (control.renameError) throw control.renameError;
    disk.delete(from);
    disk.add(to);
  });
  const unlinkSync = vi.fn((p: string) => {
    if (!disk.has(p)) {
      throw Object.assign(new Error(`ENOENT: no such file or directory, unlink '${p}'`), { code: "ENOENT" });
    }
    disk.delete(p);
  });
  const existsSync = vi.fn((p: string) => disk.has(p));
  const mkdirSync = vi.fn();
  const readFileSync = vi.fn(() => "");
  const writeFileSync = vi.fn((p: string) => {
    disk.add(p);
  });

  return {
    copyFileSync,
    renameSync,
    unlinkSync,
    existsSync,
    mkdirSync,
    readFileSync,
    writeFileSync,
    __disk: disk,
    __copyFailure: copyFailure,
    __control: control,
  };
});

vi.mock("node:child_process", () => ({
  // PATH lookup/update is irrelevant here; keep `reg` out of the test.
  spawnSync: vi.fn(() => ({ status: 1, stdout: "", stderr: "" })),
}));

vi.mock("node:os", () => ({ homedir: () => "C:\\Users\\u" }));

type FsMock = typeof fs & {
  __disk: Set<string>;
  __copyFailure: Map<string, Error>;
  __control: { renameError: Error | null };
};

const fsMock = fs as FsMock;

describe("installPaths", () => {
  it("maps windows to LOCALAPPDATA clawmessenger dir with .exe", () => {
    const p = installPaths("win32", "C:\\Users\\u", "C:\\Users\\u\\AppData\\Local");
    expect(p.dir).toBe("C:\\Users\\u\\AppData\\Local\\clawmessenger");
    expect(p.exe).toBe("clawmessenger.exe");
    expect(p.target).toBe("C:\\Users\\u\\AppData\\Local\\clawmessenger\\clawmessenger.exe");
  });

  it("falls back to AppData\\Local under homedir when localAppData is not given", () => {
    const p = installPaths("win32", "C:\\Users\\u");
    expect(p.dir).toBe("C:\\Users\\u\\AppData\\Local\\clawmessenger");
  });

  it("maps unix to ~/.local/bin without extension", () => {
    const p = installPaths("linux", "/home/u");
    expect(p.dir).toBe("/home/u/.local/bin");
    expect(p.exe).toBe("clawmessenger");
    expect(p.target).toBe("/home/u/.local/bin/clawmessenger");
  });

  it("treats darwin the same as linux", () => {
    expect(installPaths("darwin", "/Users/u").target).toBe("/Users/u/.local/bin/clawmessenger");
  });
});

describe("isSeaExecutable", () => {
  it("accepts a missing argv1 (interactive double-click scenario)", () => {
    expect(isSeaExecutable(undefined)).toBe(true);
    expect(isSeaExecutable("")).toBe(true);
  });

  it("accepts a non-.js path (SEA binary such as clawmessenger.exe)", () => {
    expect(isSeaExecutable("C:\\Users\\u\\Downloads\\clawmessenger-windows-x64.exe")).toBe(true);
    expect(isSeaExecutable("/home/u/clawmessenger-linux-x64")).toBe(true);
  });

  it("rejects node script entrypoints (dev mode)", () => {
    expect(isSeaExecutable("D:\\repo\\packages\\xiachat-cli\\dist\\bin.js")).toBe(false);
    expect(isSeaExecutable("/repo/dist/bin.js")).toBe(false);
  });

  it("rejects the SEA entry bundle (the release pipeline runs it with node)", () => {
    // scripts/build-exe.mjs 产出 dist/clawmessenger.sea.cjs，发布流水线的版本闸门
    // 用 `node dist/clawmessenger.sea.cjs --version` 跑它。判成 exe 会让一次版本
    // 检查去动真实安装目录、并把 node 二进制复制成 ~/.local/bin/clawmessenger。
    expect(isSeaExecutable("packages/xiachat-cli/dist/clawmessenger.sea.cjs")).toBe(false);
    expect(isSeaExecutable("/repo/dist/clawmessenger.sea.cjs")).toBe(false);
    expect(isSeaExecutable("/repo/dist/entry.mjs")).toBe(false);
  });
});

describe("upgradeTargetPath", () => {
  it("overwrites the running SEA executable, not the platform's install path", () => {
    // Linux 上 systemd unit 跑 /usr/local/bin/clawmessenger，installPaths 给的是
    // ~/.local/bin/clawmessenger。按平台猜会把「没人用的副本」换成新版，unit 重启
    // 后仍在跑旧版 —— 页面版本 / 磁盘文件 / 实际进程三方不一致。
    expect(
      upgradeTargetPath("linux", "/usr/local/bin/clawmessenger", "/usr/local/bin/clawmessenger", "/root"),
    ).toBe("/usr/local/bin/clawmessenger");
    expect(
      upgradeTargetPath("linux", "/opt/cm/clawmessenger", "/opt/cm/clawmessenger", "/root"),
    ).toBe("/opt/cm/clawmessenger");
  });

  it("uses execPath on windows SEA and darwin SEA alike", () => {
    expect(
      upgradeTargetPath(
        "win32",
        "C:\\Users\\u\\AppData\\Local\\clawmessenger\\clawmessenger.exe",
        "C:\\Users\\u\\AppData\\Local\\clawmessenger\\clawmessenger.exe",
        "C:\\Users\\u",
        "C:\\Users\\u\\AppData\\Local",
      ),
    ).toBe("C:\\Users\\u\\AppData\\Local\\clawmessenger\\clawmessenger.exe");
    expect(
      upgradeTargetPath("darwin", "/usr/local/bin/clawmessenger", "/usr/local/bin/clawmessenger", "/Users/u"),
    ).toBe("/usr/local/bin/clawmessenger");
  });

  it("falls back to the install path when running a node script (execPath is node itself)", () => {
    // 非 SEA 时 process.execPath 是 node 本体，覆盖它等于把 node 换成 CLI 二进制。
    expect(
      upgradeTargetPath("linux", "/repo/dist/bin.js", "/usr/bin/node", "/home/u"),
    ).toBe("/home/u/.local/bin/clawmessenger");
    expect(
      upgradeTargetPath("linux", "/repo/dist/clawmessenger.sea.cjs", "/usr/bin/node", "/home/u"),
    ).toBe("/home/u/.local/bin/clawmessenger");
    expect(
      upgradeTargetPath("win32", "D:\\repo\\dist\\bin.js", "C:\\node\\node.exe", "C:\\Users\\u", "C:\\Users\\u\\AppData\\Local"),
    ).toBe("C:\\Users\\u\\AppData\\Local\\clawmessenger\\clawmessenger.exe");
  });

  it("still returns the install path when argv1 is missing (no script entrypoint)", () => {
    // argv1 缺失时 isSeaExecutable 返回 true，走 execPath 分支；这里用 execPath 明确
    // 表示「跑的就是这个二进制」，避免退回按平台猜出来的路径。
    expect(upgradeTargetPath("linux", undefined, "/usr/local/bin/clawmessenger", "/root")).toBe(
      "/usr/local/bin/clawmessenger",
    );
  });
});

describe("samePath", () => {
  it("is case-insensitive and separator-normalizing on windows", () => {
    expect(samePath("C:\\APPDATA\\clawmessenger\\clawmessenger.exe", "c:\\appdata\\clawmessenger\\clawmessenger.exe", "win32")).toBe(true);
  });

  it("is case-sensitive on unix", () => {
    expect(samePath("/Home/u/.local/bin/clawmessenger", "/home/u/.local/bin/clawmessenger", "linux")).toBe(false);
    expect(samePath("/home/u/.local/bin/clawmessenger", "/home/u/.local/bin/clawmessenger", "linux")).toBe(true);
  });
});

describe("parseRegPathQuery", () => {
  it("extracts a REG_EXPAND_SZ value", () => {
    const out = [
      "",
      "HKEY_CURRENT_USER\\Environment",
      "    Path    REG_EXPAND_SZ    %LOCALAPPDATA%\\clawmessenger;C:\\Windows\\System32",
      "",
    ].join("\r\n");
    expect(parseRegPathQuery(out)).toBe("%LOCALAPPDATA%\\clawmessenger;C:\\Windows\\System32");
  });

  it("extracts a REG_SZ value", () => {
    const out = "HKEY_CURRENT_USER\\Environment\r\n    Path    REG_SZ    C:\\bin\r\n";
    expect(parseRegPathQuery(out)).toBe("C:\\bin");
  });

  it("returns empty for missing or unset values", () => {
    expect(parseRegPathQuery("HKEY_CURRENT_USER\\Environment\r\n")).toBe("");
    expect(parseRegPathQuery("    Path    REG_SZ    (value not set)\r\n")).toBe("");
  });
});

describe("nextPathValue", () => {
  it("appends the entry when the current value is empty", () => {
    expect(nextPathValue("", "%LOCALAPPDATA%\\clawmessenger", "win32")).toBe("%LOCALAPPDATA%\\clawmessenger");
  });

  it("appends after existing segments preserving them", () => {
    expect(nextPathValue("C:\\a;C:\\b", "C:\\c", "win32")).toBe("C:\\a;C:\\b;C:\\c");
  });

  it("drops empty segments but keeps non-empty ones", () => {
    expect(nextPathValue("C:\\a;;C:\\b;", "C:\\c", "win32")).toBe("C:\\a;C:\\b;C:\\c");
  });

  it("is a no-op when the entry already exists (case-insensitive on windows)", () => {
    const current = "C:\\Windows\\System32;%LOCALAPPDATA%\\CLAWMESSENGER";
    expect(nextPathValue(current, "%LOCALAPPDATA%\\clawmessenger", "win32")).toBe(current);
  });

  it("uses ':' separator on unix", () => {
    expect(nextPathValue("/usr/bin", "/home/u/.local/bin", "linux")).toBe("/usr/bin:/home/u/.local/bin");
  });
});

describe("ensureInstalled self-copy", () => {
  const platformDescriptor = Object.getOwnPropertyDescriptor(process, "platform");
  const originalArgv1 = process.argv[1];
  const originalLocalAppData = process.env.LOCALAPPDATA;
  let stderrSpy: MockInstance<typeof process.stderr.write>;

  const paths = installPaths("win32", "C:\\Users\\u", undefined);
  const tmpPath = `${paths.target}.tmp-${process.pid}`;
  const ebusy = () => Object.assign(new Error("EBUSY: resource busy or locked, copyfile"), { code: "EBUSY" });
  const stderrText = (): string => stderrSpy.mock.calls.map((c) => String(c[0])).join("");

  beforeAll(() => {
    // The incident happened on Windows; pin the platform so the test does not
    // depend on where CI runs.
    Object.defineProperty(process, "platform", { value: "win32", configurable: true });
  });

  afterAll(() => {
    if (platformDescriptor) Object.defineProperty(process, "platform", platformDescriptor);
    process.argv[1] = originalArgv1;
    if (originalLocalAppData === undefined) delete process.env.LOCALAPPDATA;
    else process.env.LOCALAPPDATA = originalLocalAppData;
  });

  beforeEach(() => {
    vi.clearAllMocks();
    fsMock.__disk.clear();
    fsMock.__copyFailure.clear();
    fsMock.__control.renameError = null;
    // Run as a SEA binary coming from Downloads, and make the install base
    // deterministic.
    process.argv[1] = "C:\\Users\\u\\Downloads\\clawmessenger.exe";
    delete process.env.LOCALAPPDATA;
    stderrSpy = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
  });

  afterEach(() => {
    stderrSpy.mockRestore();
  });

  it("leaves no tmp file when the fallback copy hits EBUSY on a locked target (incident)", () => {
    // rename fails (old install present), then the fallback copy fails because
    // the running exe holds the target: the pre-fix code threw here and skipped
    // unlinkSync(tmp), leaving a full-size stray file behind forever.
    fsMock.__control.renameError = Object.assign(new Error("EPERM: operation not permitted, rename"), { code: "EPERM" });
    fsMock.__copyFailure.set(paths.target, ebusy());

    let result: ReturnType<typeof ensureInstalled>;
    expect(() => {
      result = ensureInstalled();
    }).not.toThrow();

    // The fallback really was attempted and really did throw.
    expect(fsMock.copyFileSync).toHaveBeenCalledWith(process.execPath, paths.target);

    expect(result!).not.toBeNull();
    expect(result!.copied).toBe(false);
    // The tmp path is exactly the one the implementation used, and the file is
    // gone from the virtual disk — nothing is left next to the install.
    expect(fsMock.unlinkSync).toHaveBeenCalledWith(tmpPath);
    expect(fsMock.__disk.has(tmpPath)).toBe(false);
    expect(stderrText()).toContain("could not self-install");
  });

  it("cleans up the tmp file when the fallback copy succeeds", () => {
    fsMock.__control.renameError = Object.assign(new Error("EPERM: operation not permitted, rename"), { code: "EPERM" });

    const result = ensureInstalled();

    expect(fsMock.copyFileSync).toHaveBeenCalledWith(process.execPath, paths.target);
    expect(result).not.toBeNull();
    expect(result!.copied).toBe(true);
    expect(fsMock.__disk.has(paths.target)).toBe(true);
    expect(fsMock.unlinkSync).toHaveBeenCalledWith(tmpPath);
    expect(fsMock.__disk.has(tmpPath)).toBe(false);
  });

  it("keeps copied === true on the atomic rename path even though cleanup throws ENOENT", () => {
    // rename consumes tmp, so the finally-block unlink finds nothing (ENOENT)
    // and must swallow it instead of failing the install.
    const result = ensureInstalled();

    expect(result).not.toBeNull();
    expect(result!.copied).toBe(true);
    expect(fsMock.__disk.has(paths.target)).toBe(true);
    expect(fsMock.unlinkSync).toHaveBeenCalledWith(tmpPath);
    expect(fsMock.__disk.has(tmpPath)).toBe(false);
    expect(stderrText()).not.toContain("could not self-install");
  });

  it("returns copied === false and reports on stderr when the first copy to tmp fails", () => {
    fsMock.__copyFailure.set(tmpPath, Object.assign(new Error("ENOSPC: no space left on device, copyfile"), { code: "ENOSPC" }));

    let result: ReturnType<typeof ensureInstalled>;
    expect(() => {
      result = ensureInstalled();
    }).not.toThrow();

    expect(result!).not.toBeNull();
    expect(result!.copied).toBe(false);
    expect(fsMock.__disk.has(tmpPath)).toBe(false);
    expect(stderrText()).toContain("could not self-install");
    expect(stderrText()).toContain("ENOSPC");
  });
});
