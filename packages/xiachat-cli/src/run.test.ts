// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  SessionQueue,
  connectTransport,
  createBoundedMap,
  createInboundReplayDedupe,
  startRunLoop,
} from "./run.js";
import type { IMTransport, InboundIMMessage } from "./im.js";
import { XiachatApi } from "./api.js";

describe("createInboundReplayDedupe (offline replay storm)", () => {
  it("treats each UID as new once, then as a duplicate", () => {
    const dedupe = createInboundReplayDedupe();
    expect(dedupe.isDuplicate("uid-1")).toBe(false);
    expect(dedupe.isDuplicate("uid-1")).toBe(true);
    expect(dedupe.isDuplicate("uid-2")).toBe(false);
  });

  it("never dedupes when the SDK omits the UID", () => {
    const dedupe = createInboundReplayDedupe();
    expect(dedupe.isDuplicate(undefined)).toBe(false);
    expect(dedupe.isDuplicate(undefined)).toBe(false);
  });

  it("stays bounded, forgetting the oldest UIDs", () => {
    const dedupe = createInboundReplayDedupe(2);
    expect(dedupe.isDuplicate("a")).toBe(false);
    expect(dedupe.isDuplicate("b")).toBe(false);
    expect(dedupe.isDuplicate("c")).toBe(false);
    expect(dedupe.size).toBe(2);
    // "a" was evicted, so it looks new again — bounded memory over exactness.
    expect(dedupe.isDuplicate("a")).toBe(false);
    expect(dedupe.isDuplicate("c")).toBe(true);
  });

  it("remembers handled UIDs across restarts so a reconnect is a no-op", () => {
    const dir = mkdtempSync(join(tmpdir(), "clawmessenger-uids-"));
    const path = join(dir, "processed-uids.json");
    try {
      const first = createInboundReplayDedupe({ path });
      expect(first.isDuplicate("uid-1")).toBe(false);
      first.flush();

      // A fresh process (every restart, and every reconnect after a crash)
      // loads the marker and skips what it already answered.
      const second = createInboundReplayDedupe({ path });
      expect(second.size).toBe(1);
      expect(second.isDuplicate("uid-1")).toBe(true);
      expect(second.isDuplicate("uid-2")).toBe(false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("starts clean when the marker file is missing or corrupt", () => {
    const dir = mkdtempSync(join(tmpdir(), "clawmessenger-uids-"));
    const path = join(dir, "processed-uids.json");
    try {
      expect(createInboundReplayDedupe({ path }).size).toBe(0);
      writeFileSync(path, "{ not json");
      expect(createInboundReplayDedupe({ path }).size).toBe(0);
      writeFileSync(path, JSON.stringify(["ok", 42, null]));
      expect(createInboundReplayDedupe({ path }).size).toBe(1);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe("createBoundedMap (busyPeer cap)", () => {
  it("evicts the oldest entry past the cap", () => {
    const m = createBoundedMap(3);
    m.set("a", "1");
    m.set("b", "2");
    m.set("c", "3");
    m.set("d", "4");
    expect(m.size).toBe(3);
    expect(m.get("a")).toBeUndefined();
    expect(m.get("d")).toBe("4");
  });

  it("re-setting an existing key refreshes recency", () => {
    const m = createBoundedMap(3);
    m.set("a", "1");
    m.set("b", "2");
    m.set("a", "1b"); // refresh a to most-recent
    m.set("c", "3");
    m.set("d", "4"); // evicts b, not a
    expect(m.size).toBe(3);
    expect(m.get("a")).toBe("1b");
    expect(m.get("b")).toBeUndefined();
    expect(m.get("d")).toBe("4");
  });

  it("updates the value of an existing key", () => {
    const m = createBoundedMap(2);
    m.set("a", "1");
    m.set("a", "2");
    expect(m.size).toBe(1);
    expect(m.get("a")).toBe("2");
  });
});

describe("SessionQueue", () => {
  it("serializes messages within one conversation", async () => {
    const q = new SessionQueue({ concurrencyPerConversation: 1 });
    const order: number[] = [];
    const task = (n: number) => async () => {
      order.push(n);
      await new Promise((r) => setTimeout(r, 20));
      order.push(-n);
    };
    q.enqueue("conv-1", task(1));
    q.enqueue("conv-1", task(2));
    await q.idle();
    // task 2 must not start before task 1 finished
    expect(order.indexOf(-1)).toBeLessThan(order.indexOf(2));
  });

  it("runs different conversations in parallel", async () => {
    const q = new SessionQueue({ concurrencyPerConversation: 1 });
    const started: string[] = [];
    const slow = (id: string, ms: number) => async () => {
      started.push(id);
      await new Promise((r) => setTimeout(r, ms));
    };
    q.enqueue("conv-a", slow("a", 60));
    q.enqueue("conv-b", slow("b", 10));
    await q.idle();
    // b finished while a was still running -> both started before either idle
    expect(started).toContain("a");
    expect(started).toContain("b");
    expect(started.indexOf("b")).toBeLessThan(2);
  });

  it("busy message replies once when queue is at depth", async () => {
    const onBusy = vi.fn();
    const q = new SessionQueue({ concurrencyPerConversation: 1, queueDepth: 1, onBusy });
    const slow = async () => { await new Promise((r) => setTimeout(r, 40)); };
    q.enqueue("conv-1", slow);
    q.enqueue("conv-1", slow); // queued (depth 1)
    q.enqueue("conv-1", slow); // busy -> onBusy("conv-1")
    await q.idle();
    expect(onBusy).toHaveBeenCalledTimes(1);
    expect(onBusy).toHaveBeenCalledWith("conv-1");
  });

  it("keeps draining when a task throws synchronously", async () => {
    const q = new SessionQueue({ concurrencyPerConversation: 1 });
    const done: string[] = [];
    q.enqueue("conv-1", (() => { throw new Error("sync boom"); }) as () => Promise<void>);
    q.enqueue("conv-1", async () => { done.push("recovered"); });
    await q.idle();
    expect(done).toEqual(["recovered"]);
  });

  it("idle() resolves after a busy-dropped task leaves the queue", async () => {
    const q = new SessionQueue({ concurrencyPerConversation: 1, queueDepth: 1 });
    let ran = 0;
    const slow = async () => {
      ran++;
      await new Promise((r) => setTimeout(r, 10));
    };
    q.enqueue("conv-1", slow);
    q.enqueue("conv-1", slow);
    q.enqueue("conv-1", slow); // dropped as busy
    await q.idle();
    expect(ran).toBe(2);
    // Queue must accept new work after going idle.
    let ranAfter = false;
    q.enqueue("conv-1", async () => { ranAfter = true; });
    await q.idle();
    expect(ranAfter).toBe(true);
  });
});

describe("connectTransport (token-expiry auto-reconnect, spec 9.1)", () => {
  function makeHarness(opts: {
    storedToken: string;
    connectAttempts: Array<(token: string) => Promise<void>>;
    refreshToken?: (nodeId: string) => Promise<{ token: string }>;
  }) {
    const connectCalls: string[] = [];
    const transport: IMTransport = {
      connect: (appKey: string, token: string) => {
        connectCalls.push(token);
        const impl = opts.connectAttempts[connectCalls.length - 1];
        return impl(token);
      },
      sendMessage: async () => {},
      onMessage: () => {},
      disconnect: async () => {},
    };
    const api = new XiachatApi("http://unused.local");
    const refreshToken = opts.refreshToken
      ?? vi.fn(async () => ({ token: "fresh-token" }));
    vi.spyOn(api, "refreshToken").mockImplementation(refreshToken);
    vi.spyOn(api, "getConfig").mockImplementation(async () => ({ appKey: "appkey-1" }));
    return {
      transport,
      api,
      connectCalls,
      run: () =>
        connectTransport({
          creds: { nodeId: "node_1", token: opts.storedToken, serverUrl: "http://unused.local", appKey: "appkey-1" },
          api,
          transport,
        }),
    };
  }

  it("connects with the stored token when it is still valid", async () => {
    const h = makeHarness({
      storedToken: "valid-token",
      connectAttempts: [async () => {}],
    });
    await h.run();
    expect(h.connectCalls).toEqual(["valid-token"]);
  });

  it("refreshes the token once on connect failure and retries with the fresh token", async () => {
    const h = makeHarness({
      storedToken: "expired-token",
      connectAttempts: [
        async () => { throw new Error("imlib connect failed: 31004 token expired"); },
        async () => {},
      ],
    });
    await h.run();
    // First connect used the stale stored token; the single retry used the
    // refreshed token (and nothing else).
    expect(h.connectCalls).toEqual(["expired-token", "fresh-token"]);
  });

  it("propagates the failure when the retried connect also fails", async () => {
    const h = makeHarness({
      storedToken: "expired-token",
      connectAttempts: [
        async () => { throw new Error("imlib connect failed: 31004"); },
        async () => { throw new Error("imlib connect failed: 31004"); },
      ],
    });
    await expect(h.run()).rejects.toThrow("31004");
    expect(h.connectCalls).toEqual(["expired-token", "fresh-token"]);
  });

  it("does not retry more than once", async () => {
    const calls: string[] = [];
    const transport: IMTransport = {
      connect: (_appKey: string, token: string) => {
        calls.push(token);
        return Promise.reject(new Error("imlib connect failed: 31004"));
      },
      sendMessage: async () => {},
      onMessage: () => {},
      disconnect: async () => {},
    };
    const api = new XiachatApi("http://unused.local");
    const refresh = vi.fn(async () => ({ token: "fresh-token" }));
    vi.spyOn(api, "refreshToken").mockImplementation(refresh);
    vi.spyOn(api, "getConfig").mockImplementation(async () => ({ appKey: "appkey-1" }));
    await expect(
      connectTransport({
        creds: { nodeId: "node_1", token: "stale", serverUrl: "http://unused.local", appKey: "appkey-1" },
        api,
        transport,
      }),
    ).rejects.toThrow("31004");
    expect(calls).toEqual(["stale", "fresh-token"]);
    expect(refresh).toHaveBeenCalledTimes(1);
  });
});

describe("startRunLoop fast path", () => {
  interface SentFrame {
    to: string
    objectName: string
    content: string
  }

  function harness() {
    const sent: SentFrame[] = []
    const listeners: Array<(msg: InboundIMMessage) => void> = []
    let onWired: () => void = () => {}
    const wired = new Promise<void>((resolve) => { onWired = resolve })
    const transport: IMTransport = {
      connect: async () => {},
      sendMessage: async (to, objectName, content) => { sent.push({ to, objectName, content }) },
      onMessage: (cb) => { listeners.push(cb) },
      disconnect: async () => {},
      getSelfUserId: () => "rc_node_self",
    }
    const api = new XiachatApi("http://unused.local")
    vi.spyOn(api, "getConfig").mockImplementation(async () => ({ appKey: "appkey-1" }))
    return {
      sent,
      wired,
      transport,
      api,
      start: () =>
        void startRunLoop({
          creds: { nodeId: "node_1", token: "tok", serverUrl: "http://unused.local", appKey: "appkey-1" },
          api,
          transport,
          // Deliberately absent: the probe answers not_found without spawning,
          // which keeps this test free of real child processes.
          agentExecPath: join(tmpdir(), "clawmessenger-definitely-missing-opencode"),
          agentName: "opencode",
          stdout: { write: () => { onWired(); return true } } as unknown as NodeJS.WriteStream,
        }),
      emit: (content: string, objectName = "command") => {
        for (const cb of listeners) {
          cb({ objectName, fromUserId: "user_1", toUserId: "", targetId: "user_1", conversationType: 1, content })
        }
      },
    }
  }

  it("answers a runtime-recovery request end to end over the transport", async () => {
    const h = harness()
    h.start()
    await h.wired
    h.emit(JSON.stringify({
      msg_type: "claw_runtime_recovery_request",
      protocolVersion: 1,
      requestId: "rec-run-1",
      timestamp: Date.now(),
      action: "recover",
    }))
    await vi.waitFor(() => expect(h.sent).toHaveLength(1))
    const frame = JSON.parse(h.sent[0].content) as { msg_type: string; requestId: string; code: string }
    expect(h.sent[0].objectName).toBe("command")
    expect(h.sent[0].to).toBe("user_1")
    expect(frame.msg_type).toBe("claw_runtime_recovery_response")
    expect(frame.requestId).toBe("rec-run-1")
    expect(frame.code).toBe("not_found")
  })

  it("leaves ordinary traffic alone (a recovery request is not a command)", async () => {
    const h = harness()
    h.start()
    await h.wired
    // A discussion command payload must still reach its own branch, not the
    // recovery one; nothing here should be answered as a recovery response.
    h.emit(JSON.stringify({ msg_type: "claw_runtime_recovery_request", requestId: "x" }))
    h.emit(JSON.stringify({ content: "hello" }), "RC:TxtMsg")
    await new Promise((resolve) => setTimeout(resolve, 30))
    expect(h.sent.every((frame) => !frame.content.includes("claw_runtime_recovery_response"))).toBe(true)
  })
})
