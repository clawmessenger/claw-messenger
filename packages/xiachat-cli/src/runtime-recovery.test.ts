// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import {
  buildRuntimeRecoveryResponse,
  createRuntimeProber,
  createSerialGate,
  DEFAULT_RECOVER_PROBE_TIMEOUT_MS,
  isAuthFailureMessage,
  parseRuntimeRecoveryRequest,
  type RuntimeRecoveryRequest,
} from "./runtime-recovery.js";

function request(overrides: Record<string, unknown> = {}): string {
  return JSON.stringify({
    msg_type: "claw_runtime_recovery_request",
    protocolVersion: 1,
    requestId: "3f8a1c22-9b4d-4a7e-9c1d-3c1b2a0d9e77",
    timestamp: 1_760_270_000_000,
    action: "recover",
    ...overrides,
  });
}

const PARSED: RuntimeRecoveryRequest = {
  msg_type: "claw_runtime_recovery_request",
  protocolVersion: 1,
  requestId: "req-1",
  timestamp: 1_760_270_000_000,
  action: "probe",
};

describe("parseRuntimeRecoveryRequest", () => {
  it("accepts the two legal actions", () => {
    expect(parseRuntimeRecoveryRequest(request({ action: "probe" }))?.action).toBe("probe");
    expect(parseRuntimeRecoveryRequest(request({ action: "recover" }))?.action).toBe("recover");
  });

  it("rejects any key outside the exact whitelist", () => {
    // Each of these is the shape an injection attempt would take.
    for (const extra of [
      { path: "/bin/sh" },
      { args: ["-c", "rm -rf /"] },
      { shell: true },
      { commandLine: "opencode models" },
      { workdir: "/" },
      { content: { path: "/bin/sh" } },
    ]) {
      expect(parseRuntimeRecoveryRequest(request(extra))).toBeNull();
    }
  });

  it("rejects a missing key", () => {
    const full = JSON.parse(request()) as Record<string, unknown>;
    for (const key of Object.keys(full)) {
      const clone = { ...full };
      delete clone[key];
      expect(parseRuntimeRecoveryRequest(JSON.stringify(clone)), `missing ${key}`).toBeNull();
    }
  });

  it("rejects an unknown action literal", () => {
    for (const action of ["start", "stop", "recoverRuntime", "RECOVER", "", 1, null]) {
      expect(parseRuntimeRecoveryRequest(request({ action })), String(action)).toBeNull();
    }
  });

  it("rejects a malformed requestId", () => {
    for (const requestId of ["", "a".repeat(65), "has space", "semi;colon", "../etc/passwd", 42, null]) {
      expect(parseRuntimeRecoveryRequest(request({ requestId })), String(requestId)).toBeNull();
    }
  });

  it("rejects a non-integer or missing timestamp", () => {
    for (const timestamp of ["1760270000000", 1.5, Number.MAX_SAFE_INTEGER + 2, null]) {
      expect(parseRuntimeRecoveryRequest(request({ timestamp })), String(timestamp)).toBeNull();
    }
  });

  it("rejects a wrong protocolVersion or msg_type", () => {
    expect(parseRuntimeRecoveryRequest(request({ protocolVersion: 2 }))).toBeNull();
    expect(parseRuntimeRecoveryRequest(request({ msg_type: "discussion_model_catalog_request" }))).toBeNull();
  });

  it("rejects non-object JSON and garbage", () => {
    expect(parseRuntimeRecoveryRequest("not json")).toBeNull();
    expect(parseRuntimeRecoveryRequest("null")).toBeNull();
    expect(parseRuntimeRecoveryRequest("[]")).toBeNull();
    expect(parseRuntimeRecoveryRequest('"a string"')).toBeNull();
  });
});

describe("buildRuntimeRecoveryResponse", () => {
  it("emits exactly the seven wire keys", () => {
    const frame = JSON.parse(
      buildRuntimeRecoveryResponse(PARSED, { code: "ready", message: "runtime ready" }, "opencode", () => 42),
    ) as Record<string, unknown>;
    expect(Object.keys(frame).sort()).toEqual([
      "code", "data", "message", "msg_type", "protocolVersion", "requestId", "timestamp",
    ]);
    expect(frame.msg_type).toBe("claw_runtime_recovery_response");
    expect(frame.protocolVersion).toBe(1);
    expect(frame.requestId).toBe("req-1");
    expect(frame.timestamp).toBe(42);
    expect(frame.code).toBe("ready");
    expect(frame.data).toEqual({ runtime: "ready", worker: "online", agent: "opencode" });
  });

  it("never leaks a local path or stderr text through message", () => {
    const frame = JSON.parse(
      buildRuntimeRecoveryResponse(PARSED, { code: "not_found", message: "agent runtime not found on PATH" }, undefined),
    ) as { data: { agent: string } };
    expect(frame.data.agent).toBe("");
  });
});

describe("isAuthFailureMessage", () => {
  it("recognises the common credential failures", () => {
    for (const message of [
      "agent command exited with code 1: 401 Unauthorized",
      "Error: not logged in",
      "invalid api key",
      "请先登录后重试",
      "authentication required",
    ]) {
      expect(isAuthFailureMessage(message), message).toBe(true);
    }
  });

  it("does not misfire on a plain crash", () => {
    for (const message of ["agent command timeout after 20000ms", "spawn ENOENT", "exited with code 2"]) {
      expect(isAuthFailureMessage(message), message).toBe(false);
    }
  });
});

describe("createRuntimeProber", () => {
  const runCommand = vi.fn(async () => "openai/gpt-5.6-sol");

  function prober(overrides: Partial<Parameters<typeof createRuntimeProber>[0]> = {}) {
    return createRuntimeProber({
      agentName: "opencode",
      execPath: "/usr/local/bin/opencode",
      runCommand,
      isExecutable: async () => true,
      ...overrides,
    });
  }

  it("probe is the cheap path: executable means ready and the agent is never spawned", async () => {
    const spy = vi.fn(async () => "should not run");
    const result = await prober({ runCommand: spy }).probe("probe");
    expect(result.code).toBe("ready");
    expect(spy).not.toHaveBeenCalled();
  });

  it("probe reports not_found when the binary is missing", async () => {
    const result = await prober({ isExecutable: async () => false }).probe("probe");
    expect(result.code).toBe("not_found");
  });

  it("recover installs the runtime before stat(), so a missing install can be fixed", async () => {
    const order: string[] = [];
    const result = await prober({
      ensureRuntime: async () => { order.push("ensure") },
      isExecutable: async () => { order.push("stat"); return true },
      runCommand: async () => { order.push("run"); return "openai/gpt-5.6-sol" },
    }).probe("recover");
    expect(order).toEqual(["ensure", "stat", "run"]);
    expect(result.code).toBe("ready");
  });

  it("recover runs the model-list probe with the 20s ceiling", async () => {
    const spy = vi.fn(async () => "openai/gpt-5.6-sol");
    await prober({ runCommand: spy }).probe("recover");
    expect(spy).toHaveBeenCalledWith(["models"], DEFAULT_RECOVER_PROBE_TIMEOUT_MS, 256 * 1024);
  });

  it("a failed ensureRuntime degrades instead of throwing", async () => {
    const result = await prober({
      ensureRuntime: async () => { throw new Error("install failed") },
    }).probe("recover");
    expect(result.code).toBe("ready");
  });

  it("recover on an agent with no known probe reports unsupported_agent", async () => {
    const spy = vi.fn(async () => "");
    const result = await prober({ agentName: "codex", runCommand: spy }).probe("recover");
    expect(result.code).toBe("unsupported_agent");
    expect(spy).not.toHaveBeenCalled();
  });

  it("maps an auth failure to needs_auth and anything else to found_not_runnable", async () => {
    const auth = await prober({
      runCommand: async () => { throw new Error("agent command exited with code 1: 401 Unauthorized") },
    }).probe("recover");
    expect(auth.code).toBe("needs_auth");

    const broken = await prober({
      runCommand: async () => { throw new Error("agent command timeout after 20000ms") },
    }).probe("recover");
    expect(broken.code).toBe("found_not_runnable");
  });

  it("never lets an unexpected probe defect escape into the run loop", async () => {
    const result = await prober({
      isExecutable: async () => { throw new Error("EPERM") },
    }).probe("probe");
    expect(result.code).toBe("probe_failed");
  });
});

describe("createSerialGate", () => {
  it("serializes queued tasks and enforces the minimum interval", async () => {
    let clock = 0;
    const sleeps: number[] = [];
    const gate = createSerialGate({
      minIntervalMs: 5_000,
      now: () => clock,
      sleep: async (ms) => { sleeps.push(ms); clock += ms },
    });
    const order: string[] = [];
    const first = gate(async () => { order.push("a:start"); clock += 10; order.push("a:end") });
    const second = gate(async () => { order.push("b:start") });
    await Promise.all([first, second]);
    expect(order).toEqual(["a:start", "a:end", "b:start"]);
    // No sleep before the first task; the second waits out the 5s gap.
    expect(sleeps).toEqual([5_000 - 10]);
  });

  it("keeps the chain alive after a rejection", async () => {
    const gate = createSerialGate({ minIntervalMs: 0 });
    await expect(gate(async () => { throw new Error("boom") })).rejects.toThrow("boom");
    await expect(gate(async () => "ok")).resolves.toBe("ok");
  });
});
