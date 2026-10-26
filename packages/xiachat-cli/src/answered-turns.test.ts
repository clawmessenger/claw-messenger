// @vitest-environment node
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import {
  ANSWERED_TURN_MAX_ENTRIES,
  ANSWERED_TURN_MAX_TEXT_CHARS,
  createAnsweredTurnStore,
  type AnsweredTurn,
} from "./answered-turns.js";

function turn(overrides: Partial<AnsweredTurn> = {}): AnsweredTurn {
  return {
    at: 1_700_000_000_000,
    streamId: "stream-req-1-ab12cd",
    seq: 7,
    text: "圆桌答案",
    requesterId: "user_a",
    conversationType: 4,
    conversationId: "rt_abc",
    ...overrides,
  };
}

describe("createAnsweredTurnStore", () => {
  it("记过的 requestId 能读回，没记过的读不到", () => {
    const store = createAnsweredTurnStore();
    expect(store.get("r1")).toBeUndefined();
    store.remember("r1", turn());
    expect(store.get("r1")).toMatchObject({ streamId: "stream-req-1-ab12cd", seq: 7, text: "圆桌答案" });
    // 空 requestId 一律当未答：设备端有用 `req-<ts>` 兜底的路径，不能撞。
    expect(store.get("")).toBeUndefined();
  });

  it("同一 requestId 重复记账以最后一次为准，并排到队尾（不会被当最旧淘汰）", () => {
    const store = createAnsweredTurnStore({ maxEntries: 2 });
    store.remember("r1", turn({ text: "第一版" }));
    store.remember("r2", turn({ text: "第二个" }));
    store.remember("r1", turn({ text: "重跑后的第二版" }));
    expect(store.get("r1")?.text).toBe("重跑后的第二版");
    // 重新记账把 r1 移到队尾 ⇒ 接下来淘汰的是 r2。
    store.remember("r3", turn({ text: "第三个" }));
    expect(store.get("r1")?.text).toBe("重跑后的第二版");
    expect(store.get("r2")).toBeUndefined();
    expect(store.get("r3")?.text).toBe("第三个");
  });

  it("超过 maxEntries 淘汰最旧的", () => {
    const store = createAnsweredTurnStore({ maxEntries: 3 });
    for (let i = 0; i < 6; i += 1) store.remember(`r${i}`, turn({ text: `t${i}` }));
    expect(store.size).toBe(3);
    expect(store.get("r2")).toBeUndefined();
    expect(store.get("r5")?.text).toBe("t5");
  });

  it("正文超上限才截断，不超就原样留", () => {
    const store = createAnsweredTurnStore();
    store.remember("long", turn({ text: "字".repeat(ANSWERED_TURN_MAX_TEXT_CHARS + 50) }));
    store.remember("short", turn({ text: "字".repeat(100) }));
    expect(store.get("long")?.text.length).toBe(ANSWERED_TURN_MAX_TEXT_CHARS);
    expect(store.get("short")?.text.length).toBe(100);
  });

  it("落盘后新进程能读回（重启不丢，这是这条台账的全部意义）", () => {
    const path = join(mkdtempSync(join(tmpdir(), "cm-answered-")), "answered-turns.json");
    const first = createAnsweredTurnStore({ path });
    first.remember("req-uuid-1", turn({ text: "重启前的答案" }));
    first.flush();

    const second = createAnsweredTurnStore({ path });
    expect(second.size).toBe(1);
    expect(second.get("req-uuid-1")?.text).toBe("重启前的答案");
  });

  it("文件损坏或格式不对时从空开始，不抛也不带进坏条目", () => {
    const dir = mkdtempSync(join(tmpdir(), "cm-answered-"));
    const broken = join(dir, "broken.json");
    writeFileSync(broken, "{ not json");
    expect(createAnsweredTurnStore({ path: broken }).size).toBe(0);

    const mixed = join(dir, "mixed.json");
    writeFileSync(mixed, JSON.stringify([
      ["ok", turn()],
      ["bad-type", { ...turn(), conversationType: 9 }],
      ["bad-shape", "not-an-object"],
      "not-a-tuple",
      ["", turn()],
    ]));
    const store = createAnsweredTurnStore({ path: mixed });
    expect(store.size).toBe(1);
    expect(store.get("ok")).toBeDefined();
    expect(store.get("bad-type")).toBeUndefined();
  });

  it("默认上限是给「重启后重派的那一小批」留的，不是无限；落盘保序", () => {
    expect(ANSWERED_TURN_MAX_ENTRIES).toBeLessThanOrEqual(200);
    const path = join(mkdtempSync(join(tmpdir(), "cm-answered-")), "answered-turns.json");
    const store = createAnsweredTurnStore({ path });
    for (let i = 0; i < ANSWERED_TURN_MAX_ENTRIES + 5; i += 1) store.remember(`r${i}`, turn());
    expect(store.size).toBe(ANSWERED_TURN_MAX_ENTRIES);
    store.flush();
    // 落盘是 [[requestId, turn], ...] 且保持插入顺序：重启后淘汰顺序与重启前
    // 一致，否则刚答完的那几轮反而先被挤掉（它们正是最容易被重派的）。
    const parsed = JSON.parse(readFileSync(path, "utf8")) as Array<[string, unknown]>;
    expect(parsed).toHaveLength(ANSWERED_TURN_MAX_ENTRIES);
    expect(parsed[0]![0]).toBe("r5");
    expect(parsed.at(-1)![0]).toBe(`r${ANSWERED_TURN_MAX_ENTRIES + 4}`);
  });
});
