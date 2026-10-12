import { access, constants } from "node:fs/promises";
import { modelListSpec, parseModelCatalogOutput } from "./model-catalog.js";

// Protocol v1 runtime-recovery responder for the web's 设备详情 → 一键唤醒.
//
// The web sends a private "command" message to this node's RongCloud user:
//   { msg_type: "claw_runtime_recovery_request", protocolVersion: 1,
//     requestId: <uuid>, timestamp: <ms>, action: "probe" | "recover" }
// and waits (8s for probe, 20s for recover) for a
// "claw_runtime_recovery_response" from that same user.
//
// Why this exists at all: the web's 「恢复运行时」 used to go through
// sendSystemRequest('claw', 'recoverRuntime', …), but the Go compat layer's
// system channel only ever implemented ping / chatroom / device / discussion /
// ai — there has never been a `claw` service, so the request fell into the
// unknown-service branch and the browser waited out its full 30s timeout.
//
// Scope discipline (this is the security boundary, not a nicety):
//  - the request field set is an EXACT whitelist; one extra key rejects the
//    whole packet, which is what makes path/args/shell injection impossible;
//  - `action` is a closed set of two literals;
//  - `recover` performs exactly three side effects — an idempotent
//    ensureRuntime() (opencode install), one `<agent> models` probe, and a
//    model-catalog cache refresh — never an arbitrary command.
//
// Cost model (agreed with the web side):
//  - `probe` is the CHEAP path: only a stat() on the agent binary plus the
//    already-warm catalog. It never spawns the agent, because `opencode models`
//    takes ~10s cold and an 8s ceiling would misreport a working runtime as
//    found_not_runnable;
//  - `recover` is the real thing: it runs the model-list argv with a 20s
//    ceiling, so a cold start fits inside the web's 20s window.

export type RuntimeRecoveryAction = "probe" | "recover";

export type RuntimeRecoveryCode =
  | "ready"
  | "needs_auth"
  | "not_found"
  | "found_not_runnable"
  | "probe_failed"
  | "unsupported_agent";

export interface RuntimeRecoveryRequest {
  msg_type: "claw_runtime_recovery_request";
  protocolVersion: 1;
  requestId: string;
  timestamp: number;
  action: RuntimeRecoveryAction;
}

export interface RuntimeRecoveryResult {
  code: RuntimeRecoveryCode;
  message: string;
}

// The exact key set the web is allowed to send. Anything else is rejected
// outright: there is deliberately no `path`, `args`, `shell`, `commandLine`
// or `workdir` — a node must never execute what a browser tells it to.
const REQUEST_KEYS: readonly string[] = [
  "msg_type",
  "protocolVersion",
  "requestId",
  "timestamp",
  "action",
];

// crypto.randomUUID() shape, but only the token charset is asserted: a strict
// UUID pattern would reject a future client for no security gain, while a
// charset + length bound keeps the value safe to echo back and to log.
const REQUEST_ID_RE = /^[A-Za-z0-9_-]{1,64}$/;

export const RUNTIME_RECOVERY_PROTOCOL_VERSION = 1;
export const RUNTIME_RECOVERY_REQUEST_TYPE = "claw_runtime_recovery_request";
export const RUNTIME_RECOVERY_RESPONSE_TYPE = "claw_runtime_recovery_response";

/** Strictly parses an inbound request; `null` means "not ours / not valid". */
export function parseRuntimeRecoveryRequest(content: string): RuntimeRecoveryRequest | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(content);
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const record = parsed as Record<string, unknown>;
  // Exact whitelist: every required key present AND no key outside the set.
  const keys = Object.keys(record);
  if (keys.length !== REQUEST_KEYS.length) return null;
  for (const key of keys) {
    if (!REQUEST_KEYS.includes(key)) return null;
  }
  if (record.msg_type !== RUNTIME_RECOVERY_REQUEST_TYPE) return null;
  if (record.protocolVersion !== RUNTIME_RECOVERY_PROTOCOL_VERSION) return null;
  if (typeof record.requestId !== "string" || !REQUEST_ID_RE.test(record.requestId)) return null;
  if (typeof record.timestamp !== "number" || !Number.isSafeInteger(record.timestamp)) return null;
  if (record.action !== "probe" && record.action !== "recover") return null;
  return {
    msg_type: RUNTIME_RECOVERY_REQUEST_TYPE,
    protocolVersion: RUNTIME_RECOVERY_PROTOCOL_VERSION,
    requestId: record.requestId,
    timestamp: record.timestamp,
    action: record.action,
  };
}

/**
 * Builds the response. `message` stays a machine-readable short token or a
 * safe English phrase — Chinese copy is the web's job, and nothing about the
 * local machine (paths, stderr text, versions) ever leaves the device.
 */
export function buildRuntimeRecoveryResponse(
  request: RuntimeRecoveryRequest,
  result: RuntimeRecoveryResult,
  agentName: string | undefined,
  clock: () => number = Date.now,
): string {
  return JSON.stringify({
    msg_type: RUNTIME_RECOVERY_RESPONSE_TYPE,
    protocolVersion: RUNTIME_RECOVERY_PROTOCOL_VERSION,
    requestId: request.requestId,
    timestamp: clock(),
    code: result.code,
    message: result.message,
    data: {
      runtime: result.code,
      // This process is answering over IM right now, so its own worker is up
      // by construction; the interesting variable is the runtime probe above.
      worker: "online",
      agent: agentName ?? "",
    },
  });
}

// stderr/stdout fragments that mean "the CLI is there, it just is not logged
// in" — a distinct, actionable state from "the binary is broken".
const AUTH_FAILURE_RE =
  /unauthorized|authentication|not\s+logged\s+in|login\s+required|api[\s_-]?key|invalid[\s_-]?token|401|403|未登录|请先登录|鉴权|认证/i;

export function isAuthFailureMessage(message: string): boolean {
  return AUTH_FAILURE_RE.test(message);
}

export interface RuntimeProberOpts {
  agentName?: string;
  /** Absolute path of the agent binary this node runs. */
  execPath: string;
  /** Runs one short agent subcommand; see agents.ts runAgentCommand. */
  runCommand: (argv: string[], timeoutMs: number, maxOutputBytes: number) => Promise<string>;
  /**
   * Idempotent local repair (e.g. ensureOpencodeInstalled). Only called for
   * action="recover". A rejection is logged and degraded, never fatal: a failed
   * install must still produce an honest not_found rather than an exception.
   */
  ensureRuntime?: () => Promise<void>;
  /**
   * Ceiling for the real (recover-only) model-list probe. The web waits 20s
   * for a recover, so a cold `opencode models` (~10s) fits with room to spare.
   */
  recoverProbeTimeoutMs?: number;
  /** Injectable for tests; defaults to a real fs access(X_OK) check. */
  isExecutable?: (path: string) => Promise<boolean>;
  logger?: (message: string) => void;
}

export interface RuntimeProber {
  probe(action: RuntimeRecoveryAction): Promise<RuntimeRecoveryResult>;
}

export const DEFAULT_RECOVER_PROBE_TIMEOUT_MS = 20_000;

async function defaultIsExecutable(path: string): Promise<boolean> {
  try {
    await access(path, constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

/** Never rejects: every failure path becomes a code the web can explain. */
export function createRuntimeProber(opts: RuntimeProberOpts): RuntimeProber {
  const isExecutable = opts.isExecutable ?? defaultIsExecutable;
  const recoverTimeoutMs = opts.recoverProbeTimeoutMs ?? DEFAULT_RECOVER_PROBE_TIMEOUT_MS;
  const log = opts.logger ?? (() => {});

  return {
    async probe(action: RuntimeRecoveryAction): Promise<RuntimeRecoveryResult> {
      try {
        if (action === "recover" && opts.ensureRuntime) {
          // Before the stat(): ensureRuntime is what *installs* a missing
          // runtime, so checking first would report not_found on exactly the
          // device that a recover is supposed to fix.
          try {
            await opts.ensureRuntime();
          } catch (err) {
            log(`runtime recovery: ensureRuntime failed: ${describe(err)}`);
          }
        }

        if (!(await isExecutable(opts.execPath))) {
          return { code: "not_found", message: "agent runtime not found on PATH" };
        }

        // Cheap path: the binary exists and is executable. Deliberately no
        // spawn — see the cost model at the top of this file.
        if (action === "probe") {
          return { code: "ready", message: "runtime binary present" };
        }

        const spec = modelListSpec(opts.agentName);
        if (!spec) {
          return { code: "unsupported_agent", message: "no known runtime probe for this agent" };
        }

        const label = `${opts.agentName} ${spec.argv.join(" ")}`;
        try {
          const stdout = await opts.runCommand(spec.argv, recoverTimeoutMs, spec.maxOutputBytes);
          // Any successful exit is enough: parseModelCatalogOutput never throws
          // and an empty catalog (e.g. an unconfigured gateway) still means the
          // runtime itself answered.
          parseModelCatalogOutput(opts.agentName, stdout);
          log(`runtime recovery: ${label} ok`);
          return { code: "ready", message: "runtime ready" };
        } catch (err) {
          const message = describe(err);
          log(`runtime recovery: ${label} failed: ${message}`);
          if (isAuthFailureMessage(message)) {
            return { code: "needs_auth", message: "agent runtime requires login" };
          }
          return { code: "found_not_runnable", message: "agent runtime failed to run" };
        }
      } catch (err) {
        // A defect in the probe itself must never escape into the run loop and
        // take the node's message handling down with it.
        log(`runtime recovery: probe_failed: ${describe(err)}`);
        return { code: "probe_failed", message: "runtime probe failed" };
      }
    },
  };
}

/**
 * Serializes tasks and enforces a minimum gap between them.
 *
 * Used for recover: without it a burst of wake requests would each re-run the
 * (installing) ensureRuntime, and the model probe would pile up. Requests are
 * queued, not dropped, so the web still gets an answer for every requestId.
 */
export function createSerialGate(opts: {
  minIntervalMs: number;
  now?: () => number;
  sleep?: (ms: number) => Promise<void>;
}): <T>(task: () => Promise<T>) => Promise<T> {
  const now = opts.now ?? Date.now;
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
  let chain: Promise<unknown> = Promise.resolve();
  // null until the first task runs: the first caller must not wait.
  let lastStartedAt: number | null = null;

  return <T>(task: () => Promise<T>): Promise<T> => {
    const run = async (): Promise<T> => {
      const waitMs = lastStartedAt === null ? 0 : opts.minIntervalMs - (now() - lastStartedAt);
      if (waitMs > 0) await sleep(waitMs);
      lastStartedAt = now();
      return task();
    };
    // Errors must not break the chain for the tasks queued behind this one.
    const next = chain.then(run, run);
    chain = next.catch(() => undefined);
    return next;
  };
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
