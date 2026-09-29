// @vitest-environment node
import { describe, expect, it } from "vitest";
import { discoverAgents, KNOWN_AGENT_CLIS, runAgentTurn } from "./agents.js";
import { createEchoAgentScript, type EchoAgentFixture } from "./test-fixtures.js";

const windowsFlaky = process.platform === "win32";

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
