// @vitest-environment node
import { describe, expect, it } from "vitest";
import { MessageDispatcher } from "./im.js";

function makeDeps() {
  const sent: Array<{ to: string; objectName: string; content: string }> = [];
  return {
    sent,
    send: async (to: string, objectName: string, content: string) => {
      sent.push({ to, objectName, content });
    },
  };
}

describe("MessageDispatcher single chat", () => {
  it("replies to RC:TxtMsg with agent output as RC:TxtMsg", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "RC:TxtMsg",
        fromUserId: "user_1",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: JSON.stringify({ content: "hi agent" }),
      },
      { runTurn: async (prompt) => `echo:${prompt}` },
    );
    expect(deps.sent).toHaveLength(1);
    expect(deps.sent[0].objectName).toBe("RC:TxtMsg");
    expect(deps.sent[0].to).toBe("user_1");
    const parsed = JSON.parse(deps.sent[0].content);
    expect(parsed.content).toContain("echo:hi agent");
  });

  it("does not reply when agent turn fails (logs only)", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "RC:TxtMsg",
        fromUserId: "user_1",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: JSON.stringify({ content: "q" }),
      },
      { runTurn: async () => { throw new Error("boom"); } },
    );
    expect(deps.sent).toHaveLength(0);
  });
});

describe("MessageDispatcher html cards", () => {
  const REPLY_WITH_HTML = [
    "这是页面：",
    "",
    "```html",
    "<h1>v1</h1>",
    "```",
  ].join("\n");

  function txtMsg(content: string): Parameters<MessageDispatcher["handle"]>[0] {
    return {
      objectName: "RC:TxtMsg",
      fromUserId: "user_1",
      toUserId: "rc_node_x",
      targetId: "rc_node_x",
      conversationType: 1,
      content,
    };
  }

  function cardAction(cardId: string | undefined, payload: Record<string, unknown>): Parameters<MessageDispatcher["handle"]>[0] {
    return {
      objectName: "card_action",
      fromUserId: "user_1",
      toUserId: "rc_node_x",
      targetId: "rc_node_x",
      conversationType: 1,
      content: JSON.stringify({
        msg_type: "card_action",
        cardId,
        buttonId: "revise",
        action: { type: "custom", kind: "html_revise", payload },
      }),
    };
  }

  it("sends a card_message when the agent reply contains an ```html fence", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(txtMsg(JSON.stringify({ content: "做个页面" })), {
      runTurn: async () => REPLY_WITH_HTML,
    });
    expect(deps.sent).toHaveLength(1);
    const sent = deps.sent[0];
    expect(sent.objectName).toBe("card_message");
    expect(sent.to).toBe("user_1");
    const parsed = JSON.parse(sent.content);
    expect(parsed.msg_type).toBe("card_message");
    expect(parsed.card.id).toMatch(/^card-/);
    expect(parsed.card.sections.some((s: { kind: string }) => s.kind === "html")).toBe(true);
    const md = parsed.card.sections.find((s: { kind: string }) => s.kind === "markdown");
    expect(md.content).toContain("这是页面：");
  });

  it("replies as plain RC:TxtMsg when the reply has no html fence", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(txtMsg(JSON.stringify({ content: "你好" })), {
      runTurn: async () => "你好！有什么可以帮你？",
    });
    expect(deps.sent).toHaveLength(1);
    expect(deps.sent[0].objectName).toBe("RC:TxtMsg");
  });

  it("iterates via card_action: reruns the agent and card_update-replaces the same card", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(txtMsg(JSON.stringify({ content: "做个页面" })), {
      runTurn: async () => REPLY_WITH_HTML,
    });
    const first = JSON.parse(deps.sent[0].content);
    const cardId = first.card.id as string;

    const prompts: string[] = [];
    await dispatcher.handle(cardAction(cardId, { inputValue: "换成蓝色主题" }), {
      runTurn: async (prompt) => {
        prompts.push(prompt);
        return "```html\n<h1>v2 blue</h1>\n```";
      },
    });

    // The revise prompt embeds the previous html + the instruction.
    expect(prompts).toHaveLength(1);
    expect(prompts[0]).toContain("<h1>v1</h1>");
    expect(prompts[0]).toContain("换成蓝色主题");

    expect(deps.sent).toHaveLength(2);
    const update = deps.sent[1];
    expect(update.objectName).toBe("card_update");
    expect(update.to).toBe("user_1");
    const parsed = JSON.parse(update.content);
    expect(parsed.msg_type).toBe("card_update");
    expect(parsed.cardId).toBe(cardId);
    expect(parsed.mode).toBe("replace");
    expect(parsed.card.id).toBe(cardId);
    const html = parsed.card.sections.find((s: { kind: string }) => s.kind === "html");
    expect(html.content).toContain("<h1>v2 blue</h1>");
  });

  it("falls back to the user's latest card when cardId is unknown", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(txtMsg(JSON.stringify({ content: "做个页面" })), {
      runTurn: async () => REPLY_WITH_HTML,
    });
    await dispatcher.handle(cardAction("card-does-not-exist", { inputValue: "加个标题" }), {
      runTurn: async () => "```html\n<h1>v3</h1>\n```",
    });
    const update = deps.sent[1];
    expect(update.objectName).toBe("card_update");
    const parsed = JSON.parse(update.content);
    expect(parsed.cardId).toMatch(/^card-/);
    expect(parsed.cardId).not.toBe("card-does-not-exist");
  });

  it("ignores card_action without an instruction and other action kinds", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(txtMsg(JSON.stringify({ content: "做个页面" })), {
      runTurn: async () => REPLY_WITH_HTML,
    });
    const before = deps.sent.length;
    // Empty instruction
    await dispatcher.handle(cardAction(undefined, { inputValue: "  " }), { runTurn: async () => "x" });
    // Different action kind
    await dispatcher.handle(
      {
        objectName: "card_action",
        fromUserId: "user_1",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: JSON.stringify({
          msg_type: "card_action",
          cardId: "whatever",
          action: { type: "custom", kind: "other_kind", payload: { inputValue: "hi" } },
        }),
      },
      { runTurn: async () => "x" },
    );
    expect(deps.sent.length).toBe(before);
  });

  it("surfaces revise failures as a plain text error reply", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(txtMsg(JSON.stringify({ content: "做个页面" })), {
      runTurn: async () => REPLY_WITH_HTML,
    });
    const first = JSON.parse(deps.sent[0].content);
    await dispatcher.handle(cardAction(first.card.id, { inputValue: "再改" }), {
      runTurn: async () => { throw new Error("timeout"); },
    });
    const error = deps.sent[1];
    expect(error.objectName).toBe("RC:TxtMsg");
    const parsed = JSON.parse(error.content);
    expect(parsed.content).toContain("生成失败");
    expect(parsed.content).toContain("timeout");
  });

  it("replies as text when a revise turn returns no html fence", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(txtMsg(JSON.stringify({ content: "做个页面" })), {
      runTurn: async () => REPLY_WITH_HTML,
    });
    const first = JSON.parse(deps.sent[0].content);
    await dispatcher.handle(cardAction(first.card.id, { inputValue: "再改" }), {
      runTurn: async () => "抱歉，我无法生成 HTML。",
    });
    const reply = deps.sent[1];
    expect(reply.objectName).toBe("RC:TxtMsg");
    expect(JSON.parse(reply.content).content).toContain("抱歉");
  });
});

describe("MessageDispatcher discussion your_turn", () => {
  const CHATROOM_ID = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6";

  it("replies with command_result echoing request_id", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "command",
        fromUserId: "host_node",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: JSON.stringify({
          request_id: "turn_1_1_20260929",
          service: "discussion",
          action: "your_turn",
          params: { round: 1, speaking_order: 1, role_name: "reviewer", model: "sonnet-4", chatroom_id: CHATROOM_ID },
        }),
      },
      { runTurn: async (prompt, model) => `out(${model}) ${prompt.length > 0 ? "prompt" : "noprompt"}` },
    );
    expect(deps.sent).toHaveLength(1);
    expect(deps.sent[0].objectName).toBe("command");
    expect(deps.sent[0].to).toBe(CHATROOM_ID);
    const parsed = JSON.parse(deps.sent[0].content);
    expect(parsed.request_id).toBe("turn_1_1_20260929");
    expect(parsed.msg_type).toBe("command_result");
    expect(parsed.payload.kind).toBe("text");
    expect(parsed.payload.content).toContain("out(sonnet-4)");
  });

  it("replies with error command_result when the turn throws", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "command",
        fromUserId: "host_node",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: JSON.stringify({
          request_id: "r2",
          service: "discussion",
          action: "your_turn",
          params: { chatroom_id: CHATROOM_ID },
        }),
      },
      { runTurn: async () => { throw new Error("timeout"); } },
    );
    expect(deps.sent).toHaveLength(1);
    expect(deps.sent[0].to).toBe(CHATROOM_ID);
    const parsed = JSON.parse(deps.sent[0].content);
    expect(parsed.msg_type).toBe("command_result");
    expect(parsed.request_id).toBe("r2");
    expect(parsed.payload.kind).toBe("error");
    expect(parsed.payload.error).toBe("timeout");
  });

  it("ignores unknown commands and other objectNames", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    for (const objectName of ["RC:ImgMsg", "command"]) {
      await dispatcher.handle(
        {
          objectName,
          fromUserId: "u",
          toUserId: "rc_node_x",
          targetId: "rc_node_x",
          conversationType: 1,
          content: objectName === "command"
            ? JSON.stringify({ request_id: "r", service: "x", action: "other_action", params: {} })
            : "{}",
        },
        { runTurn: async () => "x" },
      );
    }
    expect(deps.sent).toHaveLength(0);
  });
});

// Cross-contract suite: pins the envelope the Go webhook gate requires
// (server/internal/integrations/rongcloud): a "command"-objectName message
// whose content carries the LITERAL msg_type "command_result"
// (channel.go isCommandResult gate), and whose webhook targetId — the
// private-message receiver — is the chatroom UUID from params.chatroom_id
// so dispatchToCoordinator can parse it (pgx accepts the 32-hex form
// emitted by chatroomKey). Mirrors rongcloud_test.go
// TestCommandResultContentMarshal expectations.
describe("MessageDispatcher Go-server envelope contract", () => {
  const CHATROOM_ID = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6";

  function yourTurnContent(): string {
    return JSON.stringify({
      request_id: "turn_1_1_20260929120000",
      service: "discussion",
      action: "your_turn",
      params: { round: 1, speaking_order: 1, chatroom_id: CHATROOM_ID },
    });
  }

  it("sends the exact JSON envelope the Go gate requires (success)", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "command",
        fromUserId: "host_node",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: yourTurnContent(),
      },
      { runTurn: async () => "final answer" },
    );
    expect(deps.sent).toHaveLength(1);
    const sent = deps.sent[0];
    // Gate: objectName must be the literal "command".
    expect(sent.objectName).toBe("command");
    // Addressing: private message TO the chatroom id -> webhook targetId
    // becomes the chatroom UUID the coordinator dispatches on.
    expect(sent.to).toBe(CHATROOM_ID);
    // Content: parses as JSON with the literal msg_type the gate checks
    // BEFORE any CommandContent interpretation.
    const content = JSON.parse(sent.content);
    expect(content.msg_type).toBe("command_result");
    expect(content.request_id).toBe("turn_1_1_20260929120000");
    // Server handleCommandResult extracts only payload.content (string).
    expect(content.payload.content).toBe("final answer");
    expect(content.payload.kind).toBe("text");
  });

  it("sends the exact JSON envelope the Go gate requires (failure)", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "command",
        fromUserId: "host_node",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: yourTurnContent(),
      },
      { runTurn: async () => { throw new Error("agent crash"); } },
    );
    const sent = deps.sent[0];
    expect(sent.objectName).toBe("command");
    expect(sent.to).toBe(CHATROOM_ID);
    const content = JSON.parse(sent.content);
    expect(content.msg_type).toBe("command_result");
    expect(content.request_id).toBe("turn_1_1_20260929120000");
    expect(content.payload.error).toBe("agent crash");
    expect(content.payload.kind).toBe("error");
  });

  it("still completes a discussion reply when params.chatroom_id is missing", async () => {
    // Defensive: older coordinators may omit chatroom_id; the reply must
    // still carry the command_result envelope (fall back to the sender).
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "command",
        fromUserId: "host_node",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: JSON.stringify({
          request_id: "r_no_room",
          service: "discussion",
          action: "your_turn",
          params: {},
        }),
      },
      { runTurn: async () => "ok" },
    );
    const sent = deps.sent[0];
    expect(sent.objectName).toBe("command");
    expect(sent.to).toBe("host_node");
    const content = JSON.parse(sent.content);
    expect(content.msg_type).toBe("command_result");
    expect(content.request_id).toBe("r_no_room");
  });
});

describe("MessageDispatcher self-echo loop guard", () => {
  // 融云会把本端自己发出的消息同步回本端；Node 环境没有 IndexDB，SDK 无法
  // 去重。若节点把自己的回复当用户提问，就会无限自问自答，并把会话队列占满，
  // 使真实的模型目录请求被丢弃（设备管理 → 默认模型 一直转圈）。
  it("ignores text messages sent by this node itself", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send, selfUserId: "rc_node_self" });
    let turns = 0;
    await dispatcher.handle(
      {
        objectName: "RC:TxtMsg",
        fromUserId: "rc_node_self",
        toUserId: "",
        targetId: "rc_node_self",
        conversationType: 1,
        content: JSON.stringify({ content: "好的，我在。请发需求。" }),
      },
      { runTurn: async () => { turns += 1; return "should not run"; } },
    );
    expect(turns).toBe(0);
    expect(deps.sent).toHaveLength(0);
  });

  it("still answers the same text when it comes from a real user", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send, selfUserId: "rc_node_self" });
    await dispatcher.handle(
      {
        objectName: "RC:TxtMsg",
        fromUserId: "user_1",
        toUserId: "",
        targetId: "user_1",
        conversationType: 1,
        content: JSON.stringify({ content: "在吗" }),
      },
      { runTurn: async () => "在的" },
    );
    expect(deps.sent).toHaveLength(1);
    expect(deps.sent[0].to).toBe("user_1");
  });

  it("answers a model catalog request from a real user even with the guard on", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({
      send: deps.send,
      selfUserId: "rc_node_self",
      loadModelCatalog: async () => ({
        defaultModel: null,
        providers: [{ id: "prov", name: "prov", models: [{ id: "m", name: "m" }] }],
      }),
    });
    await dispatcher.handle(
      {
        objectName: "command",
        fromUserId: "user_1",
        toUserId: "",
        targetId: "user_1",
        conversationType: 1,
        content: JSON.stringify({
          msg_type: "discussion_model_catalog_request",
          protocolVersion: 2,
          requestId: "req-1",
          timestamp: Date.now(),
        }),
      },
      { runTurn: async () => "unused" },
    );
    expect(deps.sent).toHaveLength(1);
    expect(deps.sent[0].to).toBe("user_1");
    const body = JSON.parse(deps.sent[0].content);
    expect(body.msg_type).toBe("discussion_model_catalog_response");
    expect(body.requestId).toBe("req-1");
    expect(body.providers).toHaveLength(1);
  });
});

describe("MessageDispatcher model catalog replay guards", () => {
  // 融云在每次重连时会把整段离线消息重放一遍（Node 端没有 IndexDB，SDK 无法
  // 去重）。若不设防，一次重连就会在同一个 tick 内发出几十条应答，被融云的
  // 发送频率限制（20604）整体拒绝 —— web 的「默认模型」面板因此永远拿不到目录。
  function requestContent(requestId: string, timestamp?: number): string {
    return JSON.stringify({
      msg_type: "discussion_model_catalog_request",
      protocolVersion: 2,
      requestId,
      ...(timestamp === undefined ? {} : { timestamp }),
    });
  }

  function requestMessage(requestId: string, timestamp?: number) {
    return {
      objectName: "command",
      fromUserId: "user_1",
      toUserId: "",
      targetId: "user_1",
      conversationType: 1,
      content: requestContent(requestId, timestamp),
    };
  }

  const catalog = {
    defaultModel: null,
    providers: [{ id: "prov", name: "prov", models: [{ id: "m", name: "m" }] }],
  };

  it("does not answer a request the web has already timed out on", async () => {
    const deps = makeDeps();
    let loads = 0;
    const dispatcher = new MessageDispatcher({
      send: deps.send,
      loadModelCatalog: async () => { loads += 1; return catalog; },
    });
    await dispatcher.handle(requestMessage("req-old", Date.now() - 5 * 60_000), {
      runTurn: async () => "unused",
    });
    expect(loads).toBe(0);
    expect(deps.sent).toHaveLength(0);
  });

  it("answers each requestId at most once, however many times it is replayed", async () => {
    const deps = makeDeps();
    let loads = 0;
    const dispatcher = new MessageDispatcher({
      send: deps.send,
      loadModelCatalog: async () => { loads += 1; return catalog; },
    });
    const turns = { runTurn: async () => "unused" };
    for (let i = 0; i < 3; i += 1) {
      await dispatcher.handle(requestMessage("req-dup", Date.now()), turns);
    }
    expect(loads).toBe(1);
    expect(deps.sent).toHaveLength(1);
    expect(JSON.parse(deps.sent[0].content).requestId).toBe("req-dup");
  });

  it("keeps a requestId retryable when the response never left the node", async () => {
    const sent: string[] = [];
    let attempts = 0;
    const dispatcher = new MessageDispatcher({
      send: async (_to, _objectName, content) => {
        attempts += 1;
        if (attempts === 1) throw new Error("imlib sendMessage failed: 20604");
        sent.push(content);
      },
      loadModelCatalog: async () => catalog,
    });
    const turns = { runTurn: async () => "unused" };
    await expect(dispatcher.handle(requestMessage("req-retry", Date.now()), turns)).rejects.toThrow();
    await dispatcher.handle(requestMessage("req-retry", Date.now()), turns);
    expect(sent).toHaveLength(1);
    expect(JSON.parse(sent[0]).requestId).toBe("req-retry");
  });
});
