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
});

function startJsonServer(routes: Record<string, { status: number; body: unknown }>) {
  const server = createServer((req, res) => {
    const key = `${req.method} ${req.url}`;
    const route = routes[key];
    if (!route) {
      res.writeHead(404).end();
      return;
    }
    res.writeHead(route.status, { "Content-Type": "application/json" });
    res.end(JSON.stringify(route.body));
  });
  return new Promise<{ server: Server; url: string }>((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      resolve({ server, url: `http://127.0.0.1:${(server.address() as { port: number }).port}` });
    });
  });
}
