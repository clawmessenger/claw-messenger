import { describe, expect, it } from "vitest";
import {
  installPaths,
  isSeaExecutable,
  nextPathValue,
  parseRegPathQuery,
  samePath,
} from "./install.js";

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
