# XiaChat CLI 全链路冒烟手册（spec §7.2）

按 spec §7.2 的 4 步执行：注册 → 单聊 → 讨论 → kill 超时。前置条件：后端（Postgres 已起、融云测试应用已安装、`MULTICA_RONGCLOUD_SECRET_KEY` 已配置）+ 本机真实 agent CLI。

## 0. 打包

```powershell
pnpm --filter @quukk/xiachat-cli build:bin   # tsc + pack.mjs shim → dist/xiachat.bundle.js
node packages/xiachat-cli/dist/xiachat.bundle.js agents   # 列出本机 agent
```

打包为无依赖 shim（esbuild 未声明为 devDep 且离线不可加，已移除）：`xiachat.bundle.js` 直接转发到 tsc 产物 `dist/bin.js`。Node 直跑需要 Node 全局垫片（`src/browser-shim.ts` 已内置，无需手工操作）。

## 1. 注册（pairing ticket）

管理员创建配对票（SQL，工作区 `48a1d54a-5a96-4fbd-9586-6ef2a1d6bc99`）：

```sql
INSERT INTO rongcloud_pairing_session (workspace_id, ticket, status, expires_at)
VALUES ('48a1d54a-5a96-4fbd-9586-6ef2a1d6bc99', 'pt_<64hex>', 'pending', now() + interval '30 minutes');
```

节点注册（二选一；均需 pairing ticket——rongcloud 表 workspace NOT NULL，无 ticket 返回 400）：

```powershell
# CLI（自动携带稳定机器 ID 作为 mac_address，同机重注册复用 rc_user_id）
node packages/xiachat-cli/dist/xiachat.bundle.js register --server http://localhost:8081 --name xiachat-node --ai-type xiachat --pairing-ticket pt_<64hex>

# curl（显式 mac_address，ai_type=xiachat 走 IM 路径）
curl -X POST http://localhost:8081/api/ai/register -H "Content-Type: application/json" `
  -d '{"name":"xiachat-node","ai_type":"xiachat","node_type":"ai","mac_address":"aa:bb:cc:dd:ee:77","pairing_ticket":"pt_<64hex>"}'
```

验证：响应 201 含 `token`；`node ... status` 显示 token；DB 行 `rongcloud_node`（`node_id`、`rongcloud_user_id = 'rc_node_' + mac_address`）。

## 2. 单聊

对端（第二个节点，经 `POST /api/claw/refresh-token/{node_id}` 取 token）连融云后向本节点发 `RC:TxtMsg`：

```javascript
// imlib-next：init({appkey}) → connect(token) →
await imlib.sendMessage(
  { conversationType: imlib.ConversationType.PRIVATE, targetId: "rc_node_<mac>" },
  new imlib.TextMessage({ content: "hello; reply with one word." }),
);
```

验证：CLI 日志出现 `im in: type=RC:TxtMsg from=rc_node_...`；连发 3 条（queueDepth=1）第 3 条触发 busy hint，对端收到 `正在思考中…` 回包。
agent 执行：claude/codex/opencode 走 argv 模式（`-p`/`exec`/`run` 子命令，见 agents.ts buildAgentArgs），>8000 字符 prompt 自动回退 STDIN。

## 3. 讨论

两个成员（host=opencode 节点 order 1，speaker=xiachat IM 节点 order 2），管理员建 chatroom + discussion：

```powershell
curl -X POST http://localhost:8081/api/workspaces/<ws>/rongcloud/chatrooms -H "Authorization: Bearer <jwt>" -d '{...}'
curl -X POST http://localhost:8081/api/workspaces/<ws>/rongcloud/discussions -H "Authorization: Bearer <jwt>" -d '{...}'
```

验证：事件端点 `GET /api/workspaces/<ws>/rongcloud/discussions/<id>/events` 依次出现 `discussion_started → round_started → turn_started → (turn_skipped) → round_completed → discussion_ended`；CLI 收到 `im in: type=command from=system`（your_turn）。

## 4. kill 超时

杀掉 CLI（`Stop-Process <pid>`），讨论内轮到该节点 → 120s 后 `turn_skipped`（事件端点可查），讨论继续。

## 已知缺口

1. ~~agent 执行 STDIN 缺口~~ **已修复（B1）**：CLI 与 Go bridge 均改为按 agent 策略 argv 调用（claude `-p` / codex `exec` / opencode `run`），>8000 字符回退 STDIN。codex 0.144.6、opencode 1.18.33 已真机验证；claude 未在本机安装，待验证。
2. **回包链路 webhook**：节点→服务端回包需融云控制台 webhook 指向本服务；localhost 不可达，需公网 URL（控制台配置）。CLI→云端→CLI 已验证。C1 修复后（回包信封 `command_result` + 寻址 chatroom UUID），需在公网环境重跑下方「复验」。

## C1 修复后复验（需公网 webhook 环境）

前置：融云控制台 webhook 配置为 `https://<公网域名>/api/webhooks/rongcloud?inst=<installation_id>`，后端经该域名可达。

1. 单聊回复闭环：对端发 `RC:TxtMsg` → CLI argv 调 agent → 回包 → 对端收到文本（不再 PENDING）。
2. 讨论回合闭环：启动讨论 → `your_turn` 到达 CLI → argv 调 agent → 回包（`msg_type:"command_result"`，寻址 chatroom UUID）→ 事件端点出现 `turn_completed`（此前只有 `turn_skipped`）。
3. 记录：把结果追加到下方「结果」区，注明日期与环境。

## 服务端缺陷（冒烟中发现并修复，见 fix(rongcloud) commit）

1. services client 凭据为空 → 融云 1002 Invalidate App-Key（client.go: ensureCreds）。
2. `GetAppKey` 用零 UUID 查安装 → `/api/config/rongcloud` 返回空 appKey（install_service.go）。
3. chatroom Config/Capabilities 为 NULL → JSONB NOT NULL 违规（chatroom_service.go）。
4. chatroomKey 缓冲区 36→32 字节 → 尾部 NUL 造成 JSONB 22P05（discussion_registry.go）。
5. emitEvent 空 content 未默认 `"{}"` → turn_started 等 23502（discussion_coordinator.go）。

## 结果

- 2026-09-29：注册 PASS；单聊投递 PASS（busy hint 收到）；讨论事件链 PASS（your_turn 下发）；kill 超时 PASS（turn_skipped）。回复完成 PENDING MANUAL（缺口 1、2）。
- 2026-09-30：缺口 1 修复（B1 argv 模式，两侧同步）；C1 修复（回包信封/寻址）。待公网环境按「C1 修复后复验」补验 turn_completed。
