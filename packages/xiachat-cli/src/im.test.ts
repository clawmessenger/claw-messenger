// @vitest-environment node
import { mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { ARGV_PROMPT_MAX_CHARS } from "./agents.js";
import { MessageDispatcher, planReadReceipt, resolveTurnVenue, turnSessionKey } from "./im.js";

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
  it("observes group/chatroom broadcast RC:TxtMsg without replying (relay arbitrates)", async () => {
    for (const conversationType of [3, 4]) {
      const deps = makeDeps();
      const dispatcher = new MessageDispatcher({ send: deps.send });
      await dispatcher.handle({
        objectName: "RC:TxtMsg",
        fromUserId: "user-1",
        toUserId: "node-a",
        targetId: "room-1",
        conversationType,
        content: JSON.stringify({ content: "大家好" }),
      }, { runTurn: async () => {
        throw new Error("group broadcast must not run the agent");
      } });
      expect(deps.sent.length).toBe(0);
    }
  });

  it("observes RC:TxtMsg with unknown/missing conversationType (fail-closed: never answer privately)", async () => {
    for (const conversationType of [0, undefined, "3"] as Array<number | undefined | string>) {
      const deps = makeDeps();
      const dispatcher = new MessageDispatcher({ send: deps.send });
      await dispatcher.handle({
        objectName: "RC:TxtMsg",
        fromUserId: "user-1",
        toUserId: "node-a",
        targetId: "room-1",
        conversationType: conversationType as number,
        content: JSON.stringify({ content: "大家好" }),
      }, { runTurn: async () => {
        throw new Error("unknown conversation type must not run the agent");
      } });
      expect(deps.sent.length).toBe(0);
    }
  });

  it("replies to private RC:TxtMsg with conversationType=1", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle({
      objectName: "RC:TxtMsg",
      fromUserId: "user-1",
      toUserId: "node-a",
      targetId: "node-a",
      conversationType: 1,
      content: JSON.stringify({ content: "你好" }),
    }, { runTurn: async (prompt) => `echo:${prompt}` });
    expect(deps.sent.length).toBe(1);
    expect(deps.sent[0].objectName).toBe("RC:TxtMsg");
  });

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

  // 长页面内嵌会撑破 agents.ts 的 argv 上限，整轮退化到 stdin（codex/opencode/
  // hermes 拒绝或挂起）→ 120s 超时 →「生成失败」。修法是落盘 + 只给路径。
  it("hands a too-long previous HTML over as a file path instead of inlining it", async () => {
    const dir = mkdtempSync(join(tmpdir(), "cm-revise-"));
    try {
      const deps = makeDeps();
      const dispatcher = new MessageDispatcher({ send: deps.send, reviseSourceDir: dir });
      const bigHtml = `<h1>v1</h1>${"x".repeat(9000)}`;
      await dispatcher.handle(txtMsg(JSON.stringify({ content: "做个页面" })), {
        runTurn: async () => `这是页面：\n\n\`\`\`html\n${bigHtml}\n\`\`\``,
      });
      const cardId = JSON.parse(deps.sent[0].content).card.id as string;

      let seenPrompt = "";
      let seenSessionKey: string | undefined;
      let fileDuringTurn = "";
      // 落盘文件名由 cardId 推出（writeReviseSource），路径可精确预期。
      const expectedPath = join(dir, `cm-revise-${cardId}.html`);
      await dispatcher.handle(cardAction(cardId, { inputValue: "换成蓝色主题" }), {
        runTurn: async (prompt, _model, sessionKey) => {
          seenPrompt = prompt;
          seenSessionKey = sessionKey;
          expect(prompt).toContain(expectedPath);
          fileDuringTurn = readFileSync(expectedPath, "utf8");
          return "```html\n<h1>v2 blue</h1>\n```";
        },
      });

      // 提示词缩回 argv 上限之内，且不再内嵌整页源码 —— 否则又落回 stdin。
      expect(seenPrompt.length).toBeLessThanOrEqual(ARGV_PROMPT_MAX_CHARS);
      expect(seenPrompt).not.toContain("x".repeat(200));
      expect(seenPrompt).toContain("换成蓝色主题");
      // revise 轮绑到该对端的私聊会话，不再落到共享的默认主会话。
      expect(seenSessionKey).toBe(turnSessionKey("private", "user_1"));
      // 文件里就是上一版 HTML，agent 读得到。
      expect(fileDuringTurn).toBe(bigHtml);
      // 用完即删。
      expect(readdirSync(dir)).toHaveLength(0);
      // 卡片照常原地替换。
      const update = JSON.parse(deps.sent[1].content);
      expect(deps.sent[1].objectName).toBe("card_update");
      expect(update.cardId).toBe(cardId);
      expect(update.card.sections.find((s: { kind: string }) => s.kind === "html").content).toContain("v2 blue");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("keeps inlining a short previous HTML and writes no file", async () => {
    const dir = mkdtempSync(join(tmpdir(), "cm-revise-"));
    try {
      const deps = makeDeps();
      const dispatcher = new MessageDispatcher({ send: deps.send, reviseSourceDir: dir });
      await dispatcher.handle(txtMsg(JSON.stringify({ content: "做个页面" })), {
        runTurn: async () => REPLY_WITH_HTML,
      });
      const cardId = JSON.parse(deps.sent[0].content).card.id as string;

      let seenPrompt = "";
      await dispatcher.handle(cardAction(cardId, { inputValue: "加个标题" }), {
        runTurn: async (prompt) => {
          seenPrompt = prompt;
          return "```html\n<h1>v2</h1>\n```";
        },
      });

      expect(seenPrompt).toContain("<h1>v1</h1>");
      expect(readdirSync(dir)).toHaveLength(0);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("cleans the handed-off file up even when the revise turn fails", async () => {
    const dir = mkdtempSync(join(tmpdir(), "cm-revise-"));
    try {
      const deps = makeDeps();
      const dispatcher = new MessageDispatcher({ send: deps.send, reviseSourceDir: dir });
      const bigHtml = `<h1>v1</h1>${"x".repeat(9000)}`;
      await dispatcher.handle(txtMsg(JSON.stringify({ content: "做个页面" })), {
        runTurn: async () => `\`\`\`html\n${bigHtml}\n\`\`\``,
      });
      const cardId = JSON.parse(deps.sent[0].content).card.id as string;

      await dispatcher.handle(cardAction(cardId, { inputValue: "再改" }), {
        runTurn: async () => {
          expect(readdirSync(dir)).toHaveLength(1);
          throw new Error("agent turn timeout after 120000ms");
        },
      });

      const parsed = JSON.parse(deps.sent[1].content);
      expect(parsed.content).toContain("生成失败");
      expect(readdirSync(dir)).toHaveLength(0);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
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

  it("adopts the server-assembled persona for this turn", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    let seenPrompt = "";
    await dispatcher.handle(
      {
        objectName: "command",
        fromUserId: "host_node",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: JSON.stringify({
          request_id: "turn_1_1_persona",
          service: "discussion",
          action: "your_turn",
          params: {
            round: 1,
            speaking_order: 1,
            role_name: "架构师",
            chatroom_id: CHATROOM_ID,
            // The host sends the operator's 人设提示词 and the role's 角色人设
            // already joined by "\n\n"; the node must feed both to the agent
            // verbatim rather than picking one.
            role_prompt: "你是主持人，负责收敛分歧。\n\n你是架构师，先给结论再讲理由。",
          },
        }),
      },
      { runTurn: async (prompt) => { seenPrompt = prompt; return "ok"; } },
    );
    expect(seenPrompt).toContain("[discussion] round=1 speaking_order=1");
    expect(seenPrompt).toContain("role: 架构师");
    expect(seenPrompt).toContain("你是主持人，负责收敛分歧。");
    expect(seenPrompt).toContain("你是架构师，先给结论再讲理由。");
    expect(seenPrompt).toContain("Stay in character");
  });

  it("keeps the legacy prompt exactly when no persona is sent", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    // A coordinator older than the persona plumbing omits role_prompt
    // entirely; a hand-written one may send blanks. Both must degrade to the
    // prompt shape this node has always sent.
    for (const rolePrompt of [undefined, "", "   "]) {
      let seenPrompt = "";
      const params: Record<string, unknown> = {
        round: 1,
        speaking_order: 1,
        role_name: "reviewer",
        chatroom_id: CHATROOM_ID,
      };
      if (rolePrompt !== undefined) params.role_prompt = rolePrompt;
      await dispatcher.handle(
        {
          objectName: "command",
          fromUserId: "host_node",
          toUserId: "rc_node_x",
          targetId: "rc_node_x",
          conversationType: 1,
          content: JSON.stringify({ request_id: "r", service: "discussion", action: "your_turn", params }),
        },
        { runTurn: async (prompt) => { seenPrompt = prompt; return "ok"; } },
      );
      expect(seenPrompt.split("\n")).toEqual([
        "[discussion] round=1 speaking_order=1",
        "role: reviewer",
        "Answer as this node in the discussion.",
      ]);
    }
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

describe("MessageDispatcher runtime recovery (一键唤醒)", () => {
  function recoveryMessage(requestId: string, action = "recover", timestamp = Date.now()) {
    return {
      objectName: "command",
      fromUserId: "user_1",
      toUserId: "",
      targetId: "user_1",
      conversationType: 1,
      content: JSON.stringify({
        msg_type: "claw_runtime_recovery_request",
        protocolVersion: 1,
        requestId,
        timestamp,
        action,
      }),
    };
  }

  it("answers over the same command channel, echoing the requestId", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({
      send: deps.send,
      agentName: "opencode",
      probeRuntime: async () => ({ code: "ready", message: "runtime ready" }),
    });
    await dispatcher.handle(recoveryMessage("rec-1"), { runTurn: async () => "unused" });
    expect(deps.sent).toHaveLength(1);
    expect(deps.sent[0].objectName).toBe("command");
    expect(deps.sent[0].to).toBe("user_1");
    const frame = JSON.parse(deps.sent[0].content);
    expect(frame.msg_type).toBe("claw_runtime_recovery_response");
    expect(frame.requestId).toBe("rec-1");
    expect(frame.code).toBe("ready");
    expect(frame.data).toEqual({ runtime: "ready", worker: "online", agent: "opencode" });
  });

  it("passes the requested action through to the prober", async () => {
    const deps = makeDeps();
    const seen: string[] = [];
    const dispatcher = new MessageDispatcher({
      send: deps.send,
      probeRuntime: async (action) => { seen.push(action); return { code: "ready", message: "ok" } },
    });
    await dispatcher.handle(recoveryMessage("rec-probe", "probe"), { runTurn: async () => "unused" });
    await dispatcher.handle(recoveryMessage("rec-recover", "recover"), { runTurn: async () => "unused" });
    expect(seen).toEqual(["probe", "recover"]);
  });

  it("runs the probe once per requestId even when 融云 replays the backlog", async () => {
    const deps = makeDeps();
    let probes = 0;
    const dispatcher = new MessageDispatcher({
      send: deps.send,
      probeRuntime: async () => { probes += 1; return { code: "ready", message: "ok" } },
    });
    const msg = recoveryMessage("rec-dup");
    for (let i = 0; i < 3; i += 1) {
      await dispatcher.handle(msg, { runTurn: async () => "unused" });
    }
    expect(probes).toBe(1);
    expect(deps.sent).toHaveLength(1);
  });

  it("skips a request the web has already timed out on", async () => {
    const deps = makeDeps();
    let probes = 0;
    const dispatcher = new MessageDispatcher({
      send: deps.send,
      probeRuntime: async () => { probes += 1; return { code: "ready", message: "ok" } },
    });
    await dispatcher.handle(
      recoveryMessage("rec-old", "recover", Date.now() - 5 * 60_000),
      { runTurn: async () => "unused" },
    );
    expect(probes).toBe(0);
    expect(deps.sent).toHaveLength(0);
  });

  it("still answers when this node has no prober wired up, so the browser never hangs", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(recoveryMessage("rec-none"), { runTurn: async () => "unused" });
    expect(deps.sent).toHaveLength(1);
    expect(JSON.parse(deps.sent[0].content).code).toBe("probe_failed");
  });

  it("degrades a throwing prober into probe_failed instead of dropping the answer", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({
      send: deps.send,
      probeRuntime: async () => { throw new Error("EPERM") },
    });
    await dispatcher.handle(recoveryMessage("rec-boom"), { runTurn: async () => "unused" });
    expect(deps.sent).toHaveLength(1);
    expect(JSON.parse(deps.sent[0].content).code).toBe("probe_failed");
  });

  it("never answers its own echoed message", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({
      send: deps.send,
      selfUserId: "user_1",
      probeRuntime: async () => ({ code: "ready", message: "ok" }),
    });
    await dispatcher.handle(recoveryMessage("rec-self"), { runTurn: async () => "unused" });
    expect(deps.sent).toHaveLength(0);
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
      probeRuntime: async () => ({ code: "ready", message: "ok" }),
    });
    const turns = { runTurn: async () => "unused" };
    await expect(dispatcher.handle(recoveryMessage("rec-retry"), turns)).rejects.toThrow();
    await dispatcher.handle(recoveryMessage("rec-retry"), turns);
    expect(sent).toHaveLength(1);
    expect(JSON.parse(sent[0]).requestId).toBe("rec-retry");
  });
});

describe("MessageDispatcher discussion recommend_roles", () => {
  const roleRequest = (
    requestId: string,
    params: Record<string, unknown> = {},
    fromUserId = "system",
  ) => ({
    objectName: "command",
    fromUserId,
    toUserId: "rc_node_host",
    targetId: "rc_node_host",
    conversationType: 1,
    content: JSON.stringify({
      request_id: requestId,
      service: "discussion",
      action: "recommend_roles",
      params: { topic: "AI 与就业", max_roles: 3, ...params },
    }),
  });

  it("runs the agent and replies with roles as a command_result", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    const roles = [
      { role_name: "主持人", role_prompt: "引导", speaking_order: 0 },
      { role_name: "质疑者", role_prompt: "反驳", speaking_order: 1 },
    ];
    let seenPrompt = "";
    await dispatcher.handle(roleRequest("roles_1"), {
      runTurn: async (prompt) => {
        seenPrompt = prompt;
        return JSON.stringify({ roles });
      },
    });

    expect(seenPrompt).toContain("Topic: AI 与就业");
    expect(deps.sent).toHaveLength(1);
    expect(deps.sent[0].to).toBe("system");
    expect(deps.sent[0].objectName).toBe("command");
    const parsed = JSON.parse(deps.sent[0].content);
    expect(parsed.request_id).toBe("roles_1");
    expect(parsed.msg_type).toBe("command_result");
    expect(parsed.payload.kind).toBe("text");
    expect(parsed.payload.roles).toEqual(roles);
  });

  it("replies with an error command_result when the answer has no valid roles", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(roleRequest("roles_bad"), {
      runTurn: async () => "I would rather not.",
    });

    const parsed = JSON.parse(deps.sent[0].content);
    expect(parsed.request_id).toBe("roles_bad");
    expect(parsed.payload.kind).toBe("error");
  });

  it("answers a replayed requestId exactly once", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    let runs = 0;
    const turns = {
      runTurn: async () => {
        runs += 1;
        return JSON.stringify({
          roles: [{ role_name: "A", role_prompt: "p", speaking_order: 0 }],
        });
      },
    };
    await dispatcher.handle(roleRequest("roles_replay"), turns);
    await dispatcher.handle(roleRequest("roles_replay"), turns);

    expect(runs).toBe(1);
    expect(deps.sent).toHaveLength(1);
  });

  it("skips a request whose timestamp is already too old", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    let runs = 0;
    await dispatcher.handle(
      roleRequest("roles_stale", { timestamp: Date.now() - 61 * 60 * 1000 }),
      {
        runTurn: async () => {
          runs += 1;
          return "{}";
        },
      },
    );
    expect(runs).toBe(0);
    expect(deps.sent).toHaveLength(0);
  });

  it("ignores a recommend_roles request with an unusable param set", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    let runs = 0;
    await dispatcher.handle(roleRequest("roles_nomax", { max_roles: 99 }), {
      runTurn: async () => {
        runs += 1;
        return "{}";
      },
    });
    expect(runs).toBe(0);
    expect(deps.sent).toHaveLength(0);
  });
});

// ---- chat_stream 流式输出（所有会话形态共用 runStreamedTurn）----

describe("MessageDispatcher chat_stream streaming", () => {
  interface SentItem { to: string; type: string; content: string }

  function buildDeps() {
    const sent: SentItem[] = []
    const streams: Array<{ conversationType: number; conversationId: string; content: string }> = []
    const deps = {
      sent,
      streams,
      send: async (to: string, type: string, content: string) => { sent.push({ to, type, content }) },
      sendStream: async (conversationType: 1 | 3 | 4, conversationId: string, _objectName: string, content: string) => {
        streams.push({ conversationType, conversationId, content })
      },
      selfUserId: "node_self",
    }
    return { deps, sent, streams }
  }

  function parseFrame(raw: string) {
    return JSON.parse(raw) as Record<string, unknown>
  }

  it("single chat streams processing -> streaming -> completed then card fallback", async () => {
    const { deps, streams, sent } = buildDeps()
    const dispatcher = new MessageDispatcher(deps)
    const turns = {
      runTurn: async () => { throw new Error("should use streamed variant") },
      runTurnStreamed: async (_p: string, _m: string | undefined, onDelta: (d: string) => void) => {
        onDelta("你好")
        onDelta("，世界")
        return "你好，世界"
      },
    }
    await dispatcher.handle({
      objectName: "RC:TxtMsg", fromUserId: "user_a", toUserId: "", targetId: "node_self",
      conversationType: 1, content: JSON.stringify({ content: "hi" }),
    }, turns)

    // 帧序列：processing(0) → streaming ≥1 → completed 终帧
    const frames = streams.map((s) => parseFrame(s.content))
    expect(frames[0].status).toBe("processing")
    expect(frames[0].seq).toBe(0)
    expect(frames[0].text).toBe("")
    expect(frames[0].node_id).toBe("node_self")
    expect(frames[0].conversation_type).toBe(1)
    expect(frames[0].conversation_id).toBe("user_a")
    expect(frames.at(-1)!.status).toBe("completed")
    expect(frames.at(-1)!.text).toContain("你好，世界")
    // seq 单调不减
    for (let i = 1; i < frames.length; i += 1) {
      expect(Number(frames[i].seq)).toBeGreaterThanOrEqual(Number(frames[i - 1].seq))
    }
    // 会话路由正确（私聊帧发给对端）
    expect(streams.every((s) => s.conversationType === 1 && s.conversationId === "user_a")).toBe(true)
    // 完成后仍有兜底回复（deliverAgentReply 的文本/卡片路径）
    expect(sent.length).toBeGreaterThanOrEqual(1)
  })

  it("relay your_turn streams into the chatroom conversation", async () => {
    const { deps, streams, sent } = buildDeps()
    const dispatcher = new MessageDispatcher(deps)
    const turns = {
      runTurn: async () => { throw new Error("should use streamed variant") },
      runTurnStreamed: async () => "正式回答",
    }
    await dispatcher.handle({
      objectName: "command", fromUserId: "host_node", toUserId: "", targetId: "node_self",
      conversationType: 1,
      content: JSON.stringify({
        request_id: "turn_1_1_x", service: "discussion", action: "your_turn",
        params: {
          round: 1, speaking_order: 1, role_name: "顾问",
          chatroom_id: "room_rc", conversation_type: 4, requester_id: "user_b",
        },
      }),
    }, turns)

    const frames = streams.map((s) => parseFrame(s.content))
    expect(frames.length).toBeGreaterThanOrEqual(2)
    expect(frames[0].status).toBe("processing")
    expect(frames.at(-1)!.status).toBe("completed")
    // 帧进聊天室会话（type 4, id room_rc），requester_id 来自 params
    expect(streams.every((s) => s.conversationType === 4 && s.conversationId === "room_rc")).toBe(true)
    expect(frames[0].requester_id).toBe("user_b")
    expect(frames[0].role_name ?? true).toBeTruthy()
    // 中继推进依赖的 command_result 兜底仍在
    const result = sent.find((s) => s.type === "command")
    expect(result).toBeDefined()
    expect(JSON.parse(result!.content).payload.kind).toBe("text")
  })

  it("chat_stop cancels the active stream and acks accepted", async () => {
    const { deps, streams, sent } = buildDeps()
    const dispatcher = new MessageDispatcher(deps)
    let rejectTurn!: (e: Error) => void
    const turnDone = new Promise<string>((_resolve, reject) => { rejectTurn = reject })
    const handleDone = dispatcher.handle({
      objectName: "RC:TxtMsg", fromUserId: "user_a", toUserId: "", targetId: "node_self",
      conversationType: 1, content: JSON.stringify({ content: "hi" }),
    }, {
      runTurn: async () => { throw new Error("unused") },
      // 模拟 runAgentTurn(signal) 的真实行为：abort 时 reject。
      runTurnStreamed: async (_p: string, _m: string | undefined, onDelta: (d: string) => void, signal?: AbortSignal) => {
        onDelta("部分内容")
        return new Promise<string>((_resolve, reject) => {
          signal?.addEventListener("abort", () => reject(new Error("agent turn aborted")), { once: true })
          rejectTurn = reject
        })
      },
    })
    // 等首帧落地（activeStreams 已注册）
    await new Promise((r) => setTimeout(r, 30))
    const streamId = (JSON.parse(streams[0].content) as { stream_id: string }).stream_id
    await dispatcher.handle({
      objectName: "chat_stop", fromUserId: "user_a", toUserId: "", targetId: "node_self",
      conversationType: 1,
      content: JSON.stringify({
        msg_type: "chat_stop", protocol_version: 1, request_id: "stop-1",
        stream_id: streamId, node_id: "node_self",
        conversation_type: 1, conversation_id: "user_a",
      }),
    }, { runTurn: async () => "" })
    rejectTurn(new Error("agent turn aborted"))
    await handleDone

    const stopResult = sent.find((s) => s.type === "chat_stop_result")
    expect(stopResult).toBeDefined()
    const parsed = JSON.parse(stopResult!.content) as Record<string, unknown>
    expect(parsed.status).toBe("accepted")
    expect(parsed.code).toBe("stop_requested")
    // 取消后不再发 completed 终帧
    const frames = streams.map((s) => parseFrame(s.content))
    expect(frames.at(-1)!.status).toBe("cancelled")
  })

  it("chat_stop for an unknown stream is rejected with no_active_task", async () => {
    const { deps, sent } = buildDeps()
    const dispatcher = new MessageDispatcher(deps)
    await dispatcher.handle({
      objectName: "chat_stop", fromUserId: "user_a", toUserId: "", targetId: "node_self",
      conversationType: 1,
      content: JSON.stringify({
        msg_type: "chat_stop", protocol_version: 1, request_id: "stop-2",
        stream_id: "stream-none", node_id: "node_self",
        conversation_type: 1, conversation_id: "user_a",
      }),
    }, { runTurn: async () => "" })
    const parsed = JSON.parse(sent[0].content) as Record<string, unknown>
    expect(parsed.status).toBe("rejected")
    expect(parsed.code).toBe("no_active_task")
  })
})

describe("planReadReceipt (已读回执判定)", () => {
  it("单聊：收到带 needReceipt 的消息 → 回给发送方", () => {
    expect(planReadReceipt({
      messageUId: "u1", needReceipt: true, messageDirection: 2,
      senderUserId: "human-1", targetId: "node-a", conversationType: 1, sentTime: 1700,
    }, "node-a")).toEqual({
      channel: "private", targetId: "human-1", messageUId: "u1", sentTime: 1700,
    })
  })

  it("群聊：回执打到群会话上（V5 通道）", () => {
    expect(planReadReceipt({
      messageUId: "u2", needReceipt: true, messageDirection: 2,
      senderUserId: "human-1", targetId: "group-9", conversationType: 3, sentTime: 1700,
    }, "node-a")).toEqual({
      channel: "group", conversationType: 3, targetId: "group-9", messageUId: "u2",
    })
  })

  it("聊天室不回执（融云聊天室没有已读能力）", () => {
    expect(planReadReceipt({
      messageUId: "u3", needReceipt: true, messageDirection: 2,
      senderUserId: "human-1", targetId: "room-1", conversationType: 4, sentTime: 1700,
    }, "node-a")).toBeNull()
  })

  it("没有 needReceipt 就不回（挡住 AI 的 relay/chat_stream 帧，避免 N² 回执风暴）", () => {
    expect(planReadReceipt({
      messageUId: "u4", messageDirection: 2,
      senderUserId: "node-b", targetId: "group-9", conversationType: 3, sentTime: 1700,
    }, "node-a")).toBeNull()
    expect(planReadReceipt({
      messageUId: "u5", needReceipt: false, messageDirection: 2,
      senderUserId: "node-b", targetId: "group-9", conversationType: 3, sentTime: 1700,
    }, "node-a")).toBeNull()
  })

  it("自己的消息（多端同步回推）不回执", () => {
    expect(planReadReceipt({
      messageUId: "u6", needReceipt: true, messageDirection: 1,
      senderUserId: "node-a", targetId: "human-1", conversationType: 1, sentTime: 1700,
    }, "node-a")).toBeNull()
    expect(planReadReceipt({
      messageUId: "u7", needReceipt: true, messageDirection: 2,
      senderUserId: "node-a", targetId: "group-9", conversationType: 3, sentTime: 1700,
    }, "node-a")).toBeNull()
  })

  it("缺 messageUId / 发送方时安全跳过", () => {
    expect(planReadReceipt({
      needReceipt: true, messageDirection: 2,
      senderUserId: "human-1", targetId: "node-a", conversationType: 1,
    }, "node-a")).toBeNull()
    expect(planReadReceipt({
      messageUId: "u8", needReceipt: true, messageDirection: 2,
      targetId: "node-a", conversationType: 1, sentTime: 1700,
    }, "node-a")).toBeNull()
  })
})

describe("会话形态判定与隔离（群聊不得被当成讨论组）", () => {
  const RELAY_PROMPT =
    "[group chat] 这是普通群聊的一次自由对话。用户刚在群里发言：\n\n今天的天气怎么样\n\n" +
    "请以「openclaw」的身份回应用户。回应会被广播回群聊，请只输出回应正文，不要输出多余格式。"

  it("conversation_type=3 → 群聊；=4 且提示词是中继文案 → 聊天室；=4 且是人设 → 讨论", () => {
    expect(resolveTurnVenue(3, RELAY_PROMPT)).toBe("group")
    expect(resolveTurnVenue(4, RELAY_PROMPT)).toBe("chatroom")
    expect(resolveTurnVenue(4, "你是主持人，负责收敛分歧。")).toBe("discussion")
    expect(resolveTurnVenue(1, "")).toBe("private")
    // 老协调器不带 conversation_type：只能靠提示词认中继
    expect(resolveTurnVenue(undefined, RELAY_PROMPT)).toBe("chatroom")
    expect(resolveTurnVenue(undefined, "你是架构师")).toBe("discussion")
  })

  it("会话键按形态 + 会话 id 分桶", () => {
    expect(turnSessionKey("group", "group_rc")).toBe("cm-group-group_rc")
    expect(turnSessionKey("discussion", "room_rc")).toBe("cm-discussion-room_rc")
    expect(turnSessionKey("private", "user_a")).toBe("cm-private-user_a")
    // 群聊与讨论组永远不共享键 —— 这正是「群聊答成等待主题发布」的病灶
    expect(turnSessionKey("group", "room_rc")).not.toBe(turnSessionKey("discussion", "room_rc"))
  })

  function relayDeps() {
    const streams: Array<{ conversationType: number; conversationId: string; content: string }> = []
    const sent: Array<{ to: string; type: string; content: string }> = []
    const deps = {
      sent,
      streams,
      send: async (to: string, type: string, content: string) => { sent.push({ to, type, content }) },
      sendStream: async (conversationType: 1 | 3 | 4, conversationId: string, _o: string, content: string) => {
        streams.push({ conversationType, conversationId, content })
      },
      selfUserId: "node_self",
    }
    return { deps, sent, streams }
  }

  it("群聊中继的提示词不再盖圆桌讨论抬头，且绑到群会话键上", async () => {
    const { deps } = relayDeps()
    const dispatcher = new MessageDispatcher(deps)
    let seenPrompt = ""
    let seenKey: string | undefined
    await dispatcher.handle({
      objectName: "command", fromUserId: "claw-system", toUserId: "", targetId: "node_self",
      conversationType: 1,
      content: JSON.stringify({
        request_id: "relay_1_0", service: "discussion", action: "your_turn",
        params: {
          round: 1, speaking_order: 0, role_name: "openclaw",
          chatroom_id: "group_rc", conversation_type: 3, requester_id: "user_b",
          role_prompt: RELAY_PROMPT,
        },
      }),
    }, {
      runTurn: async () => { throw new Error("should use streamed variant") },
      runTurnStreamed: async (prompt: string, _m, _d, _s, sessionKey?: string) => {
        seenPrompt = prompt
        seenKey = sessionKey
        return "今天多云"
      },
    })

    // 闲聊中继：服务端提示词原样下传（不加轮次/角色头，普通群聊就是闲聊）
    expect(seenPrompt).toBe(RELAY_PROMPT)
    expect(seenPrompt).toContain("今天的天气怎么样")
    // 关键回归：不得再说自己在 discussion 里等主持人
    expect(seenPrompt).not.toContain("[discussion]")
    expect(seenPrompt).not.toContain("Answer as this node in the discussion")
    expect(seenPrompt).not.toContain("speaking_order")
    expect(seenKey).toBe("cm-group-group_rc")
  })

  it("中继提示词缺失时（老协调器）也要给出可回答的闲聊提示，而不是空提示", async () => {
    const { deps } = relayDeps()
    const dispatcher = new MessageDispatcher(deps)
    let seenPrompt = ""
    await dispatcher.handle({
      objectName: "command", fromUserId: "claw-system", toUserId: "", targetId: "node_self",
      conversationType: 1,
      content: JSON.stringify({
        request_id: "relay_1_0", service: "discussion", action: "your_turn",
        params: { round: 1, speaking_order: 0, chatroom_id: "group_rc", conversation_type: 3 },
      }),
    }, {
      runTurn: async () => { throw new Error("should use streamed variant") },
      runTurnStreamed: async (prompt: string) => { seenPrompt = prompt; return "ok" },
    })
    expect(seenPrompt).toContain("[group chat]")
    expect(seenPrompt).not.toContain("[discussion]")
  })

  it("v3 讨论轮次仍走讨论抬头与人设，键按房间分桶", async () => {
    const { deps } = relayDeps()
    const dispatcher = new MessageDispatcher(deps)
    let seenPrompt = ""
    let seenKey: string | undefined
    await dispatcher.handle({
      objectName: "command", fromUserId: "host_node", toUserId: "", targetId: "node_self",
      conversationType: 1,
      content: JSON.stringify({
        request_id: "turn_1_1_x", service: "discussion", action: "your_turn",
        params: {
          round: 1, speaking_order: 1, role_name: "架构师",
          chatroom_id: "room_rc", conversation_type: 4,
          role_prompt: "你是主持人，负责收敛分歧。",
        },
      }),
    }, {
      runTurn: async () => { throw new Error("should use streamed variant") },
      runTurnStreamed: async (prompt: string, _m, _d, _s, sessionKey?: string) => {
        seenPrompt = prompt
        seenKey = sessionKey
        return "ok"
      },
    })

    expect(seenPrompt).toContain("[discussion] round=1 speaking_order=1")
    expect(seenPrompt).toContain("你是主持人，负责收敛分歧。")
    expect(seenPrompt).toContain("Stay in character")
    expect(seenKey).toBe("cm-discussion-room_rc")
  })

  it("私聊按对端分桶", async () => {
    const { deps } = relayDeps()
    const dispatcher = new MessageDispatcher(deps)
    let seenKey: string | undefined
    await dispatcher.handle({
      objectName: "RC:TxtMsg", fromUserId: "user_a", toUserId: "", targetId: "node_self",
      conversationType: 1, content: JSON.stringify({ content: "hi" }),
    }, {
      runTurn: async () => { throw new Error("should use streamed variant") },
      runTurnStreamed: async (_p: string, _m, _d, _s, sessionKey?: string) => {
        seenKey = sessionKey
        return "你好"
      },
    })
    expect(seenKey).toBe("cm-private-user_a")
  })
})

// agent 把产物写进工作目录、stdout 却是空的，是「生成个文件」这类请求的常态。
// 这一组用例锁住两件事：文件要被采集并作为 file_message 发出去；正文不能是空的
// （空正文在中继那边等于「该成员没有产出」，在私聊里等于一个永远填不满的气泡）。
describe("MessageDispatcher agent-produced files", () => {
  function makeArtifactDeps() {
    const sent: Array<{ to: string; objectName: string; content: string }> = [];
    const streamed: Array<{
      conversationType: number;
      conversationId: string;
      objectName: string;
      content: string;
    }> = [];
    const uploaded: Array<{ path: string; name: string }> = [];
    return { sent, streamed, uploaded };
  }

  it("uploads this turn's new files and sends them as file_message", async () => {
    const root = mkdtempSync(join(tmpdir(), "cm-im-artifacts-"));
    const deps = makeArtifactDeps();
    const dispatcher = new MessageDispatcher({
      send: async (to, objectName, content) => {
        deps.sent.push({ to, objectName, content });
      },
      sendStream: async (conversationType, conversationId, objectName, content) => {
        deps.streamed.push({ conversationType, conversationId, objectName, content });
      },
      artifactDir: root,
      uploadArtifact: async (artifact) => {
        deps.uploaded.push({ path: artifact.path, name: artifact.name });
        return {
          url: `/api/claw/files/node-x/${artifact.name}`,
          name: artifact.name,
          size: artifact.size,
          type: "text/html",
        };
      },
    });

    await dispatcher.handle(
      {
        objectName: "RC:TxtMsg",
        fromUserId: "user-1",
        toUserId: "node-a",
        targetId: "node-a",
        conversationType: 1,
        content: JSON.stringify({ content: "帮我生成一个落地页" }),
      },
      {
        runTurn: async () => {
          writeFileSync(join(root, "landing.html"), "<html>ok</html>", "utf8");
          // agent 只写了文件，没打印任何东西 —— 正是线上「零回复」的成因。
          return "";
        },
      },
    );

    expect(deps.uploaded.map((item) => item.name)).toEqual(["landing.html"]);
    const fileMessage = deps.streamed.find((item) => item.objectName === "file_message");
    expect(fileMessage).toBeTruthy();
    expect(fileMessage!.conversationType).toBe(1);
    expect(fileMessage!.conversationId).toBe("user-1");
    const payload = JSON.parse(fileMessage!.content);
    expect(payload.msg_type).toBe("file_message");
    expect(payload.file.url).toContain("landing.html");

    // 正文兜底：空 stdout 必须变成一句用户能看懂的话，且点到文件名。
    expect(deps.sent).toHaveLength(1);
    const reply = JSON.parse(deps.sent[0].content);
    expect(reply.content).toContain("已生成 1 个文件");
    expect(reply.content).toContain("landing.html");

    rmSync(root, { recursive: true, force: true });
  });

  it("says so explicitly when the turn produced nothing at all", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "RC:TxtMsg",
        fromUserId: "user-1",
        toUserId: "node-a",
        targetId: "node-a",
        conversationType: 1,
        content: JSON.stringify({ content: "在吗" }),
      },
      { runTurn: async () => "" },
    );
    expect(deps.sent).toHaveLength(1);
    expect(JSON.parse(deps.sent[0].content).content).toBe("（本轮没有产出内容）");
  });

  it("leaves a non-empty reply untouched (a fence must still become a card)", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "RC:TxtMsg",
        fromUserId: "user-1",
        toUserId: "node-a",
        targetId: "node-a",
        conversationType: 1,
        content: JSON.stringify({ content: "生成一个页面" }),
      },
      { runTurn: async () => "```html\n<html>hi</html>\n```" },
    );
    expect(deps.sent).toHaveLength(1);
    expect(deps.sent[0].objectName).toBe("card_message");
  });
});
