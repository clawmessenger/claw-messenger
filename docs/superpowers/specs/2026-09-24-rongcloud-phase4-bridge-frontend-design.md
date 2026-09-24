# 融云 Channel 适配器 Phase 4 — Bridge + Frontend 设计

> 基于 Phase 1-3 完成的 IM 通道、基础设施、业务逻辑与讨论编排层，
> 实现服务端 AI CLI Bridge 与前端集成，打通多 AI 节点讨论的端到端流程。

---

## §1 架构概览

### 1.1 背景

Phase 3 完成了讨论编排层：事件溯源状态机、v2/v3 协议、host 选举、协调器主循环。
协调器通过 `sendPrivateMessage` 向 AI 节点发送 `your_turn` 命令，AI 节点通过
`command_result` / `stream` / `text` 消息回复，经 webhook → `handleNodeMessage` →
`dispatchToCoordinator` → `registry.SendResponse` 馈入协调器。

但 AI 节点本身需要运行 CLI Bridge 软件才能接收 RongCloud 命令并调用 Agent CLI。
对于服务端管理的节点（通过 `POST /api/ai/register` 注册，`ai_type` 标记 Agent 类型），
Phase 4 提供服务端 Bridge 组件，在协调器发送 `your_turn` 后直接调用 Agent CLI，
无需 AI 节点运行独立 Bridge。

Phase 4 还需要完成前端集成，让用户可以通过 Web UI 管理 RongCloud 聊天室、节点、
设备、配对，以及启动/停止/监控讨论。

### 1.2 新增组件

| 组件 | 文件 | 职责 |
|------|------|------|
| DiscussionBridge | `discussion_bridge.go` | 服务端 Bridge：your_turn → Agent CLI 执行 → 流式回传 |
| Coordinator 集成 | `discussion_coordinator.go` | sendYourTurn 后对 server-managed 节点调用 Bridge |
| system_handler 补全 | `system_handler.go` | `your_turn` action：解析命令参数，委托 Bridge |
| 前端类型 | `packages/core/types/rongcloud.ts` | TypeScript 接口 |
| 前端 Schemas | `packages/core/api/schemas.ts` | zod 校验 schema + EMPTY 常量 |
| 前端 API Client | `packages/core/api/client.ts` | 30 个 API 方法 |
| 前端 Queries | `packages/core/rongcloud/queries.ts` + `index.ts` | TanStack Query options |
| 前端 Views | `packages/views/rongcloud/` | 设置面板 + 聊天室管理 + 讨论监控 |
| 前端路由 | `apps/web/app/[workspaceSlug]/settings/` | 集成到 Settings 面板 |
| Settings 集成 | `packages/views/settings/components/integrations-tab.tsx` | RongCloud Tab |

### 1.3 设计原则

1. **Bridge 在协调器侧运行**：协调器发送 `your_turn` 后，对 server-managed 节点直接
   调用 Agent CLI，将结果通过 `registry.SendResponse` 直接馈入协调器，无需经过 RongCloud 回环。
2. **外部节点兼容**：外部 AI 节点（有自己的 CLI Bridge）仍通过 RongCloud 消息流回复，
   webhook → `handleNodeMessage` → `registry.SendResponse`。
3. **Agent CLI 发现**：复用 daemon 的 `probeAgentCLIs()` 发现机制和 runtime profile 配置。
4. **前端遵循 DingTalk 模式**：types → schemas → client → queries → views → routes，
   与 lark/slack/telegram/wecom 等集成保持一致。
5. **无新表**：Phase 4 不新增数据库表，复用 Phase 2a/3 的 9 张表。

---

## §2 数据库设计

Phase 4 不新增表。复用现有：

- `rongcloud_chatroom` (host_node_id, max_rounds, conversation_kind, config, status)
- `rongcloud_chatroom_member` (speaking_order, discussion_model, role_name, model)
- `rongcloud_node` (ai_type, capabilities, deploy_status, binding_version)
- `rongcloud_device` (credential_id, credential_secret_encrypted, status)
- `rongcloud_discussion_event` (event_type, round_number, speaking_order, content)
- `rongcloud_system_config` (config_key, node_id, config, config_version)
- `rongcloud_pairing_session`, `rongcloud_user`, `rongcloud_node_model_catalog`

### 2.1 节点管理标记

`rongcloud_node.ai_type` 非空且为已知 Agent CLI 名称（如 `"claude"`, `"codex"`, `"opencode"`）
的节点被视为 **server-managed**。`ai_type` 为空或未知的节点被视为 **external**。

---

## §3 Bridge 设计

### 3.1 DiscussionBridge

```go
type DiscussionBridge struct {
    queries  *db.Queries
    logger   *slog.Logger
    agents   map[string]string // ai_type → executable path (from probeAgentCLIs)
}

func NewDiscussionBridge(queries *db.Queries, logger *slog.Logger) *DiscussionBridge
```

**方法：**

```go
// ExecuteTurn invokes the agent CLI for a server-managed node.
// Returns the full text response.
func (b *DiscussionBridge) ExecuteTurn(
    ctx context.Context,
    chatroomID pgtype.UUID,
    nodeID pgtype.UUID,
    prompt string,
    model string,
) (string, error)

// IsServerManaged checks if a node is server-managed (has a known ai_type).
func (b *DiscussionBridge) IsServerManaged(node db.RongcloudNode) bool

// RefreshAgents re-discovers available agent CLIs.
func (b *DiscussionBridge) RefreshAgents()
```

### 3.2 执行流程

1. `ExecuteTurn` 查询节点 `GetRongCloudNodeByID` 获取 `ai_type`
2. 查找 `b.agents[ai_type]` 获取可执行路径
3. 构建命令：`<executable> --print "<prompt>"` (或 `-p` 取决于 CLI)
4. 通过 `exec.CommandContext` 执行，设置超时（120s 与 turn_timeout 一致）
5. 捕获 stdout 作为完整响应
6. 返回响应文本

### 3.3 协调器集成

`DiscussionCoordinator` 新增字段：

```go
type DiscussionCoordinator struct {
    // ... existing fields ...
    bridge *DiscussionBridge // nil = no bridge (external-only mode)
}
```

`NewDiscussionCoordinator` 新增 `bridge *DiscussionBridge` 参数（可为 nil）。

`Run` 方法中，`sendYourTurn` 后增加 bridge 检查：

```go
func (c *DiscussionCoordinator) sendYourTurn(ctx context.Context, speaker SpeakerInfo, round, speakingOrder int) error {
    // ... existing: send via client.sendPrivateMessage ...

    // Phase 4: if bridge is available and node is server-managed, also invoke locally
    if c.bridge != nil {
        node, err := c.bridge.queries.GetRongCloudNodeByID(ctx, speaker.NodeID)
        if err == nil && c.bridge.IsServerManaged(node) {
            go c.executeBridgeTurn(ctx, speaker, round, speakingOrder)
        }
    }
    return nil
}

func (c *DiscussionCoordinator) executeBridgeTurn(ctx context.Context, speaker SpeakerInfo, round, speakingOrder int) {
    prompt := c.buildPrompt(speaker, round)
    resp, err := c.bridge.ExecuteTurn(ctx, c.chatroomID, speaker.NodeID, prompt, speaker.Model)
    if err != nil {
        c.responseCh <- NodeResponse{
            NodeID:   speaker.RongcloudUserID,
            MsgType:  "command_result",
            TurnID:   fmt.Sprintf("turn_%d_%d", round, speakingOrder),
            Content:  "",
            Error:    err.Error(),
        }
        return
    }
    c.responseCh <- NodeResponse{
        NodeID:   speaker.RongcloudUserID,
        MsgType:  "command_result",
        TurnID:   fmt.Sprintf("turn_%d_%d", round, speakingOrder),
        Content:  resp,
    }
}
```

### 3.4 system_handler your_turn action

`handleDiscussionCommand` 新增 `case "your_turn"`：

```go
func (h *systemHandler) handleDiscussionCommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
    switch cmd.Action {
    case "status":
        // ... existing ...
    case "your_turn":
        // Phase 4: Parse params and log (the bridge is handled in coordinator)
        // This handles the case where the system node receives a your_turn command
        // (e.g., from the webhook seeing the outgoing message)
        // No-op: the coordinator already handles bridge execution
        payload := map[string]interface{}{
            "ok":      true,
            "message": "your_turn acknowledged",
        }
        return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, payload)
    default:
        return h.sendError(ctx, msg, cmd.RequestID, "unknown discussion action: "+cmd.Action)
    }
}
```

### 3.5 DiscussionService 集成

`NewDiscussionService` 新增 `bridge *DiscussionBridge` 参数：

```go
func NewDiscussionService(
    queries *db.Queries,
    client *rongcloudAPIClient,
    registry *DiscussionRegistry,
    bridge *DiscussionBridge,
    logger *slog.Logger,
) *DiscussionService
```

`StartDiscussion` 创建 coordinator 时传入 bridge：

```go
coord := NewDiscussionCoordinator(
    chatroomID, workspaceID, state,
    s.client, eventStore, streamAssembler,
    s.bridge, // new param
    s.logger,
)
```

### 3.6 Router 集成

```go
// In env-gated block:
rcBridge := rongcloud.NewDiscussionBridge(queries, slog.Default())
rcRegistry := rongcloud.NewDiscussionRegistry()
h.RongCloudDiscussion = rongcloud.NewDiscussionService(queries, rcClient, rcRegistry, rcBridge, slog.Default())
```

---

## §4 前端设计

### 4.1 类型定义 (`packages/core/types/rongcloud.ts`)

```typescript
/** RongCloud integration configuration */
export interface RongCloudConfig {
  app_key: string;
  configured: boolean;
}

/** RongCloud chatroom */
export interface RongCloudChatroom {
  id: string;
  workspace_id: string;
  rongcloud_chatroom_id: string;
  owner_user_id: string;
  host_node_id: string | null;
  max_rounds: number;
  conversation_kind: string;
  config: Record<string, unknown>;
  status: "active" | "deleted" | string;
  created_at: string;
  updated_at: string;
}

export interface ListRongCloudChatroomsResponse {
  chatrooms: RongCloudChatroom[];
}

/** RongCloud AI node */
export interface RongCloudNode {
  id: string;
  workspace_id: string;
  owner_user_id: string;
  rongcloud_user_id: string;
  node_id: string;
  ai_type: string;
  capabilities: string[];
  deploy_status: "registered" | "deployed" | string;
  binding_version: number;
  created_at: string;
  updated_at: string;
}

export interface ListRongCloudNodesResponse {
  nodes: RongCloudNode[];
}

/** RongCloud device */
export interface RongCloudDevice {
  id: string;
  workspace_id: string;
  owner_user_id: string;
  node_id: string;
  device_name: string;
  device_type: string;
  credential_id: string;
  status: "active" | "disabled" | "deleted" | string;
  created_at: string;
  updated_at: string;
}

export interface ListRongCloudDevicesResponse {
  devices: RongCloudDevice[];
}

/** RongCloud node model catalog */
export interface RongCloudNodeModel {
  id: string;
  workspace_id: string;
  node_id: string;
  model_id: string;
  provider: string;
  model_name: string;
  config: Record<string, unknown>;
  created_at: string;
}

export interface ListRongCloudNodeModelsResponse {
  models: RongCloudNodeModel[];
}

/** RongCloud discussion state */
export interface RongCloudDiscussion {
  chatroom_id: string;
  workspace_id: string;
  status: "idle" | "starting" | "in_progress" | "paused" | "ended" | string;
  current_round: number;
  current_speaker: string | null;
  host_node_id: string | null;
  speakers: RongCloudSpeaker[];
  started_at: string | null;
  ended_at: string | null;
}

export interface RongCloudSpeaker {
  node_id: string;
  speaking_order: number;
  role_name: string;
  model: string;
}

/** RongCloud discussion event */
export interface RongCloudDiscussionEvent {
  id: string;
  chatroom_id: string;
  workspace_id: string;
  event_type: string;
  round_number: number;
  speaking_order: number;
  node_id: string;
  content: Record<string, unknown>;
  msg_uid: string | null;
  created_at: string;
}

export interface ListRongCloudDiscussionEventsResponse {
  events: RongCloudDiscussionEvent[];
}

/** RongCloud pairing session */
export interface RongCloudPairingSession {
  id: string;
  workspace_id: string;
  ticket: string;
  status: "pending" | "claimed" | "expired" | string;
  candidate_node_ids: string[];
  client_claim_key: string | null;
  expires_at: string;
  created_at: string;
}

/** RongCloud system host config */
export interface RongCloudSystemHost {
  node_id: string;
  config: Record<string, unknown>;
}
```

### 4.2 Schemas (`packages/core/api/schemas.ts`)

```typescript
// RongCloud
RongCloudConfigSchema = z.object({
  app_key: z.string(),
  configured: z.boolean(),
}).loose()
EMPTY_RONGCLOUD_CONFIG: RongCloudConfig = { app_key: "", configured: false }

RongCloudChatroomSchema = z.object({
  id: z.string(),
  workspace_id: z.string(),
  rongcloud_chatroom_id: z.string(),
  owner_user_id: z.string(),
  host_node_id: z.string().nullable().default(null),
  max_rounds: z.number().default(3),
  conversation_kind: z.string().default(""),
  config: z.record(z.unknown()).default({}),
  status: z.string().default("active"),
  created_at: z.string().default(""),
  updated_at: z.string().default(""),
}).loose()
EMPTY_RONGCLOUD_CHATROOM: RongCloudChatroom = { ... }

ListRongCloudChatroomsResponseSchema = z.object({
  chatrooms: z.array(RongCloudChatroomSchema).catch([]),
}).loose()
EMPTY_LIST_RONGCLOUD_CHATROOMS_RESPONSE = { chatrooms: [] }

// Similar for Node, Device, NodeModel, Discussion, DiscussionEvent, PairingSession, SystemHost
```

### 4.3 API Client 方法 (`packages/core/api/client.ts`)

```typescript
// Config
async getRongCloudConfig(): Promise<RongCloudConfig>
// Chatrooms
async listRongCloudChatrooms(workspaceId: string): Promise<RongCloudChatroom[]>
async getRongCloudChatroom(workspaceId: string, chatroomId: string): Promise<RongCloudChatroom>
async createRongCloudChatroom(workspaceId: string, body: { rongcloud_chatroom_id: string; max_rounds?: number; conversation_kind?: string }): Promise<RongCloudChatroom>
async updateRongCloudChatroom(workspaceId: string, chatroomId: string, body: { host_node_id?: string; max_rounds?: number; conversation_kind?: string; config?: Record<string, unknown> }): Promise<RongCloudChatroom>
async deleteRongCloudChatroom(workspaceId: string, chatroomId: string): Promise<void>
async setRongCloudChatroomMembers(workspaceId: string, chatroomId: string, body: { members: RongCloudChatroomMemberInput[] }): Promise<void>
// Nodes
async listRongCloudNodes(workspaceId: string): Promise<RongCloudNode[]>
async listRongCloudNodeModels(workspaceId: string, nodeId: string): Promise<RongCloudNodeModel[]>
async addRongCloudNodeModel(workspaceId: string, nodeId: string, body: { model_id: string; provider: string; model_name: string; config?: Record<string, unknown> }): Promise<RongCloudNodeModel>
async deleteRongCloudNode(workspaceId: string, nodeId: string): Promise<void>
// Devices
async listRongCloudDevices(workspaceId: string): Promise<RongCloudDevice[]>
async createRongCloudDevice(workspaceId: string, body: { node_id: string; device_name: string; device_type?: string }): Promise<RongCloudDevice>
async deleteRongCloudDevice(workspaceId: string, deviceId: string): Promise<void>
// System Host
async getRongCloudSystemHost(workspaceId: string): Promise<RongCloudSystemHost>
async updateRongCloudSystemHost(workspaceId: string, body: { node_id: string; config?: Record<string, unknown> }): Promise<RongCloudSystemHost>
// Discussions
async startRongCloudDiscussion(workspaceId: string, chatroomId: string): Promise<RongCloudDiscussion>
async stopRongCloudDiscussion(workspaceId: string, chatroomId: string): Promise<RongCloudDiscussion>
async pauseRongCloudDiscussion(workspaceId: string, chatroomId: string): Promise<RongCloudDiscussion>
async resumeRongCloudDiscussion(workspaceId: string, chatroomId: string): Promise<RongCloudDiscussion>
async getRongCloudDiscussion(workspaceId: string, chatroomId: string): Promise<RongCloudDiscussion>
async listRongCloudDiscussionEvents(workspaceId: string, chatroomId: string): Promise<RongCloudDiscussionEvent[]>
// Pairing
async createRongCloudPairing(workspaceId: string, body: { candidate_node_ids?: string[]; client_claim_key?: string; expires_in_seconds?: number }): Promise<RongCloudPairingSession>
async getRongCloudPairing(workspaceId: string, ticket: string): Promise<RongCloudPairingSession>
// Public endpoints
async registerRongCloudAINode(body: { name: string; mac_address: string; node_type: string; ai_type: string; capabilities?: string[] }): Promise<{ node_id: string; token: string; capabilities: string[]; device_credential_ticket: string; binding_version: number }>
async refreshRongCloudToken(nodeId: string): Promise<{ token: string }>
async enrollRongCloudDeviceCredential(body: { node_id: string }): Promise<{ credential_id: string; secret: string }>
async createRongCloudConnectionSession(body: { node_id: string }): Promise<{ session_id: string }>
async closeRongCloudConnectionSession(sessionId: string): Promise<{ ok: boolean }>
```

### 4.4 Query Options (`packages/core/rongcloud/queries.ts`)

```typescript
export const rongcloudKeys = {
  all: (wsId: string) => ["rongcloud", wsId] as const,
  config: () => ["rongcloud", "config"] as const,
  chatrooms: (wsId: string) => ["rongcloud", wsId, "chatrooms"] as const,
  chatroom: (wsId: string, id: string) => ["rongcloud", wsId, "chatrooms", id] as const,
  nodes: (wsId: string) => ["rongcloud", wsId, "nodes"] as const,
  nodeModels: (wsId: string, nodeId: string) => ["rongcloud", wsId, "nodes", nodeId, "models"] as const,
  devices: (wsId: string) => ["rongcloud", wsId, "devices"] as const,
  systemHost: (wsId: string) => ["rongcloud", wsId, "system-host"] as const,
  discussion: (wsId: string, chatroomId: string) => ["rongcloud", wsId, "discussions", chatroomId] as const,
  discussionEvents: (wsId: string, chatroomId: string) => ["rongcloud", wsId, "discussions", chatroomId, "events"] as const,
  pairing: (wsId: string, ticket: string) => ["rongcloud", wsId, "pairing", ticket] as const,
};

export function rongcloudConfigOptions() { ... }
export function rongcloudChatroomsOptions(wsId: string) { ... }
export function rongcloudNodesOptions(wsId: string) { ... }
export function rongcloudDevicesOptions(wsId: string) { ... }
export function rongcloudSystemHostOptions(wsId: string) { ... }
export function rongcloudDiscussionOptions(wsId: string, chatroomId: string) { ... }
export function rongcloudDiscussionEventsOptions(wsId: string, chatroomId: string) { ... }
```

### 4.5 Views (`packages/views/rongcloud/`)

#### `rongcloud-tab.tsx` — Settings 集成面板
- 显示 App Key 配置状态
- 系统节点管理（注册新 AI 节点）
- 系统主机设置
- 聊天室列表 + 创建/删除
- 讨论启动/停止/监控

#### `chatroom-manager.tsx` — 聊天室管理
- 列表展示所有聊天室
- 创建新聊天室表单
- 点击进入详情/编辑
- 删除按钮（带确认）
- 成员管理（查看/设置 speaking_order）

#### `discussion-monitor.tsx` — 讨论监控
- 当前讨论状态（round/speaker/status）
- 事件日志时间线
- 启动/停止/暂停/恢复按钮
- 自动刷新（5s refetchInterval）

### 4.6 Web 路由

RongCloud 管理集成到 Settings 面板，无需独立路由：

`packages/views/settings/components/integrations-tab.tsx` 新增：

```typescript
import { rongcloudConfigOptions } from "@multica/core/rongcloud/queries";
import { RongCloudTab } from "@multica/views/rongcloud/rongcloud-tab";

// In integration entries:
{
  id: "rongcloud",
  label: "RongCloud",
  description: "Multi-AI node discussion orchestration",
  icon: <MessageSquareIcon />,
  content: <RongCloudTab workspaceId={workspaceId} />,
  state: useQuery(rongcloudConfigOptions()),
}
```

---

## §5 错误处理

| 错误场景 | 处理方式 |
|---------|---------|
| Agent CLI 未安装 | Bridge 返回 error → coordinator 发送 error NodeResponse → emit error event |
| Agent CLI 执行超时 | exec.CommandContext 超时 → 同上 |
| 节点 ai_type 未知 | IsServerManaged 返回 false → 仅发送 RongCloud 命令，不调用 bridge |
| Bridge queries 为 nil | IsServerManaged 返回 false → 降级为 external 模式 |
| 前端 API 404 (后端未配置 RongCloud) | parseWithFallback 返回 EMPTY → 显示"未配置"状态 |

---

## §6 测试策略

### Go 测试 (`rongcloud_test.go`)

1. `TestDiscussionBridgeIsServerManaged` — ai_type 匹配/不匹配
2. `TestDiscussionBridgeExecuteTurnTimeout` — 超时返回 error
3. `TestHandleDiscussionCommandYourTurn` — your_turn action 返回 ok
4. `TestCoordinatorWithBridge` — bridge 非空时调用 executeBridgeTurn

### 前端测试

1. Schema 解析测试 — `rongcloud-schemas.test.ts`
2. RongCloudTab 组件渲染测试 — `rongcloud-tab.test.tsx`
3. DiscussionMonitor 状态展示测试 — `discussion-monitor.test.tsx`

---

## §7 路线图

| Phase | 状态 | 范围 |
|-------|------|------|
| Phase 1 MVP | ✅ Done | Message skeleton + ping RPC |
| Phase 2a Infrastructure | ✅ Done | DB + API + services + endpoints |
| Phase 2b Business Logic | ✅ Done | Chatroom/Device/NodeModel/Pairing 业务逻辑 |
| Phase 3 Discussion | ✅ Done | 事件溯源状态机 + v2/v3 协议 + host 选举 |
| **Phase 4 Bridge + Frontend** | **← 当前** | Bridge + 前端集成 |
| Phase 5+ | TBD | CLI Bridge 独立二进制 + 多模型路由 + 权限细化 |
