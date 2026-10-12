# 圆桌（Discussion v3）功能 Go 移植计划

> 状态：**待确认**（分阶段小 PR 交付，确认后逐段开工）
> 范围：把旧 Python 后端（`clawmessenger-server/discussion/`，8,339 行）的圆桌子系统整体移植到 Go 服务端。
> 决策记录：① 表落在 Go `public` schema，**新建表**，旧 Python `im_*` 数据仅归档不迁；② **全量含 v3**（v3_envelope / interactive rounds / wire 分帧 / delta buffer / 共享投递）；③ 分阶段小 PR。

---

## 1. 现状差距

| 维度 | Python（参考实现） | Go（现状） |
|---|---|---|
| 代码量 | `discussion/` 18 文件 8,339 行 | 讨论相关 ~1,200 行（不含测试） |
| 讨论实例 | `im_discussions`（topic/goal/mode/status/version/role_snapshot/round_policy…） | **无实例表**，只能按 chatroom 存事件 |
| 事件表 | `im_discussion_events`（seq / actor_id / target_id / payload / idempotency_key / v3_envelope） | `rongcloud_discussion_event`（event_type / round_number / speaking_order / content，11 种 CHECK） |
| 运行时 | host 决策 + contribution 增量/完成 + artifact 合成 + 逐轮文档 + 收敛 | 仅「按 `speaking_order` 固定轮流 `your_turn`」+ 事件回放 + 暂停/恢复 |
| 模式 | `moderated` / `roundtable`（interactive rounds，用户确认轮次） | 无 |
| 角色推荐 | `im_discussion_role_recommendations` + `im_node_requests` + 幂等创建状态机 | 只在管理端做一次性 IM 往返**预览**，运行时不消费 |
| 命令通道 | `chatroom` 21 个 action + `discussion` 13 个 action | `chatroom` 仅 list/get/info；`discussion` 仅 status/your_turn |
| 命令回包 | 扁平 `{requestId,service,action,status,code,data,message,timestamp,msg_type}` | 走 `sendCommandResult`（设备形状，**无 `status`**），Web 端解析不到 |
| 逐轮文档 | `im_discussion_round_documents` | 无 |
| 产物 | `im_artifacts` | 无 |
| 系统主持配置 | 接入运行时 | `claw_admin_discussion_host.go` 明写「运行时不读此配置」 |

**C 端前端（`clawmessenger-web`）依赖三条通道**，约 40 个调用点：
1. HTTP REST：`/api/chatrooms/{id}/discussions`、`/api/discussions/{id}`、`/events`、`/round-documents`、`/api/round-documents/{id}[/download]`、`/api/artifacts/{id}`
2. 系统 RPC（IM command，发给 `system`）：`service=discussion` 13 个 / `service=chatroom` 12 个
3. 节点 P2P：`discussion_model_catalog_*`、`claw_runtime_recovery_*`

**关键结论**：「新建讨论组」主入口 `RoundtableSetupDialog` 已强校验 v3（`recommendRoles` → `createRoundtable`），不做 v3 则该入口直接不可用。

---

## 2. 目标架构

```
server/internal/
├─ integrations/rongcloud/
│   ├─ discussion_repository.go        # 讨论实例 + 事件（乐观锁 compare_and_set）
│   ├─ artifact_repository.go          # 产物
│   ├─ round_document_repository.go    # 逐轮文档
│   ├─ chatroom_repository.go          # 聊天室 + 成员（补 name/portrait_uri/member_id）
│   ├─ system_repository.go            # 节点请求 / 角色推荐 / 幂等创建
│   ├─ discussion_runtime.go           # host 调度编排（替代 Python service.py 主干）
│   ├─ interactive_rounds.go           # user_confirmed 交互式轮次
│   ├─ protocol_v3.go                  # v3 信封严格校验
│   ├─ discussion_wire.go              # 分帧重组 + delta buffer
│   ├─ discussion_recovery.go          # 恢复 worker
│   └─ system_host_runtime.go          # 系统主持配置接入运行时
└─ handler/
    ├─ rongcloud_handler.go            # 现有 workspace 前缀 REST（保留）
    └─ discussion_api.go               # 新增扁平 REST（对齐 C 端）
```

命令通道：`system_handler.go` 用 `map[service+"/"+action]handler` 替换 switch，回包统一走 `sendWebCommandResult`（Web 形状），参数同时接受 camelCase / snake_case。

---

## 3. 分阶段计划

每阶段**独立可编译、可测试、可上线**，PR 单独提交。

### P1 — 命令通道底座（无迁移，最小风险）

**目标**：先修好通道，让 Web 侧能真正收到 chatroom/discussion 的回包。这是后续所有写操作的前置。

**涉及**：
- `server/internal/integrations/rongcloud/system_handler.go`
  - `handleChatroomCommand`（:87）/ `handleDiscussionCommand`（:249）的成功与错误回包，从 `sendCommandResult` 改为 `sendWebCommandResult`（扁平、带 `status`）。
  - 参数读取兼容双拼写：`chatroom_id` / `chatroomId`、`workspace_id` / `workspaceId`。
  - 未知动作契约对齐 Python：`{"code":404,"message":"未知的服务或操作: chatroom/<action>"}`（前端 `isLegacyChatroomActionError` 依赖 `code===404 && 含 'chatroom/<action>' && 含 '未知'`）。
- `server/internal/integrations/rongcloud/client.go`（:176 / :199）：确认两个回包函数的调用点边界。
- 测试：`system_handler_test.go` / `rongcloud_test.go` 补回包形状与双拼写用例。

**验收**：curl/单测断言 Web 形状回包；未知 action 返回可被前端容错命中的 404 文案。

**风险点**：需区分「Web 入站请求」与「节点回环」来源——`your_turn` 等 action 是 Go 主动发给节点的，不能在回环时误用 Web 回包。实施时先确认 `CommandContent` 是否带来源标识。

---

### P2 — 数据表与仓库层（迁移 589+）

**目标**：把 Python 的数据模型搬到 Go `public` schema，并实现仓库层。

**新建迁移**（编号从 `589` 起）：
| 表 | 对应 Python | 关键列 |
|---|---|---|
| `rongcloud_chatroom`（**加列**） | `im_chatrooms` | `name`、`portrait_uri`、`openclaw_max_rounds` |
| `rongcloud_chatroom_member`（**加列**） | `im_chatroom_members` | `member_id`、`nickname`、`portrait_uri` |
| `rongcloud_discussion` | `im_discussions` | `discussion_id`、`topic`、`goal`、`max_rounds`(nullable)、`mode`、`status`、`current_round`、`version`、`role_snapshot`、`host_config_snapshot`、`recommendation_id`、`client_request_id`、`round_policy`、`round_revision` + 唯一活跃索引 + 幂等唯一索引 |
| `rongcloud_discussion_event`（**升级**） | `im_discussion_events` | 加 `seq`、`actor_id`、`target_id`、`payload`、`idempotency_key`、`v3_envelope`；`UNIQUE(discussion_id,seq)` + `UNIQUE(discussion_id,idempotency_key)` |
| `rongcloud_artifact` | `im_artifacts` | `artifact_id`、`artifact_type`、`title`、`content`、`version`、`base_version` |
| `rongcloud_discussion_round_document` | `im_discussion_round_documents` | `document_id`、`round`、`revision`、`title`、`content` |
| `rongcloud_discussion_command` | `im_discussion_commands` | outbox：`kind`(cancel/dispatch)、`state`、`lease_until` |
| `rongcloud_node_request` | `im_node_requests` | `request_kind`、`target_node_id`、`status`、`request_payload`、`response_payload` |
| `rongcloud_role_recommendation` | `im_discussion_role_recommendations` | `recommendation_id`、`roles`(jsonb)、`expires_at` |
| `rongcloud_roundtable_create_request` | `im_roundtable_create_requests` | `(user_id, client_request_id)` PK、`request_payload_hash`、`status` |
| `rongcloud_discussion_delivery_lock` / `_wire_buffer` / `_delta_buffer` | `011` 三表 | 共享投递 |

**Go 仓库层**：`DiscussionRepository`（`compare_and_set` 乐观锁、`create_with_events`、`events_after`）、`ArtifactRepository`、`RoundDocumentRepository`、`ChatroomRepository`、系统仓库。
**测试**：仓库层单测（对照 Python `tests/test_discussion_repository.py` 语义）。

**验收**：`./bin/migrate up` 幂等可跑；Go 侧能读写上述表；`go test ./internal/integrations/rongcloud/...` 全绿。

**待确认**：现有 `rongcloud_discussion_event` 已被旧链路（`discussion_event_store.go` + 状态机）使用，是「升级复用」还是「新建独立表并存」——见第 4 节。

---

### P3 — chatroom 服务全 action

**目标**：补齐 `service=chatroom` 的 12 个 Web 动作，让「创建/管理聊天室与成员」可用。

**动作**：`create` / `update` / `getInfo` / `destroy` / `getMembers` / `join` / `leave` / `invite` / `removeMember` / `setHost` / `setMemberRole` / `setDiscussionTeam` / `getList`（+ 可选：黑白名单/禁言系列）。
**涉及**：`system_handler.go`、`chatroom_repository.go`、`rongcloud/chatroom_api.go`（融云 side）。
**参考**：`command_handler.py:4605-5536`、`_chatroom_data()`（:761）/ `_chatroom_member_data()`（:746）字段构造。
**验收**：C 端 `Home.handleCreateRoom` / `handleJoinRoom` / `handleAddMember` / `handleSetRoomHost` 全链路通。

---

### P4 — discussion 生命周期

**目标**：讨论实例的创建与推进。

**动作**：`getWorkflowCapabilities` / `getSystemHostStatus` / `start` / `continue` / `finish` / `cancel` / `retry` / `interject` / `stop` / `getActive`。
**REST**：`GET /api/chatrooms/{id}/discussions`、`GET /api/discussions/{id}`、`GET /api/discussions/{id}/events?afterSeq=`。
**涉及**：`discussion_api.go`（新，扁平路由）、`discussion_runtime.go`、`discussion_repository.go`。
**验收**：`StartDiscussionDialog`（moderated）全链路；`getActive` 返回正确快照。

---

### P5 — 角色推荐 + 圆桌创建编排（打通主入口）

**目标**：「新建讨论组」主入口可用。

**动作**：`recommendRoles` / `createRoundtable` / `resumeRoundtable`。
**多阶段幂等状态机**：`pending → chatroom_ready → members_ready → discussion_ready → ready`（`roundtable_creation.py:195-233`），幂等键 `(user_id, client_request_id)`。
**REST**：`GET /api/discussions/{id}/round-documents`、`GET /api/round-documents/{id}[/download]`、`GET /api/artifacts/{id}`。
**验收**：`RoundtableSetupDialog` 从推荐 → 提交 → 进入讨论组，全程走通。

---

### P6 — 讨论运行时（让它真的跑起来）

**目标**：host 决策、成员发言、增量汇总、产物合成、逐轮文档、事件推流。

**涉及**：`discussion_runtime.go` 主干（对照 `service.py` 的 `start/continue/finish/interject/retry/stop` 与 `handle_host_decision` / `handle_contribution_delta` / `handle_contribution_completed` / `handle_artifact_update` / `handle_node_error`）、`public_event_data()` 白名单（`service.py:210`）。
**验收**：一轮真实 LLM 讨论端到端跑完，产物/文档落库，事件推流到前端。

---

### P7 — v3 交互式协议（user_confirmed）

**目标**：`roundtable` 模式全量。

**涉及**：`protocol_v3.go`（`parse_discussion_v3` 严格校验）、`interactive_rounds.go`（调度 + cancel/retry + recover）、`discussion_wire.go`（分帧重组 + `_wire_buffer` / `_delta_buffer`）、共享投递锁、`round_document_repository.go` 的 `complete_round`。
**验收**：`user_confirmed` 模式下逐轮确认、插话、重试、超时恢复全部可用。

---

### P8 — 恢复与收尾

**目标**：进程重启后不丢讨论；系统主持配置真正接入运行时。

**涉及**：`discussion_recovery.go`（对照 `recovery.py`）、`system_host_runtime.go`（让 `ClawDiscussionHostPersona` 配置被运行时读取）、端到端联调 + 文档。

---

## 4. 需要你确认的 4 个细节

1. **`rongcloud_discussion_event` 处理**：现有表被旧「固定轮流」链路独占（11 种 event_type CHECK）。推荐**升级复用**（加列 + 放宽 CHECK + 加 UNIQUE），旧 11 种作为新集合子集保留；替代方案是新建 `rongcloud_discussion_event_v3` 并存（更干净但两套事件流）。
2. **workspace 归属**：Python 的 `im_chatrooms` 无 workspace，Go 现有 `rongcloud_chatroom` 有 `workspace_id`。新增列是否强制 workspace？（推荐保留现有 workspace 语义，C 端通过用户所属 workspace 解析。）
3. **REST 路径**：C 端要的是**扁平** `/api/chatrooms/{id}/discussions`；Go 现有是 workspace 前缀 `/api/workspaces/{id}/rongcloud/chatrooms/...`。推荐**两种并存**（保留管理端旧路径，新增扁平路径给 C 端）。
4. **旧「固定轮流」链路**：是否保留？推荐 P6 完成后**下线**，P1–P5 期间保留（避免中途破坏现有可用功能）。

---

## 5. 风险

- **量级**：Go 侧需新增 ~3,500–4,500 行（不含测试），是本次最大的一次投入。
- **v3 协议正确性**：`parse_discussion_v3` 有严格 schema 校验，前端 `discussion-v3.ts` 同步强校验，字段对不上会静默丢消息 → P7 需要双端对照测试。
- **命令通道来源区分**：Web 入站 vs 节点回环（P1 风险点），需先确认 `CommandContent` 是否携带来源标识。
- **并发**：所有讨论推进基于 `version` 乐观锁，冲突必须回 409 且携带最新快照（前端依赖此行为做重同步）。
- **回归**：现有 Go 讨论链路（管理端 workspace REST + 固定轮流）需在 P1–P5 期间保持可用。
