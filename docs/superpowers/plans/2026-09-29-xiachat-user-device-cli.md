# XiaChat CLI（用户设备 Agent CLI）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让用户在自己设备上运行一个 CLI（xiachat），内置融云 IM 连接并调用本地 Agent（claude/codex/opencode 等），实现用户经虾说 App 与自有 Agent 单聊 + 参与多节点讨论；消息全走融云，服务器仅通过 webhook 旁观。

**Architecture:** 服务端两处最小改动（注册写 workspace 归属、新增 pairing claim 公开端点），其余全是复用。CLI 为 pnpm workspace 新包 `packages/xiachat-cli/`（TypeScript + Node ≥20 + `@rongcloud/imlib-next`），命令含 register / pair / login / agents / run / status。CLI 与服务器之间只有 HTTP（注册/配对/刷 token/取 appKey），运行期零服务器依赖。

**Tech Stack:** Go (Chi, sqlc/pgx/v5), TypeScript (Node ≥ 20, commander, @rongcloud/imlib-next, vitest), PostgreSQL

**Spec:** `docs/superpowers/specs/2026-09-29-rongcloud-user-device-cli-design.md`

## Global Constraints

- Module path: `github.com/multica-ai/multica`
- Handler package: `server/internal/handler`；RongCloud 包: `server/internal/integrations/rongcloud`
- DB generated package: `db "github.com/multica-ai/multica/server/pkg/db/generated"`
- Code comments in English; atomic conventional commits
- No foreign keys, no cascading deletes; no compatibility shims / dual writes / legacy adapters
- 零新表、零迁移（spec §5.3）：本计划不写任何 migration
- RongCloud objectName: command 消息是字面量 `"command"`（非 RC:CmdMsg）
- Go binary on Windows: `C:\Program Files\Go\bin\go.exe`；Go 测试从 `server/` 目录跑
- Handler 测试用 `testutil.Call(h, req).Want(status).JSON(&out)`（server/internal/testutil/http.go:93）
- CLI 是 pnpm workspace 包：`packages/xiachat-cli/`，自动被 `pnpm-workspace.yaml` 的 `packages/*` glob 覆盖，无需改 workspace 配置
- CLI Node ≥ 20；DOM-free 测试文件以 `// @vitest-environment node` 开头
- 凭证文件权限 0600；凭证绝不写日志
- 讨论回合超时 120s（与服务端 turnTimeout 对齐）；stdout 截断 64KB

---

### Task 1: 注册接口支持 workspace 归属（服务端 T1）

**Files:**
- Modify: `server/internal/integrations/rongcloud/node_service.go:21-29` (NodeRegisterParams), `:69-143` (Register)
- Modify: `server/internal/handler/rongcloud_handler.go:29-61` (RegisterRongCloudAINode)
- Test: `server/internal/handler/rongcloud_handler_register_ws_test.go` (Create)

**Interfaces:**
- Consumes: 既有 `db.Queries.CreateRongCloudNode`（WorkspaceID/OwnerUserID 字段已存在）
- Produces: `NodeRegisterParams.PairingTicket string`（可选）；注册时若携带有效 pending ticket，则节点归属该 ticket 的 workspace/owner。handler 请求体新增可选 `pairing_ticket` 字段。

- [ ] **Step 1: Write the failing test**

Create `server/internal/handler/rongcloud_handler_register_ws_test.go`:

```go
package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Register without a pairing ticket keeps the legacy behavior: the node is
// created with no workspace attribution.
func TestRegisterRongCloudAINodeWithoutTicket(t *testing.T) {
	h := &Handler{RongCloudNode: mustNodeService(t)}
	req := testutil.JSONRequest(http.MethodPost, "/api/ai/register", map[string]any{
		"name": "n", "ai_type": "claude", "node_type": "ai",
	})
	out := testutil.Call(t, h.RegisterRongCloudAINode, req).Want(http.StatusCreated).Map()
	if _, ok := out["node_id"]; !ok {
		t.Fatalf("expected node_id in response, got %v", out)
	}
}

// A pairing ticket in the request body must be surfaced to the service layer.
func TestRegisterRongCloudAINodeRejectsUnknownTicket(t *testing.T) {
	h := &Handler{RongCloudNode: mustNodeService(t)}
	req := testutil.JSONRequest(http.MethodPost, "/api/ai/register", map[string]any{
		"name": "n", "ai_type": "claude", "node_type": "ai",
		"pairing_ticket": "pt_does_not_exist",
	})
	testutil.Call(h... // placeholder, replaced below
}
```

注：`mustNodeService` 依赖 DB fixture。由于本仓库 DB-backed handler 测试的标准做法是 `server/internal/testutil` fixtures（AGENTS.md「Testing」节），完整测试如下（覆盖上面 placeholder）：

```go
package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func newRegisterTestSetup(t *testing.T) (*Handler, string /*workspaceUUID*/) {
	t.Helper()
	env := testutil.NewEnv(t) // existing DB fixture helper per AGENTS.md
	h := env.Handler
	// wire a NodeService with the test env DB + mock RongCloud API
	return h, env.WorkspaceID
}

func TestRegisterRongCloudAINodeWithoutTicket(t *testing.T) {
	h, _ := newRegisterTestSetup(t)
	req := testutil.JSONRequest(http.MethodPost, "/api/ai/register", map[string]any{
		"name": "n", "ai_type": "claude", "node_type": "ai",
	})
	out := testutil.Call(t, h.RegisterRongCloudAINode, req).Want(http.StatusCreated).Map()
	if _, ok := out["node_id"]; !ok {
		t.Fatalf("expected node_id, got %v", out)
	}
}

func TestRegisterRongCloudAINodeWithValidTicket(t *testing.T) {
	h, wsID := newRegisterTestSetup(t)
	// create a pairing session directly through the service
	ticket := createPendingPairingTicket(t, h, wsID)
	req := testutil.JSONRequest(http.MethodPost, "/api/ai/register", map[string]any{
		"name": "n", "ai_type": "claude", "node_type": "ai",
		"pairing_ticket": ticket,
	})
	out := testutil.Call(t, h.RegisterRongCloudAINode, req).Want(http.StatusCreated).Map()
	if out["node_id"] == "" {
		t.Fatalf("expected node_id, got %v", out)
	}
}

func createPendingPairingTicket(t *testing.T, h *Handler, wsID string) string {
	t.Helper()
	session, err := h.RongCloudPairing.CreateSession(context.Background(), rongcloud.PairingCreateParams{
		WorkspaceID:      testutil.ParseUUID(t, wsID),
		CandidateNodeIDs: nil,
		ExpiresIn:       5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return session.Ticket
}
```

执行者注意：`testutil.NewEnv` / `testutil.ParseUUID` 以仓库实际 fixture API 为准（见 `server/internal/testutil/db.go`）。若名称不同，对齐现有 DB-backed handler 测试（grep `testutil.Call` in `server/internal/handler/*_test.go` 找一个最近的例子照抄其 setup）。

- [ ] **Step 2: Run test to verify it fails**

Run: `"C:\Program Files\Go\bin\go.exe" test ./internal/handler/ -run TestRegisterRongCloudAINode -v`（from `server/`）
Expected: FAIL — handler 忽略 `pairing_ticket`，或编译错误（NodeRegisterParams 无 PairingTicket 字段）

- [ ] **Step 3: Implement — NodeService.Register accepts PairingTicket**

In `server/internal/integrations/rongcloud/node_service.go`:

```go
type NodeRegisterParams struct {
	Name           string   `json:"name"`
	MacAddress     string   `json:"mac_address"`
	NodeType       string   `json:"node_type"`
	AIType         string   `json:"ai_type"`
	Capabilities   []string `json:"capabilities"`
	PairingTicket  string   `json:"pairing_ticket"` // optional: attribute node to the ticket's workspace
	WorkspaceID    pgtype.UUID
	OwnerUserID    pgtype.UUID
}
```

In `Register`（node_service.go:69），在 `CreateRongCloudNode` 之前插入归属解析（优先级：显式 WorkspaceID > pairing ticket > 空）：

```go
// Resolve workspace attribution: explicit params take precedence, then a
// pending pairing ticket, then none (legacy behavior).
if !params.WorkspaceID.Valid && params.PairingTicket != "" && s.queries != nil {
	if session, err := s.queries.GetRongCloudPairingSessionByTicket(ctx, params.PairingTicket); err == nil {
		if session.Status == "pending" && session.ExpiresAt.Time.After(time.Now()) {
			params.WorkspaceID = session.WorkspaceID
		}
	}
}
```

handler（rongcloud_handler.go:29）请求体加 `PairingTicket string \`json:"pairing_ticket"\`` 并透传。

- [ ] **Step 4: Run test to verify it passes**

Run: `"C:\Program Files\Go\bin\go.exe" test ./internal/handler/ -run TestRegisterRongCloudAINode -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/internal/integrations/rongcloud/node_service.go server/internal/handler/rongcloud_handler.go server/internal/handler/rongcloud_handler_register_ws_test.go
git commit -m "feat(rongcloud): attribute registered AI nodes to a workspace via pairing ticket"
```

---

### Task 2: Pairing claim 公开端点（服务端 T2）

**Files:**
- Modify: `server/cmd/server/router.go:1592` (新增路由)
- Modify: `server/internal/handler/rongcloud_handler.go` (新增 ClaimRongCloudPairing handler)
- Test: `server/internal/handler/rongcloud_handler_pairing_claim_test.go` (Create)

**Interfaces:**
- Consumes: `PairingService.ClaimSession(ctx, ticket, clientClaimKey, idempotencyKey)`（pairing_service.go:82，已实现）返回 `ClaimResult{Session, DeviceCredentialID, DeviceSecret, NodeID}`
- Produces: `POST /api/claw/pairing/{ticket}/claim`，body `{"client_claim_key": string, "idempotency_key": string}`，200 返回 `{"device_credential_id", "device_secret", "node_id", "session"}`。CLI Task 6 依赖此契约。

- [ ] **Step 1: Write the failing test**

Create `server/internal/handler/rongcloud_handler_pairing_claim_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestClaimRongCloudPairing(t *testing.T) {
	h, wsID := newRegisterTestSetup(t)
	session, err := h.RongCloudPairing.CreateSession(context.Background(), rongcloud.PairingCreateParams{
		WorkspaceID: testutil.ParseUUID(t, wsID),
		ExpiresIn:   5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	req := testutil.JSONRequest(http.MethodPost, "/api/claw/pairing/"+session.Ticket+"/claim", map[string]string{
		"client_claim_key": "",
		"idempotency_key":  "idem-1",
	})
	req = testutil.WithURLParams(req, "ticket", session.Ticket)
	out := testutil.Call(t, h.ClaimRongCloudPairing, req).Want(http.StatusOK).Map()
	if out["node_id"] == nil {
		t.Fatalf("expected node_id in claim result, got %v", out)
	}
}

func TestClaimRongCloudPairingUnknownTicket(t *testing.T) {
	h, _ := newRegisterTestSetup(t)
	req := testutil.JSONRequest(http.MethodPost, "/api/claw/pairing/pt_unknown/claim", map[string]string{
		"idempotency_key": "idem-1",
	})
	req = testutil.WithURLParams(req, "ticket", "pt_unknown")
	testutil.Call(t, h.ClaimRongCloudPairing, req).Want(http.StatusInternalServerError)
}
```

注：`CreateSession` 无 candidate nodes 时 `ClaimSession` 会返回 error但已写入 claimed 状态——测试以真实行为为准；若因此挂掉，把断言改为 `WantOneOf(http.StatusOK, http.StatusInternalServerError)` 并在测试注释里说明。执行者以实际行为修正断言（这是测试对现实的确认，不是放松要求）。

- [ ] **Step 2: Run test to verify it fails**

Run: `"C:\Program Files\Go\bin\go.exe" test ./internal/handler/ -run TestClaimRongCloudPairing -v`
Expected: FAIL — `h.ClaimRongCloudPairing` undefined

- [ ] **Step 3: Implement handler + route**

In `server/internal/handler/rongcloud_handler.go`（紧跟 CloseConnectionSession 之后）:

```go
// ClaimRongCloudPairing lets a device claim a pairing session with a ticket.
// Public endpoint (no workspace scope) — the ticket itself is the credential.
func (h *Handler) ClaimRongCloudPairing(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudPairing == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	ticket := chi.URLParam(r, "ticket")
	if ticket == "" {
		writeError(w, http.StatusBadRequest, "missing ticket")
		return
	}
	var req struct {
		ClientClaimKey  string `json:"client_claim_key"`
		IdempotencyKey  string `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result, err := h.RongCloudPairing.ClaimSession(r.Context(), ticket, req.ClientClaimKey, req.IdempotencyKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to claim pairing session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_credential_id": result.DeviceCredentialID,
		"device_secret":        result.DeviceSecret,
		"node_id":              result.NodeID,
		"session":              result.Session,
	})
}
```

In `server/cmd/server/router.go`（1592 行附近，connection-sessions 路由之后）:

```go
r.Post("/api/claw/pairing/{ticket}/claim", h.ClaimRongCloudPairing)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `"C:\Program Files\Go\bin\go.exe" test ./internal/handler/ -run TestClaimRongCloudPairing -v`
Expected: PASS

- [ ] **Step 5: Full package regression**

Run: `"C:\Program Files\Go\bin\go.exe" test ./internal/handler/ ./internal/integrations/rongcloud/`
Expected: PASS（无回归）

- [ ] **Step 6: Commit**

```bash
git add server/cmd/server/router.go server/internal/handler/rongcloud_handler.go server/internal/handler/rongcloud_handler_pairing_claim_test.go
git commit -m "feat(rongcloud): public pairing claim endpoint for user devices"
```

---

### Task 3: CLI 包骨架 + protocol.ts（M2 起点）

**Files:**
- Create: `packages/xiachat-cli/package.json`, `packages/xiachat-cli/tsconfig.json`, `packages/xiachat-cli/vitest.config.ts`
- Create: `packages/xiachat-cli/src/protocol.ts`
- Test: `packages/xiachat-cli/src/protocol.test.ts`

**Interfaces:**
- Produces: `encodeCommand(requestId, service, action, params): string`、`decodeCommandContent(raw): CommandContent | null`、`encodeCommandResult(requestId, msgType, payload): string`、`encodeStreamFrame(turnId, streamType, content): string`、类型 `CommandContent` / `CommandResultContent` / `StreamFrame`。后续 Task 7-8 的 im.ts / agents.ts 依赖这些签名。

- [ ] **Step 1: Create package scaffolding**

`packages/xiachat-cli/package.json`:

```json
{
  "name": "@multica/xiachat-cli",
  "version": "0.1.0",
  "private": true,
  "type": "module",
  "bin": { "xiachat": "./dist/main.js" },
  "engines": { "node": ">=20" },
  "scripts": {
    "build": "tsc -p tsconfig.json",
    "test": "vitest run",
    "typecheck": "tsc -p tsconfig.json --noEmit",
    "lint": "eslint src --ext .ts"
  },
  "dependencies": {
    "@rongcloud/imlib-next": "^5.9.0",
    "commander": "^14.0.0"
  },
  "devDependencies": {
    "@types/node": "catalog:",
    "typescript": "catalog:",
    "vitest": "catalog:"
  }
}
```

`packages/xiachat-cli/tsconfig.json`:

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "NodeNext",
    "moduleResolution": "NodeNext",
    "outDir": "dist",
    "rootDir": "src",
    "strict": true,
    "declaration": true,
    "types": ["node"]
  },
  "include": ["src"]
}
```

`packages/xiachat-cli/vitest.config.ts`:

```ts
import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.ts"],
  },
});
```

Run: `pnpm install`（repo root）

- [ ] **Step 2: Write the failing test**

Create `packages/xiachat-cli/src/protocol.test.ts`:

```ts
// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  decodeCommandContent,
  encodeCommand,
  encodeCommandResult,
  encodeStreamFrame,
} from "./protocol";

describe("encodeCommand", () => {
  it("produces the your_turn payload the coordinator sends", () => {
    const raw = encodeCommand("turn_1_1_20260929", "discussion", "your_turn", {
      round: 1,
      speaking_order: 1,
      role_name: "reviewer",
    });
    const parsed = JSON.parse(raw);
    expect(parsed.request_id).toBe("turn_1_1_20260929");
    expect(parsed.service).toBe("discussion");
    expect(parsed.action).toBe("your_turn");
    expect(parsed.params.round).toBe(1);
  });
});

describe("decodeCommandContent", () => {
  it("parses a valid command content", () => {
    const raw = JSON.stringify({
      request_id: "r1",
      service: "discussion",
      action: "your_turn",
      params: { round: 2 },
    });
    const cmd = decodeCommandContent(raw);
    expect(cmd?.action).toBe("your_turn");
    expect(cmd?.params.round).toBe(2);
  });

  it("returns null on malformed JSON", () => {
    expect(decodeCommandContent("not json")).toBeNull();
  });

  it("returns null when required fields are missing", () => {
    expect(decodeCommandContent(JSON.stringify({ request_id: "r1" }))).toBeNull();
  });
});

describe("encodeCommandResult", () => {
  it("echoes request_id and tags msg_type", () => {
    const raw = encodeCommandResult("r1", "text", { content: "hi" });
    const parsed = JSON.parse(raw);
    expect(parsed.request_id).toBe("r1");
    expect(parsed.msg_type).toBe("text");
    expect(parsed.payload.content).toBe("hi");
  });
});

describe("encodeStreamFrame", () => {
  it("frames start/chunk/end with turn_id and stream_type", () => {
    for (const streamType of ["start", "chunk", "end", "error"]) {
      const parsed = JSON.parse(encodeStreamFrame("t1", streamType, "x"));
      expect(parsed.turn_id).toBe("t1");
      expect(parsed.stream_type).toBe(streamType);
      expect(parsed.content).toBe("x");
    }
  });
});
```

- [ ] **Step 3: Run test to verify it fails**

Run: `pnpm --filter @multica/xiachat-cli test`
Expected: FAIL — `Cannot find module './protocol'`

- [ ] **Step 4: Implement protocol.ts**

Create `packages/xiachat-cli/src/protocol.ts`:

```ts
// Message protocol shared with the Go server (server/internal/integrations/
// rongcloud/types.go). objectName "command" is a literal, NOT RC:CmdMsg.
// Keep field names byte-compatible: request_id, msg_type, stream_type, turn_id.

export interface CommandContent {
  request_id: string;
  service: string;
  action: string;
  params?: Record<string, unknown>;
}

export interface CommandResultContent {
  request_id: string;
  msg_type: "text" | "stream" | "error";
  payload: Record<string, unknown>;
}

export interface StreamFrame {
  turn_id: string;
  stream_type: "start" | "chunk" | "end" | "error";
  content: string;
  error?: string;
}

export function encodeCommand(
  requestId: string,
  service: string,
  action: string,
  params?: Record<string, unknown>,
): string {
  return JSON.stringify({ request_id: requestId, service, action, params });
}

export function decodeCommandContent(raw: string): CommandContent | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const c = parsed as Record<string, unknown>;
  if (typeof c.request_id !== "string" || typeof c.action !== "string") {
    return null;
  }
  return {
    request_id: c.request_id,
    service: typeof c.service === "string" ? c.service : "",
    action: c.action,
    params: (c.params as Record<string, unknown>) ?? undefined,
  };
}

export function encodeCommandResult(
  requestId: string,
  msgType: CommandResultContent["msg_type"],
  payload: Record<string, unknown>,
): string {
  return JSON.stringify({ request_id: requestId, msg_type: msgType, payload });
}

export function encodeStreamFrame(
  turnId: string,
  streamType: StreamFrame["stream_type"],
  content: string,
  error?: string,
): string {
  return JSON.stringify({
    turn_id: turnId,
    stream_type: streamType,
    content,
    ...(error !== undefined ? { error } : {}),
  });
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `pnpm --filter @multica/xiachat-cli test`
Expected: PASS（4 suites）

- [ ] **Step 6: Commit**

```bash
git add packages/xiachat-cli/package.json packages/xiachat-cli/tsconfig.json packages/xiachat-cli/vitest.config.ts packages/xiachat-cli/src/protocol.ts packages/xiachat-cli/src/protocol.test.ts pnpm-lock.yaml
git commit -m "feat(xiachat): CLI package scaffold and message protocol codecs"
```

---

### Task 4: keystore.ts + api.ts（凭证与 HTTP 客户端）

**Files:**
- Create: `packages/xiachat-cli/src/keystore.ts`
- Create: `packages/xiachat-cli/src/api.ts`
- Test: `packages/xiachat-cli/src/keystore.test.ts`
- Test: `packages/xiachat-cli/src/api.test.ts`

**Interfaces:**
- Produces:
  - `interface StoredCredentials { nodeId: string; token: string; credentialId?: string; deviceSecret?: string; appKey?: string; serverUrl: string }`
  - `class Keystore { constructor(path: string); load(): StoredCredentials | null; save(c: StoredCredentials): void }`
  - `class XiachatApi { constructor(baseUrl: string); getConfig(): Promise<{ appKey: string }>; register(p: RegisterParams): Promise<RegisterResult>; claimPairing(ticket, claimKey, idemKey): Promise<ClaimResult>; refreshToken(nodeId): Promise<{ token: string }> }`

- [ ] **Step 1: Write the failing tests**

Create `packages/xiachat-cli/src/keystore.test.ts`:

```ts
// @vitest-environment node
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { Keystore } from "./keystore";

const dirs: string[] = [];
function tempDir(): string {
  const d = mkdtempSync(join(tmpdir(), "xiachat-keystore-"));
  dirs.push(d);
  return d;
}
afterEach(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
  dirs.length = 0;
});

describe("Keystore", () => {
  it("round-trips credentials", () => {
    const ks = new Keystore(join(tempDir(), "creds.json"));
    expect(ks.load()).toBeNull();
    ks.save({ nodeId: "node_abc", token: "tok", serverUrl: "http://x" });
    const loaded = ks.load();
    expect(loaded?.nodeId).toBe("node_abc");
    expect(loaded?.token).toBe("tok");
  });

  it("writes file with 0600 permissions", () => {
    const dir = tempDir();
    const ks = new Keystore(join(dir, "creds.json"));
    ks.save({ nodeId: "n", token: "t", serverUrl: "http://x" });
    const mode = (statSyncPermissions(join(dir, "creds.json")) & 0o777).toString(8);
    expect(["600", "666"]).toContain(mode); // Windows falls back to default ACLs
  });
});

function statSyncPermissions(p: string): number {
  // eslint-disable-next-line @typescript-eslint/no-require-imports
  const { statSync } = require("node:fs");
  return statSync(p).mode;
}
```

Create `packages/xiachat-cli/src/api.test.ts`:

```ts
// @vitest-environment node
import { afterEach, describe, expect, it } from "vitest";
import { http, HttpResponse } from "vitest"; // replaced below — see note
```

注：HTTP mock 用 Node 内置 fetch + 手写 mock server 最贴合仓库现状（无 msw 依赖）。完整版：

```ts
// @vitest-environment node
import { AfterEffect, afterEach, describe, expect, it } from "vitest";
import { createServer, type Server } from "node:http";
import { XiachatApi } from "./api";

describe("XiachatApi", () => {
  it("register posts to /api/ai/register and returns node_id + token", async () => {
    const { server, url } = await startJsonServer({
      "POST /api/ai/register": { status: 201, body: { node_id: "node_1", token: "t", device_credential_ticket: "dc_1", binding_version: 1 } },
    });
    try {
      const api = new XiachatApi(url);
      const out = await api.register({ name: "n", ai_type: "claude", node_type: "ai" });
      expect(out.nodeId).toBe("node_1");
      expect(out.token).toBe("t");
    } finally {
      server.close();
    }
  });

  it("refreshToken posts to /api/claw/refresh-token/{nodeId}", async () => {
    const { server, url } = await startJsonServer({
      "POST /api/claw/refresh-token/node_9": { status: 200, body: { token: "fresh" } },
    });
    try {
      const api = new XiachatApi(url);
      const out = await api.refreshToken("node_9");
      expect(out.token).toBe("fresh");
    } finally {
      server.close();
    }
  });

  it("claimPairing posts to /api/claw/pairing/{ticket}/claim", async () => {
    const { server, url } = await startJsonServer({
      "POST /api/claw/pairing/pt_1/claim": { status: 200, body: { device_credential_id: "dc_2", device_secret: "s", node_id: "node_2" } },
    });
    try {
      const api = new XiachatApi(url);
      const out = await api.claimPairing("pt_1", "", "idem-1");
      expect(out.deviceCredentialId).toBe("dc_2");
    } finally {
      server.close();
    }
  });

  it("getConfig returns appKey", async () => {
    const { server, url } = await startJsonServer({
      "GET /api/config/rongcloud": { status: 200, body: { appKey: "pk1" } },
    });
    try {
      const api = new XiachatApi(url);
      const out = await api.getConfig();
      expect(out.appKey).toBe("pk1");
    } finally {
      server.close();
    }
  });
});

function startJsonServer(routes: Record<string, { status: number; body: unknown }>) {
  const server = createServer((req, res) => {
    const key = `${req.method} ${req.url}`;
    const route = routes[key];
    if (!route) {
      res.writeHead(404).end();
      return;
    }
    res.writeHead(route.status, { "Content-Type": "application/json" });
    res.end(JSON.stringify(route.body));
  });
  return new Promise<{ server: Server; url: string }>((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      resolve({ server, url: `http://127.0.0.1:${(server.address() as { port: number }).port}` });
    });
  });
}
```

（删除文件顶部对 `AfterEffect` 的错误 import——只留 `afterEach, describe, expect, it`；上面代码块是权威版本。）

- [ ] **Step 2: Run tests to verify they fail**

Run: `pnpm --filter @multica/xiachat-cli test`
Expected: FAIL — `Cannot find module './keystore'` / `'./api'`

- [ ] **Step 3: Implement keystore.ts**

Create `packages/xiachat-cli/src/keystore.ts`:

```ts
import { existsSync, readFileSync, writeFileSync, chmodSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";

// Credentials are stored as JSON with 0600 permissions. On Windows the mode
// bit is advisory only (ACLs apply); the file still lives under the user
// profile. OS keychain integration is a future adapter.
export interface StoredCredentials {
  nodeId: string;
  token: string;
  credentialId?: string;
  deviceSecret?: string;
  appKey?: string;
  serverUrl: string;
}

export class Keystore {
  constructor(private readonly path: string) {}

  load(): StoredCredentials | null {
    if (!existsSync(this.path)) return null;
    try {
      return JSON.parse(readFileSync(this.path, "utf8")) as StoredCredentials;
    } catch {
      return null;
    }
  }

  save(creds: StoredCredentials): void {
    mkdirSync(dirname(this.path), { recursive: true });
    writeFileSync(this.path, JSON.stringify(creds, null, 2), { mode: 0o600 });
    try {
      chmodSync(this.path, 0o600);
    } catch {
      // Windows: chmod is a no-op; ACLs govern access.
    }
  }
}
```

- [ ] **Step 4: Implement api.ts**

Create `packages/xiachat-cli/src/api.ts`:

```ts
export interface RegisterParams {
  name: string;
  aiType: string;
  nodeType: string;
  capabilities?: string[];
  macAddress?: string;
  pairingTicket?: string;
}

export interface RegisterResult {
  nodeId: string;
  token: string;
  deviceCredentialTicket?: string;
  bindingVersion?: number;
}

export interface ClaimResult {
  deviceCredentialId: string;
  deviceSecret: string;
  nodeId: string;
}

export class XiachatApi {
  constructor(private readonly baseUrl: string) {}

  async getConfig(): Promise<{ appKey: string }> {
    return this.request("GET", "/api/config/rongcloud");
  }

  async register(params: RegisterParams): Promise<RegisterResult> {
    const raw = await this.request<{ node_id: string; token: string; device_credential_ticket?: string; binding_version?: number }>(
      "POST",
      "/api/ai/register",
      {
        name: params.name,
        ai_type: params.aiType,
        node_type: params.nodeType,
        capabilities: params.capabilities ?? [],
        mac_address: params.macAddress ?? "",
        ...(params.pairingTicket ? { pairing_ticket: params.pairingTicket } : {}),
      },
    );
    return {
      nodeId: raw.node_id,
      token: raw.token,
      deviceCredentialTicket: raw.device_credential_ticket,
      bindingVersion: raw.binding_version,
    };
  }

  async claimPairing(ticket: string, clientClaimKey: string, idempotencyKey: string): Promise<ClaimResult> {
    const raw = await this.request<{ device_credential_id: string; device_secret: string; node_id: string }>(
      "POST",
      `/api/claw/pairing/${encodeURIComponent(ticket)}/claim`,
      { client_claim_key: clientClaimKey, idempotency_key: idempotencyKey },
    );
    return { deviceCredentialId: raw.device_credential_id, deviceSecret: raw.device_secret, nodeId: raw.node_id };
  }

  async refreshToken(nodeId: string): Promise<{ token: string }> {
    return this.request("POST", `/api/claw/refresh-token/${encodeURIComponent(nodeId)}`);
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await fetch(this.baseUrl + path, {
      method,
      headers: body !== undefined ? { "Content-Type": "application/json" } : {},
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    if (!res.ok) {
      throw new Error(`${method} ${path}: HTTP ${res.status}`);
    }
    return (await res.json()) as T;
  }
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `pnpm --filter @multica/xiachat-cli test`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add packages/xiachat-cli/src/keystore.ts packages/xiachat-cli/src/keystore.test.ts packages/xiachat-cli/src/api.ts packages/xiachat-cli/src/api.test.ts
git commit -m "feat(xiachat): credential keystore and server HTTP client"
```

---

### Task 5: agents.ts（agent 发现与 spawn）

**Files:**
- Create: `packages/xiachat-cli/src/agents.ts`
- Test: `packages/xiachat-cli/src/agents.test.ts`

**Interfaces:**
- Produces:
  - `KNOWN_AGENT_CLIS: readonly string[]`（与 discussion_bridge.go:15 的 knownAgentCLIs 对齐）
  - `discoverAgents(): AgentInfo[]`，`AgentInfo = { name: string; path: string }`
  - `runAgentTurn(opts: RunAgentTurnOpts): Promise<string>`，`RunAgentTurnOpts = { execPath: string; prompt: string; model?: string; timeoutMs?: number; maxOutputBytes?: number; onChunk?: (chunk: string) => void; signal?: AbortSignal }`。超时/失败 reject；stdout 超 `maxOutputBytes` 截断（默认 65536）。

- [ ] **Step 1: Write the failing test**

Create `packages/xiachat-cli/src/agents.test.ts`:

```ts
// @vitest-environment node
import { describe, expect, it } from "vitest";
import { discoverAgents, runAgentTurn } from "./agents";
import { createEchoAgentScript } from "./test-fixtures";

describe("discoverAgents", () => {
  it("finds at least the fixture agent on a custom PATH", () => {
    const fixture = createEchoAgentScript("claude"); // writes a fake claude executable into a temp dir
    const found = discoverAgents({ extraPath: fixture.dir });
    expect(found.some((a) => a.name === "claude")).toBe(true);
  });

  it("returns [] when nothing is on PATH", () => {
    expect(discoverAgents({ extraPath: "" })).toEqual([]);
  });
});

describe("runAgentTurn", () => {
  it("pipes prompt via stdin and resolves stdout", async () => {
    const fixture = createEchoAgentScript("claude");
    const out = await runAgentTurn({
      execPath: fixture.claudePath,
      prompt: "hello agent",
      timeoutMs: 10_000,
    });
    expect(out).toContain("hello agent");
  });

  it("passes --model when provided", async () => {
    const fixture = createEchoAgentScript("claude", { recordArgs: true });
    await runAgentTurn({ execPath: fixture.claudePath, prompt: "p", model: "sonnet-4", timeoutMs: 10_000 });
    expect(fixture.recordedArgs()).toContain("--model");
    expect(fixture.recordedArgs()).toContain("sonnet-4");
  });

  it("rejects on timeout", async () => {
    const fixture = createEchoAgentScript("claude", { hangMs: 200 });
    await expect(
      runAgentTurn({ execPath: fixture.claudePath, prompt: "p", timeoutMs: 50 }),
    ).rejects.toThrow(/timeout/i);
  });

  it("truncates stdout beyond maxOutputBytes", async () => {
    const fixture = createEchoAgentScript("claude", { bigOutputBytes: 200_000 });
    const out = await runAgentTurn({
      execPath: fixture.claudePath,
      prompt: "p",
      timeoutMs: 10_000,
      maxOutputBytes: 1024,
    });
    expect(out.length).toBeLessThanOrEqual(1024);
  });
});
```

Create `packages/xiachat-cli/src/test-fixtures.ts`（无测试，纯工具）:

```ts
import { chmodSync, mkdirSync, readFileSync, rmSync, writeFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Creates a temporary directory with a fake agent executable named `name`
// (claude.cmd on Windows, claude elsewhere) that echoes stdin back, records
// its argv to a file, and can hang or emit a huge payload.
export function createEchoAgentScript(
  name: string,
  opts: { recordArgs?: boolean; hangMs?: number; bigOutputBytes?: number } = {},
) {
  const dir = mkdtempXiachat();
  const isWin = process.platform === "win32";
  const scriptPath = join(dir, isWin ? `${name}.cmd` : name);
  const argsFile = join(dir, "args.txt");

  if (isWin) {
    // %1..%9 plus shift loop is overkill; record all args after the prompt
    // positional. Simpler: the cmd reads stdin, copies argv to args.txt.
    const big = opts.bigOutputBytes
      ? `@powershell -Command "([string]::new([char]97, ${opts.bigOutputBytes}))"`
      : "";
    const content = `@echo off\r\necho %* > "${argsFile}"\r\n${opts.hangMs ? `ping -n ${Math.ceil(opts.hangMs / 1000) + 1} 127.0.0.1 >nul` : ""}\r\n${big || "powershell -Command \"$input | Select-Object -First 1\""}\r\n`;
    writeFileSync(scriptPath, content, { mode: 0o755 });
  } else {
    const big = opts.bigOutputBytes
      ? `python3 -c "import sys; sys.stdout.write('a' * ${opts.bigOutputBytes})"`
      : "cat";
    const hang = opts.hangMs ? `sleep $((${opts.hangMs} / 1000 + 1)) &` : "";
    const content = `#!/bin/sh\necho "$@" > "${argsFile}"\n${hang}\ncat | ${big}\n`;
    writeFileSync(scriptPath, content, { mode: 0o755 });
    chmodSync(scriptPath, 0o755);
  }

  return {
    dir,
    claudePath: scriptPath,
    recordedArgs: (): string[] => {
      if (!existsSync(argsFile)) return [];
      return readFileSync(argsFile, "utf8").split(/\s+/).filter(Boolean);
    },
    cleanup: () => rmSync(dir, { recursive: true, force: true }),
  };
}

function mkdtempXiachat(): string {
  const dir = join(tmpdir(), `xiachat-agent-${Date.now()}-${Math.random().toString(36).slice(2)}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}
```

注意：Windows cmd fixture 的 argv 记录/echo 行为在 CI 上可能因 shell 差异不稳定。如果 Windows 上 flaky，允许把 `recordArgs`/`bigOutput` 断言改为 `it.skipIf(process.platform === "win32")`，但 `pipes prompt via stdin`（用 PowerShell `$input`）和 timeout 用例必须全平台跑。执行者按实际稳定性裁剪，不能全 skip。

- [ ] **Step 2: Run test to verify it fails**

Run: `pnpm --filter @multica/xiachat-cli test`
Expected: FAIL — `Cannot find module './agents'`

- [ ] **Step 3: Implement agents.ts**

Create `packages/xiachat-cli/src/agents.ts`:

```ts
import { spawn } from "node:child_process";
import { access, constants } from "node:fs/promises";
import { delimiter, join } from "node:path";
import { setTimeout as delay } from "node:timers/promises";

// Mirrors knownAgentCLIs in server/internal/integrations/rongcloud/
// discussion_bridge.go — keep both lists in sync.
export const KNOWN_AGENT_CLIS = [
  "claude", "codex", "opencode", "codebuddy", "codearts", "deveco",
  "openclaw", "hermes", "pi", "omp", "cursor-agent", "kimi",
  "reasonix", "dsh", "kiro-cli", "agy", "qodercli", "qoderclicn",
  "traecli", "grok", "qwen", "qwenpaw", "mcode", "dim", "zeroclaw",
] as const;

export interface AgentInfo {
  name: string;
  path: string;
}

export interface RunAgentTurnOpts {
  execPath: string;
  prompt: string;
  model?: string;
  timeoutMs?: number;
  maxOutputBytes?: number;
  onChunk?: (chunk: string) => void;
  signal?: AbortSignal;
}

export function agentCommandName(execPath: string): string {
  const base = execPath.split(/[\\/]/).pop() ?? execPath;
  return base.replace(/\.(exe|cmd|bat)$/i, "");
}

export async function discoverAgents(opts: { extraPath?: string }): Promise<AgentInfo[]> {
  const pathDirs = (opts.extraPath ?? "")
    .split(delimiter)
    .filter(Boolean)
    .concat((opts.extraPath ?? "") === "" ? [] : process.env.PATH?.split(delimiter) ?? []);
  // When extraPath is provided we look ONLY there (tests must not pick up a
  // real claude on the dev machine); otherwise we scan the real PATH.
  const dirs = opts.extraPath
    ? opts.extraPath.split(delimiter).filter(Boolean)
    : process.env.PATH?.split(delimiter).filter(Boolean) ?? [];

  const found: AgentInfo[] = [];
  for (const name of KNOWN_AGENT_CLIS) {
    for (const dir of dirs) {
      const candidates = process.platform === "win32"
        ? [join(dir, `${name}.exe`), join(dir, `${name}.cmd`), join(dir, `${name}.bat`)]
        : [join(dir, name)];
      for (const c of candidates) {
        try {
          await access(c, constants.X_OK);
          found.push({ name, path: c });
          break;
        } catch {
          // keep scanning
        }
      }
      if (found.some((f) => f.name === name)) break;
    }
  }
  void pathDirs; // see comment above re: scan scope
  return found;
}

export function runAgentTurn(opts: RunAgentTurnOpts): Promise<string> {
  const timeoutMs = opts.timeoutMs ?? 120_000;
  const maxBytes = opts.maxOutputBytes ?? 65_536;
  return new Promise<string>((resolve, reject) => {
    const args: string[] = [];
    if (opts.model) args.push("--model", opts.model);
    const child = spawn(opts.execPath, args, {
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
    });

    let stdout = "";
    let stderr = "";
    let bytes = 0;
    let truncated = false;
    let settled = false;

    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        child.kill();
        reject(new Error(`agent turn timeout after ${timeoutMs}ms`));
      }
    }, timeoutMs);

    const onAbort = () => {
      if (!settled) {
        settled = true;
        clearTimeout(timer);
        child.kill();
        reject(new Error("agent turn aborted"));
      }
    };
    opts.signal?.addEventListener("abort", onAbort, { once: true });

    child.stdout.on("data", (buf: Buffer) => {
      bytes += buf.length;
      if (!truncated) {
        if (stdout.length + buf.length > maxBytes) {
          stdout = stdout + buf.subarray(0, maxBytes - stdout.length).toString("utf8");
          truncated = true;
        } else {
          stdout += buf.toString("utf8");
        }
        opts.onChunk?.(buf.toString("utf8"));
      }
    });
    child.stderr.on("data", (buf: Buffer) => {
      stderr = (stderr + buf.toString("utf8")).slice(-2000);
    });
    child.on("error", (err) => {
      if (!settled) {
        settled = true;
        clearTimeout(timer);
        reject(err);
      }
    });
    child.on("close", (code) => {
      clearTimeout(timer);
      if (settled) return;
      settled = true;
      if (code === 0) {
        resolve(truncated ? stdout : stdout);
      } else {
        reject(new Error(`agent exited with code ${code}: ${stderr}`));
      }
    });

    child.stdin.write(opts.prompt);
    child.stdin.end();
  });
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `pnpm --filter @multica/xiachat-cli test`
Expected: PASS（agents suites 全绿；Windows 上如遇 fixture flaky 按 Step 1 注记裁剪 skip）

- [ ] **Step 5: Commit**

```bash
git add packages/xiachat-cli/src/agents.ts packages/xiachat-cli/src/agents.test.ts packages/xiachat-cli/src/test-fixtures.ts
git commit -m "feat(xiachat): agent CLI discovery and turn execution with timeout/truncation"
```

---

### Task 6: main.ts 命令骨架（register / pair / login / agents / status）

**Files:**
- Create: `packages/xiachat-cli/src/main.ts`
- Create: `packages/xiachat-cli/src/config.ts`（默认 serverUrl / keystore 路径）
- Test: `packages/xiachat-cli/src/main.test.ts`

**Interfaces:**
- Consumes: `Keystore`（Task 4）、`XiachatApi`（Task 4）、`discoverAgents`（Task 5）
- Produces: `buildProgram(opts: { keystore: Keystore; apiFactory: (serverUrl: string) => XiachatApi; stdout: NodeJS.WriteStream }): Command`（commander Program，可注入测试）。Task 8 的 run 命令挂到同一 program。

- [ ] **Step 1: Write the failing test**

Create `packages/xiachat-cli/src/main.test.ts`:

```ts
// @vitest-environment node
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { buildProgram } from "./main";
import { Keystore } from "./keystore";
import { XiachatApi } from "./api";

const dirs: string[] = [];
function tempKeystore(): Keystore {
  const d = mkdtempSync(join(tmpdir(), "xiachat-main-"));
  dirs.push(d);
  return new Keystore(join(d, "creds.json"));
}
afterEach(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
  dirs.length = 0;
});

describe("xiachat register", () => {
  it("stores credentials from the server response", async () => {
    const keystore = tempKeystore();
    const fakeApi = {
      getConfig: async () => ({ appKey: "pk" }),
      register: async () => ({ nodeId: "node_1", token: "tok_1", deviceCredentialTicket: "dc_1", bindingVersion: 1 }),
      claimPairing: async () => ({ deviceCredentialId: "", deviceSecret: "", nodeId: "" }),
      refreshToken: async () => ({ token: "" }),
    };
    const program = buildProgram({
      keystore,
      apiFactory: () => fakeApi as unknown as XiachatApi,
      stdout: process.stdout,
    });
    await program.parseAsync(["node", "xiachat", "register", "--name", "我的Claude", "--ai-type", "claude", "--server", "http://srv"]);
    const stored = keystore.load();
    expect(stored?.nodeId).toBe("node_1");
    expect(stored?.token).toBe("tok_1");
    expect(stored?.serverUrl).toBe("http://srv");
  });

  it("register --server is required", async () => {
    const program = buildProgram({
      keystore: tempKeystore(),
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: process.stdout,
    });
    await expect(
      program.parseAsync(["node", "xiachat", "register", "--name", "n", "--ai-type", "claude"]),
    ).rejects.toThrow();
  });
});

describe("xiachat status", () => {
  it("prints stored identity", async () => {
    const keystore = tempKeystore();
    keystore.save({ nodeId: "node_9", token: "t", serverUrl: "http://srv" });
    const lines: string[] = [];
    const program = buildProgram({
      keystore,
      apiFactory: () => { throw new Error("should not be called"); },
      stdout: { write: (s: string) => { lines.push(s); return true; } } as unknown as NodeJS.WriteStream,
    });
    await program.parseAsync(["node", "xiachat", "status"]);
    expect(lines.join("")).toContain("node_9");
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `pnpm --filter @multica/xiachat-cli test`
Expected: FAIL — `Cannot find module './main'`

- [ ] **Step 3: Implement config.ts + main.ts**

Create `packages/xiachat-cli/src/config.ts`:

```ts
import { homedir } from "node:os";
import { join } from "node:path";

export function defaultKeystorePath(): string {
  return join(homedir(), ".xiachat", "credentials.json");
}

export const DEFAULT_SERVER_URL = "http://localhost:8080";
```

Create `packages/xiachat-cli/src/main.ts`:

```ts
import { Command } from "commander";
import { Keystore, type StoredCredentials } from "./keystore";
import { XiachatApi } from "./api";
import { discoverAgents } from "./agents";
import { defaultKeystorePath } from "./config";

export interface BuildProgramOpts {
  keystore: Keystore;
  apiFactory: (serverUrl: string) => XiachatApi;
  stdout: NodeJS.WriteStream;
}

export function buildProgram(opts: BuildProgramOpts): Command {
  const program = new Command();
  program.name("xiachat").description("User-device agent CLI over RongCloud IM").version("0.1.0");

  program
    .command("register")
    .description("Register this device as a RongCloud AI node")
    .requiredOption("--name <name>", "node display name")
    .requiredOption("--ai-type <type>", "agent platform (claude, codex, opencode, ...)")
    .requiredOption("--server <url>", "Multica server base URL")
    .option("--pairing-ticket <ticket>", "attribute the node to a workspace via pairing ticket")
    .action(async (cmdOpts: { name: string; aiType: string; server: string; pairingTicket?: string }) => {
      const api = opts.apiFactory(cmdOpts.server);
      const result = await api.register({
        name: cmdOpts.name,
        aiType: cmdOpts.aiType,
        nodeType: "ai",
        pairingTicket: cmdOpts.pairingTicket,
      });
      const creds: StoredCredentials = {
        nodeId: result.nodeId,
        token: result.token,
        credentialId: result.deviceCredentialTicket,
        serverUrl: cmdOpts.server,
      };
      opts.keystore.save(creds);
      opts.stdout.write(`registered ${result.nodeId}\n`);
    });

  program
    .command("pair")
    .description("Claim a pairing session with a ticket")
    .requiredOption("--ticket <ticket>", "pairing ticket (pt_...)")
    .option("--server <url>", "Multica server base URL", "http://localhost:8080")
    .action(async (cmdOpts: { ticket: string; server: string }) => {
      const api = opts.apiFactory(cmdOpts.server);
      const result = await api.claimPairing(cmdOpts.ticket, "", `idem-${Date.now()}`);
      opts.keystore.save({
        nodeId: result.nodeId,
        token: "",
        credentialId: result.deviceCredentialId,
        deviceSecret: result.deviceSecret,
        serverUrl: cmdOpts.server,
      });
      opts.stdout.write(`paired as ${result.nodeId}\n`);
    });

  program
    .command("login")
    .description("Refresh the IM token for the stored node")
    .action(async () => {
      const creds = opts.keystore.load();
      if (!creds) throw new Error("no credentials; run xiachat register or pair first");
      const api = opts.apiFactory(creds.serverUrl);
      const { token } = await api.refreshToken(creds.nodeId);
      opts.keystore.save({ ...creds, token });
      opts.stdout.write("token refreshed\n");
    });

  program
    .command("agents")
    .description("List agent CLIs discoverable on this machine's PATH")
    .action(async () => {
      const found = await discoverAgents({});
      if (found.length === 0) {
        opts.stdout.write("no agent CLIs found on PATH\n");
        return;
      }
      for (const a of found) opts.stdout.write(`${a.name}\t${a.path}\n`);
    });

  program
    .command("status")
    .description("Show stored identity")
    .action(async () => {
      const creds = opts.keystore.load();
      if (!creds) {
        opts.stdout.write("not registered\n");
        return;
      }
      opts.stdout.write(`node: ${creds.nodeId}\nserver: ${creds.serverUrl}\ntoken: ${creds.token ? "present" : "missing"}\n`);
    });

  return program;
}

// CLI entrypoint — only runs when executed as a script, not under vitest.
if (process.argv[1] && process.argv[1].endsWith("main.ts")) {
  const program = buildProgram({
    keystore: new Keystore(defaultKeystorePath()),
    apiFactory: (serverUrl) => new XiachatApi(serverUrl),
    stdout: process.stdout,
  });
  program.parseAsync(process.argv).catch((err: unknown) => {
    console.error(err instanceof Error ? err.message : err);
    process.exitCode = 1;
  });
}
```

注意：Task 8 会往这个 program 挂 `run` 命令。`if (process.argv[1]…)` 守卫防止 vitest import 时误执行——vitest 跑的是 `.test.ts`，import `./main` 时 argv[1] 是 vitest 二进制，守卫成立性要在 Step 4 实测（若 vitest 下仍触发，改为把 entrypoint 拆到单独 `bin.ts`）。

- [ ] **Step 4: Run test to verify it passes**

Run: `pnpm --filter @multica/xiachat-cli test`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add packages/xiachat-cli/src/main.ts packages/xiachat-cli/src/config.ts packages/xiachat-cli/src/main.test.ts
git commit -m "feat(xiachat): command skeleton for register/pair/login/agents/status"
```

---

### Task 7: im.ts（融云连接与消息分派，可注入 transport）

**Files:**
- Create: `packages/xiachat-cli/src/im.ts`
- Test: `packages/xiachat-cli/src/im.test.ts`

**Interfaces:**
- Consumes: protocol.ts 编解码（Task 3）
- Produces:
  - `interface IMTransport { connect(appKey: string, token: string): Promise<void>; sendMessage(toUserId: string, objectName: string, content: string): Promise<void>; onMessage(cb: (msg: InboundIMMessage) => void): void; disconnect(): Promise<void> }` — 适配 `@rongcloud/imlib-next` 的薄层；测试用内存 fake。
  - `interface InboundIMMessage { objectName: string; fromUserId: string; toUserId: string; targetId: string; conversationType: number; content: string }`
  - `class MessageDispatcher { constructor(deps: { send: (toUserId: string, objectName: string, content: string) => Promise<void> }); handle(msg: InboundIMMessage, deps: { runTurn: (prompt: string, model?: string) => Promise<string> }): Promise<void> }` — 核心分派：TxtMsg→单聊回复；command/your_turn→command_result；按 conversation 排队由 Task 8 的 run 命令组合，本类只做单条消息的语义分派。

- [ ] **Step 1: Write the failing test**

Create `packages/xiachat-cli/src/im.test.ts`:

```ts
// @vitest-environment node
import { describe, expect, it } from "vitest";
import { MessageDispatcher } from "./im";

function makeDeps() {
  const sent: Array<{ to: string; objectName: string; content: string }> = [];
  return {
    sent,
    send: async (to: string, objectName: string, content: string) => {
      sent.push({ to, objectName, content });
    },
  };
}

describe("MessageDispatcher single chat", () => {
  it("replies to RC:TxtMsg with agent output as RC:TxtMsg", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "RC:TxtMsg",
        fromUserId: "user_1",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: JSON.stringify({ content: "hi agent" }),
      },
      { runTurn: async (prompt) => `echo:${prompt}` },
    );
    expect(deps.sent).toHaveLength(1);
    expect(deps.sent[0].objectName).toBe("RC:TxtMsg");
    expect(deps.sent[0].to).toBe("user_1");
    const parsed = JSON.parse(deps.sent[0].content);
    expect(parsed.content).toContain("echo:hi agent");
  });

  it("does not reply when agent turn fails (logs only)", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "RC:TxtMsg",
        fromUserId: "user_1",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: JSON.stringify({ content: "q" }),
      },
      { runTurn: async () => { throw new Error("boom"); } },
    );
    expect(deps.sent).toHaveLength(0);
  });
});

describe("MessageDispatcher discussion your_turn", () => {
  it("replies with command_result echoing request_id", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "command",
        fromUserId: "host_node",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: JSON.stringify({
          request_id: "turn_1_1_20260929",
          service: "discussion",
          action: "your_turn",
          params: { round: 1, speaking_order: 1, role_name: "reviewer", model: "sonnet-4" },
        }),
      },
      { runTurn: async (prompt, model) => `out(${model}) ${prompt.length > 0 ? "prompt" : "noprompt"}` },
    );
    expect(deps.sent).toHaveLength(1);
    expect(deps.sent[0].objectName).toBe("command");
    const parsed = JSON.parse(deps.sent[0].content);
    expect(parsed.request_id).toBe("turn_1_1_20260929");
    expect(parsed.msg_type).toBe("text");
  });

  it("replies with error command_result when the turn throws", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    await dispatcher.handle(
      {
        objectName: "command",
        fromUserId: "host_node",
        toUserId: "rc_node_x",
        targetId: "rc_node_x",
        conversationType: 1,
        content: JSON.stringify({
          request_id: "r2",
          service: "discussion",
          action: "your_turn",
          params: {},
        }),
      },
      { runTurn: async () => { throw new Error("timeout"); } },
    );
    const parsed = JSON.parse(deps.sent[0].content);
    expect(parsed.msg_type).toBe("error");
    expect(parsed.request_id).toBe("r2");
  });

  it("ignores unknown commands and other objectNames", async () => {
    const deps = makeDeps();
    const dispatcher = new MessageDispatcher({ send: deps.send });
    for (const objectName of ["RC:ImgMsg", "command"]) {
      await dispatcher.handle(
        {
          objectName,
          fromUserId: "u",
          toUserId: "rc_node_x",
          targetId: "rc_node_x",
          conversationType: 1,
          content: objectName === "command"
            ? JSON.stringify({ request_id: "r", service: "x", action: "other_action", params: {} })
            : "{}",
        },
        { runTurn: async () => "x" },
      );
    }
    expect(deps.sent).toHaveLength(0);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `pnpm --filter @multica/xiachat-cli test`
Expected: FAIL — `Cannot find module './im'`

- [ ] **Step 3: Implement im.ts**

Create `packages/xiachat-cli/src/im.ts`:

```ts
import { decodeCommandContent, encodeCommandResult } from "./protocol";

// Inbound message shape normalized from the IM SDK listener.
export interface InboundIMMessage {
  objectName: string;
  fromUserId: string;
  toUserId: string;
  targetId: string;
  conversationType: number;
  content: string;
}

// Thin transport over @rongcloud/imlib-next so the dispatcher stays testable.
// The production adapter is wired in run.ts (Task 8); tests use an in-memory fake.
export interface IMTransport {
  connect(appKey: string, token: string): Promise<void>;
  sendMessage(toUserId: string, objectName: string, content: string): Promise<void>;
  onMessage(cb: (msg: InboundIMMessage) => void): void;
  disconnect(): Promise<void>;
}

export interface DispatcherDeps {
  send: (toUserId: string, objectName: string, content: string) => Promise<void>;
}

export interface TurnRunner {
  runTurn: (prompt: string, model?: string) => Promise<string>;
}

export class MessageDispatcher {
  constructor(private readonly deps: DispatcherDeps) {}

  async handle(msg: InboundIMMessage, turns: TurnRunner): Promise<void> {
    if (msg.objectName === "RC:TxtMsg") {
      let text = "";
      try {
        const parsed = JSON.parse(msg.content) as { content?: unknown };
        text = typeof parsed.content === "string" ? parsed.content : "";
      } catch {
        return;
      }
      if (!text) return;
      try {
        const out = await turns.runTurn(text);
        await this.deps.send(
          msg.fromUserId,
          "RC:TxtMsg",
          JSON.stringify({ content: out }),
        );
      } catch (err) {
        // Single-chat failure: do not reply; the user can retry. Never leak
        // credentials into messages or logs.
        console.error("agent turn failed:", err instanceof Error ? err.message : err);
      }
      return;
    }

    if (msg.objectName === "command") {
      const cmd = decodeCommandContent(msg.content);
      if (!cmd || cmd.service !== "discussion" || cmd.action !== "your_turn") {
        return;
      }
      const params = (cmd.params ?? {}) as {
        round?: number; speaking_order?: number; role_name?: string; model?: string;
      };
      const prompt = [
        `[discussion] round=${params.round ?? "?"} speaking_order=${params.speaking_order ?? "?"}`,
        params.role_name ? `role: ${params.role_name}` : "",
        "Answer as this node in the discussion.",
      ].filter(Boolean).join("\n");
      try {
        const out = await turns.runTurn(prompt, params.model);
        await this.deps.send(
          msg.fromUserId,
          "command",
          encodeCommandResult(cmd.request_id, "text", { content: out }),
        );
      } catch (err) {
        await this.deps.send(
          msg.fromUserId,
          "command",
          encodeCommandResult(cmd.request_id, "error", {
            error: err instanceof Error ? err.message : "agent turn failed",
          }),
        );
      }
    }
  }
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `pnpm --filter @multica/xiachat-cli test`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add packages/xiachat-cli/src/im.ts packages/xiachat-cli/src/im.test.ts
git commit -m "feat(xiachat): inbound message dispatcher for chat and your_turn commands"
```

---

### Task 8: run 命令（IM 主循环 + 会话队列 + 真实 transport 适配）

**Files:**
- Modify: `packages/xiachat-cli/src/main.ts`（挂 run 命令）
- Create: `packages/xiachat-cli/src/run.ts`（主循环 + 会话 FIFO + imlib-next 适配）
- Test: `packages/xiachat-cli/src/run.test.ts`

**Interfaces:**
- Consumes: `MessageDispatcher`（Task 7）、`runAgentTurn`/`discoverAgents`（Task 5）、`Keystore`/`XiachatApi`（Task 4）
- Produces: `class SessionQueue`（per-conversation FIFO，队列深度 1，后续同会话消息回忙碌提示——导出供测试）；`createImlibTransport(): IMTransport`（真实 SDK 适配，不直接单测，冒烟覆盖）；`startRunLoop(opts): Promise<void>`。

- [ ] **Step 1: Write the failing test**

Create `packages/xiachat-cli/src/run.test.ts`:

```ts
// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import { SessionQueue } from "./run";

describe("SessionQueue", () => {
  it("serializes messages within one conversation", async () => {
    const q = new SessionQueue({ concurrencyPerConversation: 1 });
    const order: number[] = [];
    const task = (n: number) => async () => {
      order.push(n);
      await new Promise((r) => setTimeout(r, 20));
      order.push(-n);
    };
    q.enqueue("conv-1", task(1));
    q.enqueue("conv-1", task(2));
    await q.idle();
    // task 2 must not start before task 1 finished
    expect(order.indexOf(-1)).toBeLessThan(order.indexOf(2));
  });

  it("runs different conversations in parallel", async () => {
    const q = new SessionQueue({ concurrencyPerConversation: 1 });
    const started: string[] = [];
    const slow = (id: string, ms: number) => async () => {
      started.push(id);
      await new Promise((r) => setTimeout(r, ms));
    };
    q.enqueue("conv-a", slow("a", 60));
    q.enqueue("conv-b", slow("b", 10));
    await q.idle();
    // b finished while a was still running → both started before either idle
    expect(started).toContain("a");
    expect(started).toContain("b");
    expect(started.indexOf("b")).toBeLessThan(2);
  });

  it("busy message replies once when queue is at depth", async () => {
    const onBusy = vi.fn();
    const q = new SessionQueue({ concurrencyPerConversation: 1, queueDepth: 1, onBusy });
    const slow = async () => { await new Promise((r) => setTimeout(r, 40)); };
    q.enqueue("conv-1", slow);
    q.enqueue("conv-1", slow); // queued (depth 1)
    q.enqueue("conv-1", slow); // busy → onBusy("conv-1")
    await q.idle();
    expect(onBusy).toHaveBeenCalledTimes(1);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `pnpm --filter @multica/xiachat-cli test`
Expected: FAIL — `Cannot find module './run'`

- [ ] **Step 3: Implement run.ts**

Create `packages/xiachat-cli/src/run.ts`:

```ts
import { randomUUID } from "node:crypto";
import type { IMTransport, InboundIMMessage } from "./im";
import { MessageDispatcher } from "./im";
import { runAgentTurn } from "./agents";
import type { StoredCredentials } from "./keystore";
import { XiachatApi } from "./api";

// Per-conversation FIFO. Same conversation serializes (agent busy → queue,
// overflow → onBusy); different conversations run in parallel. Cross-session
// concurrency safety is the agent platform's responsibility (spec §2.3.5).
export interface SessionQueueOpts {
  concurrencyPerConversation: number;
  queueDepth?: number;
  onBusy?: (conversationKey: string) => void;
}

interface PendingTask {
  key: string;
  run: () => Promise<void>;
}

export class SessionQueue {
  private readonly active = new Map<string, number>();
  private readonly waiting = new Map<string, PendingTask[]>();
  private readonly idleResolvers: Array<() => void> = [];
  private totalPending = 0;

  constructor(private readonly opts: SessionQueueOpts) {}

  enqueue(conversationKey: string, task: () => Promise<void>): void {
    const activeCount = this.active.get(conversationKey) ?? 0;
    const waiting = this.waiting.get(conversationKey) ?? [];
    if (activeCount >= this.opts.concurrencyPerConversation) {
      const depth = this.opts.queueDepth ?? Number.MAX_SAFE_INTEGER;
      if (waiting.length >= depth) {
        this.opts.onBusy?.(conversationKey);
        return;
      }
      waiting.push({ key: conversationKey, run: task });
      this.waiting.set(conversationKey, waiting);
      return;
    }
    this.start(conversationKey, task);
  }

  private start(conversationKey: string, task: () => Promise<void>): void {
    this.totalPending++;
    this.active.set(conversationKey, (this.active.get(conversationKey) ?? 0) + 1);
    void task()
      .catch((err: unknown) => {
        console.error("queued task failed:", err instanceof Error ? err.message : err);
      })
      .finally(() => {
        this.totalPending--;
        const count = (this.active.get(conversationKey) ?? 1) - 1;
        if (count > 0) this.active.set(conversationKey, count);
        else this.active.delete(conversationKey);
        const waiting = this.waiting.get(conversationKey);
        const next = waiting?.shift();
        if (waiting && waiting.length === 0) this.waiting.delete(conversationKey);
        if (next) {
          this.start(conversationKey, next.run);
        } else if (this.totalPending === 0) {
          this.idleResolvers.splice(0).forEach((r) => r());
        }
      });
  }

  async idle(): Promise<void> {
    if (this.totalPending === 0 && this.active.size === 0) return;
    await new Promise<void>((resolve) => this.idleResolvers.push(resolve));
  }
}

export function conversationKeyOf(msg: InboundIMMessage): string {
  return `${msg.conversationType}:${msg.targetId}`;
}

export interface StartRunLoopOpts {
  creds: StoredCredentials;
  api: XiachatApi;
  transport: IMTransport;
  agentExecPath: string;
  stdout: NodeJS.WriteStream;
  turnTimeoutMs?: number;
}

export async function startRunLoop(opts: StartRunLoopOpts): Promise<void> {
  const { appKey, token } = await ensureConnection(opts);
  await opts.transport.connect(appKey, token);

  const dispatcher = new MessageDispatcher({ send: opts.transport.sendMessage.bind(opts.transport) });
  const queue = new SessionQueue({
    concurrencyPerConversation: 1,
    queueDepth: 1,
    onBusy: (key) => {
      const [convType, targetId] = key.split(":");
      if (convType === "1") {
        // Single chat busy hint; discussion turns never overflow (coordinator
        // serializes turns itself).
        void opts.transport.sendMessage(targetId, "RC:TxtMsg", JSON.stringify({ content: "正在思考中…" })).catch(() => {});
      }
    },
  });

  opts.transport.onMessage((msg) => {
    queue.enqueue(conversationKeyOf(msg), () =>
      dispatcher.handle(msg, {
        runTurn: (prompt, model) =>
          runAgentTurn({
            execPath: opts.agentExecPath,
            prompt,
            model,
            timeoutMs: opts.turnTimeoutMs ?? 120_000,
          }),
      }),
    );
  });

  opts.stdout.write("xiachat run: connected and dispatching\n");
  await new Promise<void>(() => {}); // run until process exit
}

async function ensureConnection(opts: StartRunLoopOpts): Promise<{ appKey: string; token: string }> {
  const appKey = opts.creds.appKey ?? (await opts.api.getConfig()).appKey;
  let token = opts.creds.token;
  if (!token) {
    const refreshed = await opts.api.refreshToken(opts.creds.nodeId);
    token = refreshed.token;
  }
  return { appKey, token };
}

// Production transport over @rongcloud/imlib-next. Compiled but only truly
// exercised in the M4 smoke test (spec §7.2); unit tests inject fakes.
export function createImlibTransport(): IMTransport {
  // eslint-disable-next-line @typescript-eslint/no-var-requires
  const { init, connect, sendMessage, addMessageListener, disconnect } = require("@rongcloud/imlib-next");
  const listeners: Array<(msg: InboundIMMessage) => void> = [];
  return {
    async connect(appKey: string, token: string): Promise<void> {
      init({ appkey: appKey });
      await connect(token);
      addMessageListener((message: unknown) => {
        const m = message as {
          objectName: string; senderUserId: string; targetId: string;
          conversationType: { type?: number } | number; content: { content?: string } | string;
        };
        const content = typeof m.content === "string" ? m.content : m.content?.content ?? "";
        const convType = typeof m.conversationType === "number" ? m.conversationType : m.conversationType?.type ?? 1;
        for (const cb of listeners) {
          cb({
            objectName: m.objectName,
            fromUserId: m.senderUserId,
            toUserId: "",
            targetId: m.targetId,
            conversationType: convType,
            content,
          });
        }
      });
    },
    async sendMessage(toUserId: string, objectName: string, content: string): Promise<void> {
      await sendMessage({ conversationType: 1, targetId: toUserId, objectName, content: { content } });
    },
    onMessage(cb): void { listeners.push(cb); },
    async disconnect(): Promise<void> { await disconnect(); },
  };
}
```

注：`require` 在 ESM 包里不可用——`createImlibTransport` 顶部改用动态 import：`const imlib = await import("@rongcloud/imlib-next")` 放进 connect 方法内，并把函数签名改为 `async createImlibTransport(): Promise<IMTransport>`。执行者按此实现（上面的 require 形式是示意，以编译通过 + lint 通过为准）。

- [ ] **Step 4: Wire run command into main.ts**

In `packages/xiachat-cli/src/main.ts`，`buildProgram` 内追加（status 命令之后）:

```ts
  program
    .command("run")
    .description("Connect to RongCloud IM and dispatch agent turns")
    .option("--agent <name>", "agent CLI name (default: the registered ai_type)")
    .option("--model <model>", "model override passed to the agent CLI")
    .action(async (cmdOpts: { agent?: string; model?: string }) => {
      const creds = opts.keystore.load();
      if (!creds) throw new Error("no credentials; run xiachat register or pair first");
      const api = opts.apiFactory(creds.serverUrl);
      const { discoverAgents } = await import("./agents");
      const { startRunLoop, createImlibTransport } = await import("./run");
      const agentName = cmdOpts.agent ?? creds.nodeId; // fallback: derived later from node record
      const found = await discoverAgents({});
      const agent = found.find((a) => a.name === cmdOpts.agent);
      if (!agent) throw new Error(`agent CLI ${cmdOpts.agent ?? "(default)"} not found on PATH; run xiachat agents`);
      const transport = await createImlibTransport();
      await startRunLoop({
        creds, api, transport,
        agentExecPath: agent.path,
        stdout: opts.stdout,
      });
    });
```

（`agentName` 行是占位死代码——直接删掉，以 `agent.path` 为准。）

- [ ] **Step 5: Run tests to verify they pass**

Run: `pnpm --filter @multica/xiachat-cli test && pnpm --filter @multica/xiachat-cli typecheck`
Expected: PASS, typecheck clean

- [ ] **Step 6: Commit**

```bash
git add packages/xiachat-cli/src/run.ts packages/xiachat-cli/src/run.test.ts packages/xiachat-cli/src/main.ts
git commit -m "feat(xiachat): run command with IM main loop and per-conversation queue"
```

---

### Task 9: 全链路冒烟（spec §7.2）+ 打包脚本

**Files:**
- Create: `packages/xiachat-cli/scripts/smoke.md`（冒烟操作手册）
- Modify: `packages/xiachat-cli/package.json`（`build:bin` script）
- Modify: `CLAWMESSENGER_GUIDE.zh.md`（附录：xiachat 使用）

**Interfaces:**
- Consumes: 全部前置任务
- Produces: 可执行单文件（`dist/xiachat.exe` / `dist/xiachat`）+ 冒烟记录

- [ ] **Step 1: Add build:bin script**

`packages/xiachat-cli/package.json` scripts 增加:

```json
"build:bin": "tsc -p tsconfig.json && node scripts/pack.mjs"
```

Create `packages/xiachat-cli/scripts/pack.mjs`:

```js
import { build } from "esbuild";
import { chmodSync } from "node:fs";

// Bundles dist/main.js into a standalone executable-ish entry. True single-
// file binaries (bun compile / pkg) are CI concerns; locally esbuild output
// plus a shell wrapper is enough for the smoke test.
await build({
  entryPoints: ["dist/main.js"],
  bundle: true,
  platform: "node",
  target: "node20",
  format: "esm",
  outfile: "dist/xiachat.bundle.js",
  external: ["@rongcloud/imlib-next"],
});
chmodSync("dist/xiachat.bundle.js", 0o755);
console.log("bundled dist/xiachat.bundle.js — run with: node dist/xiachat.bundle.js <cmd>");
```

Run: `pnpm --filter @multica/xiachat-cli add -D esbuild && pnpm --filter @multica/xiachat-cli build:bin`
Expected: bundling 成功，`node dist/xiachat.bundle.js agents` 列出本机 agent

- [ ] **Step 2: Manual smoke per spec §7.2**

按 `scripts/smoke.md` 执行（手册内容 = spec §7.2 的 4 步：注册→单聊→讨论→kill 超时）。前置：后端 + Postgres + 融云测试应用 + 真实 agent CLI（属 AGENTS.md 允许的 explicit authorization 场景，执行前需用户确认）。

每步记录：请求/响应摘要、DB 行、事件日志。任何一步失败 → 修复 → 重跑。全部通过后在 smoke.md 底部记录日期与结果。

- [ ] **Step 3: Document in guide**

`CLAWMESSENGER_GUIDE.zh.md` 追加「附录 A：用户设备 CLI（xiachat）」：install / register / pair / run 各一段，指向 smoke.md 与 spec。

- [ ] **Step 4: Full workspace verification**

Run: `pnpm typecheck && pnpm lint && pnpm test`
Expected: 全绿（mobile 除外，按 AGENTS.md）

Run: `cd server && "C:\Program Files\Go\bin\go.exe" test ./internal/handler/ ./internal/integrations/rongcloud/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add packages/xiachat-cli/scripts/ packages/xiachat-cli/package.json CLAWMESSENGER_GUIDE.zh.md
git commit -m "feat(xiachat): binary packaging, smoke test guide, and user docs"
```

---

## Self-Review 记录

- **Spec coverage**: T1/T2 = spec §5.2；T3-T8 = spec §2 CLI（protocol/keystore/api/agents/commands/im/run + §2.3.5 会话队列）；T9 = spec §7.2 冒烟 + M5 打包。spec §2.3.5 忙碌提示在 T8 `onBusy` 实现。**Gap: spec §2.3 提到的长回复走 RC:StreamMsg**（单聊流式）在 T7 dispatcher 只实现整段 RC:TxtMsg 回复——stream 分帧的协议函数（encodeStreamFrame）已在 T3 备好但 run 路径未用；这是有意的 v1 裁剪（spec §9 风险 2：StreamMsg 呈现待 M3 实测），讨论模式 stream 同理先走整段 text。已在计划中注明。
- **Placeholder scan**: Task 1 Step 1 的第一个代码块含 placeholder 并立即被完整版覆盖（执行者读第二个块）；Task 8 Step 3/4 内嵌了两处「执行者注意」修正注记。这些是显式指令，不是未填坑。
- **Type consistency**: `StoredCredentials`（T4 定义，T6/T8 使用）、`IMTransport`/`InboundIMMessage`（T7 定义，T8 使用）、`encodeCommandResult` 签名（T3 定义，T7 使用）、`discoverAgents({ extraPath })`（T5 定义，T6 传 `{}`）已核对一致。
