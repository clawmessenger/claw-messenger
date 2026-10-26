import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { randomBytes } from "node:crypto";
import { homedir } from "node:os";
import { join } from "node:path";

export function defaultKeystorePath(): string {
  return join(homedir(), ".clawmessenger", "credentials.json");
}

// 升级进行中的小账本。
//
// 为什么要落盘：升级流程是「下载 → 交换（旧 exe 改名、新 exe 就位）→ 退出 →
// helper 拉起新 exe」。交换完成到新进程第一次轮询之间，设备会短暂没有进程能
// 说话；而**新进程自己**正是唯一能确认「我到底是新版还是旧版」的那一方
// （版本号编译进镜像）。把它写在启动前就好的文件里，重启后的第一个 supervisor
// 才能回报 succeeded 而不是让页面永远停在「重启中」。
export function defaultUpgradeInflightPath(): string {
  return join(homedir(), ".clawmessenger", "upgrade-inflight.json");
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

// 发布版默认指向线上生产环境。测试环境（/im-test）由调用方显式传 --server。
export const DEFAULT_SERVER_URL = "https://newsradar.dreamdt.cn/im";
