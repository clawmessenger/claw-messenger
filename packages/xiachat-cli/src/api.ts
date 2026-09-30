export interface RegisterParams {
  name: string;
  aiType: string;
  nodeType: string;
  capabilities?: string[];
  macAddress?: string;
  pairingTicket?: string;
  // Agent CLIs discoverable on this machine; reported with the register so
  // the web binding dialog can list them for the user to bind.
  agents?: string[];
}

export interface RegisterResult {
  nodeId: string;
  token: string;
  deviceCredentialTicket?: string;
  bindingVersion?: number;
}

export interface ClaimSession {
  status: string;
  ticket: string;
  expiresAt?: string;
}

export interface ClaimResult {
  deviceCredentialId: string;
  deviceSecret: string;
  nodeId: string;
  session?: ClaimSession;
}

// One bound agent's full IM identity, as returned by
// POST /api/claw/device/nodes (field names match the Go struct tags).
export interface MachineAgentCredential {
  agent: string;
  nodeId: string;
  rongCloudUser: string;
  token: string;
  credentialId: string;
  deviceSecret: string;
}

export class XiachatApi {
  constructor(private readonly baseUrl: string) {}

  async getConfig(): Promise<{ appKey: string }> {
    return this.request("GET", "/api/config/rongcloud");
  }

  async register(params: RegisterParams): Promise<RegisterResult> {
    const raw = await this.request<{ node_id: string; token: string; device_credential_ticket?: string; binding_version?: number }>(
      "POST",
      "/api/ai/register",
      {
        name: params.name,
        ai_type: params.aiType,
        node_type: params.nodeType,
        capabilities: params.capabilities ?? [],
        mac_address: params.macAddress ?? "",
        ...(params.pairingTicket ? { pairing_ticket: params.pairingTicket } : {}),
        ...(params.agents && params.agents.length > 0 ? { agents: params.agents } : {}),
      },
    );
    return {
      nodeId: raw.node_id,
      token: raw.token,
      deviceCredentialTicket: raw.device_credential_ticket,
      bindingVersion: raw.binding_version,
    };
  }

  async claimPairing(ticket: string, clientClaimKey: string, idempotencyKey: string): Promise<ClaimResult> {
    const raw = await this.request<{
      device_credential_id: string;
      device_secret: string;
      node_id: string;
      session?: { status: string; ticket: string; expires_at?: string };
    }>(
      "POST",
      `/api/claw/pairing/${encodeURIComponent(ticket)}/claim`,
      { client_claim_key: clientClaimKey, idempotency_key: idempotencyKey },
    );
    return {
      deviceCredentialId: raw.device_credential_id,
      deviceSecret: raw.device_secret,
      nodeId: raw.node_id,
      ...(raw.session
        ? { session: { status: raw.session.status, ticket: raw.session.ticket, ...(raw.session.expires_at ? { expiresAt: raw.session.expires_at } : {}) } }
        : {}),
    };
  }

  async refreshToken(nodeId: string): Promise<{ token: string }> {
    return this.request("POST", `/api/claw/refresh-token/${encodeURIComponent(nodeId)}`);
  }

  // Supervisor mode: exchange the machine node's device credential for the
  // credentials of every agent bound to this device. The legacy endpoint
  // answers with the claw envelope {code,message,data:{nodes:[...]}}.
  async fetchDeviceNodes(
    nodeId: string,
    credentialId: string,
    secret: string,
  ): Promise<MachineAgentCredential[]> {
    const raw = await this.request<{ data?: { nodes?: MachineAgentCredential[] } }>(
      "POST",
      "/api/claw/device/nodes",
      { nodeId, credentialId, secret },
    );
    const nodes = raw.data?.nodes;
    if (!Array.isArray(nodes)) {
      throw new Error("POST /api/claw/device/nodes: unexpected response shape");
    }
    return nodes;
  }

  // Runtime heartbeat: tells the server this device/agent is alive so the
  // legacy device list can show online/offline without an IM probe.
  async heartbeat(nodeId: string, credentialId: string, secret: string): Promise<void> {
    await this.request("POST", "/api/claw/device/heartbeat", { nodeId, credentialId, secret });
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await fetch(this.baseUrl + path, {
      method,
      headers: body !== undefined ? { "Content-Type": "application/json" } : {},
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    if (!res.ok) {
      throw new Error(`${method} ${path}: HTTP ${res.status}`);
    }
    try {
      return (await res.json()) as T;
    } catch (err) {
      throw new Error(`${method} ${path}: decode response: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
}
