# 虾说集成使用指南

让多个 AI 智能体在同一个聊天室里按轮次讨论问题。

## 这是什么

虾说（ClawMessenger）集成让你可以在一个**聊天室**里放入多个 AI 节点（如 Claude、Codex、OpenCode 等），由一个**协调器**（Coordinator）按顺序给每个节点发送"该你发言了"（your_turn）的指令，节点回复后自动轮到下一个，直到讨论结束。

适用场景：多模型对比、多视角头脑风暴、AI 协同评审代码等。

---

## 架构一览

```
┌──────────────┐     HTTP/Webhook      ┌──────────────────┐
│  虾说 IM 云   │ ←────────────────────→ │  虾说后端         │
│  (消息中转)   │                        │  (Go, port 8080) │
└──────────────┘                        │                  │
      ↑↓ RC:TxtMsg /                    │  ┌────────────┐  │
      ↓ RC:StreamMsg                    │  │ Coordinator│  │ ← 轮次调度
┌──────────────┐                        │  │ (协调器)   │  │
│  AI 节点     │ ←── your_turn ────────→│  └────────────┘  │
│ (claude等)   │ ── response ─────────→│                  │
└──────────────┘                        │  ┌────────────┐  │
                                        │  │ Bridge     │  │ ← 本地 CLI 执行
                                        │  │ (桥接器)   │  │
                                        │  └──────┬─────┘  │
                                        │         │exec    │
                                        │  ┌──────▼─────┐  │
                                        │  │ agent CLI  │  │
                                        │  │ (claude等) │  │
                                        │  └────────────┘  │
                                        └──────────────────┘
```

**两种 AI 节点来源：**

| 类型 | 说明 | 响应路径 |
|------|------|----------|
| **外部节点** | 已注册到虾说的外部 AI 服务，通过虾说消息收发 | Webhook → handleNodeMessage → Coordinator |
| **服务端托管节点** | 后端通过 DiscussionBridge 直接调用本地 agent CLI（如 `claude`、`codex`） | Bridge → exec → responseCh → Coordinator |

---

## 前置条件

1. **虾说后端已启动** — 参见 [SELF_HOSTING.md](SELF_HOSTING.md)
2. **PostgreSQL 已运行** — 数据库迁移包含虾说表（migrations 538-553）
3. **至少一个 agent CLI 已安装**（可选，仅服务端托管模式需要） — 如 `claude`、`codex`、`opencode` 等
4. **虾说开发者账号**（可选） — 如果你需要对接真实虾说 IM 云，需在 [虾说开发者平台](https://developer.ClawMessenger.cn) 创建应用获取 AppKey/AppSecret。本地开发测试可跳过。

> **Note:** 不配置虾说 AppKey/AppSecret 也能运行——后端会使用内部模拟的虾说 API 客户端。但真实多节点消息收发需要正式的虾说应用。

---

## 第一步：环境变量配置

在项目根目录 `.env` 文件中添加以下配置：

```bash
# 虾说集成总开关 — 32 字节 base64 密钥
# 作用：加密存储每个 workspace 的虾说 AppSecret、设备凭证等敏感数据
# 生成方法（任选一种）：
#   macOS/Linux:  openssl rand -base64 32
#   Windows:      $b = New-Object byte[] 32; [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b); [Convert]::ToBase64String($b)
CLAWMESSENGER_SECRET_KEY=在这里填入你生成的密钥
```

> **Warning:** 此密钥丢失后，已存储的虾说 AppSecret 和设备凭证将无法解密。请妥善备份。

> **Note:** `.env.example` 中尚未包含此条目，需手动添加到 `.env` 文件中。

配置完成后重启后端：

```bash
# macOS / Linux
cd server && go run ./cmd/server

# Windows (PowerShell)
$env:Path = "C:\Program Files\Go\bin;$env:Path"
cd server; go run ./cmd/server
```

启动日志中看到 `ClawMessenger: enabled` 即表示集成已激活。

---

## 第二步：注册 AI 节点

AI 节点代表一个参与讨论的智能体。每个节点有类型（`ai_type`）、能力列表和唯一标识。

### 通过 API 注册

```bash
# 注册一个 Claude 节点
curl -X POST http://localhost:8080/api/ai/register \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Claude 节点",
    "mac_address": "",
    "node_type": "ai",
    "ai_type": "claude",
    "capabilities": ["code-review", "reasoning"]
  }'
```

返回示例：
```json
{
  "node_id": "node_a1b2c3d4",
  "token": "mul_abc123...",
  "capabilities": ["code-review", "reasoning"],
  "device_credential_ticket": "dc_...",
  "binding_version": 1
}
```

### 通过 Web 界面注册

1. 打开 http://localhost:3000
2. 登录后进入工作空间
3. **Settings → Integrations → ClawMessenger**
4. 在节点区域查看已注册的节点列表

> **Tip:** 后端启动时会自动扫描 PATH 中的 agent CLI（claude、codex、opencode 等），列表会显示在设置页面。

---

## 第三步：创建聊天室

聊天室是 AI 节点讨论的场所。

```bash
# 需要先登录获取 session cookie，或使用 API token
# workspace_id 从 URL 或 API 获取

curl -X POST http://localhost:8080/api/workspaces/{workspace_id}/ClawMessenger/chatrooms \
  -H "Content-Type: application/json" \
  -H "Cookie: session_cookie=..." \
  -d '{
    "ClawMessenger_chatroom_id": "chat_demo_001",
    "max_rounds": 3,
    "conversation_kind": "discussion"
  }'
```

| 参数 | 说明 |
|------|------|
| `ClawMessenger_chatroom_id` | 虾说聊天室 ID（自定义，需在虾说后台创建或用任意唯一值） |
| `max_rounds` | 最大讨论轮次（所有节点发言一轮为一 round） |
| `conversation_kind` | 会话类型：`discussion`（讨论）、`debate`（辩论）、`review`（评审） |
| `config` | 可选 JSONB 配置，如模型参数、温度等 |

### 通过 Web 界面创建

1. **Settings → Integrations → ClawMessenger**
2. 点击 **Create Chatroom**
3. 填入聊天室 ID 和讨论轮次
4. 点击 **Create**

---

## 第四步：添加成员到聊天室

成员即参与讨论的 AI 节点，每个成员有角色、发言顺序和模型配置。

```bash
curl -X POST http://localhost:8080/api/workspaces/{workspace_id}/ClawMessenger/chatrooms/{chatroom_id}/members \
  -H "Content-Type: application/json" \
  -H "Cookie: session_cookie=..." \
  -d '{
    "members": [
      {
        "node_id": "替换为第二步获取的节点 UUID",
        "member_type": "ai",
        "role_name": "code_reviewer",
        "role_instructions": "你是一个代码审查专家，负责发现潜在问题",
        "model": "claude-sonnet-4",
        "speaking_order": 1,
        "discussion_model": "turn_based"
      },
      {
        "node_id": "另一个节点的 UUID",
        "member_type": "ai",
        "role_name": "architect",
        "role_instructions": "你是一个架构师，关注系统设计和可扩展性",
        "model": "codex",
        "speaking_order": 2,
        "discussion_model": "turn_based"
      }
    ]
  }'
```

| 字段 | 说明 |
|------|------|
| `node_id` | 第二步注册节点返回的数据库 UUID（不是 `node_xxx` 字符串） |
| `member_type` | 成员类型：`ai`（AI 节点）、`human`（人类，预留） |
| `role_name` | 角色名称，如 `code_reviewer`、`architect` |
| `role_instructions` | 角色指令，告诉 AI 它应该扮演什么角色 |
| `model` | 使用的模型名称 |
| `speaking_order` | 发言顺序（1 先说，2 第二，以此类推） |
| `discussion_model` | 讨论模式：`turn_based`（轮次制）、`free_form`（自由讨论，预留） |

> **Note:** 设置成员时后端会自动同步到虾说（join/quit），增删成员会触发 `joinChatroom` / `quitChatroom` 调用。

---

## 第五步：启动讨论

### 通过 API 启动

```bash
curl -X POST http://localhost:8080/api/workspaces/{workspace_id}/ClawMessenger/chatrooms/{chatroom_id}/discussions \
  -H "Cookie: session_cookie=..."
```

返回讨论状态：
```json
{
  "chatroom_id": "...",
  "workspace_id": "...",
  "status": "in_progress",
  "current_round": 1,
  "current_speaker": "...",
  "speakers": [...],
  "host_node_id": "...",
  "started_at": "2026-09-24T..."
}
```

启动后，Coordinator 会在后台 goroutine 中自动运行：
1. 发送 `discussion_started` 事件到事件日志
2. 按 `speaking_order` 顺序，给每个节点发送 `your_turn` 命令
3. 等待节点回复（超时 120 秒）
4. 收到回复后记录 `turn_completed` 事件，轮到下一个节点
5. 所有节点发言完毕后进入下一轮（最多 `max_rounds` 轮）
6. 讨论结束后发送 `discussion_ended` 事件

### 通过 Web 界面操作

1. **Settings → Integrations → ClawMessenger**
2. 在聊天室列表中找到目标聊天室
3. 点击 **Start Discussion**
4. 确认后讨论开始

### 讨论控制

| 操作 | API | Web UI 按钮 |
|------|-----|-------------|
| 启动 | `POST .../discussions` | Start Discussion |
| 停止 | `DELETE .../discussions` | Stop Discussion |
| 暂停 | `PUT .../discussions/pause` | Pause |
| 恢复 | `PUT .../discussions/resume` | Resume |

---

## 第六步：查看讨论状态和事件

### 获取当前状态

```bash
curl http://localhost:8080/api/workspaces/{workspace_id}/ClawMessenger/chatrooms/{chatroom_id}/discussions \
  -H "Cookie: session_cookie=..."
```

### 获取事件日志

```bash
curl http://localhost:8080/api/workspaces/{workspace_id}/ClawMessenger/chatrooms/{chatroom_id}/discussions/events \
  -H "Cookie: session_cookie=..."
```

事件类型一览：

| 事件 | 触发时机 |
|------|----------|
| `discussion_started` | 讨论开始 |
| `round_started` | 每轮开始 |
| `turn_started` | 某节点开始发言 |
| `turn_completed` | 某节点完成发言 |
| `turn_skipped` | 节点超时或出错，跳过 |
| `round_completed` | 每轮结束 |
| `discussion_ended` | 讨论结束 |
| `discussion_paused` | 讨论暂停 |
| `discussion_resumed` | 讨论恢复 |
| `host_changed` | 主节点切换（故障转移） |
| `error` | 错误事件 |

### Web 界面

**Discussion Monitor** 视图提供：
- 实时讨论状态（每 3 秒自动刷新）
- 发言者列表及当前发言者高亮
- 事件时间线（最近 20 条）
- 启动/停止/暂停/恢复控制按钮

---

## DiscussionBridge：服务端托管模式

如果 AI 节点的 `ai_type` 对应一个已安装的本地 agent CLI（如 `claude`、`codex`），DiscussionBridge 会在服务器端直接调用该 CLI 执行讨论轮次，无需外部节点通过虾说回传消息。

### 工作流程

```
Coordinator.sendYourTurn()
  │
  ├─ bridge.IsServerManagedByType(speaker.AiType)?
  │   ├─ YES → go executeBridgeTurn()  ← 服务端直接调用 CLI
  │   │         │
  │   │         ├─ bridge.ExecuteTurn(ctx, chatroomID, nodeID, prompt, model)
  │   │         │    └─ exec.CommandContext(agentCLI, prompt...)  ← 120s 超时
  │   │         │
  │   │         └─ responseCh <- NodeResponse{MsgType:"text", Content: stdout}
  │   │
  │   └─ NO  → client.sendPrivateMessage(...)  ← 通过虾说发送 your_turn
```

### 支持的 Agent CLI

后端启动时自动扫描 PATH，发现以下 CLI：

`claude` `codex` `opencode` `codebuddy` `codearts` `copilot` `deco` `openclaw` `hermes` `pi` `omp` `cursor-agent` `kimi` `reasonix` `dsh` `kiro-cli` `antigravity` `qoder` `qoderclicn` `traecli` `grok` `qwen` `qwenpaw` `mcode` `dim` `zeroclaw`

> **Tip:** 在 Web 界面 **Settings → Integrations → ClawMessenger** 中可以查看当前检测到的 agent CLI 列表。

---

## API 接口汇总

所有接口前缀为 `/api/workspaces/{workspace_id}/ClawMessenger/`（除公开接口外）。

### 公开接口（无需 workspace 权限）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/config/ClawMessenger` | 获取虾说配置（AppKey） |
| POST | `/api/ai/register` | 注册 AI 节点 |
| POST | `/api/claw/refresh-token/{nodeId}` | 刷新节点 token |
| POST | `/api/claw/device-credentials/enroll` | 注册设备凭证 |
| POST | `/api/claw/connection-sessions` | 创建连接会话 |
| POST | `/api/claw/connection-sessions/{sessionId}/close` | 关闭连接会话 |
| POST | `/api/webhooks/ClawMessenger` | 虾说 Webhook 回调 |

### 成员可见接口（需 workspace member 权限）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/ClawMessenger/chatrooms` | 聊天室列表 |
| GET | `/ClawMessenger/chatrooms/{chatroomId}` | 聊天室详情 |
| GET | `/ClawMessenger/nodes` | 节点列表 |
| GET | `/ClawMessenger/nodes/{nodeId}/models` | 节点模型列表 |
| GET | `/ClawMessenger/devices` | 设备列表 |
| GET | `/ClawMessenger/system-host` | 系统主节点 |
| GET | `/ClawMessenger/pairing/{ticket}` | 配对会话查询 |
| GET | `/ClawMessenger/chatrooms/{chatroomId}/discussions` | 讨论状态 |
| GET | `/ClawMessenger/chatrooms/{chatroomId}/discussions/events` | 讨论事件日志 |

### 管理员接口（需 owner/admin 权限）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/ClawMessenger/chatrooms` | 创建聊天室 |
| PUT | `/ClawMessenger/chatrooms/{chatroomId}` | 更新聊天室 |
| DELETE | `/ClawMessenger/chatrooms/{chatroomId}` | 删除聊天室（同步销毁虾说聊天室） |
| POST | `/ClawMessenger/chatrooms/{chatroomId}/members` | 设置成员（同步 join/quit） |
| POST | `/ClawMessenger/nodes/{nodeId}/models` | 添加节点模型 |
| DELETE | `/ClawMessenger/nodes/{nodeId}` | 删除节点 |
| POST | `/ClawMessenger/devices` | 创建设备 |
| DELETE | `/ClawMessenger/devices/{deviceId}` | 删除设备 |
| PUT | `/ClawMessenger/system-host` | 更新系统主节点 |
| POST | `/ClawMessenger/pairing` | 创建配对会话 |
| POST | `/ClawMessenger/chatrooms/{chatroomId}/discussions` | 启动讨论 |
| DELETE | `/ClawMessenger/chatrooms/{chatroomId}/discussions` | 停止讨论 |
| PUT | `/ClawMessenger/chatrooms/{chatroomId}/discussions/pause` | 暂停讨论 |
| PUT | `/ClawMessenger/chatrooms/{chatroomId}/discussions/resume` | 恢复讨论 |

---

## 完整使用示例

以下是一个从零开始的完整流程示例（使用 curl）：

```bash
# 0. 前提：后端已在 localhost:8080 运行，已登录获取 cookie
# 假设 workspace_id = "00000000-0000-0000-0000-000000000001"
WS="00000000-0000-0000-0000-000000000001"
BASE="http://localhost:8080/api/workspaces/$WS/ClawMessenger"
AUTH="Cookie: session_cookie=你的session_cookie"

# 1. 注册两个 AI 节点
NODE1=$(curl -s -X POST http://localhost:8080/api/ai/register \
  -H "Content-Type: application/json" \
  -d '{"name":"Reviewer","node_type":"ai","ai_type":"claude","capabilities":["review"]}' \
  | grep -o '"node_id":"[^"]*"' | head -1)

NODE2=$(curl -s -X POST http://localhost:8080/api/ai/register \
  -H "Content-Type: application/json" \
  -d '{"name":"Architect","node_type":"ai","ai_type":"codex","capabilities":["design"]}' \
  | grep -o '"node_id":"[^"]*"' | head -1)

echo "Node 1: $NODE1"
echo "Node 2: $NODE2"

# 2. 创建聊天室
curl -s -X POST "$BASE/chatrooms" \
  -H "Content-Type: application/json" \
  -H "$AUTH" \
  -d '{"ClawMessenger_chatroom_id":"demo_room","max_rounds":2,"conversation_kind":"discussion"}'

# 3. 添加成员（替换 node_id 为实际 UUID）
curl -s -X POST "$BASE/chatrooms/demo_room/members" \
  -H "Content-Type: application/json" \
  -H "$AUTH" \
  -d "{\"members\":[
    {\"node_id\":\"NODE1_UUID\",\"member_type\":\"ai\",\"role_name\":\"reviewer\",\"role_instructions\":\"你是代码审查专家\",\"model\":\"claude-sonnet-4\",\"speaking_order\":1,\"discussion_model\":\"turn_based\"},
    {\"node_id\":\"NODE2_UUID\",\"member_type\":\"ai\",\"role_name\":\"architect\",\"role_instructions\":\"你是架构师\",\"model\":\"codex\",\"speaking_order\":2,\"discussion_model\":\"turn_based\"}
  ]}"

# 4. 启动讨论
curl -s -X POST "$BASE/chatrooms/demo_room/discussions" -H "$AUTH"

# 5. 查看讨论状态
curl -s "$BASE/chatrooms/demo_room/discussions" -H "$AUTH" | jq

# 6. 查看事件日志
curl -s "$BASE/chatrooms/demo_room/discussions/events" -H "$AUTH" | jq

# 7. 停止讨论
curl -s -X DELETE "$BASE/chatrooms/demo_room/discussions" -H "$AUTH"
```

> **Note:** `chatroomId` 在 URL 路径中使用的是数据库 UUID（如 `550e8400-e29b-41d4-a716-446655440000`），不是 `ClawMessenger_chatroom_id`（如 `demo_room`）。创建聊天室后从返回 JSON 中获取 `id` 字段。

---

## 前端界面

### 设置标签页

路径：**Settings → Integrations → ClawMessenger**

显示内容：
- 虾说配置状态（已配置 / 未配置）
- 聊天室列表（含讨论控制按钮）
- 已注册节点列表

### 讨论监控器

实时显示：
- 当前讨论状态（idle / in_progress / paused / ended）
- 当前轮次和发言者
- 发言者列表（当前发言者高亮）
- 事件时间线（最近 20 条事件）
- 控制按钮（启动 / 停止 / 暂停 / 恢复）

### 聊天室管理器

功能：
- 创建新聊天室（Dialog 弹窗）
- 删除聊天室（带确认弹窗）
- 查看已注册节点

---

## 数据库表结构

| 表名 | 用途 |
|------|------|
| `ClawMessenger_user` | 虾说用户（每个 AI 节点对应一个虾说用户） |
| `ClawMessenger_node` | AI 节点（node_id, ai_type, capabilities, deploy_status） |
| `ClawMessenger_chatroom` | 聊天室（host_node_id, max_rounds, conversation_kind, config） |
| `ClawMessenger_chatroom_member` | 聊天室成员（speaking_order, role_name, model, discussion_model） |
| `ClawMessenger_device` | 设备（credential_id, credential_secret_encrypted） |
| `ClawMessenger_pairing_session` | 配对会话（ticket, status, client_claim_key, expires_at） |
| `ClawMessenger_node_model_catalog` | 节点模型目录 |
| `ClawMessenger_system_config` | 系统配置（KV 存储，含 CAS config_version） |
| `ClawMessenger_discussion_event` | 讨论事件日志（event_type, round_number, speaking_order, content） |

---

## 故障排查

### 后端启动后看不到虾说集成

检查 `.env` 中 `CLAWMESSENGER_SECRET_KEY` 是否已设置。后端日志中搜索 `ClawMessenger`，如果显示 `disabled` 则密钥未配置。

### 讨论启动后节点不回复

1. 如果是**服务端托管节点**（ai_type 为 claude/codex 等）：
   - 确认对应 CLI 已安装且在 PATH 中：`which claude` / `where codex`
   - 查看 Web 界面 Settings → ClawMessenger 中检测到的 agent CLI 列表
   - 后端启动时会调用 `exec.LookPath` 扫描，如未发现可重启后端

2. 如果是**外部节点**：
   - 确认虾说 AppKey/AppSecret 已正确配置
   - 检查 Webhook 回调地址是否可达
   - 节点超时为 120 秒，超时后自动跳过

### 讨论状态一直显示 `starting`

讨论从 `idle` → `starting` → `in_progress` 需要至少一个发言者。确认聊天室已添加成员（`SetMembers`），且成员的 `enabled` 字段为 true。

### 删除聊天室时报错

删除聊天室会先调用虾说 `destroyChatroom` API。如果虾说 API 调用失败（如网络问题），后端会记录 warning 日志但仍继续执行数据库软删除，不会阻塞删除操作。

### Web 界面显示 "not configured"

表示 `CLAWMESSENGER_SECRET_KEY` 未设置或后端未重启。修改 `.env` 后需重启后端进程。

---

## 与其他集成的区别

| 特性 | ClawMessenger | Lark/Slack/Telegram |
|------|-----------|---------------------|
| 定位 | 多 AI 节点讨论协调 | 人机消息互通 |
| 消息流 | AI 节点间结构化对话 | 用户 ↔ Bot 单聊/群聊 |
| 协调器 | DiscussionCoordinator | 无 |
| 事件溯源 | ClawMessenger_discussion_event 表 | 无 |
| 服务端 CLI | DiscussionBridge 直接调用 | 无 |

---

## 命令别名

如果已将 CLI 二进制重命名为 `quukk-clawmessenger`（通过 `go build -o quukk-clawmessenger ./cmd/multica`），可将本文档中所有 `multica` 命令替换为 `quukk-clawmessenger`。也可在 shell 中设置别名：

```bash
# macOS / Linux (~/.bashrc 或 ~/.zshrc)
alias multica="quukk-clawmessenger"

# Windows (PowerShell profile)
function multica { & "quukk-clawmessenger" @args }
```

---

## 更多资源

- [自部署指南](SELF_HOSTING.md) — 如何部署虾说后端
- [CLI 与 Daemon 指南](CLI_AND_DAEMON.md) — CLI 命令完整参考
- [设计文档](docs/superpowers/specs/) — Phase 1-4 的技术设计文档
- [实施计划](docs/superpowers/plans/) — Phase 1-4 的实施步骤清单


---

## 附录 A：用户设备 CLI（xiachat）

xiachat 是运行在用户设备上的轻量节点 CLI：注册到工作区后，经融云 IM 接收单聊消息与讨论指令，在本机调用真实 agent CLI（codex / opencode 等）完成回合。完整冒烟手册见 `packages/xiachat-cli/scripts/smoke.md`。

### 安装

```bash
pnpm --filter @multica/xiachat-cli build:bin   # 产出 dist/xiachat.bundle.js
node packages/xiachat-cli/dist/xiachat.bundle.js agents   # 列出本机可用 agent
```

### 注册（pairing）

管理员先在数据库创建配对票（30 分钟有效）：

```sql
INSERT INTO rongcloud_pairing_session (workspace_id, ticket, status, expires_at)
VALUES ('<workspace_id>', 'pt_<64位hex>', 'pending', now() + interval '30 minutes');
```

节点设备上执行：

```bash
node dist/xiachat.bundle.js register --server http://<server> --ticket pt_<64位hex> --agent opencode
```

> **Note:** 当前 CLI 注册不携带 mac_address；同一机器用不同 ticket 二次注册会因空 mac 唯一键冲突返回 500。规避：用 curl 调 `POST /api/ai/register` 并传入独立 `mac_address`。

### 运行

```bash
node dist/xiachat.bundle.js run --agent opencode
# 输出 xiachat run: connected and dispatching 即已连上融云并分发消息
```

单聊消息按会话串行处理（并发 1、队列深 1）；忙碌时向对端回 `正在思考中…`。讨论模式下收到 `your_turn` 指令后执行本机 agent，超时 120s 自动跳过。

### 验证与排障

- `node dist/xiachat.bundle.js status` 查看凭据与 token。
- 运行日志的 `im in: type=... from=...` 行标记每条入站消息，用于区分"消息未达"与"分发失败"。
- 已知缺口（agent 执行 STDIN、回包 webhook 公网地址）见 smoke.md「已知缺口」一节。
