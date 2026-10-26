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

// 服务端下发的 exe 升级指令（POST /api/claw/device/control 响应里的 upgrade）。
//
// mirrors 是有序的下载地址：先 GitHub 直连，失败再回退服务端相对路径
// （"/api/agent/upgrade/download?..."）。相对路径由设备自己拼上 serverUrl ——
// 服务端不需要知道自己的公网地址。
export interface DeviceUpgradeIntent {
  generation: number;
  targetVersion: string;
  assetName: string;
  sha256: string;
  force: boolean;
  mirrors: string[];
}

/** 一次设备轮询的结果：启停意图 + 可选的升级指令。 */
export interface DeviceControlSnapshot {
  agents: AgentControlState[];
  upgrade?: DeviceUpgradeIntent;
}

/** 升级阶段（与迁移 601 的 CHECK 一致，设备只能回传这些）。 */
export type DeviceUpgradeStatus =
  | "waiting_idle"
  | "downloading"
  | "verifying"
  | "restarting"
  | "succeeded"
  | "failed"
  | "cancelled";

// 一次按账号启停的结果（POST /api/claw/device/control/set）。
export interface DeviceControlResult {
  agent: string;
  nodeId: string;
  desiredState: "running" | "stopped";
  /** control 行推进后的版本号；supervisor 据此识别 restart（版本变、状态不变）。 */
  version: number;
  restart: boolean;
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

  // Device-code pairing: the terminal asks the server for a one-time session
  // and shows the human a verify URL. Anonymous by design — the device has no
  // identity yet, and possession of the ticket is the only secret.
  async startDevicePairing(params: {
    machineId: string;
    hostname: string;
    platform: string;
    agents: string[];
  }): Promise<{
    ticket: string;
    userCode: string;
    verifyUrl: string;
    expiresAt: string;
    intervalSeconds: number;
  }> {
    const raw = await this.request<{
      data?: {
        ticket?: string;
        user_code?: string;
        verify_url?: string;
        expires_at?: string;
        interval_seconds?: number;
      };
    }>("POST", "/api/claw/pairing/device", {
      machine_id: params.machineId,
      hostname: params.hostname,
      platform: params.platform,
      agents: params.agents,
    });
    const data = raw.data;
    if (!data?.ticket || !data.user_code || !data.verify_url) {
      throw new Error("POST /api/claw/pairing/device: unexpected response shape");
    }
    return {
      ticket: data.ticket,
      userCode: data.user_code,
      verifyUrl: data.verify_url,
      expiresAt: data.expires_at ?? "",
      intervalSeconds: data.interval_seconds && data.interval_seconds > 0 ? data.interval_seconds : 2,
    };
  }

  // Poll the device-side view of a pairing session: the human binds agents in
  // the browser, and the terminal learns about it here. Exposes status and the
  // bound agents only — never the user code or the workspace.
  async pollDevicePairing(ticket: string): Promise<{
    status: string;
    expiresAt: string;
    boundAgents: { agent: string; nodeId: string }[];
  }> {
    const raw = await this.request<{
      data?: {
        status?: string;
        expires_at?: string;
        bound_agents?: { agent?: string; node_id?: string }[];
      };
    }>("GET", `/api/claw/pairing/device/${encodeURIComponent(ticket)}`);
    const data = raw.data;
    if (!data?.status) {
      throw new Error("GET /api/claw/pairing/device: unexpected response shape");
    }
    return {
      status: data.status,
      expiresAt: data.expires_at ?? "",
      boundAgents: (data.bound_agents ?? []).map((b) => ({ agent: b.agent ?? "", nodeId: b.node_id ?? "" })),
    };
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
  //
  // 同一次往返还带两件事（见 server/internal/handler/claw_device_upgrade.go）：
  //   · `report` 把本机 exe 版本与平台报上去 —— 版本是机器属性，而这是唯一的
  //     机器级定时轮询，不需要再开一条通道或往 30s 心跳里塞字段；
  //   · 响应里的 `upgrade` 是本机待执行的升级指令（没有就不下发）。
  // 旧服务端会忽略 report、也不会返回 upgrade：两个字段都是可选的，天然兼容。
  async fetchDeviceControl(
    nodeId: string,
    credentialId: string,
    secret: string,
    report?: { version?: string; platform?: string },
  ): Promise<DeviceControlSnapshot> {
    const raw = await this.request<{ data?: { agents?: AgentControlState[]; upgrade?: DeviceUpgradeIntent } }>(
      "POST",
      "/api/claw/device/control",
      {
        nodeId,
        credentialId,
        secret,
        ...(report?.version ? { version: report.version } : {}),
        ...(report?.platform ? { platform: report.platform } : {}),
      },
    );
    const agents = raw.data?.agents;
    if (!Array.isArray(agents)) {
      throw new Error("POST /api/claw/device/control: unexpected response shape");
    }
    const upgrade = raw.data?.upgrade;
    return upgrade && typeof upgrade.generation === "number"
      ? { agents, upgrade }
      : { agents };
  }

  // 回传升级进度（阶段跃迁时调用，不是按百分比刷）。服务端按代次防抖：代次对不上
  // 的回报会被 409 丢掉，这里把它当成功返回 —— 设备对「这条回传过期了」没有可
  // 采取的动作，下一轮轮询自然会拿到新代次。
  async reportDeviceUpgrade(
    nodeId: string,
    credentialId: string,
    secret: string,
    report: {
      generation: number;
      status: DeviceUpgradeStatus;
      errorCode?: string;
      errorDetail?: string;
      reportedVersion?: string;
      reportedPlatform?: string;
    },
  ): Promise<void> {
    try {
      await this.request("POST", "/api/claw/device/upgrade/report", {
        nodeId,
        credentialId,
        secret,
        generation: report.generation,
        status: report.status,
        ...(report.errorCode ? { errorCode: report.errorCode } : {}),
        ...(report.errorDetail ? { errorDetail: report.errorDetail } : {}),
        ...(report.reportedVersion ? { reportedVersion: report.reportedVersion } : {}),
        ...(report.reportedPlatform ? { reportedPlatform: report.reportedPlatform } : {}),
      });
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      // 409 = 代次过期，属于正常竞态，不算失败；其余只记日志（进度回传失败绝不
      // 应该让正在进行的升级中断）。
      if (message.includes("HTTP 409")) return;
      throw err;
    }
  }

  // 按账号启停单个智能体。服务端复用管理端的 rongcloud_node_control 表，
  // supervisor 下次轮询（10s）时收敛。
  //
  // 服务端会校验作用域：per-agent 运维账号只能操作自己服务的那个 agent，
  // 越界返回 403（见 wholeMachineActionRejection 的姊妹约束）。
  async setDeviceControl(
    nodeId: string,
    credentialId: string,
    secret: string,
    target: string,
    action: "start" | "stop" | "restart",
  ): Promise<DeviceControlResult> {
    const raw = await this.request<{ data?: DeviceControlResult }>(
      "POST",
      "/api/claw/device/control/set",
      { nodeId, credentialId, secret, target, action },
    );
    const data = raw.data;
    if (!data || !data.agent) {
      throw new Error("POST /api/claw/device/control/set: unexpected response shape");
    }
    return data;
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

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    // A hung request would stall `pair` forever while it waits for a human,
    // so every call gets a bounded wall clock. 15s is well above any server
    // response this API produces.
    let res: Response;
    try {
      res = await fetch(this.baseUrl + path, {
        method,
        headers: body !== undefined ? { "Content-Type": "application/json" } : {},
        body: body !== undefined ? JSON.stringify(body) : undefined,
        signal: AbortSignal.timeout(15_000),
      });
    } catch (err) {
      if (err instanceof Error && err.name === "TimeoutError") {
        throw new Error(`${method} ${path}: timeout after 15000ms`);
      }
      throw err;
    }
    if (!res.ok) {
      // 把服务端的 message 带出来：403（越权）与 404（本机没有该智能体）
      // 都是给人看的业务结论，只留一个 HTTP 状态码等于让模型猜。
      // 服务端有两套错误形状：claw 信封给 message，writeError 给 error。
      // 只读 message 会让 409/410 这类结论变成"无原因 400"，两处都要看。
      let detail = "";
      try {
        const failure = (await res.json()) as { message?: string; error?: string };
        const text = failure?.message || failure?.error;
        if (text) detail = `: ${text}`;
      } catch {
        // 非 JSON 错误页，保持原样。
      }
      throw new Error(`${method} ${path}: HTTP ${res.status}${detail}`);
    }
    try {
      return (await res.json()) as T;
    } catch (err) {
      throw new Error(`${method} ${path}: decode response: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
}
