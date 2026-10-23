// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  CUSTOM_MESSAGE_TYPES,
  SessionQueue,
  connectTransport,
  createInboundReplayDedupe,
  sendBusyDropNotice,
  sendStopDropNotice,
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

describe("sendBusyDropNotice (queue-full terminal frame)", () => {
  const msg: InboundIMMessage = {
    objectName: "RC:TxtMsg",
    fromUserId: "user_1",
    targetId: "ops_1",
    conversationType: 1,
    content: "{}",
    messageUId: "uid-9",
  } as unknown as InboundIMMessage;

  function fakeTransport() {
    const sent: Array<{ to: string; objectName: string; content: string }> = [];
    const transport = {
      sent,
      sendMessage(to: string, objectName: string, content: string): Promise<void> {
        sent.push({ to, objectName, content });
        return Promise.resolve();
      },
    };
    return transport as unknown as IMTransport & { sent: typeof sent };
  }

  it("sends a chat_stream failed frame with error_code=agent_busy", async () => {
    const transport = fakeTransport();
    sendBusyDropNotice(transport, msg, "ops_1");
    await new Promise((r) => setTimeout(r, 0));
    expect(transport.sent).toHaveLength(1);
    const { to, objectName, content } = transport.sent[0];
    expect(to).toBe("user_1");
    expect(objectName).toBe("chat_stream");
    const frame = JSON.parse(content);
    // 信封字段逐条对齐 web 端 validStreamDelivery 的私聊校验：
    // node_id=发送账号、conversation_id/requester_id=发消息的用户。
    expect(frame).toMatchObject({
      msg_type: "chat_stream",
      protocol_version: 1,
      status: "failed",
      error_code: "agent_busy",
      text: "",
      seq: 0,
      node_id: "ops_1",
      conversation_type: 1,
      conversation_id: "user_1",
      requester_id: "user_1",
      request_message_id: "uid-9",
    });
    expect(frame.stream_id).toContain("uid-9");
  });

  it("stays silent for non-private conversations or unknown self id", () => {
    const transport = fakeTransport();
    sendBusyDropNotice(transport, { ...msg, conversationType: 3 } as InboundIMMessage, "ops_1");
    sendBusyDropNotice(transport, msg, undefined);
    expect(transport.sent).toHaveLength(0);
  });
});

describe("sendStopDropNotice (chat_stop dropped a queued message)", () => {
  const msg: InboundIMMessage = {
    objectName: "RC:TxtMsg",
    fromUserId: "user_1",
    targetId: "ops_1",
    conversationType: 1,
    content: "{}",
    messageUId: "uid-10",
  } as unknown as InboundIMMessage;

  function fakeTransport() {
    const sent: Array<{ to: string; objectName: string; content: string }> = [];
    const transport = {
      sent,
      sendMessage(to: string, objectName: string, content: string): Promise<void> {
        sent.push({ to, objectName, content });
        return Promise.resolve();
      },
    };
    return transport as unknown as IMTransport & { sent: typeof sent };
  }

  it("sends a chat_stream cancelled terminal frame so the placeholder stops spinning", async () => {
    const transport = fakeTransport();
    sendStopDropNotice(transport, msg, "ops_1");
    await new Promise((r) => setTimeout(r, 0));
    expect(transport.sent).toHaveLength(1);
    const { to, objectName, content } = transport.sent[0];
    expect(to).toBe("user_1");
    expect(objectName).toBe("chat_stream");
    const frame = JSON.parse(content);
    expect(frame).toMatchObject({
      msg_type: "chat_stream",
      protocol_version: 1,
      status: "cancelled",
      text: "",
      seq: 0,
      node_id: "ops_1",
      conversation_type: 1,
      conversation_id: "user_1",
      requester_id: "user_1",
      request_message_id: "uid-10",
    });
  });

  it("stays silent for non-private conversations or unknown self id", () => {
    const transport = fakeTransport();
    sendStopDropNotice(transport, { ...msg, conversationType: 4 } as InboundIMMessage, "ops_1");
    sendStopDropNotice(transport, msg, undefined);
    expect(transport.sent).toHaveLength(0);
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
    expect(q.enqueue("conv-1", slow)).toBe(true);
    expect(q.enqueue("conv-1", slow)).toBe(true); // queued (depth 1)
    expect(q.enqueue("conv-1", slow)).toBe(false); // busy -> dropped, caller must answer
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

  it("dropWaiting cancels queued tasks, fires onDropped, and unblocks idle()", async () => {
    const q = new SessionQueue({ concurrencyPerConversation: 1, queueDepth: 2 });
    let ran = 0;
    const running = new Promise<void>((resolve) => {
      q.enqueue("conv-1", async () => {
        ran++;
        await new Promise((r) => setTimeout(r, 30));
        resolve();
      });
    });
    const droppedFor: string[] = [];
    q.enqueue("conv-1", async () => { ran++; }, () => { droppedFor.push("second"); });
    q.enqueue("conv-1", async () => { ran++; }, () => { droppedFor.push("third"); });
    expect(q.dropWaiting("conv-1")).toBe(2);
    expect(droppedFor).toEqual(["second", "third"]);
    // idle 不能被卡死：撤掉的等待任务永远不会跑 finish，dropWaiting 必须补检查。
    await q.idle();
    await running;
    expect(ran).toBe(1); // 只有正在执行的那个跑完，排队的两个被撤
    // 撤空之后队列还能正常接活。
    let ranAfter = false;
    q.enqueue("conv-1", async () => { ranAfter = true; });
    await q.idle();
    expect(ranAfter).toBe(true);
  });

  it("dropWaiting returns 0 and is a no-op when nothing is queued", async () => {
    const q = new SessionQueue({ concurrencyPerConversation: 1 });
    let ran = 0;
    const onDropped = vi.fn();
    q.enqueue("conv-1", async () => { ran++; });
    expect(q.dropWaiting("conv-1")).toBe(0);
    expect(onDropped).not.toHaveBeenCalled();
    await q.idle();
    expect(ran).toBe(1);
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

describe("CUSTOM_MESSAGE_TYPES（融云注册表）", () => {
  it("包含 chat_stream 与 chat_stream_chunk（>9KB 帧的 wire 分块）", () => {
    const names = CUSTOM_MESSAGE_TYPES.map(([name]) => name);
    expect(names).toContain("chat_stream");
    expect(names).toContain("chat_stream_chunk");
    // 分块与主帧同策略：持久化存历史、不计未读。
    const chunk = CUSTOM_MESSAGE_TYPES.find(([name]) => name === "chat_stream_chunk")!;
    expect(chunk[1]).toBe(true);
    expect(chunk[2]).toBe(false);
  });

  it("web 端注册的流类型这里一个都不能少（缺了融云直接拒发）", () => {
    // web 端 src/services/im.ts CUSTOM_MESSAGE_TYPES 里与本端同源的类型。
    const webSide = ["chat_stream", "chat_stream_chunk", "chat_stop", "chat_stop_result",
      "command", "card_message", "card_update", "card_action"];
    const names = new Set(CUSTOM_MESSAGE_TYPES.map(([name]) => name));
    for (const type of webSide) expect(names.has(type)).toBe(true);
  });
});

describe("SendThrottle（全连接串行发送队列）", () => {
  it("相邻发送间隔不小于 220ms，且错误不卡死队列", async () => {
    vi.useFakeTimers();
    const { SendThrottle } = await import("./run.js");
    const throttle = new SendThrottle();
    const stamps: number[] = [];
    const p1 = throttle.run(async () => { stamps.push(Date.now()); });
    const p2 = throttle.run(async () => { stamps.push(Date.now()); });
    const p3 = throttle.run(async () => { throw new Error("boom"); });
    const p4 = throttle.run(async () => { stamps.push(Date.now()); });
    await vi.runAllTimersAsync();
    await Promise.all([p1, p2, p3.catch(() => {}), p4]);
    expect(stamps.length).toBe(3);
    for (let i = 1; i < stamps.length; i += 1) {
      expect(stamps[i] - stamps[i - 1]).toBeGreaterThanOrEqual(200);
    }
    await expect(p3).rejects.toThrow("boom");
    vi.useRealTimers();
  });
});

describe("聊天室成员身份（23406 NOT_IN_CHATROOM）", () => {
  it("只把「不在聊天室 / 聊天室不存在」当作需要重新加入", async () => {
    const { isChatroomMembershipError } = await import("./run.js");
    expect(isChatroomMembershipError(23406)).toBe(true);
    expect(isChatroomMembershipError(23410)).toBe(true);
    // 限频、禁言、参数错误都不是成员身份问题
    expect(isChatroomMembershipError(20604)).toBe(false);
    expect(isChatroomMembershipError(23408)).toBe(false);
    expect(isChatroomMembershipError(23412)).toBe(false);
  });

  it("会员身份丢失 → 重新加入后重发；限频 → 退避重发；其余 → 放弃", async () => {
    const { planSendFailure } = await import("./run.js");
    expect(planSendFailure(23406, 1)).toBe("rejoin");
    expect(planSendFailure(23410, 1)).toBe("rejoin");
    // 重新加入的次数有上限，避免房间彻底不可用时死循环
    expect(planSendFailure(23406, 2)).toBe("fail");
    expect(planSendFailure(20604, 1)).toBe("retry");
    expect(planSendFailure(20604, 9)).toBe("fail");
    expect(planSendFailure(23408, 1)).toBe("fail");
  });
});
