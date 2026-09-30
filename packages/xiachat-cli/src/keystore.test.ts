// @vitest-environment node
import { mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { Keystore } from "./keystore.js";

const dirs: string[] = [];
function tempDir(): string {
  const d = mkdtempSync(join(tmpdir(), "xiachat-keystore-"));
  dirs.push(d);
  return d;
}
afterEach(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
  dirs.length = 0;
});

describe("Keystore", () => {
  it("round-trips credentials", () => {
    const ks = new Keystore(join(tempDir(), "creds.json"));
    expect(ks.load()).toBeNull();
    ks.save({ nodeId: "node_abc", token: "tok", serverUrl: "http://x" });
    const loaded = ks.load();
    expect(loaded?.nodeId).toBe("node_abc");
    expect(loaded?.token).toBe("tok");
  });

  it("writes file with 0600 permissions", () => {
    const dir = tempDir();
    const ks = new Keystore(join(dir, "creds.json"));
    ks.save({ nodeId: "n", token: "t", serverUrl: "http://x" });
    const mode = (statSyncPermissions(join(dir, "creds.json")) & 0o777).toString(8);
    expect(["600", "666"]).toContain(mode); // Windows falls back to default ACLs
  });
});

function statSyncPermissions(p: string): number {
  return statSync(p).mode;
}
