export interface RegisterParams {
  name: string;
  aiType: string;
  nodeType: string;
  capabilities?: string[];
  macAddress?: string;
  pairingTicket?: string;
}

export interface RegisterResult {
  nodeId: string;
  token: string;
  deviceCredentialTicket?: string;
  bindingVersion?: number;
}

export interface ClaimResult {
  deviceCredentialId: string;
  deviceSecret: string;
  nodeId: string;
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
    const raw = await this.request<{ device_credential_id: string; device_secret: string; node_id: string }>(
      "POST",
      `/api/claw/pairing/${encodeURIComponent(ticket)}/claim`,
      { client_claim_key: clientClaimKey, idempotency_key: idempotencyKey },
    );
    return { deviceCredentialId: raw.device_credential_id, deviceSecret: raw.device_secret, nodeId: raw.node_id };
  }

  async refreshToken(nodeId: string): Promise<{ token: string }> {
    return this.request("POST", `/api/claw/refresh-token/${encodeURIComponent(nodeId)}`);
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
    return (await res.json()) as T;
  }
}
