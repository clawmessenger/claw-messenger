# 融云 Channel 适配器 Phase 2a — 基础设施设计

> 基于 Phase 1 MVP（commit b1e3d7383），扩展融云 Channel 适配器的基础设施层：数据库表、融云 Server API 扩展、HTTP API 端点、服务层。为 Phase 2b 业务逻辑（Chatroom/Device/NodeModelCatalog/Pairing）提供数据持久化和 API 基础。

## 1. 架构概览

### 背景

Phase 1 完成了 RongCloud Channel 适配器 MVP 骨架：消息收发 + command→command_result RPC 回环（ping）。Phase 2a 在此基础上建设基础设施：

- **数据库层**：8 张新表存储融云用户、AI 节点、聊天室、设备、配对会话、模型目录、system 配置
- **融云 API 扩展**：复用 Phase 1 的 `rongcloudAPIClient`，新增 User/Chatroom/Group REST API 方法
- **HTTP API 端点**：公开端点（AI 节点注册/认证）+ workspace 端点（业务 CRUD）
- **服务层**：5 个 service 封装 DB + 融云 API + secretbox，为 handler 提供业务接口

### 新增组件

| 组件 | 文件 | 职责 |
|------|------|------|
| DB 迁移 | `migrations/538_rongcloud_tables.up/down.sql` + `539_rongcloud_indexes.up/down.sql` | 8 张表 + CONCURRENTLY 索引 |
| sqlc 查询 | `pkg/db/queries/rongcloud.sql` | 全表 CRUD 查询 |
| User API | `integrations/rongcloud/user_api.go` | 融云用户管理（getToken/refresh/checkOnline/expireToken/getUserInfo） |
| Chatroom API | `integrations/rongcloud/chatroom_api.go` | 融云聊天室管理（create/destroy/join/quit/get/getMembers/ensure） |
| Group API | `integrations/rongcloud/group_api.go` | 融云群组管理（create/dismiss/join/quit/refresh） |
| InstallService | `integrations/rongcloud/install_service.go` | installation CRUD + appKey 暴露 + system 节点用户 |
| NodeService | `integrations/rongcloud/node_service.go` | AI 节点注册 + token 刷新 + 设备凭证 + 连接会话 |
| ChatroomService | `integrations/rongcloud/chatroom_service.go` | 聊天室业务 CRUD + 成员管理 |
| PairingService | `integrations/rongcloud/pairing_service.go` | 设备配对会话 + claim |
| HTTP Handler | `handler/rongcloud_handler.go` | 公开 + workspace 端点 |
| Router 接线 | `cmd/server/router.go`（修改） | 路由注册 + service 注入 |

### 设计原则

- **纯增量扩展**：不修改 Phase 1 已有文件（types.go/config.go/client.go/webhook.go/inbound.go/system_handler.go/channel.go/outbound.go/registration.go）
- **复用 Phase 1 基础**：rongcloudAPIClient struct（同包扩展）、postForm 方法、签名算法
- **遵循上游模式**：channel_installation 表模式、handler 模式、secretbox 加密约定
- **无外键**（MUL-3515 §4）、CONCURRENTLY 索引、JSONB config、UUID PK

## 2. 数据库 Schema

### 2.1 表设计

8 张新表，遵循 channel_* 模式（JSONB config、无外键、UUID PK）：

#### rongcloud_user — 融云用户映射

| 字段 | 类型 | 说明 |
|------|------|------|
| id | UUID PK | 内部 ID |
| workspace_id | UUID NOT NULL | 工作空间 |
| rongcloud_user_id | TEXT NOT NULL UNIQUE | 融云用户 ID |
| name | TEXT | 显示名 |
| portrait_uri | TEXT | 头像 |
| token_encrypted | TEXT | 融云 token（secretbox 加密后 base64） |
| is_system_reserved | BOOLEAN DEFAULT FALSE | 是否 system 节点 |
| is_ai_node | BOOLEAN DEFAULT FALSE | 是否 AI 节点 |
| node_type | TEXT DEFAULT 'human' CHECK IN ('system','ai','human') | 节点类型 |
| created_at | TIMESTAMPTZ DEFAULT now() | |
| updated_at | TIMESTAMPTZ DEFAULT now() | |

#### rongcloud_node — AI 节点

| 字段 | 类型 | 说明 |
|------|------|------|
| id | UUID PK | 内部 ID |
| workspace_id | UUID NOT NULL | 工作空间 |
| owner_user_id | UUID NOT NULL | 拥有者 |
| rongcloud_user_id | TEXT NOT NULL | 关联融云用户 |
| node_id | TEXT NOT NULL | 节点 ID（注册时生成） |
| ai_type | TEXT | AI 类型 |
| capabilities | JSONB DEFAULT '{}' | 能力声明 |
| deploy_status | TEXT DEFAULT 'offline' CHECK IN ('online','offline','error') | 部署状态 |
| binding_version | INT DEFAULT 1 | 绑定版本 |
| created_at | TIMESTAMPTZ DEFAULT now() | |
| updated_at | TIMESTAMPTZ DEFAULT now() | |

UNIQUE(rongcloud_user_id)

#### rongcloud_chatroom — 聊天室

| 字段 | 类型 | 说明 |
|------|------|------|
| id | UUID PK | 内部 ID |
| workspace_id | UUID NOT NULL | |
| rongcloud_chatroom_id | TEXT NOT NULL UNIQUE | 融云聊天室 ID |
| owner_user_id | UUID NOT NULL | |
| host_node_id | UUID | 主持节点（关联 rongcloud_node.id） |
| max_rounds | INT DEFAULT 0 | 最大轮次 |
| conversation_kind | TEXT | 聊天室类型 |
| config | JSONB DEFAULT '{}' | 扩展配置 |
| status | TEXT DEFAULT 'active' CHECK IN ('active','deleted') | |
| created_at | TIMESTAMPTZ DEFAULT now() | |
| updated_at | TIMESTAMPTZ DEFAULT now() | |

#### rongcloud_chatroom_member — 聊天室 AI 成员

| 字段 | 类型 | 说明 |
|------|------|------|
| id | UUID PK | |
| chatroom_id | UUID NOT NULL | 关联 rongcloud_chatroom.id |
| node_id | UUID | 关联 rongcloud_node.id |
| member_type | TEXT NOT NULL CHECK IN ('user','ai') | 成员类型 |
| role_name | TEXT | 角色名 |
| role_instructions | TEXT | 角色指令 |
| capabilities | JSONB DEFAULT '{}' | |
| model | TEXT | AI 模型 |
| speaking_order | INT | 发言顺序 |
| enabled | BOOLEAN DEFAULT TRUE | |
| discussion_model | TEXT | 讨论模式 |
| created_at | TIMESTAMPTZ DEFAULT now() | |
| updated_at | TIMESTAMPTZ DEFAULT now() | |

UNIQUE(chatroom_id, node_id)

#### rongcloud_device — IoT 设备

| 字段 | 类型 | 说明 |
|------|------|------|
| id | UUID PK | |
| workspace_id | UUID NOT NULL | |
| owner_user_id | UUID NOT NULL | |
| node_id | UUID NOT NULL | 关联 rongcloud_node.id |
| device_name | TEXT NOT NULL | 设备名 |
| device_type | TEXT | 设备类型 |
| credential_id | TEXT | 凭证 ID |
| credential_secret_encrypted | TEXT | 凭证密钥（secretbox 加密） |
| status | TEXT DEFAULT 'active' CHECK IN ('active','disabled','deleted') | |
| created_at | TIMESTAMPTZ DEFAULT now() | |
| updated_at | TIMESTAMPTZ DEFAULT now() | |

#### rongcloud_pairing_session — 设备配对会话

| 字段 | 类型 | 说明 |
|------|------|------|
| id | UUID PK | |
| workspace_id | UUID NOT NULL | |
| ticket | TEXT NOT NULL UNIQUE | 配对票据 |
| status | TEXT DEFAULT 'pending' CHECK IN ('pending','claimed','expired','cancelled') | |
| client_claim_key | TEXT | 客户端认领密钥 |
| idempotency_key | TEXT | 幂等键 |
| candidate_node_ids | JSONB DEFAULT '[]' | 候选节点 |
| expires_at | TIMESTAMPTZ NOT NULL | 过期时间 |
| created_at | TIMESTAMPTZ DEFAULT now() | |
| updated_at | TIMESTAMPTZ DEFAULT now() | |

#### rongcloud_node_model_catalog — 节点模型目录

| 字段 | 类型 | 说明 |
|------|------|------|
| id | UUID PK | |
| workspace_id | UUID NOT NULL | |
| node_id | UUID NOT NULL | 关联 rongcloud_node.id |
| model_id | TEXT NOT NULL | 模型 ID |
| provider | TEXT | 提供者 |
| model_name | TEXT | 模型名 |
| config | JSONB DEFAULT '{}' | |
| created_at | TIMESTAMPTZ DEFAULT now() | |
| updated_at | TIMESTAMPTZ DEFAULT now() | |

UNIQUE(node_id, model_id)

#### rongcloud_system_config — system 节点配置

| 字段 | 类型 | 说明 |
|------|------|------|
| id | UUID PK | |
| workspace_id | UUID NOT NULL | |
| config_key | TEXT NOT NULL | 配置键 |
| node_id | UUID | 关联 rongcloud_node.id |
| config | JSONB DEFAULT '{}' | |
| config_version | INT DEFAULT 1 | CAS 版本号 |
| created_at | TIMESTAMPTZ DEFAULT now() | |
| updated_at | TIMESTAMPTZ DEFAULT now() | |

UNIQUE(workspace_id, config_key)

### 2.2 迁移文件

- `538_rongcloud_tables.up.sql` / `.down.sql` — 全表 CREATE/DROP
- `539_rongcloud_indexes.up.sql` / `.down.sql` — CONCURRENTLY 索引（单独文件，非事务）
  - idx_rongcloud_user_workspace ON (workspace_id)
  - idx_rongcloud_user_rongcloud_id ON (rongcloud_user_id)
  - idx_rongcloud_node_workspace ON (workspace_id)
  - idx_rongcloud_node_owner ON (owner_user_id)
  - idx_rongcloud_node_rongcloud_user ON (rongcloud_user_id)
  - idx_rongcloud_chatroom_workspace ON (workspace_id)
  - idx_rongcloud_chatroom_owner ON (owner_user_id)
  - idx_rongcloud_chatroom_member_chatroom ON (chatroom_id)
  - idx_rongcloud_chatroom_member_node ON (node_id)
  - idx_rongcloud_device_workspace ON (workspace_id)
  - idx_rongcloud_device_owner ON (owner_user_id)
  - idx_rongcloud_device_node ON (node_id)
  - idx_rongcloud_pairing_session_ticket ON (ticket)
  - idx_rongcloud_pairing_session_status ON (status)
  - idx_rongcloud_node_model_catalog_node ON (node_id)
  - idx_rongcloud_system_config_workspace_key ON (workspace_id, config_key)

### 2.3 sqlc 查询文件

`server/pkg/db/queries/rongcloud.sql` — 全表 CRUD，遵循 `-- name: QueryName :one|:many|:exec|:execrows` 模式，使用 `sqlc.arg('name')` 命名参数，`RETURNING *` 用于 `:one`。

## 3. 融云 API 扩展

扩展 `rongcloudAPIClient`（Phase 1 已有 postForm/signRequest/sendPrivateMessage/sendCommandResult），新增方法在新文件中，复用同一 struct（同包）。

### 3.1 user_api.go — 融云用户 API

| 方法 | 融云端点 | 关键字段 |
|------|----------|----------|
| getUserToken(userId, name, portraitUri) → token | POST /user/getToken.json | userId, name, portraitUri |
| refreshUser(userId, name, portraitUri) → token | POST /user/refresh.json | userId, name, portraitUri |
| checkOnline(userId) → bool | POST /user/checkOnline.json | userId |
| expireToken(userId, time) | POST /user/token/expire.json | userId, time(ms) |
| getUserInfo(userId) → userInfoResult | POST /user/info.json | userId |

result structs: `userInfoResult{UserID, Name, PortraitUri}`

### 3.2 chatroom_api.go — 融云聊天室 API

| 方法 | 融云端点 | 关键字段 |
|------|----------|----------|
| createChatroom(chatroomId) | POST /chatroom/create_new.json | chatroomId, destroyType=0, destroyTime=10080 |
| destroyChatroom(chatroomId) | POST /chatroom/destroy.json | chatroomId |
| joinChatroom(chatroomId, userIds) | POST /chatroom/join.json | chatroomId, userIds(comma-joined) |
| quitChatroom(chatroomId, userIds) | POST /chatroom/quit.json | chatroomId, userIds(comma-joined) |
| getChatroomInfo(chatroomId) → chatroomInfoResult | POST /chatroom/get.json | chatroomId |
| getChatroomMembers(chatroomId, count) → chatroomMembersResult | POST /chatroom/user/query.json | chatroomId, count, nextId |
| ensureChatroom(chatroomId, name) → chatroomInfoResult | 组合：get→create→get | name 可选 |

result structs: `chatroomInfoResult{ChatroomID, Name}`, `chatroomMembersResult{Users []chatroomMember{UserID, ...}}`

### 3.3 group_api.go — 融云群组 API

| 方法 | 融云端点 | 关键字段 |
|------|----------|----------|
| createGroup(groupId, groupName, userIds) | POST /group/create.json | groupId, groupName, userId(repeated param) |
| dismissGroup(groupId, memberId) | POST /group/dismiss.json | groupId, memberId |
| joinGroup(groupId, userIds) | POST /group/join.json | groupId, userId(repeated) |
| quitGroup(groupId, userIds) | POST /group/quit.json | groupId, userId(repeated) |
| refreshGroupInfo(groupId, groupName, portraitUri) | POST /group/refresh.json | groupId, groupName, portraitUri |

注意：Group API 的 userId 参数是 repeated form param（`userId=xxx&userId=yyy`），不是 comma-joined。

## 4. HTTP API 端点

### 4.1 公开端点（非 workspace scoped）

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| GET | /api/config/rongcloud | 无 | 返回 appKey |
| POST | /api/ai/register | X-Node-Enrollment-Token (HMAC-SHA256) 或 Bearer existingNodeToken 或 Pairing authorization | AI 节点注册 |
| POST | /api/claw/refresh-token/{nodeId} | Bearer node token | 刷新融云 token |
| POST | /api/claw/device-credentials/enroll | Bearer node token | 设备凭证注册 |
| POST | /api/claw/connection-sessions | Bearer node token | 创建连接会话 |
| POST | /api/claw/connection-sessions/{sessionId}/close | Bearer node token | 关闭连接会话 |

**AI 节点注册请求/响应**：

请求体：
```json
{
  "name": "node-name",
  "mac_address": "stable-mac",
  "node_type": "ai",
  "ai_type": "opencode",
  "capabilities": ["discussion_host", "discussion_participant"]
}
```

响应体：
```json
{
  "node_id": "node-uuid",
  "token": "rongcloud-token",
  "capabilities": ["discussion_host", ...],
  "device_credential_ticket": "dc-ticket",
  "binding_version": 1
}
```

**enrollment token HMAC-SHA256**：`HMAC-SHA256(bridgeSecret, "quukk/server-enrollment/v1\0{serverUrl}\0{runtimeId}")`

### 4.2 Workspace 端点（/api/workspaces/{id}/rongcloud/）

**Member group（查询）**:
- GET /rongcloud/chatrooms — 列出聊天室
- GET /rongcloud/chatrooms/{chatroomId} — 聊天室详情
- GET /rongcloud/nodes — 列出 AI 节点
- GET /rongcloud/nodes/{nodeId}/models — 节点模型目录
- GET /rongcloud/devices — 列出设备
- GET /rongcloud/system-host — system 主持人配置

**Admin group（修改）**:
- POST /rongcloud/chatrooms — 创建聊天室
- PUT /rongcloud/chatrooms/{chatroomId} — 更新聊天室
- DELETE /rongcloud/chatrooms/{chatroomId} — 删除聊天室
- POST /rongcloud/chatrooms/{chatroomId}/members — 设置 AI 成员
- POST /rongcloud/nodes/{nodeId}/models — 添加模型
- DELETE /rongcloud/nodes/{nodeId} — 删除节点
- POST /rongcloud/devices — 添加设备
- DELETE /rongcloud/devices/{deviceId} — 删除设备
- PUT /rongcloud/system-host — 更新 system 主持人
- POST /rongcloud/pairing — 创建配对会话
- GET /rongcloud/pairing/{ticket} — 查询配对状态

### 4.3 Handler 扩展

`Handler` struct 新增字段：
```go
RongCloudInstall    *rongcloud.InstallService
RongCloudNode       *rongcloud.NodeService
RongCloudChatroom   *rongcloud.ChatroomService
RongCloudPairing    *rongcloud.PairingService
RongCloudUser       *rongcloud.UserService
```

新方法放 `handler/rongcloud_handler.go`，遵循现有 handler 模式：nil-check → requireUserID → parseUUID → authz → delegate → translate errors → writeJSON。

Router 在 env-gated 块内构造 service 并注入 Handler，新增路由注册块。

## 5. 服务层设计

### 5.1 InstallService

```go
type InstallService struct {
    queries *db.Queries
    box     *secretbox.Box
    logger  *slog.Logger
}
```

职责：installation CRUD（复用 channel_installation 表）、appKey 暴露、system 节点用户管理。

### 5.2 NodeService

```go
type NodeService struct {
    queries *db.Queries
    client  *rongcloudAPIClient
    box     *secretbox.Box
    logger  *slog.Logger
}
```

核心注册流程：
1. 调融云 getToken 创建 IM 用户 → 存 rongcloud_user（token secretbox 加密）
2. 存 rongcloud_node（owner_user_id, ai_type, capabilities, deploy_status="offline"）
3. 返回 node_id + token + capabilities + binding_version

其他：RefreshToken、EnrollDeviceCredential（生成 dc_<32hex> + 32 字节 base64url secret）、OpenConnectionSession/CloseConnectionSession（stub）、ListNodeModels/AddNodeModel/RemoveNode、enrollmentToken HMAC 验证。

### 5.3 ChatroomService

```go
type ChatroomService struct {
    queries *db.Queries
    client  *rongcloudAPIClient
    logger  *slog.Logger
}
```

职责：聊天室 CRUD（融云 API + DB 同步）、成员管理（replace 模式：删旧+插新）。

### 5.4 PairingService

```go
type PairingService struct {
    queries *db.Queries
    logger  *slog.Logger
}
```

职责：配对会话创建（生成 ticket + candidate_node_ids + 过期时间）、查询、claim（idempotency_key 去重 + client_claim_key 验证 + 设备绑定）。

### 5.5 AI 节点消息路由 stub

system_handler.go 扩展（新增方法，不修改已有 handleCommand）：
- `handleNodeMessage(ctx, msg)` — stub：收到 AI 节点 command 消息时记录日志并返回。Phase 4 bridge 包补全实际路由到 AI CLI。

### 5.6 服务注入

router.go env-gated 块内构造所有 service 并注入 Handler。env 未设置时指针为 nil，handler 做 nil-check 返回 feature disabled。

## 6. 错误处理与测试策略

### 错误处理

| 错误类型 | 处理方式 |
|----------|----------|
| 融云 API 超时/5xx | 返回 503，客户端重试 |
| 融云 API 4xx | 返回 400 + 错误消息 |
| DB 操作失败 | 返回 500，记录日志 |
| secretbox 解密失败 | 返回 500（配置错误，不可恢复） |
| enrollment token 验证失败 | 返回 401 |
| workspace 权限不足 | 返回 403 |
| 节点注册重复 | 返回 409 Conflict |
| 配对 ticket 过期/已用 | 返回 410 Gone |

### 测试策略

- **单元测试**：每个 service 方法用 httptest.NewServer mock 融云 API（同 Phase 1 模式），testLogger() + identity decrypter
- **DB 测试**：参照 telegram main_test.go 模式，TestMain 连 pgxpool，DB 不可用时 skip
- **Handler 测试**：mock service 指针，验证 HTTP 状态码和 JSON 响应体
- **验证命令**：`make sqlc` → `go build ./cmd/server/` → `go test ./internal/integrations/rongcloud/` → `make test`

## 7. 路线图

| 阶段 | 状态 | 说明 |
|------|------|------|
| Phase 1 MVP 骨架 | ✅ 完成 | 消息收发 + ping 回环 |
| **Phase 2a 基础设施** | **← 当前** | DB + 融云 API 扩展 + HTTP 端点 + 服务层 |
| Phase 2b 业务逻辑 | 待定 | Chatroom/Device/NodeModelCatalog/Pairing 完整业务逻辑 |
| Phase 3 Discussion 全量 | 待定 | 事件溯源状态机 + v2/v3 协议 + host 选举（~3000 行 Go 重写） |
| Phase 4 Bridge + 前端 | 待定 | AI 节点注册 + 流式回复 + 前端对接 |
