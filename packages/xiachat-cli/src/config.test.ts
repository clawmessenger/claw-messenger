// @vitest-environment node
import { describe, expect, it } from "vitest";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { MACHINE_ID_LENGTH, machineId, randomMachineId } from "./config.js";

const ALPHABET = /^[0-9a-z]+$/;

// Mirrors the server's budget in
// server/internal/integrations/rongcloud/node_service.go: RongCloud allows 64
// chars, and "rc_node_" (8) + "_" (1) + the longest agent name (24) are
// reserved, leaving 31 for the machine id. Anything longer gets sha256-hashed
// into an opaque "m_<24 hex>" key.
const RONGCLOUD_ID_LIMIT = 64;
const MACHINE_ID_BUDGET = 31;

function withTempDir<T>(fn: (dir: string) => T): T {
  const dir = mkdtempSync(join(tmpdir(), "clawmessenger-config-"));
  try {
    return fn(dir);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

describe("randomMachineId", () => {
  it("mints the requested number of base36 characters", () => {
    const id = randomMachineId();
    expect(id).toHaveLength(MACHINE_ID_LENGTH);
    expect(id).toMatch(ALPHABET);
    for (const length of [8, 9, 11]) {
      const sized = randomMachineId(length);
      expect(sized).toHaveLength(length);
      expect(sized).toMatch(ALPHABET);
    }
  });

  it("stays inside the RongCloud id budget so the server keeps it verbatim", () => {
    expect(MACHINE_ID_LENGTH).toBeGreaterThanOrEqual(8);
    expect(MACHINE_ID_LENGTH).toBeLessThanOrEqual(11);
    expect(MACHINE_ID_LENGTH).toBeLessThanOrEqual(MACHINE_ID_BUDGET);

    // The longest node id this can produce is rc_node_<id>_<24-char agent>.
    const longest = `rc_node_${randomMachineId()}_`.padEnd(
      "rc_node_".length + MACHINE_ID_LENGTH + 1 + 24,
      "x",
    );
    expect(longest.length).toBeLessThanOrEqual(RONGCLOUD_ID_LIMIT);
  });

  it("does not repeat across many draws", () => {
    const seen = new Set<string>();
    for (let i = 0; i < 5000; i += 1) seen.add(randomMachineId());
    // 36^10 possibilities: 5000 draws must all be distinct in practice.
    expect(seen.size).toBe(5000);
  });

  it("only draws from the alphabet (no biased symbols)", () => {
    const counts = new Map<string, number>();
    const draws = 3600;
    for (let i = 0; i < draws; i += 1) {
      for (const ch of randomMachineId()) counts.set(ch, (counts.get(ch) ?? 0) + 1);
    }
    // 36000 characters over 36 symbols averages ~1000 each; a symbol stuck at
    // 0 (or at double) would mean the modulo bias crept back in.
    const expected = (draws * MACHINE_ID_LENGTH) / 36;
    expect(counts.size).toBe(36);
    for (const n of counts.values()) {
      expect(n).toBeGreaterThan(expected * 0.7);
      expect(n).toBeLessThan(expected * 1.3);
    }
  });
});

describe("machineId", () => {
  it("mints a short id and persists it for later runs", () =>
    withTempDir((dir) => {
      const keystore = join(dir, "credentials.json");
      const first = machineId(keystore);
      expect(first).toHaveLength(MACHINE_ID_LENGTH);
      expect(first).toMatch(ALPHABET);
      // Written next to the keystore, not inside it.
      expect(readFileSync(join(dir, "machine_id"), "utf8").trim()).toBe(first);
      expect(machineId(keystore)).toBe(first);
    }));

  it("returns a legacy long id verbatim instead of re-minting", () =>
    withTempDir((dir) => {
      const keystore = join(dir, "credentials.json");
      const legacy = "clawmessenger-3cb5ee6e-eb6b-45d6-afcb-54c9ef5bac7c";
      writeFileSync(join(dir, "machine_id"), `${legacy}\n`);
      // A device already bound to the RongCloud account derived from this id
      // must keep it, otherwise re-pairing orphans the account and duplicates
      // every node under it.
      expect(machineId(keystore)).toBe(legacy);
    }));

  it("ignores an empty id file and mints a new one", () =>
    withTempDir((dir) => {
      const keystore = join(dir, "credentials.json");
      writeFileSync(join(dir, "machine_id"), "   \n");
      const id = machineId(keystore);
      expect(id).toHaveLength(MACHINE_ID_LENGTH);
      expect(readFileSync(join(dir, "machine_id"), "utf8").trim()).toBe(id);
    }));
});
