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

describe("MessageDispatcher discussion your_turn", () => {
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
          params: { round: 1, speaking_order: 1, role_name: "reviewer", model: "sonnet-4" },
        }),
      },
      { runTurn: async (prompt, model) => `out(${model}) ${prompt.length > 0 ? "prompt" : "noprompt"}` },
    );
    expect(deps.sent).toHaveLength(1);
    expect(deps.sent[0].objectName).toBe("command");
    const parsed = JSON.parse(deps.sent[0].content);
    expect(parsed.request_id).toBe("turn_1_1_20260929");
    expect(parsed.msg_type).toBe("text");
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
          params: {},
        }),
      },
      { runTurn: async () => { throw new Error("timeout"); } },
    );
    const parsed = JSON.parse(deps.sent[0].content);
    expect(parsed.msg_type).toBe("error");
    expect(parsed.request_id).toBe("r2");
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
