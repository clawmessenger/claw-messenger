// @vitest-environment node
import { afterEach, describe, expect, it } from "vitest";
import { createServer, type Server } from "node:http";
import { XiachatApi } from "./api.js";

describe("XiachatApi", () => {
  it("register posts to /api/ai/register and returns node_id + token", async () => {
    const { server, url } = await startJsonServer({
      "POST /api/ai/register": { status: 201, body: { node_id: "node_1", token: "t", device_credential_ticket: "dc_1", binding_version: 1 } },
    });
    try {
      const api = new XiachatApi(url);
      const out = await api.register({ name: "n", aiType: "claude", nodeType: "ai" });
      expect(out.nodeId).toBe("node_1");
      expect(out.token).toBe("t");
    } finally {
      server.close();
    }
  });

  it("register sends ai_type, node_type and pairing_ticket in the request body", async () => {
    const { server, url, captured } = await startJsonServer({
      "POST /api/ai/register": { status: 201, body: { node_id: "node_1", token: "t" } },
    });
    try {
      const api = new XiachatApi(url);
      await api.register({ name: "n", aiType: "claude", nodeType: "ai", pairingTicket: "pt_9" });
      expect(captured["POST /api/ai/register"]).toMatchObject({
        ai_type: "claude",
        node_type: "ai",
        pairing_ticket: "pt_9",
      });
    } finally {
      server.close();
    }
  });

  it("register omits pairing_ticket when not provided", async () => {
    const { server, url, captured } = await startJsonServer({
      "POST /api/ai/register": { status: 201, body: { node_id: "node_1", token: "t" } },
    });
    try {
      const api = new XiachatApi(url);
      await api.register({ name: "n", aiType: "claude", nodeType: "ai" });
      expect(captured["POST /api/ai/register"]).not.toHaveProperty("pairing_ticket");
    } finally {
      server.close();
    }
  });

  it("refreshToken posts to /api/claw/refresh-token/{nodeId}", async () => {
    const { server, url } = await startJsonServer({
      "POST /api/claw/refresh-token/node_9": { status: 200, body: { token: "fresh" } },
    });
    try {
      const api = new XiachatApi(url);
      const out = await api.refreshToken("node_9");
      expect(out.token).toBe("fresh");
    } finally {
      server.close();
    }
  });

  it("claimPairing posts to /api/claw/pairing/{ticket}/claim", async () => {
    const { server, url } = await startJsonServer({
      "POST /api/claw/pairing/pt_1/claim": { status: 200, body: { device_credential_id: "dc_2", device_secret: "s", node_id: "node_2" } },
    });
    try {
      const api = new XiachatApi(url);
      const out = await api.claimPairing("pt_1", "", "idem-1");
      expect(out.deviceCredentialId).toBe("dc_2");
    } finally {
      server.close();
    }
  });

  it("claimPairing sends client_claim_key and idempotency_key in the request body", async () => {
    const { server, url, captured } = await startJsonServer({
      "POST /api/claw/pairing/pt_1/claim": { status: 200, body: { device_credential_id: "dc_2", device_secret: "s", node_id: "node_2" } },
    });
    try {
      const api = new XiachatApi(url);
      await api.claimPairing("pt_1", "cck-1", "idem-2");
      expect(captured["POST /api/claw/pairing/pt_1/claim"]).toMatchObject({
        client_claim_key: "cck-1",
        idempotency_key: "idem-2",
      });
    } finally {
      server.close();
    }
  });

  it("claimPairing maps session status/ticket/expiresAt when present", async () => {
    const { server, url } = await startJsonServer({
      "POST /api/claw/pairing/pt_1/claim": {
        status: 200,
        body: {
          device_credential_id: "dc_2",
          device_secret: "",
          node_id: "node_2",
          session: { status: "replayed", ticket: "st_1", expires_at: "2026-10-01T00:00:00Z" },
        },
      },
    });
    try {
      const api = new XiachatApi(url);
      const out = await api.claimPairing("pt_1", "cck-1", "idem-2");
      expect(out.session).toEqual({ status: "replayed", ticket: "st_1", expiresAt: "2026-10-01T00:00:00Z" });
      expect(out.session?.status).toBe("replayed");
    } finally {
      server.close();
    }
  });

  it("getConfig returns appKey", async () => {
    const { server, url } = await startJsonServer({
      "GET /api/config/rongcloud": { status: 200, body: { appKey: "pk1" } },
    });
    try {
      const api = new XiachatApi(url);
      const out = await api.getConfig();
      expect(out.appKey).toBe("pk1");
    } finally {
      server.close();
    }
  });

  it("rejects with method and path context when a 2xx response has malformed JSON", async () => {
    const { server, url } = await startJsonServer({
      "GET /api/config/rongcloud": { status: 200, raw: "not-json{{" },
    });
    try {
      const api = new XiachatApi(url);
      await expect(api.getConfig()).rejects.toThrow(/GET \/api\/config\/rongcloud: decode response:/);
    } finally {
      server.close();
    }
  });
});

interface CapturedServer {
  server: Server;
  url: string;
  captured: Record<string, unknown>;
}

function startJsonServer(routes: Record<string, { status: number; body?: unknown; raw?: string }>): Promise<CapturedServer> {
  const captured: Record<string, unknown> = {};
  const server = createServer((req, res) => {
    const key = `${req.method} ${req.url}`;
    const route = routes[key];
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      const bodyText = Buffer.concat(chunks).toString("utf8");
      if (bodyText) {
        try {
          captured[key] = JSON.parse(bodyText);
        } catch {
          captured[key] = bodyText;
        }
      }
      if (!route) {
        res.writeHead(404).end();
        return;
      }
      res.writeHead(route.status, { "Content-Type": "application/json" });
      res.end(route.raw ?? JSON.stringify(route.body));
    });
  });
  return new Promise<CapturedServer>((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      resolve({ server, url: `http://127.0.0.1:${(server.address() as { port: number }).port}`, captured });
    });
  });
}
