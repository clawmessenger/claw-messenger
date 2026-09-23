# 融云 Channel 适配器设计文档

日期：2026-09-23
状态：已批准（设计阶段完成）
项目：虾说业务对接 claw-messenger（Multica）— 子项目 1：融云 Channel 适配器

## 背景

虾说是一个业务平台（圆桌讨论 Discussion、聊天室管理 Chatroom、IoT 设备控制 Device、节点模型目录 NodeModelCatalog），其全部交互层依托融云 IM。此前自建后端将被 claw-messenger（Multica，开源）替代，约束为**尽量不修改源码**，以便官方更新时可轻松合并。

以下决策已与用户全部确认：

1. 融云作为 Channel 适配器接入（参照 Lark/Feishu 模式）。
2. 保留虾说 AI 节点模型 — 每个 AI 角色/agent 是独立融云 IM 用户（nodeId）。
3. 全部保留虾说业务模块：Discussion、Chatroom、Device、NodeModelCatalog。
4. 虾说 system 节点业务逻辑嵌入融云 Channel 适配器内部。
5. 保留虾说独立前端（web React+Vite + uniapp Vue）；claw-messenger 作为后端平台。
6. 所有消息交互全部通过融云 IM 直连（前端 ↔ 融云 ↔ 后端），不存在服务器直连前端的 HTTP 消息通道。
7. AI 节点通过 bridge 包注册并交互（子项目 2，另出 spec）。
8. 在上游 claw-messenger 上做扩展，不使用 quukk fork 的代码；fork 仅作协议和业务逻辑参考。
9. 服务器端 Go 适配器 + AI 节点独立 Node.js bridge 包。
10. 接受最小源码修改：仅在 Go 后端新建 handler 文件并注册路由（加 import + 路由注册行）。
11. bridge 包参照上游 monorepo 结构和约定重写。
12. 项目已分解；本 spec 覆盖子项目 1：融云 Channel 适配器（Go 服务端）。
13. 连接模型：Webhook 接收 + REST Server API 发送，无长连接。

## 范围

**MVP**：消息收发骨架 + command → command_result 基本 RPC 回环（ping）。

**后续**（另出 spec/迭代）：Discussion、Chatroom、Device、NodeModelCatalog 全量业务逻辑；bridge 包；前端适配。

## 架构

适配器是 Go 包，位于 `server/internal/integrations/rongcloud/`，注册到 channel Registry。

```
虾说前端（融云 SDK）
    │ command 消息（RC:CmdMsg）
    ▼
融云 IM Server
    │ webhook 推送
    ▼
rongcloudChannel（Go，claw-messenger server 内）
    ├── webhook.go    → 解析 + 验签
    ├── inbound.go    → 归一化为 channel.InboundMessage
    ├── system_handler.go → command 解析、service 分发
    ├── outbound.go / client.go → 融云 Server API（REST）
    ▼
融云 IM Server → command_result → 前端
```

### 与核心的关系

- 适配器调用 `Register("rongcloud", factory)` 注册到 `channel.Registry` — 这是文档化的扩展点（注释原话：「Adding a platform is 'register a factory here', never 'edit the core'」）。
- 归一化后的普通文本消息进入核心消息路由（claw-messenger 原生行为，可能触发 workspace CLI agent）。
- command 消息被适配器自身的 InboundHandler 实现拦截，分发到 system_handler（见数据流）。
- 出站普通文本走标准 `channel.Send(OutboundMessage)` → 融云 REST API。command、command_result、chat_stream 等自定义消息类型通过适配器内部扩展方法发送，因为 `OutboundMessage` 只建模文本。

### 对上游合并的影响

上游更新时仅有的合并冲突：Registry 注册文件（1 行）和 Chi 路由文件（几行）— 均为 trivial re-merge。所有新代码都在新目录内。

## Channel 接口实现

现有适配器均自管理传输层（Feishu WS 长连接、Telegram long-poll、Slack Socket Mode）。融云用 webhook 模式：`Connect` 需注册 HTTP 路由。

```go
type rongcloudChannel struct {
    cfg      channel.Config
    handler  channel.InboundHandler
    client   *rongcloudAPIClient
    webhook  *webhookHandler
    logger   *slog.Logger
    done     chan struct{}
}
```

- `Type()` → `"rongcloud"`
- `Connect(ctx)` → 在 Chi router 上注册 webhook HTTP handler（`/webhooks/rongcloud/{installation_id}`），然后 `<-ctx.Done()` 阻塞。webhook handler 解析融云推送，归一化后调 `c.handler(ctx, inboundMsg)`。
- `Disconnect(ctx)` → 注销 webhook handler，`close(done)`。
- `Send(ctx, out)` → 调融云 Server API（`POST /message/system/publish.json`），支持文本和自定义消息类型。
- `Capabilities()` → `CapText | CapRichCard`（自定义消息类型通过 RichCard 扩展点）。

**Webhook 注册注入方式**：通过 `RongCloudChannelDeps` 注入 `WebhookRegistrar` 接口（封装 Chi router 的 `Route()` 注册能力），适配器不直接依赖 Chi。

### 消息归一化（inbound.go）

- `RC:TxtMsg` → `InboundMessage{Type: MsgText, Text: content}`
- 自定义消息（`RC:CmdMsg` 等 objectName）→ `InboundMessage{Type: MsgText, Raw: rawContent, CommandText: extracted}`；`Raw` 保留原始 JSON 供 system_handler 解析协议。
- senderId → `Source.SenderID`，targetId → `Source.ChatID`，conversationType（1=p2p, 3=group）→ `Source.ChatType`。

### 出站（outbound.go）

`OutboundMessage.Text` → 融云 `RC:TxtMsg`。command_result 等自定义类型通过适配器内部方法发送（system_handler 使用），不走标准 `channel.Send`。

## system 节点业务逻辑（MVP）

```
前端通过融云发 command → webhook → 归一化 → system_handler 解析 command
  → 按 service+action 路由 → 执行业务逻辑 → 通过 REST 发 command_result 回融云
  → 前端收到 command_result
```

**systemHandler** 结构：

```go
type systemHandler struct {
    client   *rongcloudAPIClient
    logger   *slog.Logger
}
func (h *systemHandler) handleCommand(ctx context.Context, msg NormalizedMessage) {
    // 解析 command content: {service, action, params, request_id}
    // switch service { case "ping": echo back }  // MVP
    // 后续: case "discussion": ... case "chatroom": ... case "device": ...
}
```

- MVP 只实现 `ping`（验证消息回环）。
- 模块布局：适配器只做路由分发，每个业务模块独立文件（`discussion.go`、`chatroom.go`、`device.go`），`system_handler.go` 做 service→handler 分发。符合上游「小而专注的单元」设计原则。

## 数据流

### command 消息往返（MVP ping）

1. 前端融云 SDK 发 command：`{objectName: "RC:CmdMsg", content: {service:"ping", action:"echo", params:{...}, request_id:"req_123"}}` → 融云 IM Server。
2. 融云 Server 推送到 `POST /webhooks/rongcloud/{installation_id}`，body：`{msgType, fromUserId, targetId, conversationType, objectName, content, ...}` → webhookHandler。
3. `normalizeInbound()` → `channel.InboundMessage{Source:{SenderID, ChatID, ChatType}, Raw: rawContent, CommandText: "echo"}` → `c.handler(ctx, inboundMsg)`（注入的 InboundHandler 回调）。
4. `system_handler.handleCommand()` 解析 Raw JSON → `{service:"ping", action:"echo", params, request_id}` → 分发 → 构造 result `{request_id, data}`。
5. `rongcloudAPIClient.sendCommandResult()` → `POST https://api.rongcloud-api.com/message/system/publish.json`，body：`{fromUserId:"system", toUserId:{sender}, objectName:"RC:CmdMsg", content:{type:"command_result", request_id, data}}` → 融云 IM Server → 前端。
6. 前端 SDK 收到 command_result → resolve sendSystemRequest 的 Promise。

### 普通文本消息

前端文本 → webhook → 归一化 → `InboundMessage{Type:MsgText}` → 核心 channel router（addressed-to-bot 逻辑可能命中）→ 可能触发 workspace CLI agent 执行（claw-messenger 原生行为，本适配器不介入）。

### command 拦截决策（方案 B — 已批准）

适配器的 InboundHandler 实现检查 Raw 内容是否包含 command 协议：是 → 走 system_handler；否 → 传递给核心下游 handler 链并 return nil。理由：`SkipAgentRun` 是核心字段，语义为「产品决定不跑 agent」；command 分发是适配器内部业务路由，不应污染核心 InboundMessage 语义。

## 错误处理

Webhook 模式不涉及长连接重连（不同于 Feishu WS / Telegram long-poll）。需处理的错误类别：

1. **Webhook 接收失败**
   - 融云对非 200 响应会重试 HTTP 请求（有限次）。
   - Handler panic → recover middleware 兜底返回 500 → 融云重试。
   - 策略：基础设施错误（DB 宕机等）→ 返回 500 让融云重试；消息解析失败（格式错误、未知 objectName）→ 返回 200 并丢弃 — 与 InboundHandler 的 fire-and-classify 语义一致。

2. **REST API 发送失败**
   - 网络超时 / 5xx → 指数退避重试（3 次：1s→2s→4s）。
   - 4xx（参数错误/鉴权失败）→ 不重试，记录日志。
   - Token 过期（融云错误码 40002 等）→ 刷新 token 后重试 1 次。
   - 最终失败 → 记录到 execution log（claw-messenger 原生日志基础设施）。

3. **system_handler 业务错误**
   - command 解析失败 → 发 command_result 带 error 字段回前端（不吞错）。
   - 业务逻辑 panic → recover + 发 error result（保证前端 RPC Promise 不悬挂）。
   - 超时（后续长流程如 Discussion）→ context cancel + 发 timeout result。

4. **Webhook 签名验证**
   - 每个 webhook 请求验证 appSecret HMAC 签名，不通过返回 401。防止伪造请求注入业务消息。

## 测试策略

### 单元测试（Go `*_test.go`）

- `inbound_test.go`：融云 webhook payload → 归一化 InboundMessage，覆盖所有消息类型（text/command/command_result/chat_stream 等）。
- `outbound_test.go`：OutboundMessage → 融云 REST 请求体构造。
- `system_handler_test.go`：command 解析 + service 路由 + ping 回环。
- `client_test.go`：REST 客户端，用 `httptest.NewServer` mock 融云 API。
- `webhook_test.go`：签名验证、重试逻辑、panic recovery。

### 集成测试

- 参照 Lark/Feishu 集成测试模式（`server/internal/integrations/lark/*_test.go`）：testserver 模拟融云 Server，端到端验证 webhook → 归一化 → handler → REST 发送回环。
- 通过 `make test` 跑全量 Go 测试。

### 手动验证（MVP 阶段）

- 配置融云测试 appKey/appSecret + webhook URL（ngrok 或内网穿透）。
- 前端发 command ping → 观察 webhook 收到 → system_handler 处理 → REST 发 command_result → 前端收到。

### 验证命令（上游约定）

- `make test`（Go 后端全量测试）
- `pnpm typecheck` / `pnpm lint`（前端/bridge 包，后续阶段）

## 路线图（子项目分解）

1. **本 spec**：融云 Channel 适配器 — 消息骨架 + command RPC 回环（ping）。
2. Bridge 包（上游 monorepo 风格独立 Node.js 包）：AI 节点注册 + 融云连接 + AI CLI 任务路由。
3. 全量业务逻辑：适配器内 Discussion、Chatroom、Device、NodeModelCatalog。
4. 前端适配（web + uniapp）对接新后端。
