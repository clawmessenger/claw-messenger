import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { randomBytes } from "node:crypto";
import { homedir } from "node:os";
import { join } from "node:path";

export function defaultKeystorePath(): string {
  return join(homedir(), ".clawmessenger", "credentials.json");
}

// Local record of RongCloud message UIDs the run loop has already acted on.
// 融云 re-delivers the entire offline history on every reconnect and the Node
// shim has no IndexDB for the SDK to keep its own sync cursor, so without this
// every restart re-runs agent turns for messages answered days ago.
//
// Keyed by node: the supervisor runs one child process per bound agent, and
// they would otherwise race on a single shared file.
export function defaultProcessedUidsPath(nodeId?: string): string {
  const suffix = nodeId ? `-${nodeId.replace(/[^A-Za-z0-9._-]/g, "_")}` : "";
  return join(homedir(), ".clawmessenger", `processed-uids${suffix}.json`);
}

// Length of a freshly minted machine id, in decimal digits.
//
// The machine id is embedded in every RongCloud user id this device owns:
// "<agent>_<machine>" for agent nodes and "rc_node_<machine>" for the machine
// node. The format follows the long-standing node id convention used by the
// Python server (id_generator.py: generate_numeric_id mints 6-10 digits with a
// non-zero leading digit, and the RongCloud id is "<node_type>_<id>"), so node
// ids read like hermes_1234567890.
//
// RongCloud caps a user id at 64 chars and the longest agent prefix is 11
// (antigravity), so 11 digits still leaves ample room. 10 digits carries
// 9×10^9 values — collisions need on the order of 10^4-10^5 devices on one
// deployment before the birthday bound matters, and the id is only ever minted
// once per machine and persisted.
export const MACHINE_ID_LENGTH = 10;

// drawDigit returns a uniform integer in [min, max]. randomBytes yields
// multiples of 256 values, and 256 is not a multiple of most spans, so bytes at
// or above the cutoff are rejected rather than taken modulo — that keeps the
// distribution uniform instead of favouring the low symbols.
function drawDigit(min: number, max: number): number {
  const span = max - min + 1;
  const cutoff = 256 - (256 % span);
  for (;;) {
    const byte = randomBytes(1)[0];
    if (byte < cutoff) return min + (byte % span);
  }
}

// randomMachineId returns a numeric machine id whose first digit is non-zero,
// matching the ids the Python server has always minted.
export function randomMachineId(length: number = MACHINE_ID_LENGTH): string {
  if (!Number.isInteger(length) || length < 1) {
    throw new RangeError(`machine id length must be a positive integer, got ${length}`);
  }
  let id = String(drawDigit(1, 9));
  while (id.length < length) {
    id += String(drawDigit(0, 9));
  }
  return id;
}

// Stable per-machine identifier, minted once and stored next to the
// keystore. Sent as mac_address on register so re-registering on one
// machine reuses the same rc_user_id instead of colliding on the empty
// default (server unique key) and 500ing.
export function machineId(keystorePath: string = defaultKeystorePath()): string {
  const idPath = join(dirnameOf(keystorePath), "machine_id");
  try {
    // Returned verbatim, including the legacy "clawmessenger-<uuid>" form: the
    // device is already bound to the RongCloud account derived from it, so
    // re-minting would orphan that account and duplicate every node under it.
    const existing = readFileSync(idPath, "utf8").trim();
    if (existing) return existing;
  } catch {
    // absent or unreadable: mint below
  }
  const id = randomMachineId();
  try {
    mkdirSync(dirnameOf(idPath), { recursive: true });
    writeFileSync(idPath, id, { mode: 0o600 });
  } catch {
    // Best effort: a fresh id per call still registers, it just loses
    // cross-run stability on read-only homes.
  }
  return id;
}

function dirnameOf(p: string): string {
  const idx = p.lastIndexOf("/");
  const idxWin = p.lastIndexOf("\\");
  const cut = Math.max(idx, idxWin);
  return cut === -1 ? "." : p.slice(0, cut);
}

export const DEFAULT_SERVER_URL = "http://localhost:8080";
