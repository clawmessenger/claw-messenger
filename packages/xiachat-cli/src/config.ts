import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { randomUUID } from "node:crypto";
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

// Stable per-machine identifier, minted once and stored next to the
// keystore. Sent as mac_address on register so re-registering on one
// machine reuses the same rc_user_id instead of colliding on the empty
// default (server unique key) and 500ing.
export function machineId(keystorePath: string = defaultKeystorePath()): string {
  const idPath = join(dirnameOf(keystorePath), "machine_id");
  try {
    const existing = readFileSync(idPath, "utf8").trim();
    if (existing) return existing;
  } catch {
    // absent or unreadable: mint below
  }
  const id = `clawmessenger-${randomUUID()}`;
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
