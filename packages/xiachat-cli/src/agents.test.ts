// @vitest-environment node
import { describe, expect, it } from "vitest";
import { discoverAgents, KNOWN_AGENT_CLIS, runAgentTurn, trimIncompleteUtf8Tail } from "./agents.js";
import { createEchoAgentScript, type EchoAgentFixture } from "./test-fixtures.js";

describe("KNOWN_AGENT_CLIS", () => {
  it("matches the server-side knownAgentCLIs list", () => {
    expect(KNOWN_AGENT_CLIS).toEqual([
      "claude", "codex", "opencode", "codebuddy", "codearts", "deveco",
      "openclaw", "hermes", "pi", "omp", "cursor-agent", "kimi",
      "reasonix", "dsh", "kiro-cli", "agy", "qodercli", "qoderclicn",
      "traecli", "grok", "qwen", "qwenpaw", "mcode", "dim", "zeroclaw",
    ]);
  });
});

describe("discoverAgents", () => {
  it("scans only extraPath when provided", async () => {
    const fixture = createEchoAgentScript("claude");
    try {
      const found = await discoverAgents({ extraPath: fixture.dir });
      expect(found.some((a) => a.name === "claude")).toBe(true);
      expect(found.every((a) => a.path.startsWith(fixture.dir))).toBe(true);
    } finally {
      fixture.cleanup();
    }
  });

  it("returns [] when extraPath has no known agent", async () => {
    const fixture = createEchoAgentScript("not-a-known-agent");
    try {
      expect(await discoverAgents({ extraPath: fixture.dir })).toEqual([]);
    } finally {
      fixture.cleanup();
    }
  });

  it("returns [] for an empty extraPath", async () => {
    expect(await discoverAgents({ extraPath: "" })).toEqual([]);
  });
});

describe("trimIncompleteUtf8Tail", () => {
  // "你" = E4 BD A0 (3-byte); "😀" = F0 9F 98 80 (4-byte).
  const cjk = Buffer.from("你", "utf8"); // e4 bd a0
  const emoji = Buffer.from("😀", "utf8"); // f0 9f 98 80
  const ascii = Buffer.from("a", "utf8");

  it("returns the buffer unchanged when it ends on a complete ASCII char", () => {
    const buf = Buffer.concat([ascii, cjk, ascii]);
    expect(trimIncompleteUtf8Tail(buf)).toEqual(buf);
  });

  it("returns an empty buffer for input that is only a partial sequence", () => {
    expect(trimIncompleteUtf8Tail(cjk.subarray(0, 1))).toEqual(Buffer.alloc(0));
    expect(trimIncompleteUtf8Tail(emoji.subarray(0, 1))).toEqual(Buffer.alloc(0));
  });

  it("trims a cut after the lead byte of a 3-byte char (E4)", () => {
    const buf = Buffer.concat([ascii, cjk.subarray(0, 1)]); // ...e4
    expect(trimIncompleteUtf8Tail(buf)).toEqual(ascii);
  });

  it("trims a cut after the 1st continuation byte of a 3-byte char (E4 BD)", () => {
    const buf = Buffer.concat([ascii, cjk.subarray(0, 2)]); // ...e4 bd
    expect(trimIncompleteUtf8Tail(buf)).toEqual(ascii);
  });

  it("trims a cut after the lead byte of a 4-byte char (F0)", () => {
    const buf = Buffer.concat([ascii, emoji.subarray(0, 1)]); // ...f0
    expect(trimIncompleteUtf8Tail(buf)).toEqual(ascii);
  });

  it("trims a cut after the 1st continuation byte of a 4-byte char (F0 9F)", () => {
    const buf = Buffer.concat([ascii, emoji.subarray(0, 2)]); // ...f0 9f
    expect(trimIncompleteUtf8Tail(buf)).toEqual(ascii);
  });

  it("trims a cut after the 2nd continuation byte of a 4-byte char (F0 9F 98)", () => {
    const buf = Buffer.concat([ascii, emoji.subarray(0, 3)]); // ...f0 9f 98
    expect(trimIncompleteUtf8Tail(buf)).toEqual(ascii);
  });

  it("keeps a complete multi-byte char at the end", () => {
    const buf = Buffer.concat([ascii, cjk]);
    expect(trimIncompleteUtf8Tail(buf)).toEqual(buf);
    const withEmoji = Buffer.concat([ascii, emoji]);
    expect(trimIncompleteUtf8Tail(withEmoji)).toEqual(withEmoji);
  });

  it("cuts the trailing continuation run when no lead byte exists within 4 bytes (invalid UTF-8)", () => {
    const garbage = Buffer.from([0x80, 0x80, 0x80, 0x80]); // bare continuations
    const buf = Buffer.concat([ascii, garbage]);
    expect(trimIncompleteUtf8Tail(buf)).toEqual(ascii);
  });
});

describe("runAgentTurn", () => {
  it("pipes prompt via stdin and resolves stdout", async () => {
    const fixture = createEchoAgentScript("claude");
    try {
      const out = await runAgentTurn({ execPath: fixture.agentPath, prompt: "hello agent", timeoutMs: 30_000 });
      expect(out).toContain("hello agent");
    } finally {
      fixture.cleanup();
    }
  });

  it("passes --model when provided", async () => {
    const fixture = createEchoAgentScript("claude", { recordArgs: true });
    try {
      await runAgentTurn({ execPath: fixture.agentPath, prompt: "p", model: "sonnet-4", timeoutMs: 30_000 });
      const args = fixture.recordedArgs();
      expect(args).toContain("--model");
      expect(args).toContain("sonnet-4");
    } finally {
      fixture.cleanup();
    }
  });

  it("omits --model when not provided", async () => {
    const fixture = createEchoAgentScript("claude", { recordArgs: true });
    try {
      await runAgentTurn({ execPath: fixture.agentPath, prompt: "p", timeoutMs: 30_000 });
      expect(fixture.recordedArgs()).not.toContain("--model");
    } finally {
      fixture.cleanup();
    }
  });

  it("rejects on timeout", async () => {
    const fixture = createEchoAgentScript("claude", { hangMs: 8000 });
    try {
      await expect(
        runAgentTurn({ execPath: fixture.agentPath, prompt: "p", timeoutMs: 500 }),
      ).rejects.toThrow(/timeout/i);
    } finally {
      fixture.cleanup();
    }
  });

  it("truncates stdout beyond maxOutputBytes", async () => {
    const fixture = createEchoAgentScript("claude", { bigOutputBytes: 200_000 });
    try {
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        prompt: "p",
        timeoutMs: 30_000,
        maxOutputBytes: 1024,
      });
      expect(out.length).toBeLessThanOrEqual(1024);
      expect(out).toBe("a".repeat(1024));
    } finally {
      fixture.cleanup();
    }
  });

  it("keeps CJK characters intact when a code point splits across stdout chunks", async () => {
    const fixture = createEchoAgentScript("claude", { cjkSplit: true, cjkRepeat: 100 });
    try {
      const chunks: string[] = [];
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        prompt: "p",
        timeoutMs: 30_000,
        onChunk: (c) => { chunks.push(c); },
      });
      const expected = "你好".repeat(100);
      expect(out).toBe(expected);
      expect(out).not.toContain("\uFFFD");
      expect(chunks.join("")).toBe(out);
    } finally {
      fixture.cleanup();
    }
  });

  it("truncates CJK output on a byte cap without corrupting the cut", async () => {
    // 100 CJK chars = 300 UTF-8 bytes; cap 100 bytes holds 33 complete chars
    // (99 bytes: 16 full 你好 pairs + one 你); the 34th char's dangling lead
    // byte must be dropped, never decoded to U+FFFD.
    const fixture = createEchoAgentScript("claude", { cjkSplit: true, cjkRepeat: 50 });
    try {
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        prompt: "p",
        timeoutMs: 30_000,
        maxOutputBytes: 100,
      });
      expect(Buffer.byteLength(out, "utf8")).toBeLessThanOrEqual(100);
      expect(out).toBe("你好".repeat(16) + "你");
      expect(out).not.toContain("\uFFFD");
    } finally {
      fixture.cleanup();
    }
  });

  it("rejects immediately when the signal is already aborted", async () => {
    const fixture = createEchoAgentScript("claude");
    try {
      const controller = new AbortController();
      controller.abort();
      await expect(
        runAgentTurn({ execPath: fixture.agentPath, prompt: "p", timeoutMs: 30_000, signal: controller.signal }),
      ).rejects.toThrow(/abort/i);
    } finally {
      fixture.cleanup();
    }
  });

  it("rejects with stderr context on nonzero exit", async () => {
    const fixture = createEchoAgentScript("claude", { failWith: { code: 3, message: "agent-boom" } });
    try {
      await expect(
        runAgentTurn({ execPath: fixture.agentPath, prompt: "p", timeoutMs: 30_000 }),
      ).rejects.toThrow(/agent-boom/);
    } finally {
      fixture.cleanup();
    }
  });

  it("streams chunks through onChunk before resolving", async () => {
    const fixture = createEchoAgentScript("claude");
    try {
      let chunks = "";
      const out = await runAgentTurn({
        execPath: fixture.agentPath,
        prompt: "stream me",
        timeoutMs: 30_000,
        onChunk: (c) => { chunks += c; },
      });
      expect(out).toContain("stream me");
      expect(chunks).toContain("stream me");
    } finally {
      fixture.cleanup();
    }
  });

  it("rejects promptly when the signal aborts", async () => {
    const fixture = createEchoAgentScript("claude", { hangMs: 8000 });
    try {
      const controller = new AbortController();
      const pending = runAgentTurn({
        execPath: fixture.agentPath,
        prompt: "p",
        timeoutMs: 30_000,
        signal: controller.signal,
      });
      setTimeout(() => controller.abort(), 300);
      await expect(pending).rejects.toThrow(/abort/i);
    } finally {
      fixture.cleanup();
    }
  });
});
