// @vitest-environment node
import { describe, expect, it } from "vitest";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { MACHINE_ID_LENGTH, machineId, randomMachineId } from "./config.js";

// Node ids are "<agent>_<machine id>" (agent nodes) and "rc_node_<machine id>"
// (the machine node); the machine id itself is pure digits, following the
// Python server's id_generator.generate_numeric_id.
const DIGITS = /^[1-9][0-9]*$/;

// Mirrors the server's constraint in
// server/internal/integrations/rongcloud/node_service.go: RongCloud allows 64
// chars, and the longest agent prefix ("antigravity") plus the "_" separator
// are spoken for. Anything longer gets sha256-hashed into an opaque
// "m_<24 hex>" key.
const RONGCLOUD_ID_LIMIT = 64;
const LONGEST_AGENT = "antigravity";

describe("randomMachineId", () => {
  it("mints pure numeric ids of the requested length", () => {
    const id = randomMachineId();
    expect(id).toHaveLength(MACHINE_ID_LENGTH);
    expect(id).toMatch(DIGITS);
    for (const length of [8, 9, 11]) {
      const sized = randomMachineId(length);
      expect(sized).toHaveLength(length);
      expect(sized).toMatch(DIGITS);
    }
  });

  it("rejects a non-positive length instead of minting nonsense", () => {
    expect(() => randomMachineId(0)).toThrow(RangeError);
    expect(() => randomMachineId(-1)).toThrow(RangeError);
    expect(() => randomMachineId(1.5)).toThrow(RangeError);
  });

  it("stays inside the RongCloud id budget so the server keeps it verbatim", () => {
    expect(MACHINE_ID_LENGTH).toBeGreaterThanOrEqual(8);
    expect(MACHINE_ID_LENGTH).toBeLessThanOrEqual(11);

    // The longest node id this can produce is <agent>_<machine id>.
    const longest = `${LONGEST_AGENT}_${randomMachineId()}`;
    expect(longest.length).toBeLessThanOrEqual(RONGCLOUD_ID_LIMIT);
  });

  it("does not repeat across many draws", () => {
    const seen = new Set<string>();
    for (let i = 0; i < 5000; i += 1) seen.add(randomMachineId());
    // 9×10^9 possibilities: 5000 draws must all be distinct in practice.
    expect(seen.size).toBe(5000);
  });

  it("draws digits uniformly, never with a leading zero", () => {
    const draws = 3000;
    const counts = new Map<string, number>();
    let leadingZero = 0;
    for (let i = 0; i < draws; i += 1) {
      const id = randomMachineId();
      if (id.startsWith("0")) leadingZero += 1;
      for (const ch of id) counts.set(ch, (counts.get(ch) ?? 0) + 1);
    }
    expect(leadingZero).toBe(0);
    expect(counts.size).toBe(10);
    // "0" can never be the leading digit, so it shows up in 9 of the 10
    // positions; every other digit shows up in all 10 plus its share of the
    // leading slot. A symbol stuck at 0 (or at double) would mean the modulo
    // bias crept back in.
    const zeroExpected = (draws * (MACHINE_ID_LENGTH - 1)) / 10;
    const otherExpected = zeroExpected + draws / 9;
    for (const [digit, n] of counts) {
      const expected = digit === "0" ? zeroExpected : otherExpected;
      expect(n).toBeGreaterThan(expected * 0.8);
      expect(n).toBeLessThan(expected * 1.2);
    }
  });
});

describe("machineId", () => {
  it("mints a short id and persists it for later runs", () =>
    withTempDir((dir) => {
      const keystore = join(dir, "credentials.json");
      const first = machineId(keystore);
      expect(first).toHaveLength(MACHINE_ID_LENGTH);
      expect(first).toMatch(DIGITS);
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

function withTempDir<T>(fn: (dir: string) => T): T {
  const dir = mkdtempSync(join(tmpdir(), "clawmessenger-config-"));
  try {
    return fn(dir);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}
