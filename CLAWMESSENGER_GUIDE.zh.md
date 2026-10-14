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

> **Note:** 注册必须携带 `pairing_ticket`（管理员预建的 pending 配对票）：rongcloud 表的 workspace 为 NOT NULL，无 ticket 返回 400，见设计文档 §3.2 的实现决定。

**获取配对票（推荐）**：在虾说 Web「远程设备」页点 **「绑定远程设备」** 一键生成（见附录 A.1），用户设备上用 `clawmessenger pair` 认领即可完成注册 + 绑定，无需手工 curl。下方 API 方式适用于脚本化场景。

### 通过 API 注册

```bash
# 注册一个 Claude 节点（必须携带 pairing_ticket：rongcloud 表的 workspace 为 NOT NULL，
# 无 ticket 注册返回 400 "workspace attribution required"）
curl -X POST http://localhost:8080/api/ai/register \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Claude 节点",
    "mac_address": "aa:bb:cc:dd:ee:01",
    "node_type": "ai",
    "ai_type": "claude",
    "capabilities": ["code-review", "reasoning"],
    "pairing_ticket": "pt_<64位hex>"
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

# 1. 注册两个 AI 节点（注册必须携带 pairing_ticket，否则 400；ticket 需管理员预先创建且为 pending）
NODE1=$(curl -s -X POST http://localhost:8080/api/ai/register \
  -H "Content-Type: application/json" \
  -d '{"name":"Reviewer","node_type":"ai","ai_type":"claude","capabilities":["review"],"mac_address":"aa:bb:cc:dd:ee:01","pairing_ticket":"pt_<64位hex>"}' \
  | grep -o '"node_id":"[^"]*"' | head -1)

NODE2=$(curl -s -X POST http://localhost:8080/api/ai/register \
  -H "Content-Type: application/json" \
  -d '{"name":"Architect","node_type":"ai","ai_type":"codex","capabilities":["design"],"mac_address":"aa:bb:cc:dd:ee:02","pairing_ticket":"pt_<64位hex>"}' \
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

## 附录 A：用户设备 CLI（clawmessenger）— 绑定与使用

clawmessenger 是运行在用户设备上的轻量节点 CLI：绑定到工作区后，经融云 IM 接收单聊消息与讨论指令，在本机调用真实 agent CLI（codex / opencode 等）完成回合。

- **A.1** 绑定流程 — 生成配对票并在设备上认领
- **A.2** 日常使用 — 上线、行为说明、常用命令
- **A.3** 后台常驻与开机自启（CLI 原生）— 让设备长期在线（`clawmessenger run` 本身只有前台模式）
- **A.4** 备选：Windows 守护脚本 — 随 Release 分发的 PowerShell 版（A.3 覆盖不到的老环境用）
- **A.5** 备选：Linux / macOS 守护脚本 — 随 Release 分发的 bash 版
- **A.6** 验证与排障

完整冒烟手册见 `packages/xiachat-cli/scripts/smoke.md`。

### A.1 绑定流程（Web 端发起）

绑定 = 管理员在 Web 生成一张**配对票**（10 分钟有效），用户在设备上用 CLI 认领。

**第 1 步：在虾说 Web 生成配对票**

1. 打开虾说 Web（旧站），进入 **「远程设备」** 标签页
2. 点击 **「绑定远程设备」** 按钮（需登录；任何账号均可绑定自己的设备）
3. 弹窗自动生成一张 10 分钟有效的票据，并显示一条完整命令（`--server` 已自动填好），形如：

```bash
clawmessenger pair --ticket pt_<64位hex> --server https://<你的服务地址>
```

弹窗会每 2 秒自动检查票据状态，无需手动刷新；倒计时结束未认领会自动提示并可一键重新生成。

**第 2 步：用户在设备上安装并认领**

```bash
 # 安装：从 GitHub Releases 下载对应平台的单文件可执行版（无需安装 Node）
 #   https://github.com/clawmessenger/claw-messenger/releases/latest
 # Windows 直链示例：
 #   https://github.com/clawmessenger/claw-messenger/releases/latest/download/clawmessenger-windows-x64.exe
 # Linux/macOS 将文件名替换为 clawmessenger-linux-x64 / clawmessenger-linux-arm64 /
 # clawmessenger-darwin-x64 / clawmessenger-darwin-arm64，下载后 chmod +x
 # 开发者也可在仓库内自行构建：pnpm --filter @quukk/xiachat-cli build:exe
 #   → packages/xiachat-cli/dist/clawmessenger.exe（Linux/macOS 为 dist/clawmessenger）

 # 确认本机有可用的 agent CLI（claude / codex / opencode 等）
 clawmessenger agents

 # 认领配对票（复制 Web 弹窗里的命令，在设备上执行）
 clawmessenger pair --ticket pt_<64位hex> --server https://<你的服务地址>
```

> 也可用 `build:bin` 产出 `dist/clawmessenger.bundle.js`（需本机装有 Node ≥ 20，用 `node clawmessenger.bundle.js <cmd>` 运行）。

**第 3 步：确认绑定成功**

- Web 弹窗自动变为 **「设备已绑定」**
- 设备上 `clawmessenger.exe status` 可看到节点凭据

> **Note:** 认领失败提示 "ticket already claimed" 表示票据已被其他设备使用；「票据已过期」则重新点「绑定远程设备」生成。
> 绑定只需做一次；凭据保存在 `~/.clawmessenger/credentials.json`（Windows 为 `%USERPROFILE%\.clawmessenger\credentials.json`，权限 0600）。同一设备重复 pair 是安全的（幂等：复用已有节点，不会重复创建）。

### A.2 日常使用

**上线**

```bash
# 推荐：后台常驻（v0.2.15+）—— 关掉终端也不掉线，细节见 A.3
clawmessenger start
clawmessenger status          # 看 PID、agent 列表、日志位置

# 前台（排障时用，窗口一关就全下线）
clawmessenger run             # 不带 --agent = supervisor 模式，拉起本设备已绑定的「全部」agent
# 输出 clawmessenger supervisor: N of M agent(s) online 即已拉起全部 agent 并连上融云

# 只跑单个 agent（--agent 可选，不是必填）
clawmessenger run --agent opencode   # 可用值见 clawmessenger agents
# 输出 clawmessenger run: connected and dispatching 即已连上融云并开始分发消息
```

> **Warning:** `clawmessenger run` 是**前台进程**：它不脱离终端，会一直占着当前窗口，**关掉这个窗口（或 Ctrl+C、注销）会把该设备上所有 agent 一起下线**，设备在管理端随之变离线。日常常驻请用 [A.3 后台常驻与开机自启](#a3-后台常驻与开机自启cli-原生推荐) 的 `clawmessenger start`，不要把它挂在终端窗口里跑。

上线后即可：

| 场景 | 做法 |
|------|------|
| **单聊** | 用虾说 App（或任意融云客户端）向该节点发消息 → CLI 调本机 agent → 回复直达 App |
| **多节点讨论** | 管理员建聊天室并把该节点加为成员（见第三、四步）→ 启动讨论 → 节点按发言顺序自动参与 |

**行为说明**

- 单聊按会话串行（并发 1、队列深 1）；agent 忙碌时对端收到 `正在思考中…`
- agent 调用：claude 用 `-p`、codex 用 `exec`、opencode 用 `run` 子命令（argv 模式）；超长 prompt（>8000 字符）自动回退 STDIN
- 讨论回合超时 120s 自动跳过（事件日志记 `turn_skipped`），不影响其他节点
- token 过期自动刷新重连，无需人工干预
- **supervisor 模式（不带 `--agent`）**：一个父进程托管本设备**全部**已绑定 agent，每个 agent 起一个子进程（融云 SDK 是进程级单例，一个 IM 用户必须独占一个进程）。子进程异常退出会按 1s → 2s → 4s → 8s → 16s 退避重启，连续 5 次后转入「5 分钟一次」的慢车道；存活满 60s 视为健康、退避归零
- 管理端可以**单独停/起某个 agent**（服务端存期望状态，supervisor 每 10s 轮询收敛）。被人工停掉的 agent 不会被上面的自愈逻辑重新拉起，这是刻意设计
- 启动时会为本设备自动绑定内置的 `ops`（运维）agent，因此子进程数通常比你在管理端手工勾选的 agent 多一个

**常用命令**

```bash
clawmessenger agents            # 列出本机可发现的 agent CLI（下文以 clawmessenger 指代 exe 或 node 运行方式）
clawmessenger status            # 身份/token/服务器 + 后台运行状态（v0.2.15+）
clawmessenger login             # 手动刷新 IM token（一般不需要）
clawmessenger start / stop      # 后台常驻起停（v0.2.15+，见 A.3）
clawmessenger logs              # 看后台日志尾部（v0.2.15+）
```

### A.3 后台常驻与开机自启（CLI 原生，推荐）

`clawmessenger run` 只有前台模式，而且 supervisor 只处理 SIGINT / SIGTERM（**不处理 SIGHUP**）：关掉终端、注销都会让这台设备上的全部 agent 一起下线。CLI 自 **v0.2.15** 起自带一组托管命令解决这件事 —— **不需要额外下载任何脚本**：

| 命令 | 作用 |
|------|------|
| `clawmessenger start` | 后台启动 supervisor（脱离终端；关窗、注销都不掉线） |
| `clawmessenger stop` | 停止 supervisor 及它拉起的全部 agent 子进程 |
| `clawmessenger restart` | 停掉再后台启动（换过二进制版本后用这个） |
| `clawmessenger status` | 身份 + 后台运行状态（PID、已运行时长、agent 列表、exe 路径、日志位置、自启配置） |
| `clawmessenger logs` | 看 supervisor 日志尾部（`--lines <n>`，默认 40 行） |
| `clawmessenger install-service` | 配置开机 / 登录自启（Linux systemd、macOS LaunchAgent、Windows 登录启动） |
| `clawmessenger uninstall` | 取消自启（不删二进制、不删日志） |

```bash
clawmessenger start                       # 后台常驻，最常用
clawmessenger status                      # PID / agent 列表 / 日志位置 / 自启配置
clawmessenger logs --lines 100            # 看日志
clawmessenger restart                     # 换过二进制版本后
clawmessenger stop                        # 停止（含它拉起的全部 agent）
clawmessenger install-service             # 顺手配上开机自启
```

`clawmessenger daemon <动作>` 与上表**完全等价**，供偏好命名空间的场景使用。

可选参数（跟在 `start` / `restart` / `install-service` 后面）：

| 参数 | 说明 |
|------|------|
| `--model <名称>` | 透传给 `clawmessenger run --model <名称>` |
| `--workdir <路径>` | supervisor 的工作目录，也是 agent 轮次的 cwd（默认 = 家目录） |

> **Common mistake:** 如果你敲 `clawmessenger start` 得到 `error: unknown command 'start'`，说明手上的二进制**旧于 v0.2.15** —— 这组托管命令是 v0.2.15 才编进 CLI 的（更早的版本只能靠下面 A.4 / A.5 的随包脚本）。CLI 的六个**基础**命令一直是 `register` / `pair` / `login` / `agents` / `run` / `status`。

#### 平台差异

| 平台 | `start` 怎么脱离终端 | `install-service` 写什么 |
|------|----------------------|--------------------------|
| Linux | detached + 独立会话（`setsid` 等价语义） | systemd unit：root → `/etc/systemd/system/clawmessenger.service`；普通用户 → `~/.config/systemd/user/clawmessenger.service`（并尝试 `enable-linger`，否则注销即停） |
| macOS | detached（会话脱离） | `~/Library/LaunchAgents/com.clawmessenger.supervisor.plist`（`RunAtLoad` + `KeepAlive`） |
| Windows | detached + 隐藏窗口，**不占控制台** | 「启动」文件夹里的 `clawmessenger.vbs`（登录时隐藏窗口拉起，**免管理员**） |

#### 状态文件与日志

| 内容 | 路径 |
|------|------|
| supervisor PID | `~/.clawmessenger/supervisor.pid` |
| 运行详情（PID / 启动时间 / agent 列表 / 工作目录 / 模型） | `~/.clawmessenger/supervisor.json` |
| 日志 | `~/.clawmessenger/logs/supervisor.log`（超过 5 MB 自动轮转成 `supervisor.log.1`） |

`status` 读的就是这两个文件，**不要手改它们**。进程已经不在、文件却还在时会被判为 stale，`stop` 会顺手清掉。

#### 四条硬约束

1. **必须用 `pair` 时的那个用户运行。** 凭据在 `$HOME/.clawmessenger/`；用 root 跑而 `pair` 用的是普通用户，会**找不到凭据**，或注册出一批**全新的节点**。
2. **停止一律用 `clawmessenger stop`。** 别 `kill -9`：SIGKILL 跳过 supervisor 的优雅关闭，它拉起的 agent 会变成孤儿（现象：`status` 说没在运行，管理端设备仍在线）。`stop` 先 POSIX SIGTERM（约 15 秒还没退才升级 SIGKILL），Windows 上改用 `taskkill /T /F` 收整棵树。
3. **`install-service` 写进 systemd unit / plist 的 PATH 取自执行安装时的环境。** systemd 默认 PATH 很简，而 agent CLI（opencode / codex / claude）常装在 nvm 等非标准位置；换过安装位置或新增 agent CLI 之后要**重新执行一次 `install-service`**。若 `status` 列出的 agent 比管理端勾选的少，先查这里。
4. **用装好的那个 `clawmessenger` 启动。** `start` 拉起的 supervisor 记的是**当时的可执行文件路径**。首次运行会自动把自己复制到 `~/.local/bin/clawmessenger`，所以请用 PATH 里的 `clawmessenger start`，而不是还躺在「下载」目录里的 `./clawmessenger-linux-x64 start` —— 否则以后删掉那个下载文件，supervisor 就再也起不来了。`status` 会打印它实际用的是哪个 exe。

另外，`stop` 之后设备不会立刻在管理端离线：子进程心跳停止后约 **90 秒**才判定离线。

#### 更新可执行文件版本之后

```bash
clawmessenger restart            # 新进程会用新二进制
clawmessenger install-service    # 装过自启的还要重跑一次：unit / plist 里写的是绝对路径与旧 PATH
```

> **Note:** Windows 上如果之前用 `clawmessenger-daemon.cmd install-startup` 配过自启，`clawmessenger install-service` 会**覆盖同一个** `Startup\clawmessenger.vbs` —— 旧版经 `powershell → ps1 → exe` 三层，新版直接拉起 exe，少两层、也不再依赖脚本文件。

### A.4 备选：Windows 守护脚本

> **Note:** 这一节是 v0.2.13 之前的做法（随 Release 分发 PowerShell 脚本）。**新部署请直接用 [A.3](#a3-后台常驻与开机自启cli-原生推荐)**；只有在无法升级到 v0.2.15 及以上二进制、或已有依赖这份脚本的运维流程时才需要它。

`clawmessenger run` 只提供前台模式，CLI 里**没有** `--daemon` 或注册为服务的参数。仓库在 `packages/xiachat-cli/scripts/win/` 下配套了两个脚本，把 supervisor 放到后台常驻：

| 文件 | 用途 |
|------|------|
| `clawmessenger-daemon.ps1` | 主体（启动/停止/状态/日志/自启） |
| `clawmessenger-daemon.cmd` | 薄包装，方便在 cmd 里直接调用 |

> **Note:** 发布流水线会额外把这两个文件打包成 `clawmessenger-windows-x64-daemon-scripts.zip` 附在 Release 资产里（见 `.github/workflows/clawmessenger-release.yml`），下载后解压到 exe 同目录即可；若你手上的 Release 里还没有这个 zip，直接从本仓库 `packages/xiachat-cli/scripts/win/` 取同样两个文件，并保持它们在同一目录。执行过一次 `install-startup` 或 `install` 后，脚本会自动复制一份到 `%LOCALAPPDATA%\clawmessenger\`（连同 `.cmd`），之后用安装目录里的副本即可，不再依赖仓库位置。

#### 命令

| 动作 | 作用 |
|------|------|
| `start` | 后台启动 supervisor（隐藏窗口、**独立隐藏控制台**，关掉父窗口不受影响） |
| `stop` | 停止 supervisor 及其全部 agent 子进程 |
| `restart` | 停掉再后台启动 |
| `status` | 看 exe 路径、自启配置、PID、已运行时长、agent 子进程列表 |
| `logs` | 打印最新一份日志的末尾 |
| `run` | 前台阻塞运行（**供计划任务 / 启动文件夹内部调用**，不建议手动用） |
| `install-startup` | 配置登录静默自启（写 .vbs 到「启动」文件夹，**不需要管理员**） |
| `install` | 配置计划任务自启（登录常驻 + 异常退出每分钟自动重试，**需要管理员**） |
| `uninstall` | 取消自启配置（不删 exe、不删日志） |

```bat
# 后台启动（最常用；关掉这个窗口也不掉线）
%LOCALAPPDATA%\clawmessenger\clawmessenger-daemon.cmd start

# 查看状态 / 停止 / 看日志
%LOCALAPPDATA%\clawmessenger\clawmessenger-daemon.cmd status
%LOCALAPPDATA%\clawmessenger\clawmessenger-daemon.cmd stop
%LOCALAPPDATA%\clawmessenger\clawmessenger-daemon.cmd logs

# 配置登录自启（二选一）
%LOCALAPPDATA%\clawmessenger\clawmessenger-daemon.cmd install-startup   # 免管理员
%LOCALAPPDATA%\clawmessenger\clawmessenger-daemon.cmd install           # 需管理员 PowerShell
```

> 上面是 cmd 写法；在 PowerShell 里把 `%LOCALAPPDATA%` 换成 `$env:LOCALAPPDATA`。

不用 `.cmd` 也可以直接调 `.ps1`（`.cmd` 内部就是这么做的）：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File "$env:LOCALAPPDATA\clawmessenger\clawmessenger-daemon.ps1" -Action start
```

可选参数（追加在动作后面即可，例如 `... start -Model gpt-5`）：

| 参数 | 说明 |
|------|------|
| `-Model <名称>` | 透传给 `clawmessenger run --model <名称>` |
| `-Exe <路径>` | 显式指定 exe（默认从 PATH / `%LOCALAPPDATA%\clawmessenger` 找） |
| `-WorkDir <路径>` | supervisor 工作目录，同时是 agent 轮次的 cwd（默认 = exe 所在目录） |
| `-TaskName <名称>` | 计划任务名，默认 `ClawMessenger Agents` |

#### 状态文件与日志

| 内容 | 路径 |
|------|------|
| supervisor PID | `%USERPROFILE%\.clawmessenger\supervisor.pid` |
| 运行日志 | `%USERPROFILE%\.clawmessenger\logs\supervisor-<yyyyMMdd-HHmmss>.out.log` / `.err.log` |
| 自启项（免管理员） | `%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\clawmessenger.vbs` |
| 自启项（计划任务） | 任务计划程序中的 `ClawMessenger Agents` |

每次启动都用带时间戳的新日志文件，不会覆盖历史（IM SDK 会把重连、心跳、每条入站消息都写进去，排查「消息没到」还是「分发失败」靠它）。

#### 三条硬约束

1. **必须在同一个 Windows 用户下运行。** 凭据和机器 ID 在 `%USERPROFILE%\.clawmessenger\`，换成别的账号（尤其把 exe 以 SYSTEM 身份注册成服务）会**找不到凭据**，或注册出一批**全新的节点**。计划任务因此固定配置为「当前用户登录时」触发。
2. **停止一律用 `-Action stop`。** 它走 `taskkill /PID <pid> /T /F`。Windows 上没有 POSIX 信号：`Stop-Process` 走 TerminateProcess，Node 收不到 SIGTERM，supervisor 的优雅关闭和子进程清理**都不会执行**，会留下孤儿 agent 进程（现象：`status` 显示没在运行，但管理端设备仍在线）。
3. **不要同时起两个 supervisor。** 两个会重复拉起同一批 agent（IM 侧一个用户只能一个连接）。`start` 会先检测已有实例并直接拒绝（它连「手动 bat 启的前台 supervisor」也能按命令行特征认出来）；切换方式请先 `stop` 再 `start`。`run` 是自启入口，出于「登录时必须能拉起来」的考虑不做这个检查，所以别手动重复调用它。

另外，`stop` 后设备不会立刻在管理端离线：子进程心跳停止后约 **90 秒**才判定离线。

#### 更新 exe 版本之后

自启项的指向是 `%LOCALAPPDATA%\clawmessenger\clawmessenger.exe` 的**绝对路径**（不是仓库里的构建产物），所以按既有静默升级流程替换掉那个 exe 之后，执行一次：

```bat
%LOCALAPPDATA%\clawmessenger\clawmessenger-daemon.cmd restart
```

（或直接下次登录，自启会用新 exe 拉起）即可生效，不需要重新配置自启。

#### Linux / macOS

Linux / macOS 另有一份**同名同义**的守护脚本，见 [A.5](#a5-备选linux--macos-守护脚本)。

### A.5 备选：Linux / macOS 守护脚本

> **Note:** 与 A.4 同理，这是 v0.2.14 之前的做法（随 Release 分发 bash 脚本）。**新部署请直接用 [A.3](#a3-后台常驻与开机自启cli-原生推荐)** —— `clawmessenger start` / `stop` / `install-service` 已经原生覆盖了下面全部动作，不用再下载、`chmod`、也不受 GitHub 连通性影响。

仓库在 `packages/xiachat-cli/scripts/linux/` 下提供 `clawmessenger-daemon.sh`，动作名与 Windows 版**一一对应**：

| 文件 | 用途 |
|------|------|
| `clawmessenger-daemon.sh` | 主体（启动/停止/状态/日志/systemd 服务） |

> **Note:** 发布流水线会把它打包成 `clawmessenger-linux-daemon-scripts.tar.gz` 附在 Release 资产里：
>
> ```bash
> tar -xzf clawmessenger-linux-daemon-scripts.tar.gz
> chmod +x clawmessenger-daemon.sh
> ```

> **Common mistake:** `start` / `stop` / `restart` / `logs` / `install-service` **不是 `clawmessenger` 的子命令**。CLI 一共只有 `register` / `pair` / `login` / `agents` / `run` / `status` 六个命令，直接敲 `clawmessenger start` 只会得到 `error: unknown command 'start'`。下面这些动作全部由**这个脚本**（一个独立的 bash 文件，不依赖 node）提供 —— 所以脚本必须先落到磁盘上，敲的也是 `./clawmessenger-daemon.sh start` 而不是 `clawmessenger start`。

#### GitHub 下载不通时（国内网络）

GitHub Release 直链在国内经常连不上（典型报错 `curl: (28) Failed to connect to github.com port 443: Connection timed out`）。三条退路，按推荐顺序：

1. **走加速镜像**（把原链整段拼在镜像域名后面）：

   ```bash
   curl -fLO https://ghfast.top/https://github.com/clawmessenger/claw-messenger/releases/latest/download/clawmessenger-linux-daemon-scripts.tar.gz \
     || curl -fLO https://gh-proxy.com/https://github.com/clawmessenger/claw-messenger/releases/latest/download/clawmessenger-linux-daemon-scripts.tar.gz \
     || curl -fLO https://ghproxy.net/https://github.com/clawmessenger/claw-messenger/releases/latest/download/clawmessenger-linux-daemon-scripts.tar.gz
   ```

   这些镜像可用性会变，失败就换下一个域名；也可以在**有外网的机器**上下载好，再 `scp` 到目标服务器。

2. **不装脚本，先手工放后台**（`setsid` + `nohup` 让进程脱离终端，关窗、`exit`、注销都不掉线）：

   ```bash
   mkdir -p ~/.clawmessenger/logs
   cd ~ && setsid nohup clawmessenger run >>~/.clawmessenger/logs/supervisor.out.log 2>&1 </dev/null &
   ```

   停止：`pkill -f 'clawmessenger run'`（一并停掉它拉起的 agent 子进程）。看日志：`tail -f ~/.clawmessenger/logs/supervisor.out.log`。
   代价是没有 `status` / `logs` / 开机自启，且**必须用 `setsid`**——少了它，`nohup ... &` 在部分 shell 里仍会随终端退出被 HUP 掉（supervisor 只处理 SIGINT / SIGTERM，不处理 SIGHUP）。

3. **从仓库复制**：脚本源码在 `packages/xiachat-cli/scripts/linux/clawmessenger-daemon.sh`（约 530 行，纯 bash）。在能访问仓库的机器上取出内容、原样存成同名文件再 `chmod +x` 即可。**注意保持 LF 行尾**（别经过会把行尾改成 CRLF 的编辑器/传输方式），否则会报 `bad interpreter: /usr/bin/env bash^M`。

#### 命令

| 动作 | 作用 |
|------|------|
| `start` | 后台启动 supervisor（装了 systemd 服务时走 `systemctl start`，否则 `setsid` + `nohup` 脱离终端） |
| `stop` | 停止 supervisor 及其全部 agent 子进程（先 SIGTERM 优雅退出，约 15s 后仍存活才升级 SIGKILL） |
| `restart` | 停掉再后台启动 |
| `status` | 看 exe 路径、自启配置、systemd 服务状态、PID、已运行时长、agent 子进程列表 |
| `logs` | 看日志（装了 systemd 服务时读 `journalctl`，否则读日志文件末尾） |
| `run` | 前台阻塞运行（**供 systemd 内部调用**，不建议手动用） |
| `install-service` | 安装 systemd 服务（开机自启 + 崩溃自动重启；root 写系统级 unit，非 root 写 `--user` unit 并尝试 `enable-linger`） |
| `uninstall` | 移除 systemd 服务（不删可执行文件、不删日志） |

```bash
# 后台启动（最常用；关掉这个终端也不掉线）
./clawmessenger-daemon.sh start

# 查看状态 / 停止 / 看日志
./clawmessenger-daemon.sh status
./clawmessenger-daemon.sh stop
./clawmessenger-daemon.sh logs

# 配置开机自启（推荐：交给 systemd，崩溃自动重启）
sudo ./clawmessenger-daemon.sh install-service    # root：系统级服务
./clawmessenger-daemon.sh install-service         # 非 root：--user 服务（会尝试 enable-linger）
```

可选参数（追加在动作后面，例如 `./clawmessenger-daemon.sh start --model gpt-5`）：

| 参数 | 说明 |
|------|------|
| `--model <名称>` | 透传给 `clawmessenger run --model <名称>` |
| `--exe <路径>` | 显式指定可执行文件（默认从 PATH / `~/.local/bin` / `/usr/local/bin` / `/usr/bin` 找） |
| `--workdir <路径>` | supervisor 工作目录，同时是 agent 轮次的 cwd（默认 = `$HOME`） |
| `--name <服务名>` | systemd 服务名，默认 `clawmessenger` |

#### 状态文件与日志

| 内容 | 路径 |
|------|------|
| supervisor PID | `~/.clawmessenger/supervisor.pid` |
| 运行日志（非 systemd 模式） | `~/.clawmessenger/logs/supervisor-<yyyyMMdd-HHmmss>.out.log` / `.err.log` |
| 日志（systemd 模式） | `journalctl -u clawmessenger -f`（非 root 的 `--user` 服务加 `--user`） |
| 自启项 | `/etc/systemd/system/clawmessenger.service` 或 `~/.config/systemd/user/clawmessenger.service` |

#### 三条硬约束

1. **必须用 `pair` 时的那个用户运行。** 凭据和机器 ID 在 `$HOME/.clawmessenger/`。用 root 跑而 `pair` 用的是普通用户，会**找不到凭据**，或注册出一批**全新的节点**。
2. **停止一律用 `stop`。** 别用 `kill -9`：SIGKILL 会跳过 supervisor 的优雅关闭，它拉起的 agent 会变成孤儿（现象：`status` 显示没在运行，但管理端设备仍在线）。`stop` 先发 SIGTERM，超时才升级到 SIGKILL，最后还会扫一遍残留的 agent 子进程收尾。
3. **`install-service` 写进 unit 的 PATH 取自执行安装时的环境。** systemd 默认 PATH 很简，而 agent CLI（opencode / codex / claude）常装在 nvm 等非标准位置；换过安装位置或新增 agent CLI 之后要**重新执行一次 `install-service`**。若 `status` 列出的 agent 子进程比管理端勾选的少，先查这里。

另外，`stop` 后设备不会立刻在管理端离线：子进程心跳停止后约 **90 秒**才判定离线。

#### 更新可执行文件版本之后

unit 里的 `ExecStart` 是**绝对路径**，所以按既有静默升级流程替换掉那个文件之后，执行一次：

```bash
sudo ./clawmessenger-daemon.sh restart      # 等价：sudo systemctl restart clawmessenger
```

#### macOS

macOS 没有 systemd，**脚本版的** `install-service` 不可用；`start` / `stop` / `restart` / `status` / `logs` / `run` 可用（没有 `setsid`，脚本自动退化成 `nohup` + `disown` 脱离终端）。要开机自启请自行写 launchd 的 `~/Library/LaunchAgents/<label>.plist`（`RunAtLoad` + `KeepAlive`），`ProgramArguments` 填 `<可执行文件> run` —— 或者直接用 [A.3](#a3-后台常驻与开机自启cli-原生推荐) 的 `clawmessenger install-service`，CLI 会帮你把这份 plist 写好并 `launchctl load`。

### A.6 验证与排障

- 运行日志的 `im in: type=... from=...` 行标记每条入站消息，用于区分"消息未达"与"分发失败"。
- 单聊不通：先查 `clawmessenger agents` 是否列出了目标 agent；再看日志有无 `im in:`（无 = IM 未连上，检查 token/网络）。
- 讨论只有 `turn_skipped` 没有 `turn_completed`：回包需经融云 webhook 回服务器，确认融云控制台 webhook 已配置为公网可达地址（见 smoke.md「C1 修复后复验」）。
- 注册/绑定报 `workspace attribution required`：票据无效或已过期，在「远程设备」页重新点「绑定远程设备」获取新票据。
- **关掉终端窗口后设备整体离线**：说明当时用的是 A.2 的前台方式（`clawmessenger run` 挂着那个窗口）。改走 [A.3](#a3-后台常驻与开机自启cli-原生推荐) 的后台方式：`clawmessenger start`，想让它在重启后自动回来再加 `clawmessenger install-service`。
- **敲 `clawmessenger start` 报 `error: unknown command 'start'`**：手上的二进制旧于 **v0.2.15**。这组托管命令是 v0.2.15 才编进 CLI 的；升级到该版本及以上即可（在那之前只能用 A.4 / A.5 的随包脚本）。
- **`status` 说没在运行，但管理端设备仍显示在线**：典型的孤儿 agent —— 父 supervisor 被强杀（任务管理器结束进程、直接关窗口、`kill -9`），子进程还活着。`clawmessenger stop` 会清掉残留（POSIX 上还会给残留的 agent 子进程补一次 SIGTERM），然后重新 `start`。
- **`clawmessenger start` 打印「supervisor exited immediately after start」**：它会顺手把日志尾部贴出来，按那段日志定位即可 —— 最常见的是**这台设备还没绑定**（`no credentials; run clawmessenger register or pair first`）或**没有勾选任何 agent**。失败时 pid / 状态文件会被自动清掉，不会留下假象。
- **配了自启但重启后没起来**：`clawmessenger status` 会打印已配置的自启项与路径。Windows 免管理员方式是「启动」文件夹里的 `clawmessenger.vbs`；Linux 用 `systemctl status clawmessenger`（非 root 用户的服务加 `--user`）看上次退出码；macOS 用 `launchctl list | grep clawmessenger`。
- **`status` 列出的 agent 比管理端勾选的少**：`start` 成功后 `status` 会列出 supervisor 报上来的 agent 名字。绑定了但本机 PATH 里没有对应 CLI 的 agent 会被跳过，日志里有 `agent CLI <name> not found on PATH; skipping`。Linux / macOS 上是自启模式时，多半是 unit / plist 里的 PATH 过时 —— 重新执行一次 `clawmessenger install-service`（见 [A.3](#a3-后台常驻与开机自启cli-原生推荐) 第 3 条硬约束）。
- **`status` 显示 running 但 agents 是 unknown**：supervisor 刚起、还没把状态写出来（要先连 IM、拉绑定列表）。等几秒再 `clawmessenger status`；一直是这样就看 `clawmessenger logs`。
- **Linux 上 `systemd` 服务起不来**：`systemctl status clawmessenger` 看报错，然后 `systemctl show clawmessenger -p LoadState -p LoadError` 能拿到 systemd 藏起来的那句真话。常见三种：① `install-service` 打印 `Unit clawmessenger.service has a bad unit file setting.` —— **v0.2.15 及更早**生成的 unit 把 `WorkingDirectory=` 值加了引号；systemd 只对 `Environment=` / `ExecStart=` 做去引号处理，`WorkingDirectory=` 会把引号当字面量，于是 `"/root"` 这条"路径"不是绝对路径，unit 被记成 `bad-setting`，`systemctl start` 只报那一句。升级到 v0.2.16+ 重跑 `install-service`；不升级也行，手工去掉那一行的引号即可：`sed -i '/^WorkingDirectory=/ s/"//g' /etc/systemd/system/clawmessenger.service`，再 `systemctl daemon-reload`，然后 `clawmessenger stop`（收掉手工起的 supervisor，别留两份）→ `systemctl enable --now clawmessenger`。**v0.2.16 起 `install-service` 会先用 `systemd-analyze verify` 校验，unit 不合法就直接返回 —— 不停 supervisor、不 enable，设备不会因为你装自启而掉线。** ② `ExecStart` 里的绝对路径已失效（换过二进制但没重跑 `install-service`）。③ `HOME` 指向的用户下没有 `~/.clawmessenger/credentials.json`（服务用户和 `pair` 用户不是同一个）。
- **用自定义进程管理器（supervisord 等）托管时 `clawmessenger status` 认不出**：`status` 只认 `clawmessenger start` / 前台 `run` 写下的 pid / 状态文件。用别的管理器时它不写这两个文件，请用 `ps -ef | grep clawmessenger` 直接确认，**管理端的在线状态才是最终依据**。
- **日志里出现 `reg.exe` 被安全策略/黑名单拦截**：无害噪音（exe 每次启动用它自查用户 PATH），不影响运行。
- 其余已知事项见 smoke.md「已知缺口」一节。
