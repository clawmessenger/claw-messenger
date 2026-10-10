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

// Length of a freshly minted machine id, in base36 characters.
//
// The machine id is embedded in every RongCloud user id this device owns:
// "rc_node_<machine>[_<agent>]". RongCloud caps a user id at 64 chars and the
// server reserves 33 of them for the "rc_node_" prefix, the "_" separator and
// the longest agent name, leaving 31 for the machine id. The previous
// "clawmessenger-<uuid>" form was 50 chars, so the server had to sha256 it into
// an opaque "m_<24 hex>" key — which is why node ids read like
// rc_node_m_e99ad19f108d9dad78ad07e3_codex.
//
// 10 base36 chars fits the budget with room to spare (the server keeps it
// verbatim, so node ids stay readable) while carrying ~52 bits of randomness:
// a collision needs on the order of 10^8 machines on one deployment.
export const MACHINE_ID_LENGTH = 10;
const MACHINE_ID_ALPHABET = "0123456789abcdefghijklmnopqrstuvwxyz";

// randomMachineId returns a machine id drawn uniformly from
// MACHINE_ID_ALPHABET. randomBytes gives multiples of 256 values, which is not
// a multiple of 36, so bytes at or above the cutoff are rejected instead of
// taken modulo — that keeps the distribution uniform rather than biased toward
// the first four symbols.
export function randomMachineId(length: number = MACHINE_ID_LENGTH): string {
  const cutoff = 256 - (256 % MACHINE_ID_ALPHABET.length);
  let id = "";
  while (id.length < length) {
    for (const byte of randomBytes(length * 2)) {
      if (byte >= cutoff) continue;
      id += MACHINE_ID_ALPHABET[byte % MACHINE_ID_ALPHABET.length];
      if (id.length === length) break;
    }
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
