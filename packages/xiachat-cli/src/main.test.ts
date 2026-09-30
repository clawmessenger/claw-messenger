// @vitest-environment node
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { buildProgram } from "./main.js";
import { Keystore } from "./keystore.js";
import type { XiachatApi } from "./api.js";

const dirs: string[] = [];
function tempKeystore(): Keystore {
  const d = mkdtempSync(join(tmpdir(), "xiachat-main-"));
  dirs.push(d);
  return new Keystore(join(d, "creds.json"));
}
afterEach(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
  dirs.length = 0;
});

describe("xiachat register", () => {
  it("stores credentials from the server response", async () => {
    const keystore = tempKeystore();
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async () => ({ nodeId: "node_1", token: "tok_1", deviceCredentialTicket: "dc_1", bindingVersion: 1 }),
      claimPairing: async () => ({ deviceCredentialId: "", deviceSecret: "", nodeId: "" }),
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      machineId: () => "test-machine-1",
    });
    await program.parseAsync(["node", "xiachat", "register", "--name", "我的Claude", "--ai-type", "claude", "--server", "http://srv"]);
    const stored = keystore.load();
    expect(stored?.nodeId).toBe("node_1");
    expect(stored?.token).toBe("tok_1");
    expect(stored?.serverUrl).toBe("http://srv");
  });

  it("sends a non-empty stable mac_address on register", async () => {
    const keystore = tempKeystore();
    const seenMac: string[] = [];
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async (params: { macAddress?: string }) => {
        seenMac.push(params.macAddress ?? "");
        return { nodeId: "node_1", token: "tok_1" };
      },
      claimPairing: async () => ({ deviceCredentialId: "", deviceSecret: "", nodeId: "" }),
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      machineId: () => "test-machine-2",
    });
    await program.parseAsync(["node", "xiachat", "register", "--name", "n", "--ai-type", "claude", "--server", "http://srv"]);
    await program.parseAsync(["node", "xiachat", "register", "--name", "n", "--ai-type", "claude", "--server", "http://srv"]);
    expect(seenMac[0]).toBe("test-machine-2");
    expect(seenMac[1]).toBe(seenMac[0]);
  });

  it("register --server is required", async () => {
    const program = buildProgram({
      keystore: tempKeystore(),
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: process.stdout,
    });
    await expect(
      program.parseAsync(["node", "xiachat", "register", "--name", "n", "--ai-type", "claude"]),
    ).rejects.toThrow();
  });

  it("surfaces a pairing-ticket hint on workspace attribution errors", async () => {
    const keystore = tempKeystore();
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async () => {
        throw new Error("POST /api/ai/register: HTTP 400: workspace attribution required: provide a pairing_ticket");
      },
      claimPairing: async () => ({ deviceCredentialId: "", deviceSecret: "", nodeId: "" }),
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      machineId: () => "test-machine-1",
    });
    await expect(
      program.parseAsync(["node", "xiachat", "register", "--name", "n", "--ai-type", "claude", "--server", "http://srv"]),
    ).rejects.toThrow(/--pairing-ticket/);
    expect(keystore.load()).toBeNull();
  });
});

describe("xiachat pair", () => {
  it("does not overwrite the keystore when the ticket was already claimed", async () => {
    const keystore = tempKeystore();
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async () => ({ nodeId: "node_1", token: "tok_1" }),
      claimPairing: async () => ({
        deviceCredentialId: "dc_9",
        deviceSecret: "",
        nodeId: "node_2",
        session: { status: "claimed", ticket: "st_1" },
      }),
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
    });
    await expect(
      program.parseAsync(["node", "xiachat", "pair", "--ticket", "pt_1"]),
    ).rejects.toThrow(/already claimed/);
    // The register step still persisted its own node identity, but the
    // claimed-by-another-device result must not overwrite it.
    expect(keystore.load()?.nodeId).toBe("node_1");
  });

  it("derives the same idempotency key across retries on one machine", async () => {
    const keystore = tempKeystore();
    const seenIdemKeys: string[] = [];
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async () => ({ nodeId: "node_5", token: "tok_5" }),
      claimPairing: async (_ticket: string, _cck: string, idemKey: string) => {
        seenIdemKeys.push(idemKey);
        return {
          deviceCredentialId: "dc_1",
          deviceSecret: "sec_1",
          nodeId: "node_5",
        };
      },
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      machineId: () => "machine-A",
    });
    // A retried claim after a lost response: same ticket, same machine.
    await program.parseAsync(["node", "xiachat", "pair", "--ticket", "pt_same"]);
    await program.parseAsync(["node", "xiachat", "pair", "--ticket", "pt_same"]);
    expect(seenIdemKeys).toHaveLength(2);
    expect(seenIdemKeys[0]).toBe(seenIdemKeys[1]);
    expect(seenIdemKeys[0]).toMatch(/^idem-[0-9a-f]{24}$/);

    // A different ticket on the same machine derives a different key.
    await program.parseAsync(["node", "xiachat", "pair", "--ticket", "pt_other"]);
    expect(seenIdemKeys[2]).not.toBe(seenIdemKeys[0]);
  });

  it("derives a different idempotency key per machine", async () => {
    const keystore = tempKeystore();
    const seenIdemKeys: string[] = [];
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async () => ({ nodeId: "node_6", token: "tok_6" }),
      claimPairing: async (_ticket: string, _cck: string, idemKey: string) => {
        seenIdemKeys.push(idemKey);
        return { deviceCredentialId: "dc_1", deviceSecret: "s", nodeId: "node_6" };
      },
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      machineId: () => "machine-B",
    });
    await program.parseAsync(["node", "xiachat", "pair", "--ticket", "pt_same"]);
    const machineBKey = seenIdemKeys[0];
    const program2 = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
      machineId: () => "machine-C",
    });
    await program2.parseAsync(["node", "xiachat", "pair", "--ticket", "pt_same"]);
    expect(seenIdemKeys[1]).not.toBe(machineBKey);
  });
});

describe("xiachat status", () => {
  it("prints stored identity", async () => {
    const keystore = tempKeystore();
    keystore.save({ nodeId: "node_9", token: "t", serverUrl: "http://srv" });
    const lines: string[] = [];
    const program = buildProgram({
      keystore,
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: { write: (s: string) => { lines.push(s); return true; } } as unknown as NodeJS.WriteStream,
    });
    await program.parseAsync(["node", "xiachat", "status"]);
    expect(lines.join("")).toContain("node_9");
  });
});
