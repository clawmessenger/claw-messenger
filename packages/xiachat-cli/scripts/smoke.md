# XiaChat CLI 全链路冒烟手册（spec §7.2）

按 spec §7.2 的 4 步执行：注册 → 单聊 → 讨论 → kill 超时。前置条件：后端（Postgres 已起、融云测试应用已安装、`MULTICA_RONGCLOUD_SECRET_KEY` 已配置）+ 本机真实 agent CLI。

## 0. 打包

```powershell
pnpm --filter @multica/xiachat-cli build:bin   # tsc + esbuild → dist/xiachat.bundle.js
node packages/xiachat-cli/dist/xiachat.bundle.js agents   # 列出本机 agent
```

Node 直跑 bundle 需要 Node 全局垫片（`src/browser-shim.ts` 已内置，无需手工操作）。

## 1. 注册（pairing ticket）

管理员创建配对票（SQL，工作区 `48a1d54a-5a96-4fbd-9586-6ef2a1d6bc99`）：

```sql
INSERT INTO rongcloud_pairing_session (workspace_id, ticket, status, expires_at)
VALUES ('48a1d54a-5a96-4fbd-9586-6ef2a1d6bc99', 'pt_<64hex>', 'pending', now() + interval '30 minutes');
```

节点注册（二选一）：

```powershell
# CLI（注意：当前 CLI 不发送 mac_address，同一机器重复注册空 mac 会 500 重复键）
node packages/xiachat-cli/dist/xiachat.bundle.js register --server http://localhost:8081 --ticket pt_<64hex> --agent opencode

# curl（带 mac_address，ai_type=xiachat 走 IM 路径）
curl -X POST http://localhost:8081/api/ai/register -H "Content-Type: application/json" `
  -d '{"ticket":"pt_<64hex>","ai_type":"xiachat","mac_address":"aa:bb:cc:dd:ee:77","agent":"opencode"}'
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
注意：回复完成依赖 agent 执行 —— 当前 `runAgentTurn` 仅经 STDIN 喂 prompt，真实 codex 拒绝（"stdin is not a terminal"）、opencode 交互挂起（120s 超时）。`opencode run '<prompt>'`（argv）可用。见「已知缺口」。

## 3. 讨论

两个成员（host=opencode 节点 order 1，speaker=xiachat IM 节点 order 2），管理员建 chatroom + discussion：

```powershell
curl -X POST http://localhost:8081/api/workspaces/<ws>/rongcloud/chatrooms -H "Authorization: Bearer <jwt>" -d '{...}'
curl -X POST http://localhost:8081/api/workspaces/<ws>/rongcloud/discussions -H "Authorization: Bearer <jwt>" -d '{...}'
```

验证：事件端点 `GET /api/workspaces/<ws>/rongcloud/discussions/<id>/events` 依次出现 `discussion_started → round_started → turn_started → (turn_skipped) → round_completed → discussion_ended`；CLI 收到 `im in: type=command from=system`（your_turn）。

## 4. kill 超时

杀掉 CLI（`Stop-Process <pid>`），讨论内轮到该节点 → 120s 后 `turn_skipped`（事件端点可查），讨论继续。

## 已知缺口（冒烟时发现，未在本任务修复）

1. **agent 执行 STDIN 缺口**：`runAgentTurn`（CLI）与 Go DiscussionBridge 均以 STDIN 喂 prompt；真实 codex 拒绝、opencode 交互挂起。argv 方式（`opencode run '<prompt>'`）可用。回合完成（单聊回复、讨论 turn_completed）因此标记 PENDING MANUAL。
2. **回包链路 webhook**：节点→服务端回包需融云控制台 webhook 指向本服务；localhost 不可达，需公网 URL（控制台配置）。CLI→云端→CLI 已验证。
3. **CLI 重复注册空 mac**：CLI register 不发送 mac_address，同机二次注册空 mac 触发 500（唯一键冲突）。workaround：curl 带独立 mac_address。

## 服务端缺陷（冒烟中发现并修复，见 fix(rongcloud) commit）

1. services client 凭据为空 → 融云 1002 Invalidate App-Key（client.go: ensureCreds）。
2. `GetAppKey` 用零 UUID 查安装 → `/api/config/rongcloud` 返回空 appKey（install_service.go）。
3. chatroom Config/Capabilities 为 NULL → JSONB NOT NULL 违规（chatroom_service.go）。
4. chatroomKey 缓冲区 36→32 字节 → 尾部 NUL 造成 JSONB 22P05（discussion_registry.go）。
5. emitEvent 空 content 未默认 `"{}"` → turn_started 等 23502（discussion_coordinator.go）。

## 结果

- 2026-09-29：注册 PASS；单聊投递 PASS（busy hint 收到）；讨论事件链 PASS（your_turn 下发）；kill 超时 PASS（turn_skipped）。回复完成 PENDING MANUAL（缺口 1、2）。
