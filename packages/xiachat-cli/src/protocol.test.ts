// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  decodeCommandContent,
  encodeCommand,
  encodeCommandResult,
  encodeStreamFrame,
} from "./protocol.js";

describe("encodeCommand", () => {
  it("produces the your_turn payload the coordinator sends", () => {
    const raw = encodeCommand("turn_1_1_20260929", "discussion", "your_turn", {
      round: 1,
      speaking_order: 1,
      role_name: "reviewer",
    });
    const parsed = JSON.parse(raw);
    expect(parsed.request_id).toBe("turn_1_1_20260929");
    expect(parsed.service).toBe("discussion");
    expect(parsed.action).toBe("your_turn");
    expect(parsed.params.round).toBe(1);
  });
});

describe("decodeCommandContent", () => {
  it("parses a valid command content", () => {
    const raw = JSON.stringify({
      request_id: "r1",
      service: "discussion",
      action: "your_turn",
      params: { round: 2 },
    });
    const cmd = decodeCommandContent(raw);
    expect(cmd?.action).toBe("your_turn");
    expect(cmd?.params?.round).toBe(2);
  });

  it("returns null on malformed JSON", () => {
    expect(decodeCommandContent("not json")).toBeNull();
  });

  it("returns null when required fields are missing", () => {
    expect(decodeCommandContent(JSON.stringify({ request_id: "r1" }))).toBeNull();
  });
});

describe("encodeCommandResult", () => {
  it("echoes request_id and tags msg_type", () => {
    const raw = encodeCommandResult("r1", "text", { content: "hi" });
    const parsed = JSON.parse(raw);
    expect(parsed.request_id).toBe("r1");
    expect(parsed.msg_type).toBe("text");
    expect(parsed.payload.content).toBe("hi");
  });
});

describe("encodeStreamFrame", () => {
  it("frames start/chunk/end with turn_id and stream_type", () => {
    for (const streamType of ["start", "chunk", "end", "error"] as const) {
      const parsed = JSON.parse(encodeStreamFrame("t1", streamType, "x"));
      expect(parsed.turn_id).toBe("t1");
      expect(parsed.stream_type).toBe(streamType);
      expect(parsed.content).toBe("x");
    }
  });
});
