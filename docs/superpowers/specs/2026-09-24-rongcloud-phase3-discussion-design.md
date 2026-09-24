# 融云 Channel 适配器 Phase 3 — Discussion 设计

> 基于 Phase 2a/2b 基础设施与业务逻辑，实现多 AI 节点讨论编排层：
> 事件溯源状态机 + v2/v3 协议 + host 选举，约 3000 行 Go 代码。

---

## §1 架构概览

### 1.1 背景

Phase 2b 完成了 Chatroom / Device / NodeModelCatalog / Pairing 的完整业务逻辑和全部 HTTP 端点。
chatroom 表已包含 `host_node_id`、`max_rounds`、`conversation_kind`、`config` JSONB 字段，
chatroom_member 表已包含 `speaking_order`、`discussion_model`、`role_name`、`role_instructions` 字段，
但均为静态配置，尚无运行时讨论编排。

Phase 3 需要实现：

- **事件溯源状态机**：记录讨论生命周期中的每个事件（讨论开始、轮次开始、发言开始、发言完成、轮次结束、讨论结束），通过事件回放重建讨论状态。
- **v2 协议**：基于 `objectNameCommand` 的命令式轮次控制（discussion.start / discussion.your_turn / discussion.turn_complete / discussion.end）。
- **v3 协议**：基于 `objectNameStream` (RC:StreamMsg) 的流式响应（stream_start / stream_chunk / stream_end）。
- **Host 选举**：chatroom 的 `host_node_id` 为指定 host；当 host 节点无响应时，按 `speaking_order` 选举新 host。
- **node_message_handler 全分发**：处理 AI 节点返回的 command_result / stream / text 消息，馈入讨论协调器。

### 1.2 新增组件

| 组件 | 文件 | 职责 |
|------|------|------|
| DiscussionEventStore | `discussion_event_store.go` | 事件持久化到 `rongcloud_discussion_event` 表 |
| DiscussionStateMachine | `discussion_state_machine.go` | 事件回放重建状态、校验状态转换 |
| DiscussionCoordinator | `discussion_coordinator.go` | 主循环：轮次 → 发言者 → your_turn → 等待响应 → 推进 |
| StreamAssembler | `discussion_stream.go` | 缓冲 v3 流式分片，组装完整消息 |
| DiscussionRegistry | `discussion_registry.go` | 内存注册表：chatroomID → *DiscussionCoordinator + 响应通道 |
| DiscussionService | `discussion_service.go` | 服务层：Start / Stop / Pause / Resume / Status / ListEvents |
| system_handler 补全 | `system_handler.go` | 新增 `case "discussion"` 命令路由 |
| node_message_handler 补全 | `node_message_handler.go` | 全分发：command_result / stream / text → 馈入 coordinator |
| Handler 端点 | `rongcloud_handler.go` | 6 个讨论控制端点 |
| Router 路由 | `router.go` | 6 条新路由 |
| DB 迁移 | `551_rongcloud_discussion_events.up/down.sql` | 事件表 + 索引 |
| sqlc 查询 | `rongcloud.sql` | 事件 CRUD 查询 |

### 1.3 设计原则

1. **讨论层位于 rongcloud 包内**，不侵入 channel/engine 引擎层。引擎处理 IM 管道；Phase 3 在其之上编排多 AI 节点讨论。
2. **事件溯源**：所有状态变更通过追加事件记录，通过回放重建当前状态。无 UPDATE/DELETE 操作。
3. **协调器在服务器端运行**：服务器作为中央协调者，通过 RongCloud client 向各节点发送 your_turn 命令，收集响应后推进。
4. **内存注册表 + DB 事件日志**：运行时协调器状态在内存（`DiscussionRegistry`），持久化通过事件日志。服务重启后回放事件重建。
5. **v2/v3 兼容**：节点可返回 v2（command_result 含完整文本）或 v3（stream 分片）。协调器统一处理。
6. **无外键**：遵循 MUL-3515 §4，应用层维护引用完整性。

---

## §2 数据库设计

### 2.1 新增表

```sql
CREATE TABLE IF NOT EXISTS rongcloud_discussion_event (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chatroom_id     UUID NOT NULL,
    workspace_id    UUID NOT NULL,
    event_type      TEXT NOT NULL CHECK (event_type IN (
        'discussion_started',
        'round_started',
        'turn_started',
        'turn_completed',
        'turn_skipped',
        'round_completed',
        'discussion_ended',
        'discussion_paused',
        'discussion_resumed',
        'host_changed',
        'error'
    )),
    round_number    INT NOT NULL DEFAULT 0,
    speaking_order  INT NOT NULL DEFAULT 0,
    node_id         UUID,
    content         JSONB NOT NULL DEFAULT '{}'::jsonb,
    msg_uid         TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### 2.2 索引（CONCURRENTLY，单独迁移文件）

```sql
-- 552_rongcloud_discussion_event_chatroom_idx.up.sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_rongcloud_discussion_event_chatroom
    ON rongcloud_discussion_event (chatroom_id, created_at);

-- 553_rongcloud_discussion_event_workspace_idx.up.sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_rongcloud_discussion_event_workspace
    ON rongcloud_discussion_event (workspace_id, created_at);
```

### 2.3 已有表依赖

| 表 | Phase 3 使用方式 |
|----|-----------------|
| rongcloud_chatroom | 读取 host_node_id / max_rounds / conversation_kind / config / status |
| rongcloud_chatroom_member | 读取 speaking_order 排序的成员列表 |
| rongcloud_node | 读取 node_id / rongcloud_user_id（映射到 RongCloud user） |
| rongcloud_system_config | 讨论暂停/恢复状态存储（config_key="discussion_state:{chatroomID}"） |

---

## §3 讨论状态机

### 3.1 状态定义

```
idle → starting → in_progress ⇄ paused → ended
                                 ↗
                        (any state, force stop)
```

| 状态 | 描述 |
|------|------|
| `idle` | 讨论尚未开始 |
| `starting` | 收到 start 命令，正在初始化（获取成员、发送通知） |
| `in_progress` | 讨论进行中，轮次循环执行 |
| `paused` | 讨论暂停，等待 resume |
| `ended` | 讨论结束（正常完成或被停止） |

### 3.2 状态转换

| From | Event | To |
|------|-------|-----|
| idle | discussion_started | starting |
| starting | round_started (round=1) | in_progress |
| in_progress | discussion_paused | paused |
| paused | discussion_resumed | in_progress |
| in_progress | discussion_ended | ended |
| starting | error | ended |
| any | discussion_ended (force) | ended |

### 3.3 DiscussionState 结构

```go
type DiscussionState struct {
    ChatroomID    pgtype.UUID
    WorkspaceID   pgtype.UUID
    Status        string // idle, starting, in_progress, paused, ended
    CurrentRound   int
    CurrentSpeaker pgtype.UUID // current speaking node ID
    Speakers      []SpeakerInfo
    HostNodeID    pgtype.UUID
    StartedAt     time.Time
    EndedAt       *time.Time
}

type SpeakerInfo struct {
    NodeID        pgtype.UUID
    SpeakingOrder int
    RoleName      string
    Model         string
    RongcloudUserID string // from rongcloud_node
}
```

### 3.4 事件回放

```go
func ReplayState(events []DiscussionEvent) (DiscussionState, error)
```

按 `created_at` 排序，逐事件 apply：

- `discussion_started` → Status="starting", StartedAt=event.CreatedAt, Speakers from content
- `round_started` → Status="in_progress", CurrentRound=event.RoundNumber
- `turn_started` → CurrentSpeaker=event.NodeID
- `turn_completed` → CurrentSpeaker=zero (cleared)
- `turn_skipped` → CurrentSpeaker=zero (cleared)
- `round_completed` → CurrentRound unchanged (round done)
- `discussion_paused` → Status="paused"
- `discussion_resumed` → Status="in_progress"
- `discussion_ended` → Status="ended", EndedAt=event.CreatedAt
- `host_changed` → HostNodeID=event.NodeID

---

## §4 v2/v3 协议设计

### 4.1 v2 协议（命令式轮次控制）

使用 `objectNameCommand` 通过 `client.sendPrivateMessage` 发送：

**discussion.start** — Host 通知所有节点讨论开始：
```json
{
    "request_id": "req_<uuid>",
    "service": "discussion",
    "action": "start",
    "params": {
        "chatroom_id": "rc_<id>",
        "host_node_id": "node_abc12345",
        "max_rounds": 5,
        "conversation_kind": "round_robin"
    }
}
```

**discussion.your_turn** — Host 通知节点轮到它发言：
```json
{
    "request_id": "req_<uuid>",
    "service": "discussion",
    "action": "your_turn",
    "params": {
        "chatroom_id": "rc_<id>",
        "round": 1,
        "speaking_order": 2,
        "topic": "讨论主题",
        "context": "前序发言摘要..."
    }
}
```

**discussion.end** — Host 通知所有节点讨论结束：
```json
{
    "request_id": "req_<uuid>",
    "service": "discussion",
    "action": "end",
    "params": {
        "chatroom_id": "rc_<id>",
        "reason": "completed"
    }
}
```

### 4.2 v2 响应（command_result）

节点完成发言后通过 `objectNameCommand` 返回 command_result：

```json
{
    "request_id": "req_<uuid>",
    "msg_type": "command_result",
    "payload": {
        "ok": true,
        "turn_id": "turn_<uuid>",
        "content": "这是我的发言内容..."
    }
}
```

### 4.3 v3 协议（流式响应）

使用 `objectNameStream` (RC:StreamMsg)：

**stream_start**：
```json
{
    "type": "stream_start",
    "request_id": "req_<uuid>",
    "turn_id": "turn_<uuid>",
    "node_id": "node_abc12345"
}
```

**stream_chunk**：
```json
{
    "type": "stream_chunk",
    "turn_id": "turn_<uuid>",
    "chunk_index": 0,
    "content": "这是部分内容..."
}
```

**stream_end**：
```json
{
    "type": "stream_end",
    "turn_id": "turn_<uuid>",
    "content_length": 1024
}
```

**stream_error**：
```json
{
    "type": "stream_error",
    "turn_id": "turn_<uuid>",
    "error": "模型调用失败"
}
```

---

## §5 Service 层设计

### 5.1 DiscussionService

```go
type DiscussionService struct {
    queries   *db.Queries
    client    *rongcloudAPIClient
    registry  *DiscussionRegistry
    logger    *slog.Logger
}
```

**方法**：

- `StartDiscussion(ctx, chatroomID pgtype.UUID) (DiscussionState, error)`
  - 获取 chatroom + members（按 speaking_order 排序）
  - 为每个 member 查询 rongcloud_node 获取 rongcloud_user_id
  - 创建 `discussion_started` 事件
  - 向所有节点发送 `discussion.start` 命令
  - 启动 coordinator goroutine
  - 返回初始 DiscussionState

- `StopDiscussion(ctx, chatroomID pgtype.UUID) (DiscussionState, error)`
  - 创建 `discussion_ended` 事件
  - 从 registry 移除 coordinator
  - 向所有节点发送 `discussion.end` 命令
  - 返回最终 DiscussionState

- `PauseDiscussion(ctx, chatroomID pgtype.UUID) (DiscussionState, error)`
  - 创建 `discussion_paused` 事件
  - coordinator 暂停轮次循环
  - 返回当前 DiscussionState

- `ResumeDiscussion(ctx, chatroomID pgtype.UUID) (DiscussionState, error)`
  - 创建 `discussion_resumed` 事件
  - coordinator 恢复轮次循环
  - 返回当前 DiscussionState

- `GetDiscussionStatus(ctx, chatroomID pgtype.UUID) (DiscussionState, error)`
  - 从 registry 获取内存状态，或回放事件重建

- `ListEvents(ctx, chatroomID pgtype.UUID) ([]DiscussionEvent, error)`
  - 查询事件日志

### 5.2 DiscussionCoordinator

```go
type DiscussionCoordinator struct {
    chatroomID    pgtype.UUID
    workspaceID   pgtype.UUID
    state         DiscussionState
    client        *rongcloudAPIClient
    eventStore    *DiscussionEventStore
    responseCh    chan NodeResponse
    streamAssembler *StreamAssembler
    logger        *slog.Logger
    mu            sync.Mutex
    paused        bool
    done          chan struct{}
}
```

**Run 方法**（主循环，在 goroutine 中运行）：

```
for round = 1 to max_rounds:
    emit round_started
    for each speaker in speakers (by speaking_order):
        emit turn_started
        send your_turn command to speaker's rongcloud_user_id
        wait for response on responseCh (with timeout, e.g. 120s)
        if response received:
            if stream: assemble stream → turn_completed
            if text: record as turn_completed
        else (timeout):
            emit turn_skipped
            if speaker was host: emit host_changed, elect next
        emit turn_completed
    emit round_completed
emit discussion_ended
remove from registry
```

**NodeResponse**（从 node_message_handler 发送）：

```go
type NodeResponse struct {
    NodeID      string
    MsgType     string // "command_result", "stream", "text"
    RequestID   string
    TurnID      string
    Content     string
    StreamType  string // "start", "chunk", "end", "error"
    ChunkIndex  int
    Error       string
}
```

### 5.3 DiscussionRegistry

```go
type DiscussionRegistry struct {
    coordinators sync.Map // chatroomID string -> *DiscussionCoordinator
}

func (r *DiscussionRegistry) Get(chatroomID pgtype.UUID) (*DiscussionCoordinator, bool)
func (r *DiscussionRegistry) Put(chatroomID pgtype.UUID, c *DiscussionCoordinator)
func (r *DiscussionRegistry) Delete(chatroomID pgtype.UUID)
func (r *DiscussionRegistry) SendResponse(chatroomID pgtype.UUID, resp NodeResponse) bool
```

### 5.4 StreamAssembler

```go
type StreamAssembler struct {
    streams sync.Map // turnID string -> *streamBuffer
}

type streamBuffer struct {
    mu       sync.Mutex
    chunks   []string
    started  bool
    complete bool
}

func (a *StreamAssembler) StartStream(turnID, nodeID string)
func (a *StreamAssembler) AppendChunk(turnID string, chunk string) error
func (a *StreamAssembler) CompleteStream(turnID string) (string, error)
func (a *StreamAssembler) AbortStream(turnID string)
```

### 5.5 DiscussionEventStore

```go
type DiscussionEventStore struct {
    queries *db.Queries
    logger  *slog.Logger
}

func (s *DiscussionEventStore) Append(ctx, event DiscussionEvent) (DiscussionEvent, error)
func (s *DiscussionEventStore) ListByChatroom(ctx, chatroomID pgtype.UUID) ([]DiscussionEvent, error)
```

### 5.6 Host 选举

```go
func ElectHost(speakers []SpeakerInfo, failedNodeID pgtype.UUID) (pgtype.UUID, error)
```

- 按 `speaking_order` 排序
- 跳过 failedNodeID
- 返回下一个 speaker 的 NodeID
- 如无可用节点，返回 error

---

## §6 HTTP API 端点

### 6.1 Workspace Admin 端点（RequireWorkspaceRoleFromURL, owner/admin）

| Method | Path | Handler 方法 |
|--------|------|-------------|
| POST | `/rongcloud/chatrooms/{chatroomId}/discussions` | StartRongCloudDiscussion |
| DELETE | `/rongcloud/chatrooms/{chatroomId}/discussions` | StopRongCloudDiscussion |
| PUT | `/rongcloud/chatrooms/{chatroomId}/discussions/pause` | PauseRongCloudDiscussion |
| PUT | `/rongcloud/chatrooms/{chatroomId}/discussions/resume` | ResumeRongCloudDiscussion |

### 6.2 Workspace Member 端点（RequireWorkspaceMemberFromURL）

| Method | Path | Handler 方法 |
|--------|------|-------------|
| GET | `/rongcloud/chatrooms/{chatroomId}/discussions` | GetRongCloudDiscussion |
| GET | `/rongcloud/chatrooms/{chatroomId}/discussions/events` | ListRongCloudDiscussionEvents |

### 6.3 端点请求/响应

**POST /rongcloud/chatrooms/{chatroomId}/discussions**
```json
// Response 201
{
    "chatroomId": "uuid",
    "status": "starting",
    "currentRound": 0,
    "speakers": [{"nodeId": "uuid", "speakingOrder": 1, "roleName": "host"}],
    "hostNodeId": "uuid",
    "startedAt": "2026-09-24T12:00:00Z"
}
```

**DELETE /rongcloud/chatrooms/{chatroomId}/discussions**
```json
// Response 200
{"ok": true, "status": "ended"}
```

**PUT /rongcloud/chatrooms/{chatroomId}/discussions/pause**
```json
// Response 200
{"ok": true, "status": "paused"}
```

**PUT /rongcloud/chatrooms/{chatroomId}/discussions/resume**
```json
// Response 200
{"ok": true, "status": "in_progress"}
```

**GET /rongcloud/chatrooms/{chatroomId}/discussions**
```json
// Response 200
{
    "chatroomId": "uuid",
    "status": "in_progress",
    "currentRound": 2,
    "currentSpeaker": "uuid",
    "speakers": [...],
    "hostNodeId": "uuid",
    "startedAt": "2026-09-24T12:00:00Z"
}
```

**GET /rongcloud/chatrooms/{chatroomId}/discussions/events**
```json
// Response 200
[
    {"eventType": "discussion_started", "roundNumber": 0, "nodeId": null, "createdAt": "..."},
    {"eventType": "round_started", "roundNumber": 1, "nodeId": null, "createdAt": "..."},
    {"eventType": "turn_started", "roundNumber": 1, "speakingOrder": 1, "nodeId": "uuid", "createdAt": "..."},
    {"eventType": "turn_completed", "roundNumber": 1, "speakingOrder": 1, "nodeId": "uuid", "content": {"text": "..."}, "createdAt": "..."}
]
```

---

## §7 system_handler 命令路由

Phase 3 在 `handleCommand` 中新增 `case "discussion"` 分发：

| Action | 处理 | 权限 |
|--------|------|------|
| `start` | 触发 StartDiscussion | admin |
| `stop` | 触发 StopDiscussion | admin |
| `status` | 返回当前讨论状态 | member |
| `pause` | 暂停讨论 | admin |
| `resume` | 恢复讨论 | admin |
| `your_turn_result` | 节点报告发言完成 → 馈入 coordinator | node |

---

## §8 node_message_handler 全分发

Phase 3 补全 `handleNodeMessage`，将节点响应分发到对应的 coordinator：

| ObjectName | 处理逻辑 |
|------------|---------|
| `objectNameCommand` | 解析 command_result → 检查 payload.ok → 通过 registry.SendResponse 馈入 coordinator |
| `objectNameStream` | 解析 stream 消息 → 按 type (start/chunk/end/error) 馈入 coordinator |
| `objectNameText` | 纯文本响应 → 作为 turn_completed 内容馈入 coordinator |
| 其他 | 日志记录，忽略 |

**关键**：handler 需要从消息内容中提取 `chatroom_id`（或通过 target_id 映射）来找到对应的 coordinator。

---

## §9 错误处理与测试

### 9.1 错误处理

| 场景 | 处理 |
|------|------|
| 讨论已在进行中 | 409 Conflict |
| 讨论未开始但尝试 stop/pause | 409 Conflict |
| chatroom 不存在 | 404 Not Found |
| chatroom 无成员 | 400 Bad Request |
| coordinator 超时无响应 | 记录 turn_skipped 事件，继续下一个 speaker |
| host 节点无响应 | 记录 host_changed 事件，选举新 host |
| 所有节点无响应 | 记录 error 事件，结束讨论 |
| 流式组装超时 | 记录 turn_skipped 事件，继续 |
| RongCloud API 错误 | 日志记录，继续（best-effort 通知） |

### 9.2 测试策略

- 扩展 `rongcloud_test.go` 覆盖：
  - 状态机事件回放（各种事件序列 → 正确状态）
  - 状态转换校验（非法转换 → error）
  - StreamAssembler（start → chunk × N → end → 完整内容）
  - HostElector（跳过失败节点，选举下一个）
  - DiscussionRegistry（Put/Get/Delete/SendResponse）
  - system_handler discussion 命令路由
  - node_message_handler 全分发（command_result / stream / text）
  - DiscussionService StartDiscussion / StopDiscussion（mock client）
- `make test` 必须全通过

---

## §10 路线图

| Phase | 状态 | 范围 |
|-------|------|------|
| Phase 1 MVP | ✅ 完成 | 消息骨架 + ping RPC |
| Phase 2a Infrastructure | ✅ 完成 | DB + API + 服务骨架 + 6 端点 |
| Phase 2b Business Logic | ✅ 完成 | Chatroom/Device/NodeModelCatalog/Pairing 完整业务逻辑 + 17 端点 + system_handler 命令路由 |
| **Phase 3 Discussion** | **← 当前** | 事件溯源状态机 + v2/v3 协议 + host 选举 + 讨论协调器 + 6 端点 (~3000 行 Go) |
| Phase 4 Bridge + Frontend | 待定 | AI node 注册 + 流式传输 + 前端 |
