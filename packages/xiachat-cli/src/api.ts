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
  // 仅 per-agent 运维节点（agent === "ops" 且 rc 为 "<agentRC>_ops"）携带：
  // 该 ops 子进程专属服务的 agent 融云 id。缺省 = 机器级 ops（全机共享）。
  opsFor?: string;
}

// One bound agent's operator-desired run-state, as returned by
// POST /api/claw/device/control. `version` is 0 when the server has never
// recorded an intent for this agent — the same "run it" baseline the supervisor
// assumes on a fresh bind.
export interface AgentControlState {
  agent: string;
  nodeId: string;
  desiredState: "running" | "stopped" | string;
  version: number;
}

// Result of POST /api/claw/node-files: where the file landed and how the web
// should reference it. `url` is an API path (site-relative), never a filesystem
// path — the web prefixes it with its own API base before rendering.
export interface NodeFileUploadResult {
  url: string;
  key: string;
  filename: string;
  type: string;
  size: number;
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
      // wantPerAgentOps：请服务端为本机每个 agent 确保专属运维节点并下发
      // （运维会话按智能体隔离）；旧服务端不认识该字段会忽略，结果退化为
      // 只有机器级 ops —— 与旧行为一致。
      { nodeId, credentialId, secret, wantPerAgentOps: true },
    );
    const nodes = raw.data?.nodes;
    if (!Array.isArray(nodes)) {
      throw new Error("POST /api/claw/device/nodes: unexpected response shape");
    }
    return nodes;
  }

  // Bind additional agent nodes to this machine, authenticated by the
  // machine node's device credential (the supervisor auto-binds its
  // built-in ops agent here). Idempotent server-side: re-binding an
  // existing agent node reuses it and never clobbers its credentials.
  async bindDeviceAgents(
    nodeId: string,
    credentialId: string,
    secret: string,
    agents: string[],
  ): Promise<void> {
    await this.request("POST", "/api/claw/device/bind-agents", { nodeId, credentialId, secret, agents });
  }

  // Runtime heartbeat: tells the server this device/agent is alive so the
  // legacy device list can show online/offline without an IM probe.
  async heartbeat(nodeId: string, credentialId: string, secret: string): Promise<void> {
    await this.request("POST", "/api/claw/device/heartbeat", { nodeId, credentialId, secret });
  }

  // Operator-desired run-state for one bound agent, as polled by the
  // supervisor. The server holds the intent rather than pushing it: the only
  // process that can start or stop an agent is the supervisor, and it holds no
  // IM connection (its children own those), so it has to come and ask.
  async fetchDeviceControl(
    nodeId: string,
    credentialId: string,
    secret: string,
  ): Promise<AgentControlState[]> {
    const raw = await this.request<{ data?: { agents?: AgentControlState[] } }>(
      "POST",
      "/api/claw/device/control",
      { nodeId, credentialId, secret },
    );
    const agents = raw.data?.agents;
    if (!Array.isArray(agents)) {
      throw new Error("POST /api/claw/device/control: unexpected response shape");
    }
    return agents;
  }

  // 把 agent 本轮产出的文件送上来。鉴权沿用同组设备接口的约定：机器节点的
  // device credential 以表单字段随 multipart 一起提交。服务端存好后返回一个
  // API 路径（/api/claw/files/<key>），设备端把它塞进 file_message 发出去，
  // web 端据此渲染文件气泡与右侧预览面板。
  async uploadNodeFile(
    nodeId: string,
    credentialId: string,
    secret: string,
    file: { data: Uint8Array; name: string; contentType?: string },
  ): Promise<NodeFileUploadResult> {
    const form = new FormData();
    form.append("nodeId", nodeId);
    form.append("credentialId", credentialId);
    form.append("secret", secret);
    form.append(
      "file",
      new Blob([file.data as BlobPart], file.contentType ? { type: file.contentType } : undefined),
      file.name,
    );
    const res = await fetch(this.baseUrl + "/api/claw/node-files", { method: "POST", body: form });
    if (!res.ok) {
      throw new Error(`POST /api/claw/node-files: HTTP ${res.status}`);
    }
    const body = (await res.json()) as { code?: number; message?: string; data?: NodeFileUploadResult };
    const data = body.data;
    if (!data?.url) {
      throw new Error(`POST /api/claw/node-files: ${body.message || "unexpected response shape"}`);
    }
    return data;
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {    const res = await fetch(this.baseUrl + path, {
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
