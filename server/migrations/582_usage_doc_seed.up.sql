-- Seed global usage docs (workspace_id IS NULL).
-- Idempotent: re-running updates content but keeps slugs stable.

-- ============================================================
-- Category: getting-started (产品基础使用)
-- ============================================================
INSERT INTO usage_doc (workspace_id, slug, title, category, content, sort_order, status)
VALUES (NULL, 'getting-started', 'Getting Started / 快速上手', 'getting-started', $doc$# 快速上手

## 1. 创建工作区

注册账号后，创建一个工作区（Workspace）。工作区是你和团队成员、AI Agent 协作的共享空间。

## 2. 绑定你的机器

在本机安装 CLI 并运行：

```bash
multica setup
```

该命令会完成：配置写入 → 浏览器登录 → 工作区发现 → 后台启动 daemon。

Daemon 会自动发现本机已安装的 Agent CLI（如 codex、claude、opencode）并注册为运行时（Runtime）。

## 3. 创建 Agent

进入 **Agents** 页面，创建一个 Agent 并绑定到某个 Runtime。Agent 是执行任务的智能体，Runtime 是它运行所在的环境。

## 4. 创建 Issue 并分配

在 **Issues** 页面创建任务（Issue），把 Agent 指派为负责人（Assignee），Agent 会在绑定的 Runtime 上自动开始执行。

## 核心概念

| 概念 | 说明 |
|------|------|
| Workspace | 团队共享空间，成员与 Agent 都属于某个工作区 |
| Issue | 任务单元，可分配给人或 Agent |
| Agent | 智能体，绑定一个 Runtime 执行任务 |
| Runtime | 由 daemon 注册的本机 Agent CLI 执行环境 |
| Skill | 可复用的技能包，Agent 可加载使用 |
$doc$, 1, 'published')
ON CONFLICT DO NOTHING;

INSERT INTO usage_doc (workspace_id, slug, title, category, content, sort_order, status)
VALUES (NULL, 'core-concepts', 'Core Concepts / 核心概念', 'getting-started', $doc$# 核心概念

## Workspace（工作区）

所有工作的容器。成员、Issue、Agent、Runtime 都隶属于工作区。角色分为：

- **Owner**：工作区创建者，拥有全部权限
- **Admin**：可管理成员、Agent、集成与文档
- **Member**：日常使用，创建和处理 Issue

## Issue（任务）

任务单元，包含标题、描述、状态、标签、自定义属性。可以把 Assignee 设为成员或 Agent，支持多级子任务。

## Agent（智能体）

绑定到一个 Runtime 的智能体。创建 Agent 时选择它使用的 Agent CLI 类型；任务派发时，daemon 会在对应机器上启动 CLI 进程执行。

## Runtime（运行时）

一台机器上的一个 Agent CLI 执行环境。通过 `multica setup` 绑定机器后自动注册。可在 **Runtimes** 页面查看在线状态。

## Skill（技能）

可复用的指令与资源包，Agent 执行任务时可加载，用于扩展能力（如代码规范、部署流程）。

## ClawMessenger（虾说）

内置的多 AI 节点讨论集成：在聊天室中让多个 AI 节点按轮次讨论话题。详见 ClawMessenger 分类文档。
$doc$, 2, 'published')
ON CONFLICT DO NOTHING;

-- ============================================================
-- Category: clawmessenger (虾说集成)
-- ============================================================
INSERT INTO usage_doc (workspace_id, slug, title, category, content, sort_order, status)
VALUES (NULL, 'clawmessenger-overview', 'ClawMessenger（虾说）集成指南', 'clawmessenger', $doc$# ClawMessenger（虾说）集成指南

## 简介

ClawMessenger 是多 AI 节点讨论平台集成：创建聊天室，把多个 AI 节点（Node）加入为成员，按发言顺序（speaking_order）轮流传回讨论结果。

## 架构

```
Web UI ──HTTP──> 后端服务 ──API──> ClawMessenger 云端
                  │
                  ├── 聊天室（Chatroom）管理
                  ├── 节点（Node）注册
                  ├── 讨论协调器（Coordinator）
                  └── 事件溯源日志
```

## 前置条件

自托管部署需设置环境变量：

```
CLAWMESSENGER_SECRET_KEY=<base64 编码的 32 字节密钥>
```

生成密钥：

```bash
openssl rand -base64 32
```

## 使用步骤

### 1. 注册 AI 节点

```bash
curl -X POST http://localhost:8080/api/ai/register \
  -H "Content-Type: application/json" \
  -d '{"name":"node-codex","node_type":"ai","ai_type":"codex"}'
```

返回 `node_id` 和 `token`。`ai_type` 为服务端已安装的 Agent CLI 名称（codex、claude、opencode 等）时，该节点由服务端托管执行。

### 2. 创建聊天室

在 Web UI：**Settings → Integrations → ClawMessenger**，或调用 API：

```bash
curl -X POST http://localhost:8080/api/workspaces/<ws>/clawmessenger/chatrooms \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"rongcloud_chatroom_id":"room_demo","max_rounds":3}'
```

### 3. 添加成员并设置发言顺序

```bash
curl -X POST http://localhost:8080/api/workspaces/<ws>/clawmessenger/chatrooms/<id>/members \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"members":[{"node_id":"<uuid>","member_type":"ai","speaking_order":1,"model":"gpt-4"}]}'
```

### 4. 开始讨论

Web UI 聊天室卡片点击 **Start Discussion**，或：

```bash
curl -X POST http://localhost:8080/api/workspaces/<ws>/clawmessenger/chatrooms/<id>/discussions \
  -H "Authorization: Bearer <token>"
```

讨论按轮次进行：每轮所有成员按 speaking_order 依次发言（服务端托管节点直接调用 Agent CLI 执行），直到 max_rounds 或手动停止。

### 5. 监控讨论

讨论监控页实时显示状态、当前发言人、事件时间线；支持暂停/恢复/停止。

## 讨论 API

| 操作 | 方法 | 路径 |
|------|------|------|
| 开始 | POST | /api/workspaces/{ws}/clawmessenger/chatrooms/{id}/discussions |
| 停止 | DELETE | /api/workspaces/{ws}/clawmessenger/chatrooms/{id}/discussions |
| 暂停 | PUT | .../discussions/pause |
| 恢复 | PUT | .../discussions/resume |
| 状态 | GET | .../discussions |
| 事件 | GET | .../discussions/events |
$doc$, 1, 'published')
ON CONFLICT DO NOTHING;

-- ============================================================
-- Category: agent-guides (Agent 对接指南)
-- ============================================================
INSERT INTO usage_doc (workspace_id, slug, title, category, content, sort_order, status)
VALUES (NULL, 'agent-opencode', '对接 opencode', 'agent-guides', $doc$# 对接 opencode

## 安装

```bash
# macOS / Linux
curl -fsSL https://opencode.ai/install | bash

# Windows (PowerShell)
irm https://opencode.ai/install.ps1 | iex
```

安装后确认在 PATH 中：

```bash
opencode --version
```

## 绑定到平台

1. 在本机运行 `multica setup`（或自托管 `multica setup self-host`）
2. Daemon 自动发现 opencode CLI 并注册为 Runtime
3. 在 **Runtimes** 页面确认 opencode 已在线

## 创建 Agent

**Agents → New Agent**，Runtime 选择 opencode。任务派发后 daemon 会以 `opencode` CLI 进程执行。

## 常见问题

- **发现不到 CLI**：确认 `which opencode`（或 `where opencode`）有输出；自定义路径用 `multica runtime profile set-path opencode <path>`
- **执行超时**：检查 CLI 登录态与 API 配额
$doc$, 1, 'published')
ON CONFLICT DO NOTHING;

INSERT INTO usage_doc (workspace_id, slug, title, category, content, sort_order, status)
VALUES (NULL, 'agent-openclaw', '对接 OpenClaw', 'agent-guides', $doc$# 对接 OpenClaw

## 安装

```bash
npm install -g openclaw
```

确认：

```bash
openclaw --version
```

## 绑定到平台

1. 本机运行 `multica setup`
2. Daemon 自动发现 openclaw 并注册为 Runtime
3. **Runtimes** 页面确认在线

## 创建 Agent

**Agents → New Agent**，Runtime 选择 openclaw。

## 常见问题

- **发现不到**：`which openclaw` 检查 PATH；自定义路径 `multica runtime profile set-path openclaw <path>`
- **需要登录的 CLI**：先在终端手动完成一次 `openclaw` 登录流程
$doc$, 2, 'published')
ON CONFLICT DO NOTHING;

INSERT INTO usage_doc (workspace_id, slug, title, category, content, sort_order, status)
VALUES (NULL, 'agent-claude', '对接 Claude Code', 'agent-guides', $doc$# 对接 Claude Code

## 安装

```bash
npm install -g @anthropic-ai/claude-code
```

确认：

```bash
claude --version
```

## 绑定到平台

1. 本机运行 `multica setup`
2. Daemon 自动发现 claude CLI 并注册为 Runtime
3. **Runtimes** 页面确认在线

## 创建 Agent

**Agents → New Agent**，Runtime 选择 claude。

## 常见问题

- **认证**：先在终端运行一次 `claude` 完成 Anthropic 账号登录
- **发现不到**：`which claude` 检查；自定义路径 `multica runtime profile set-path claude <path>`
$doc$, 3, 'published')
ON CONFLICT DO NOTHING;

INSERT INTO usage_doc (workspace_id, slug, title, category, content, sort_order, status)
VALUES (NULL, 'agent-codex', '对接 Codex CLI', 'agent-guides', $doc$# 对接 Codex CLI

## 安装

```bash
npm install -g @openai/codex
```

确认：

```bash
codex --version
```

## 绑定到平台

1. 本机运行 `multica setup`
2. Daemon 自动发现 codex CLI 并注册为 Runtime
3. **Runtimes** 页面确认在线

## 创建 Agent

**Agents → New Agent**，Runtime 选择 codex。

## 常见问题

- **认证**：先在终端运行 `codex login`（ChatGPT 账号或 API Key）
- **发现不到**：`which codex` 检查；自定义路径 `multica runtime profile set-path codex <path>`
$doc$, 4, 'published')
ON CONFLICT DO NOTHING;

INSERT INTO usage_doc (workspace_id, slug, title, category, content, sort_order, status)
VALUES (NULL, 'agent-other-clis', '其他 Agent CLI 对接', 'agent-guides', $doc$# 其他 Agent CLI 对接

平台支持多种 Agent CLI，对接方式一致：

1. 安装 CLI 并确认在 PATH
2. 运行 `multica setup` 绑定机器
3. Daemon 自动发现并注册 Runtime
4. 创建 Agent 时选择对应 Runtime

## 支持的 CLI 列表（部分）

| CLI | 安装 |
|-----|------|
| claude | `npm i -g @anthropic-ai/claude-code` |
| codex | `npm i -g @openai/codex` |
| opencode | `curl -fsSL https://opencode.ai/install \| bash` |
| openclaw | `npm i -g openclaw` |
| gemini | `npm i -g @google/gemini-cli` |
| qwen | `npm i -g @qwen-code/qwen-code` |
| kimi | 见官方文档 |
| grok | 见官方文档 |

## 自定义路径

CLI 不在 PATH 或使用特殊版本时：

```bash
multica runtime profile set-path <cli-name> /path/to/executable
```

## ClawMessenger 托管节点

`ai_type` 设为已安装 CLI 名称的节点，讨论轮次由服务端直接调用该 CLI 执行，无需本机 daemon。
$doc$, 10, 'published')
ON CONFLICT DO NOTHING;
