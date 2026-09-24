# 融云 Channel 适配器 Phase 2b — 业务逻辑设计

> 基于 Phase 2a 基础设施，补全 Chatroom / Device / NodeModelCatalog / Pairing 的完整业务逻辑，
> 并将所有 spec §4.1 / §4.2 中标记为 ❌ 的 HTTP 端点实现到位。

---

## §1 架构概览

### 1.1 背景

Phase 2a 完成了 DB 迁移、sqlc 查询、RongCloud API 封装、四层服务骨架（InstallService、
NodeService、ChatroomService、PairingService）以及 6 个 HTTP 端点的 Handler + Router 注册。
但存在以下问题：

- **Service 层存在 STUB**：`EnrollDeviceCredential` 不持久化、`OpenConnectionSession` /
  `CloseConnectionSession` 是空操作、`DeleteChatroom` 不同步 RongCloud 销毁、
  `SetMembers` 不同步 RongCloud 加入/退出。
- **Handler 层缺 15 个端点**（spec §4.1 / §4.2 中标记 ❌ 的全部端点）。
- **Router 层缺 ~16 条路由注册**。
- **system_handler 只处理 ping**，无法路由 chatroom / device 命令。
- **ListRongCloudNodes / ListRongCloudDevices 是 STUB**，返回空数组。

### 1.2 新增组件

| 组件 | 文件 | 职责 |
|------|------|------|
| ChatroomService 补全 | `server/internal/integrations/rongcloud/chatroom_service.go` | DeleteChatroom 同步 RongCloud destroy；SetMembers 同步 join/quit |
| NodeService 补全 | `server/internal/integrations/rongcloud/node_service.go` | EnrollDeviceCredential 持久化；OpenConnectionSession / CloseConnectionSession 真实跟踪 |
| PairingService 补全 | `server/internal/integrations/rongcloud/pairing_service.go` | ClaimSession 时绑定 device |
| InstallService 补全 | `server/internal/integrations/rongcloud/install_service.go` | system_config CRUD（UpsertSystemConfig / GetSystemConfig / ListSystemConfigs / DeleteSystemConfig） |
| system_handler 补全 | `server/internal/integrations/rongcloud/system_handler.go` | 路由 chatroom / device 命令 |
| Handler 端点补全 | `server/internal/handler/rongcloud_handler.go` | 15 个新 Handler 方法 + 2 个 stub 完成 |
| Router 路由补全 | `server/cmd/server/router.go` | ~16 条新路由注册 |

### 1.3 设计原则

1. **不新增 DB 表** — 所有表已在 Phase 2a 迁移 538/539 中创建。
2. **不新增 sqlc 查询** — 所有查询已在 `rongcloud.sql` 中定义。
3. **复用已有 RongCloud API 方法** — `destroyChatroom`、`joinChatroom`、`quitChatroom` 已在 `chatroom_api.go` 中实现。
4. **非 Discussion 命令路由** — system_handler 新增 chatroom / device 命令分发；Discussion 命令留到 Phase 3。
5. **node_message_handler 保持 STUB** — Phase 4 bridge 负责 AI CLI 完整路由（spec §5.5）。

---

## §2 数据库依赖

Phase 2b 不新增迁移。使用 Phase 2a 已创建的 8 张表：

| 表 | Phase 2b 使用方式 |
|----|-------------------|
| rongcloud_chatroom | Update / Delete 操作（已有查询） |
| rongcloud_chatroom_member | SetMembers — 删除+重建（已有查询） |
| rongcloud_device | EnrollDeviceCredential 持久化；ListByWorkspace / ListByOwner（已有查询） |
| rongcloud_node | ListNodeModels / DeleteNode（已有查询） |
| rongcloud_node_model_catalog | AddNodeModel / RemoveNodeModel（已有查询） |
| rongcloud_pairing_session | Create / GetByTicket / UpdateStatus（已有查询） |
| rongcloud_system_config | Upsert / Get / List / Delete（已有查询，InstallService 封装） |

---

## §3 Service 层补全设计

### 3.1 ChatroomService

**DeleteChatroom(ctx, id pgtype.UUID) error**
- 调用 `GetRongCloudChatroomByID` 获取 chatroom 记录（含 `rongcloud_chatroom_id`）
- 调用 `client.destroyChatroom(ctx, rongcloudChatroomID)` — 忽略 "not found" 错误
- 调用 `DeleteRongCloudChatroom` 执行 DB 软删除

**SetMembers(ctx, chatroomID pgtype.UUID, members []ChatroomMemberConfig) error**
- 获取旧成员列表 `ListRongCloudChatroomMembers`
- DB 替换：`DeleteByChatroom` + 循环 `Create`
- 对新增成员调用 `client.joinChatroom(ctx, rongcloudChatroomID, newUserIDs)` — 需获取 chatroom 的 rongcloud_id
- 对移除成员调用 `client.quitChatroom(ctx, rongcloudChatroomID, removedUserIDs)`
- 忽略 RongCloud API 的 "already member" / "not member" 错误

### 3.2 NodeService

**EnrollDeviceCredential(ctx, nodeID string) (DeviceCredential, error)**
- 获取 node 记录 → 获取关联的 rongcloud_user 的 user_id
- 调用 `generateDeviceCredential()` 生成 `dc_<32hex>` ID + secret
- 调用 `box.Seal(secret)` 加密 secret
- 调用 `CreateRongCloudDevice` 持久化到 DB（workspace_id / owner_user_id / node_id / credential_id / credential_secret_encrypted / status="active"）
- 返回 DeviceCredential{CredentialID, CredentialSecret}

**OpenConnectionSession(ctx, nodeID string) (string, error)**
- 生成 session ID：`sess_<32hex>`
- 将 session 存入 `rongcloud_system_config`（config_key="connection_session:{sessionID}", config={"node_id": nodeID, "opened_at": timestamp}）
- 返回 sessionID

**CloseConnectionSession(ctx, sessionID string) error**
- 从 `rongcloud_system_config` 查询 key="connection_session:{sessionID}"
- 调用 `DeleteRongCloudSystemConfig` 清除记录
- 如不存在，返回 nil（幂等）

### 3.3 PairingService

**ClaimSession 补全**：
- 在 `UpdateStatus` 到 "claimed" 后，如有 `deviceCredentialTicket` 参数，绑定到 session 记录
- 更新 session 的 `metadata` JSONB（如字段存在）或写入 `config` 中追加 device 信息
- 当前 DB schema 中 `rongcloud_pairing_session` 无 device 字段，使用 `metadata` JSONB 存储

### 3.4 InstallService — SystemConfig CRUD

新增方法：
- `UpsertSystemConfig(ctx, workspaceID pgtype.UUID, configKey string, nodeID pgtype.UUID, config json.RawMessage) error`
- `GetSystemConfig(ctx, workspaceID pgtype.UUID, configKey string) (config json.RawMessage, err error)`
- `ListSystemConfigs(ctx, workspaceID pgtype.UUID) ([]SystemConfigEntry, error)`
- `DeleteSystemConfig(ctx, workspaceID pgtype.UUID, configKey string) error`

SystemHost 端点使用 configKey="system_host" 存储 host node_id。

---

## §4 HTTP API 端点

### 4.1 Public 端点（无需 workspace 认证）

| Method | Path | Status | Handler 方法 |
|--------|------|--------|-------------|
| POST | `/api/claw/device-credentials/enroll` | ❌→✅ | EnrollDeviceCredential |
| POST | `/api/claw/connection-sessions` | ❌→✅ | CreateConnectionSession |
| POST | `/api/claw/connection-sessions/{sessionId}/close` | ❌→✅ | CloseConnectionSession |

**请求/响应设计**：

**POST /api/claw/device-credentials/enroll**
```json
// Request
{"nodeId": "node_abc12345", "enrollmentToken": "hex..."}
// Response 201
{"credentialId": "dc_...", "credentialSecret": "base64url..."}
```

**POST /api/claw/connection-sessions**
```json
// Request
{"nodeId": "node_abc12345"}
// Response 201
{"sessionId": "sess_..."}
```

**POST /api/claw/connection-sessions/{sessionId}/close**
```json
// Response 200
{"ok": true}
```

### 4.2 Workspace 端点（需 workspace 认证）

**Member GET（RequireWorkspaceMemberFromURL）**：

| Method | Path | Status | Handler 方法 |
|--------|------|--------|-------------|
| GET | `/rongcloud/chatrooms/{chatroomId}` | ❌→✅ | GetRongCloudChatroom |
| GET | `/rongcloud/nodes` | stub→✅ | ListRongCloudNodes |
| GET | `/rongcloud/nodes/{nodeId}/models` | ❌→✅ | ListRongCloudNodeModels |
| GET | `/rongcloud/devices` | stub→✅ | ListRongCloudDevices |
| GET | `/rongcloud/system-host` | ❌→✅ | GetRongCloudSystemHost |
| GET | `/rongcloud/pairing/{ticket}` | ❌→✅ | GetRongCloudPairing |

**Admin POST/PUT/DELETE（RequireWorkspaceRoleFromURL, owner/admin）**：

| Method | Path | Status | Handler 方法 |
|--------|------|--------|-------------|
| PUT | `/rongcloud/chatrooms/{chatroomId}` | ❌→✅ | UpdateRongCloudChatroom |
| POST | `/rongcloud/chatrooms/{chatroomId}/members` | ❌→✅ | SetRongCloudChatroomMembers |
| POST | `/rongcloud/nodes/{nodeId}/models` | ❌→✅ | AddRongCloudNodeModel |
| DELETE | `/rongcloud/nodes/{nodeId}` | ❌→✅ | DeleteRongCloudNode |
| POST | `/rongcloud/devices` | ❌→✅ | CreateRongCloudDevice |
| DELETE | `/rongcloud/devices/{deviceId}` | ❌→✅ | DeleteRongCloudDevice |
| PUT | `/rongcloud/system-host` | ❌→✅ | UpdateRongCloudSystemHost |
| POST | `/rongcloud/pairing` | ❌→✅ | CreateRongCloudPairing |

### 4.3 端点请求/响应

**GET /rongcloud/chatrooms/{chatroomId}** → 200 chatroom JSON

**PUT /rongcloud/chatrooms/{chatroomId}**
```json
{"maxRounds": 10, "conversationKind": "round_robin", "config": {}}
```

**POST /rongcloud/chatrooms/{chatroomId}/members**
```json
{"members": [{"nodeId": "uuid", "memberType": "ai", "roleName": "host", "model": "doubao-pro"}]}
```

**GET /rongcloud/nodes** → 200 `[{nodeId, name, type, status, capabilities}]`

**GET /rongcloud/nodes/{nodeId}/models** → 200 `[{modelId, provider, modelName}]`

**POST /rongcloud/nodes/{nodeId}/models**
```json
{"modelId": "doubao-pro-32k", "provider": "volcengine", "modelName": "Doubao Pro 32k", "config": {}}
```

**DELETE /rongcloud/nodes/{nodeId}** → 200 `{ok: true}`

**GET /rongcloud/devices** → 200 `[{deviceId, nodeName, status}]`

**POST /rongcloud/devices**
```json
{"nodeId": "node_abc12345"}
```

**DELETE /rongcloud/devices/{deviceId}** → 200 `{ok: true}`

**GET /rongcloud/system-host** → 200 `{nodeId: "uuid", config: {}}`

**PUT /rongcloud/system-host**
```json
{"nodeId": "uuid", "config": {}}
```

**POST /rongcloud/pairing**
```json
{"candidateNodeIds": ["uuid1", "uuid2"], "clientClaimKey": "base64url...", "expiresIn": 300}
```

**GET /rongcloud/pairing/{ticket}** → 200 `{ticket, status, expiresAt}`

---

## §5 system_handler 命令路由

Phase 2b 在 `handleCommand` 中新增非 Discussion 命令分发：

| Service 值 | Action 值 | 处理 |
|------------|-----------|------|
| `chatroom` | `list` | 返回 workspace chatroom 列表 |
| `chatroom` | `create` | 创建 chatroom |
| `chatroom` | `delete` | 删除 chatroom |
| `device` | `enroll` | 注册设备凭据 |
| `device` | `list` | 列出设备 |
| `ping` | * | 已有实现 |

Discussion 命令（`discussion` service）留到 Phase 3。

---

## §6 错误处理与测试

### 6.1 错误处理

| 场景 | HTTP 状态 | 错误码 |
|------|-----------|--------|
| RongCloud 未配置 | 503 | rongcloud_not_configured |
| chatroom 不存在 | 404 | chatroom_not_found |
| node 不存在 | 404 | node_not_found |
| device 不存在 | 404 | device_not_found |
| pairing session 过期 | 410 | pairing_expired |
| pairing session 已被认领 | 409 | pairing_already_claimed |
| enrollment token 无效 | 401 | invalid_enrollment_token |
| RongCloud API 错误 | 502 | rongcloud_api_error |

### 6.2 测试策略

- 扩展 `rongcloud_test.go` 覆盖所有新增 Service 方法和 Handler 端点
- 使用 mock client（已有 `mockRongCloudClient` 模式）
- 测试 ChatroomService.DeleteChatroom 的 RongCloud destroy 调用
- 测试 SetMembers 的 join/quit 同步
- 测试 EnrollDeviceCredential 的 DB 持久化
- 测试 system_handler 的 chatroom/device 命令路由
- `make test` 必须全通过

---

## §7 路线图

| Phase | 状态 | 范围 |
|-------|------|------|
| Phase 1 MVP | ✅ 完成 | 消息骨架 + ping RPC |
| Phase 2a Infrastructure | ✅ 完成 | DB + API + 服务骨架 + 6 端点 |
| **Phase 2b Business Logic** | **← 当前** | Chatroom/Device/NodeModelCatalog/Pairing 完整业务逻辑 + 15 端点 + system_handler 命令路由 |
| Phase 3 Discussion | 待定 | 事件溯源状态机 + v2/v3 协议 + host 选举 (~3000 行 Go) |
| Phase 4 Bridge + Frontend | 待定 | AI node 注册 + 流式传输 + 前端 |
