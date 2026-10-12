# 运行时恢复（一键唤醒）设计 — 让「恢复运行时」真的有对端

> 目标：把设备详情里那个点了就 30 秒超时的「一键唤醒」做成真正可用的能力。
> 结论先行：这**不是"缺一个接口"**，而是设备端缺一个「可自愈、可探测的运行时边界」。
> 真正收益最大的是 **CLI supervisor 的退避重启（本地自愈，零协议）**，
> 其次是复用**已有的浏览器↔节点 P2P 命令通道**做按需探测/修复（**零服务端改动**）。
> 服务端 broker 只在需要"服务端主动巡检"时才值得做，见 §7。

---

## §1 现状与差距

### 1.1 用户看到的现象

Web 设备详情 → 一键唤醒 / 圆桌设置里唤醒离线设备 → 请求发出后**干等 30 秒**才失败。

### 1.2 根因（已实测确认）

1. **Go 兼容层从来没有 `claw` 服务。** `server/internal/integrations/rongcloud/system_handler.go:46`
   的 `switch cmd.Service` 只有 `ping / chatroom / device / discussion / ai`。
   web 的 `recoverRuntime` 走 `sendSystemRequest('claw', 'recoverRuntime', …)`，
   落进 unknown service 分支被丢弃，**永远不会有回包**，只能等 30s 超时。
   同类问题还有 `claw/start`、`claw/stop`、`claw/unbind`（unbind 已改 HTTP）。

2. **旧 Python 后端有真实现，但设备端缺对端。** `clawmessenger-server/discussion/node_requests.py:235`
   的 `start_runtime_recovery` 向节点 rc 账号发 `device_control` / `command: recover_runtime`，
   节点回 `device_control_result`，由 `message_router.py` 交给 broker 收口。
   而当前 CLI 的 `MessageDispatcher.handle`（`packages/xiachat-cli/src/im.ts:70`）
   只认三类入站：`RC:TxtMsg` / `card_action` / `command`，其中 `command` 只处理
   **模型目录请求**和 **discussion 的 your_turn** —— 没有 `device_control`、没有 `recover_runtime`。

### 1.3 已经具备的基础（这是本设计能"零服务端改动"的原因）

| 能力 | 位置 | 状态 |
|---|---|---|
| 常驻 supervisor | `packages/xiachat-cli/src/main.ts:257` `runSupervisor` | 已有，但有缺口（见 1.4） |
| agent CLI 发现 | `agents.ts:227` `discoverAgents()` + `KNOWN_AGENT_CLIS`（25 个） | 已有 |
| agent CLI 执行 | `agents.ts:270` `runAgentCommand` / `agents.ts:327` `runAgentTurn` | 已有，含超时与 stdout 截断 |
| **运行时探测** | `model-catalog.ts:63` `AGENT_MODEL_LIST` / `modelListSpec()`（`opencode models` / `openclaw models list --json`） | 已有，本质就是一次运行时健康探测 |
| **浏览器↔节点请求/响应通道** | web `node-model-catalog.ts` ↔ CLI `run.ts:322` | **已有且已验证**，见 1.5 |
| 设备在线判定 | CLI 每 30s `POST /api/claw/device/heartbeat`（`main.ts:209`）；服务端 `IsOnline` = 心跳 90s 内（`node_service.go:63`） | 已有 |

### 1.4 supervisor 的缺口（本设计的主收益点）

`main.ts:313` 的子进程退出处理只有一行日志：

```ts
child.on("exit", (code) => {
  console.error(`agent ${agent.agent} exited with code ${code}`);
});
```

**子进程挂了不会重启。** agent 节点随之掉线（心跳停），设备列表变离线，
用户必须回到那台机器上手工重跑 `clawmessenger run`。
这就是"运行时需要恢复"的最常见、也最真实的形态 —— 而它是**本地问题，应该本地自愈**。

### 1.5 已验证的现成协议通道：模型目录

这是本次设计最重要的复用对象，链路完整且已在生产使用：

```
web  sendPrivateProtocolCommand(device.sn,             // objectName = "command"
       { msg_type:'discussion_model_catalog_request',
         protocolVersion:2, requestId, timestamp })
        └─ pending Map + 8s 超时 + expectedSenderId 校验
                    ↓ 融云 P2P（不经服务端）
CLI  run.ts:322  parseModelCatalogRequest() 命中 → 绕过 SessionQueue 走快路径
        └─ model-catalog.ts 跑 `opencode models`，回 discussion_model_catalog_response
                    ↑ 融云 P2P
web  im.ts:645 → onPrivateProtocolCommand → handleNodeModelCatalogResponse()
```

三个关键性质，本设计全部继承：

1. **objectName 是 `command`**（字面量，非 `RC:CmdMsg`），**两端都已注册**（CLI `run.ts:421`、
   web `im.ts:201` 的 `CUSTOM_MESSAGE_TYPES`）→ **不需要新增消息类型**。
2. **必须走 run loop 的"快路径"**：`run.ts:320` 的注释写得很清楚 —— agent 一次回合最长 120s、
   会话队列只有 1 个等待位，走队列会让 web 的 8s 超时必然失败。
3. **回包地址是 `msg.fromUserId`**（不是 `targetId`）。

---

## §2 目标与非目标

### 目标

- **G1（最高价值）本地自愈**：agent 子进程异常退出后，supervisor 按指数退避自动重启。
  零协议、零服务端改动，直接消灭"设备莫名离线"。
- **G2 状态可观测**：能查到某节点运行时的**真实**状态
  （`ready` / `needs_auth` / `not_found` / `found_not_runnable`），而不是靠心跳猜在线。
- **G3 一键唤醒**：web 点「一键唤醒」后，若 agent 节点在线则**真实探测并可修复**
  （重跑 `ensureOpencodeInstalled`、重跑探测、重置模型目录缓存）；若不可达则**立刻**给出准确原因。
- **G4 零服务端改动**（Phase 1）：继续走浏览器↔节点 P2P，不新增表、不新增 HTTP 端点。

### 非目标

- **N1 不做"让已死进程起死回生"**。远端没有任何办法启动用户机器上的进程；
  进程整体挂掉只能靠 G1 的本地自愈或用户手工启动。**任何声称能远程启动的方案都是骗人的。**
- **N2 不做任意命令执行**。不引入 `path` / `args` / `shell` / `commandLine` / `workdir` 参数。
- **N3 不做 Go 侧 broker**（Phase 2 备选，见 §7）。
- **N4 不兼容旧 Python 的 `device_control` 协议**（`clawmessenger-operations-assistant` 那套）。

---

## §3 方案选型

| | 方案 A（推荐）浏览器↔节点 P2P | 方案 B 服务端 broker（移植 Python） | 方案 C 服务端代发 + webhook 收口 |
|---|---|---|---|
| 通道 | 融云 P2P 直投 | 服务端发 P2P，webhook 回包 | 服务端发，webhook 收 |
| 服务端改动 | **零** | 新表 + 2 个 HTTP 端点 + 入站路由 | 2 个 HTTP 端点 + 入站路由 |
| 覆盖「子进程已死」 | ❌（由 G1 本地自愈覆盖） | ✅（先 `CheckOnline` 判离线） | ✅ |
| 复用 catalog 通道 | ✅ 完全复用 | ❌ 重造一套 | ❌ |
| 新增组件 | 一个 web service 文件 + 一个 CLI 文件 | +broker +持久化 +定时器 | +同 B |
| 上线成本 | 一次 CLI 发版 + 一次 web 发版 | 还要重启 Go 服务（本机无 SSH，成本高） | 同 B |

**选 A，理由**：方案 A 的唯一盲区是"agent 节点离线 → 无人应答"，而这个盲区**恰好由 G1 覆盖** ——
子进程死后 supervisor 会自动把它拉起来，节点重新上线，P2P 恢复可达。
所以 **A + G1 在语义上闭合**，不需要 B/C 的复杂度。
另外 A 完全复用了已经在生产跑通的 catalog 通道形态，风险最低。

---

## §4 协议定义

**objectName 固定为 `command`**（复用，`isPersisted=false`、`isCounted=false`，两端均已注册）。
所有 `msg_type` 带 `claw_` 前缀，与既有 `discussion_*` 命名空间隔离。

### 4.1 请求（web → 节点 rc 账号 `device.sn`）

```json
{
  "msg_type": "claw_runtime_recovery_request",
  "protocolVersion": 1,
  "requestId": "6f1c0a2e-9f4b-4a7e-9c1d-3c1b2a0d9e77",
  "timestamp": 1760270000000,
  "action": "recover"
}
```

- `action`：`"probe"`（只读探测，用于状态刷新）| `"recover"`（探测 + 允许修复，用于一键唤醒）。
- **字段集合必须精确等于上述 5 个键**，多一个即整包拒（照抄 `node-model-catalog.ts:35` 的
  `allowed` 白名单做法，从协议层堵死参数注入）。
- `requestId`：UUID v4 形态，长度 ≤ 64。
- `timestamp`：`Number.isSafeInteger`。

### 4.2 响应（节点 → web，发回 `msg.fromUserId`）

```json
{
  "msg_type": "claw_runtime_recovery_response",
  "protocolVersion": 1,
  "requestId": "6f1c0a2e-9f4b-4a7e-9c1d-3c1b2a0d9e77",
  "timestamp": 1760270000431,
  "code": "ready",
  "message": "ready",
  "data": { "runtime": "ready", "worker": "online", "agent": "opencode" }
}
```

- 字段集合同样是精确白名单（6 个键 + `data` 的 3 个键）。
- `message` 只放**机器可读的短标识或安全的英文短语**，中文文案由 web 侧统一映射（§6.4），
  避免把服务端/本机细节（路径、stderr）泄露到 UI。

### 4.3 code 表

| code | 含义 | 判定依据 |
|---|---|---|
| `ready` | 运行时就绪 | 探测命令成功返回 |
| `needs_auth` | CLI 存在但未登录 / 凭证缺失 | stderr 命中 401 / unauthorized / login / api key |
| `not_found` | PATH 上找不到该 agent CLI | `access(execPath, X_OK)` 失败 |
| `found_not_runnable` | 找到但无法执行 | 探测命令超时 / 非零退出 / 输出无法解析 |
| `probe_failed` | 探测本身失败 | 未预期的异常 |
| `unsupported_agent` | 该 agent 无已知探测方式 | `modelListSpec(agentName) === null` |

### 4.4 时序与超时

- web 侧超时 **8s**（与 catalog 对齐，`node-model-catalog.ts:82` 默认值）。
- 节点侧探测上限 **5s**（`runAgentCommand` 的 `timeoutMs`）。
- 幂等：同一 `requestId` 在 60s 内只处理一次（融云会重放离线消息，
  CLI 已有 `replayDedupe`，但那是按 `messageUId`，协议层再加一层按 `requestId` 的护栏）。
- 串行：同一 `nodeId` 的 `recover` 串行执行，且两次之间最小间隔 **5s**
  （防止被刷成"反复安装 opencode"）。

---

## §5 CLI 侧设计

### 5.1 新增 `packages/xiachat-cli/src/runtime-recovery.ts`

纯函数 + 一个 prober，全部可单测：

```ts
export type RuntimeCode =
  | "ready" | "needs_auth" | "not_found"
  | "found_not_runnable" | "probe_failed" | "unsupported_agent";

export interface RuntimeRecoveryRequest { requestId: string; action: "probe" | "recover"; }

// 严格解析：字段精确白名单 + action 白名单 + requestId 形态
export function parseRuntimeRecoveryRequest(content: string): RuntimeRecoveryRequest | null;

export function buildRuntimeRecoveryResponse(
  requestId: string, code: RuntimeCode, agentName: string,
): Record<string, unknown>;

export function createRuntimeProber(opts: {
  agentName?: string;
  execPath: string;
  runCommand: (argv: string[], timeoutMs: number, maxOutputBytes: number) => Promise<string>;
  ensureRuntime?: () => Promise<void>;   // action="recover" 时才调用
}): { probe(action: "probe" | "recover"): Promise<{ code: RuntimeCode; message: string }> };
```

`probe` 的实现顺序（**从零成本到高成本，先便宜的先查**）：

1. `access(execPath, X_OK)` 失败 → `not_found`（零成本）。
2. `action === "recover"` 且 `ensureRuntime` 存在 → `await ensureRuntime()`
   （即复用 `ensureOpencodeInstalled`，幂等，失败不抛而是降级）。
3. `modelListSpec(agentName)` 为 `null` → `unsupported_agent`（不要硬编探测命令）。
4. 复用 `AGENT_MODEL_LIST` 的 argv 跑一次，`timeoutMs = 5_000`：
   - 成功且能解析 → `ready`
   - stderr 命中认证关键词 → `needs_auth`
   - 失败/超时/解析不了 → `found_not_runnable`
5. 任何未预期异常 → `probe_failed`（绝不把异常逃逸到 run loop）。

**探测成功的顺手收益**：把结果喂给 `createModelCatalogLoader`（`catalogLoader.warm()`），
这样用户点完「一键唤醒」再打开"默认模型"面板时，8s 超时就不会再打空 —— 一箭双雕。

### 5.2 接线 `run.ts`（快路径，绕过 SessionQueue）

在 `startRunLoop` 的 `opts.transport.onMessage` 里，与 `parseModelCatalogRequest` **同级**加一支：

```ts
if (msg.objectName === "command") {
  const catalogRequest = parseModelCatalogRequest(msg.content);
  if (catalogRequest) { /* 既有逻辑 */ return; }
  const recovery = parseRuntimeRecoveryRequest(msg.content);   // ← 新增
  if (recovery) {
    void dispatcher.handleRuntimeRecovery(msg, recovery, prober).catch(...);
    return;
  }
  /* 既有 discussion your_turn 逻辑 */
}
```

**为什么必须走快路径**：与 `run.ts:320` 的既有注释完全同因 ——
agent 一次回合最长 120s、会话队列只有 1 个等待位，走队列会让 web 的 8s 超时必然失败。

回包地址用 `msg.fromUserId`（与 catalog 一致），`objectName` 同为 `command`。
日志沿用既有 `im in:` 风格，加一行 `runtime recovery: action=… code=…`，便于真机 smoke 定位。

### 5.3 supervisor 退避重启（G1，建议**独立先合**）

改 `main.ts` 的 `runSupervisor`，把 `children: ChildProcess[]` 提升为受管集合：

```ts
const managed = new Map<string, {
  child: ChildProcess; restarts: number; nextDelayMs: number; timer?: NodeJS.Timeout;
}>();
let stopping = false;
```

- `child.on("exit", code)` → 若 `stopping` 为 false：指数退避重启
  **1s → 2s → 4s → … 上限 60s**；连续 5 次失败后进入 `backoff` 态，改为每 5 分钟重试一次
  （而不是无限快速重试，避免把机器打爆）。
- 日志：`[agent] exited (code=N), restarting in Xms (attempt K)`。
- `shutdown()` 先置 `stopping = true` 并 `clearTimeout` 所有重试定时器 ——
  **否则 SIGTERM 之后子进程会被自动拉起，和现有 shutdown 语义冲突**。
- 可选：`clawmessenger supervisor status` 输出每个 agent 的
  `{state, pid, restarts, lastExitCode}`，让"设备为什么离线"能在本地一句话问清楚。

这一项**不依赖任何协议**，单独上线就能消灭最常见的"设备莫名离线"。

---

## §6 Web 侧设计

### 6.1 新增 `src/services/runtime-recovery.ts`

结构**逐条对齐** `node-model-catalog.ts`（这是仓库里已验证的同款形态）：

- 模块级 `pending: Map<requestId, { nodeId, expectedSenderId, resolve, reject, timeoutId }>`
- `requestRuntimeRecovery({ nodeId, memberId, action }, timeoutMs = 8_000)`
  → `sendPrivateProtocolCommand(memberId, {…})`，超时 reject「获取运行时状态超时」
- `handleRuntimeRecoveryResponse(senderId, payload): boolean`
  → 严格解析 + `expectedSenderId` 校验 → settle
- `isPendingRuntimeRecoverySender(senderId): boolean`
- `cancelAllRuntimeRecoveryRequests(reason)`（退出登录 / 断线时清理，参照 catalog 的做法）

### 6.2 改造 `device.ts`

- `recoverRuntime(nodeId, memberId)` → 转调 `requestRuntimeRecovery({nodeId, memberId, action:'recover'})`。
- 保留**上一轮**刚加的 `RuntimeRecoveryUnavailableError`，语义收窄为
  「**IM 未连接 / 无法投递**」的降级路径（`sendPrivateProtocolCommand` 直接抛错时）。
- 新增 `probeRuntime(nodeId, memberId)` → `action:'probe'`。
- ⚠️ **调用点必须补 `memberId`**：当前 `Home.handleRecoverRuntime(device)` 和
  `RoundtableSetupDialog.wakeDevice(device)` 都只传了 `device.id`，
  而 P2P 需要目标 rc user id（= `device.sn`，见 `Home.tsx:1343`
  `sn: node.rongcloud_id || node.node_id`）。这是本次改造**最容易漏的一处**。

### 6.3 分发接线（最容易出"消息到了但没分发"）

web 的讨论入口是**白名单式**的（`isDiscussionSystemPayload` + `expectedGeneration` +
node-sender 三重校验）。新 msg_type 必须在两处加同款豁免，完全照抄
`discussion_model_catalog_response` 的既有特判：

- `im.ts:645`（`handleDiscussionImMessage` 末尾）：加
  `if (payload.msg_type === 'claw_runtime_recovery_response') { callbacks.onPrivateProtocolCommand?.(…); return true }`
- `im.ts:661`（`enqueueDiscussionImMessage` 的 node-sender 白名单）：把新 msg_type 一并放行
- `Home.tsx:970`：`onPrivateProtocolCommand` 回调里串上 `handleRuntimeRecoveryResponse`，
  并挂 `isPendingRuntimeRecoverySender` 到 `callbacks`

### 6.4 文案与 UI

- 把 `Home.handleRecoverRuntime` 与 `DeviceDetail.recover()` 里**重复的两份 code→文案表合并**
  成 `runtime-recovery.ts` 导出的一份，例如：

  | code | 文案 |
  |---|---|
  | `ready` | AI 运行时已恢复 |
  | `needs_auth` | 需要在本机完成 AI 平台登录认证 |
  | `not_found` | 未找到本机 AI 运行时，请先安装或配置路径 |
  | `found_not_runnable` | 已找到本机 AI 运行时，但当前无法执行 |
  | `probe_failed` | 运行时探测失败，请稍后重试 |
  | `unsupported_agent` | 该 Agent 暂不支持自动探测，请在本机检查 |

- `DeviceDetail.recover()` 的 catch 已改为透出 `error.message`（上一轮改动），继续沿用。
- **顺带修一个隐藏问题**：`DeviceDetail` 的「一键唤醒」区块当前**不可达** ——
  `runtimeUnavailable` 依赖 `device.runtimeState`，而 `getNodeStatus()` 只返回 `online`，
  从不给 `runtimeState`（恒 `undefined`）。见 §11 开放问题 5。

---

## §7 服务端改动

### Phase 1：**零改动**。

### Phase 2（仅在需要"服务端主动巡检 / 离线也能量"时才做）

把 Python 的 broker 移植到 Go。**Go 侧已有全部地基**，只是没接线：

| 需要的能力 | Go 现状 |
|---|---|
| 发 P2P | `client.go:156 sendPrivateMessage` ✅ 已有 |
| 收节点回包 | `node_message_handler.go:30 handleCommandResult` ✅ 已有，目前只 `dispatchToCoordinator` 给讨论用 |
| 在线判定（快速失败 `bridge_offline`） | `user_api.go:52 checkOnline` ✅ 已有 |
| 持久化 | 需新增表 `claw_node_request(request_id, kind, requester_user_id, target_node_id, target_ai_user_id, status, request_payload, response_payload, error_code, expires_at)`（沿用仓库约定：无外键、无级联） |
| HTTP | `POST /api/claw/nodes/{nodeId}/runtime-recovery` + `GET /api/claw/nodes/{nodeId}/runtime-recovery/{requestId}`（挂在既有 `claw_compat.go` 的 `clawRequireClawUser` 鉴权下，与 unbind 同款） |
| 过期回收 | 一个 ticker 把 `pending` 且 `expires_at` 已过的记录置 `timeout`（Python 是惰性判定 + `_expire_or_fail`，Go 照做即可） |

服务端发起的好处：**P2P 不依赖好友关系**，且能先 `CheckOnline` 判 `bridge_offline`。
代价：一次 Go 服务重启（本机无 SSH，成本不低）。

---

## §8 安全与边界

1. **鉴权来自好友关系**：只有与该节点存在融云好友边的用户能 P2P 投递，
   非好友根本发不到（服务端绑定时写好友边，见 `node_service.go bindAgentsToMachine`）。
2. **协议层字段白名单**：请求字段集合精确匹配，杜绝 `path` / `args` / `shell` / `commandLine` / `workdir`。
   `action` 只接受 `probe|recover` 两个字面量。
3. **副作用上限**：`recover` 只做三件事 —— 幂等的 `ensureOpencodeInstalled()`、
   跑一次 `<agent> models` 探测、重置模型目录缓存。**不写配置、不装新软件、不执行任意命令。**
4. **速率护栏**：同 `nodeId` 串行 + 最小间隔 5s；同 `requestId` 60s 内只处理一次。
5. **不泄露本机细节**：响应只回 6 个协议字段 + 3 个枚举值，不回路径、不回 stderr 原文。

---

## §9 测试与验收

### 单测

- **CLI `runtime-recovery.test.ts`**：字段精确白名单（多键拒/缺键拒）；`action` 白名单；
  坏 `requestId` 拒；六类 code 的分类各一例；探测超时归入 `found_not_runnable`；
  `unsupported_agent` 不硬编命令。
- **CLI `main.test.ts`**（supervisor）：用 fake spawn 断言
  「退出 → 1s 重启 → 再退出 → 2s 重启」的退避序列；
  `SIGTERM` 后**不再**重启；连续 5 次失败后进入 `backoff`。
- **web `runtime-recovery.test.ts`**：pending map、`expectedSenderId` 校验（错发者拒收）、
  8s 超时、坏响应拒收、`cancelAll` 清理。
- **web `device-recovery.test.ts`**（更新）：IM 未连接时降级为 `RuntimeRecoveryUnavailableError`
  且**不发任何 IM 请求**。
- **web `Home` / `RoundtableSetupDialog`**：断言新调用点**带上了 `memberId`**。

### 真机 smoke（必做）

1. 正常路径：web 点「一键唤醒」→ CLI 日志出现 `runtime recovery: action=recover code=ready`
   → web toast「AI 运行时已恢复」；**P95 应 < 1.5s**。
2. `not_found`：把 `agentExecPath` 改名 → 应得「未找到本机 AI 运行时」。
3. **自愈**：`kill` 掉 agent 子进程 → supervisor 日志 `restarting in 1000ms (attempt 1)`
   → **3s 内该节点重新出现在在线列表**。
4. 离线路径：整体 `kill` 掉 `clawmessenger run` → 点一键唤醒 → **≤ 8s** 给出准确文案（不得 30s 空转）。

### 验收标准

- 正常路径 P95 < 1.5s；任何失败路径 ≤ 8s 且文案指向真实原因。
- 全链路**不再出现 30 秒空转**。
- 表、服务端路由零新增（Phase 1）。

---

## §10 任务拆分（建议顺序）

| # | 任务 | 说明 |
|---|---|---|
| **T1** | CLI `runtime-recovery.ts` + 单测 | 纯函数，无 IO，可独立合 |
| **T2** | CLI `run.ts` 快路径接线 + 单测 | 与 catalog 同款，注意 `msg.fromUserId` |
| **T3** | **supervisor 退避重启 + 单测** | **收益最大、零协议，建议先合并单独发版** |
| T4 | web `runtime-recovery.ts` + 单测 | 逐条对齐 `node-model-catalog.ts` |
| T5 | web `im.ts` ingress 豁免 + `device.ts` 改造 + 调用点补 `memberId` + 单测 | 最容易漏分发的环节 |
| T6 | 文案合并 + `runtimeState` 探测 | 见 §11 开放问题 5，需单独评审 |
| T7 | 真机 smoke + 上线（CLI 发版 + web 发版） | Phase 1 全程零 Go 改动 |

> T1–T3 只动 CLI，可以先走完一条完整链路；T4–T5 打通前端。T3 与其余任务无依赖，可并行。

---

## §11 风险与开放问题

1. **IM 未连接时无解**：web 必须先登录融云才能发 P2P；断线时 `sendPrivateProtocolCommand` 直接抛错
   → 保持**快速失败 + 明确文案**（上一轮已实现，不要回退）。
2. **离线消息回放**：融云重连会重放离线消息（CLI 已有按 `messageUId` 的 `replayDedupe`），
   §4.4 的按 `requestId` 去重是第二道防线。
3. **重启窗口内的请求会丢**：子进程重启期间收到的 recover 请求无人应答 → 只能靠 web 超时。
   可接受，因为 G1 的自愈会在秒级把节点拉回在线。
4. **`probe` 的代价**：`opencode models` 冷启动约 10s（见 `model-catalog.ts:327` 注释），
   5s 超时会误判成 `found_not_runnable`。**开放问题**：
   - 方案 a：`probe` 只做 `access(X_OK)` + 复用已 warm 的 catalog（零成本、零误判），
     真实运行探测只在 `action:'recover'` 时做、且 web 超时放宽到 20s；
   - 方案 b：保持 5s 统一超时，接受冷启动误判。
   **倾向 a**，需要拍板。
5. **`runtimeState` 的来源语义**：目前 `getNodeStatus()` 只回 `online`。
   要填 `runtimeState`，是"每次列表刷新都对所有节点探测"（会刷出一批 P2P 请求），
   还是"只在设备详情页/用户主动刷新时探测"？**倾向后者**，需要拍板。
6. **与遗留节点共存**：若 `clawmessenger-operations-assistant`（Python 时代的 `device_control` 节点）
   还在线上，两者互不干扰 —— objectName 相同但 `msg_type` 命名空间不同，双方都是精确匹配。

---

## §12 与上一轮改动的关系

上一轮（web `a081619`）已把 `recoverRuntime` / `getRecoveryStatus` / `startNode` / `stopNode`
改成**立即失败**，消灭了 30s 空转。本设计是在那个"诚实失败"的地基上，把它变成**真的能用**：

- `device.ts` 里那几个 `Promise.reject(...)` 就是本次要替换的**唯一接缝**；
  换成 `requestRuntimeRecovery(...)` 即可，调用方签名不变。
- `RuntimeRecoveryUnavailableError` 不删，语义收窄为"IM 不可达"，作为降级路径保留。
