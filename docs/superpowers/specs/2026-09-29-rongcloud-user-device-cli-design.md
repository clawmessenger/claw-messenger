# 融云用户设备 CLI（XiaChat CLI）设计 — 用户与自有 Agent 直接对话

> 目标：用户在自己设备上运行**一个 CLI**，该 CLI 内置融云 IM 连接并调用本地 Agent
> （claude / codex / opencode 等）。用户通过虾说 App 与自己的 Agent 单聊，讨论模式
> （多节点轮次）继续走既有协调器。所有内容消息全部经融云 IM 云投递；服务器仅通过
> webhook 旁观消息（日志/审计/协调），不做消息中转。**不使用 daemon，不使用 WebSocket。**

---

## §1 架构概览

### 1.1 现状与差距

Phase 1-4 已完成：IM channel 适配器（webhook 收 / Server API 发）、9 张表、
注册/凭证/配对 API、讨论协调器（your_turn / command_result / stream 协议）、
服务端 Bridge（服务器代打 Agent CLI）。

差距：**不存在"跑在用户设备上的 CLI"**。当前外部节点只是协议上的假设，
没有任何实现；用户无法与自有设备上的 Agent 对话。

### 1.2 目标形态

```
用户(虾说App/任何融云客户端)
    │  RC:TxtMsg / RC:StreamMsg（点对点，融云 IM 云直投）
    ↓
融云 IM 云 ──webhook（保留）──→ 服务器（旁观：入库/审计/讨论协调）
    │
    ↓
用户设备上的 XiaChat CLI（唯一新增组件）
    │  收：RC:TxtMsg（单聊）+ command/your_turn（讨论）
    │  发：RC:TxtMsg / RC:StreamMsg / command_result
    ↓
本地 Agent CLI（claude / codex / opencode / …，stdin→stdout）
```

关键性质：

1. **消息路径不含服务器**：用户↔Agent 的内容消息由融云点对点投递，
   CLI 以 Agent 的 `rongcloud_user_id` 身份登录 IM 即可直收。
2. **webhook 是服务器的眼睛，不是中转**：服务器看到消息用于讨论协调
   （多节点场景）与审计日志；单聊不依赖服务器在线。
3. **零 daemon、零 WebSocket**：CLI 与服务器之间只有启动时的 HTTP
   （注册/配对/token 刷新）。

### 1.3 非目标

- 不改造既有 daemon WebSocket 通道（主产品功能不受影响）。
- 不删除服务端 Bridge（DiscussionBridge 保留作兜底；部署策略见 §6.3）。
- 不做移动端 App（CLI 先覆盖桌面/服务器场景）。
- 不自研融云私有协议客户端。

---

## §2 CLI 设计

### 2.1 技术选型

| 项 | 选择 | 理由 |
|----|------|------|
| 语言 | TypeScript (Node.js ≥ 20) | 融云官方客户端 SDK 仅有 JS/TS（`@rongcloud/imlib-next`）等；Go 无官方客户端 SDK |
| IM SDK | `@rongcloud/imlib-next` | Web/Node 环境官方 IM 连接 |
| 打包 | `bun build --compile` 或 `pkg` | 产出单文件可执行，用户无需装 Node |
| Agent 调用 | `child_process.spawn` | 对齐服务端 `DiscussionBridge.ExecuteTurn` 的 stdin→stdout 约定（discussion_bridge.go:70） |

### 2.2 命令面

```
xiachat register --name "我的Claude" --ai-type claude [--server https://…]
    # 调 POST /api/ai/register，凭证写入本地 keystore

xiachat pair --ticket pt_xxx            # 扫码/贴码配对（复用 pairing session claim）
xiachat login                           # token 过期时调 POST /api/claw/refresh-token/{nodeId}
xiachat run [--agent claude] [--model …] [--workdir …]
    # 主循环：连 IM → 收消息 → 调 agent → 回消息
xiachat agents                          # 列出本机 PATH 上可发现的 agent CLI
xiachat status                          # 显示当前身份/连接状态/最近回合
```

### 2.3 主循环（run）

1. 从本地 keystore 读取 node_id / token（文件权限 0600；Windows 用 DPAPI 可后补）。
2. `RongIMClient.init(appKey)` + `connect(token)`；appKey 来自
   `GET /api/config/rongcloud`（handler 已有，rongcloud_handler.go:17）。
3. 订阅消息，按 `objectName` 分派：

| 收到 | 处理 | 回复 |
|------|------|------|
| `RC:TxtMsg`（用户单聊） | 拼 prompt（system: 你是 XX 节点 + 用户正文）→ spawn agent → stdout | `RC:TxtMsg`（短回复直接发；长回复走 `RC:StreamMsg` start/chunk/end） |
| `command` + `action=your_turn`（讨论） | 按 params（round / speaking_order / role_name / model）拼 prompt → spawn agent | `command_result`（msg_type=text）或 stream 序列（msg_type=stream，stream_type=start/chunk/end） |
| `command` + 其他 action | 记日志忽略 | — |

4. Agent 执行约束：单回合超时 120s（与服务端 turnTimeout 对齐）、stdout 截断上限
   （建议 64KB）、失败时回 `command_result` msg_type=error（讨论）或不回（单聊重试提示）。
5. **会话隔离与排队（决策）**：每个会话（单聊/群聊/讨论组）是独立 context，
   CLI 按 conversation（targetId + conversationType）维护独立 FIFO 队列——同会话
   内 agent 忙碌时排队（v1 队列深度 1，后续消息回"正在思考"提示），不同会话之间
   并行 spawn、互不阻塞。跨会话/跨回合的会话隔离与并发安全由 agent 平台自身
   负责，本项目不做全局锁。
6. 断线重连：SDK 内置重连；重连后无需重新注册。

### 2.4 与服务端 Bridge 的行为对齐

CLI 的 agent 调用语义必须与 `DiscussionBridge.ExecuteTurn` 一致，保证讨论协调器
无感知（对协调器而言，server-managed 与 external 节点只是响应来源不同）：

- prompt 从 stdin 传入；`--model` 透传（非空时）。
- `request_id` 原样回填到 `command_result.request_id`。
- 流式：每 chunk 一条 `RC:StreamMsg`，内容 JSON 含 `turn_id` / `stream_type` / `content`
  （对齐 discussion_coordinator.go:145 的 streamAssembler 协议）。

### 2.5 目录建议（新仓库或 monorepo 内 `packages/xiachat-cli/`）

```
src/
  main.ts          # 命令入口（commander）
  im.ts            # 融云连接/重连/消息分派
  agents.ts        # agent 发现（PATH 探测，名单对齐 knownAgentCLIs）+ spawn 封装
  protocol.ts      # CommandContent / CommandResultContent / stream 帧的 TS 类型与编解码
  keystore.ts      # 凭证本地加密存储
  api.ts           # register / pairing claim / refresh-token / config 的 HTTP 封装
```

---

## §3 绑定与配对流程

### 3.1 身份模型（决策）

- **用户**：用户在融云侧注册，获得普通融云身份（`rongcloud_user` 表
  node_type=human），可选择性接入任意数量的 agent 平台。
- **Agent 平台 = 融云用户**：后端支持多个 agent 平台（claude / codex / opencode …），
  **每个"用户 × 平台"组合注册为一个独立融云用户**（现有 `/api/ai/register`
  即此模型：`rc_node_<mac>` + `ai_type`）。用户设备上的 CLI 以该组合身份登录 IM，
  点对点消息天然按身份隔离，不同用户/不同平台互不串扰。
- 一个用户可注册多个平台节点（多台设备各跑一个 CLI），每个节点独立
  register → 独立 token → 独立 CLI 实例。

### 3.2 路径 A：直接注册（个人快速上手）

```
CLI → POST /api/ai/register {name, ai_type, node_type:"ai", capabilities:[…]}
    ← {node_id, token, device_credential_ticket, binding_version}
CLI 本地保存 → 连 IM → 上线
```

现状缺口：`Register` 内 `WorkspaceID/OwnerUserID` 为空（Phase 2b 预留），
注册出的节点不挂在任何 workspace，讨论模式的成员管理页看不到它。
需在 spec 范围内补：注册请求可选携带 pairing ticket，由服务端将节点归属
写入 workspace（见 §5.2 T1）。

### 3.3 路径 B：扫码/贴码配对（工作空间内）

复用既有 pairing session：

```
管理员(Web UI) → POST /ClawMessenger/pairing {candidate_node_ids} → ticket pt_xxx
用户 CLI      → xiachat pair --ticket pt_xxx
              → POST /api/claw/pairing/{ticket}/claim   ← 现状缺公开端点，需新增
              ← {device_credential_id, device_secret, node_id}
CLI 用 claim 返回的身份 login + run
```

现状缺口：`PairingService.ClaimSession` 已实现（pairing_service.go:82）但没有
公开 HTTP 端点暴露给 CLI。需新增 `POST /api/claw/pairing/{ticket}/claim`
（body: client_claim_key + idempotency_key），属最小服务端改动。

### 3.4 凭证与安全

- IM token 过期：CLI 调 `POST /api/claw/refresh-token/{nodeId}` 刷新（已有）。
- `device_secret` 与 IM token 本地加密存储（跨平台先用 0600 明文文件 + 计划中的
  OS keychain 适配位）；绝不写日志。
- webhook 侧签名校验已有（channel.go:69），不改动。

---

## §4 消息协议（全部复用，零新增）

| 方向 | objectName | content 结构 | 已有实现位置 |
|------|-----------|--------------|--------------|
| 服务器→CLI（讨论） | `command` | `CommandContent{request_id, service:"discussion", action:"your_turn", params}` | discussion_coordinator.go:179 |
| CLI→服务器（讨论） | `command`（content.msg_type=command_result） | `CommandResultContent{request_id, msg_type:"text"|"stream"|"error", payload}` | node_message_handler.go:39 |
| CLI→服务器（流式） | `RC:StreamMsg` | `{turn_id, stream_type, content}` | discussion_stream.go |
| 用户→CLI（单聊） | `RC:TxtMsg` | `{content:"…"}` | inbound.go |
| CLI→用户（单聊回复） | `RC:TxtMsg` / `RC:StreamMsg` | 同上 | —（CLI 新增，服务器仅旁观） |

单聊消息服务器侧行为：webhook 收到用户→agent 的 `RC:TxtMsg` 后走
`normalizeInbound` → channel inbound handler（现有通用 IM 通道逻辑），
**不新增路由**；讨论协调器只关心 command/stream，互不干扰。

---

## §5 服务端改动清单（最小化）

### 5.1 无需改动

- webhook 解析/签名/分发（channel.go）
- 讨论协调器、事件存储、状态机
- 注册/刷新 token/设备凭证/连接会话端点
- 前端设置页（自动多出 external 节点展示）

### 5.2 需要改动

| # | 改动 | 位置 | 说明 |
|---|------|------|------|
| T1 | 注册可选携带 workspace 归属 | rongcloud_handler.go `RegisterRongCloudAINode` + NodeService.Register | 请求体加可选 `pairing_ticket` 或 `workspace_id`（后者仅限已认证 session），把 `WorkspaceID/OwnerUserID` 写进 `rongcloud_node` |
| T2 | 新增 pairing claim 公开端点 | router.go + rongcloud_handler.go | `POST /api/claw/pairing/{ticket}/claim`，body `{client_claim_key, idempotency_key}`，调既有 `PairingService.ClaimSession` |
| T3 | 节点在线状态展示（可选，v1 可不做） | deploy_status 字段已有 | CLI 定期发一条 command_result 心跳或在 IM presence 上报；v1 用 `deploy_status=active`（注册时已写）+ 最后活跃时间即可 |

### 5.3 数据库

**零新表、零迁移**。复用 `rongcloud_node` / `rongcloud_device` /
`rongcloud_pairing_session` / `rongcloud_user`。

---

## §6 部署与兼容策略

### 6.1 讨论模式双来源并存

协调器 `sendYourTurn` 的分派逻辑不变：`bridge.IsServerManagedByType(ai_type)`
命中 → 服务端代打；否则发 `your_turn` 等 CLI 回。二者可并存于同一聊天室。

### 6.2 服务器代打的退场路径（运营选择，不是代码开关）

若决定"全走用户设备"：服务器不安装 agent CLI 即可 —— PATH 探测不到，
`IsServerManagedByType` 自然全 false，全部回合走外部节点。无代码改动。

### 6.3 版本兼容

- 协议字段只增不改；CLI 读取 params 时对未知字段宽容。
- `binding_version` 已入库（当前=1），CLI 首版同=1；未来凭证轮换靠
  `/api/claw/device-credentials/enroll` 重新签发，不需 breaking。

---

## §7 测试与验收

### 7.1 CLI 单测（vitest）

- protocol.ts：CommandContent/CommandResultContent/stream 帧编解码矩阵
  （与服务端 rongcloud_test.go 的用例对齐，特别是 `objectName="command"`
  字面量、非 RC:CmdMsg）。
- agents.ts：PATH 探测、spawn 超时、stdout 截断、失败 stderr 上报。

### 7.2 集成冒烟（本地全链路）

前置：后端 + Postgres + 融云测试应用（AppKey/AppSecret）。

1. `xiachat register` → 数据库出现 node/device 行
2. `xiachat run` → 虾说 App（或融云 Web Demo 以用户身份）发 `RC:TxtMsg`
   → CLI 调 agent → App 收到回复
3. 建聊天室 + 把该节点加为成员 + 启动讨论 → `your_turn` 到达 CLI →
   `command_result` 回传 → 事件日志出现 `turn_completed`
4. kill CLI → 回合超时 → 事件日志 `turn_skipped`（协调器健壮性验证）

### 7.3 服务端回归

`go test ./internal/integrations/rongcloud/ ./internal/handler/`；
T1/T2 各补表驱动测试（沿用 testutil 模式）。

---

## §8 任务拆分（建议顺序）

| 里程碑 | 内容 | 依赖 |
|--------|------|------|
| M1 服务端开洞 | T1 + T2 + 测试 | 无 |
| M2 CLI 骨架 | register/pair/login/agents 命令 + keystore + api.ts | M1 |
| M3 单聊打通 | im.ts + RC:TxtMsg → agent → 回复 | M2 |
| M4 讨论打通 | your_turn → command_result / stream | M3 |
| M5 打包分发 | 单文件可执行 + 安装文档（对齐 CLI_INSTALL.md 风格） | M4 |

---

## §9 风险与开放问题

1. **融云 token 有效期**：`getUserToken` 返回的 token 期限由融云应用配置决定；
   CLI 需在 connect 失败时自动走 refresh-token。验收用例必须覆盖 token 过期重连。
2. **StreamMsg 在单聊的呈现**：取决于用户端 App 是否解析 RC:StreamMsg；
   若不支持则 CLI 长回复退化为分段 RC:TxtMsg。需在 M3 联调时实测确认。

### 已决策（原开放问题）

- ~~用户身份来源~~ → §3.1：用户注册融云获得身份；每个"用户 × agent 平台"注册为
  独立融云用户，用户可选择性接入多平台，各平台独立 CLI 实例。
- ~~单聊并发~~ → §2.3 第 5 条：会话粒度隔离，同会话 FIFO 排队，跨会话并发由
  agent 平台自身负责，本项目不做全局锁。
