# RongCloud Phase 4 Bridge + Frontend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Each Task is self-contained and ends with a commit.

**Goal:** Add the DiscussionBridge (server-managed AI node execution via agent CLIs) and a complete frontend integration (types, schemas, API client methods, React Query hooks, settings tab, chatroom manager, and discussion monitor) for the RongCloud integration.

**Architecture:** A DiscussionBridge struct discovers locally installed agent CLIs (claude, codex, opencode, etc.) and executes turns on behalf of server-managed nodes. The DiscussionCoordinator checks `bridge.IsServerManaged(node)` in `sendYourTurn` â€?if true, it launches `executeBridgeTurn` in a goroutine that calls `bridge.ExecuteTurn` and sends the result to `responseCh`; otherwise it sends the `your_turn` command via RongCloud's private message API as before. The system_handler gets a `case "your_turn"` no-op ack. On the frontend, TypeScript types, zod schemas, API client methods, React Query hooks, and React views follow the established DingTalk/Telegram pattern.

**Tech Stack:** Go 1.24 (Chi, sqlc, pgx/v5), Next.js 15, React 19, TanStack Query v5, zod, pnpm monorepo

**Spec:** `docs/superpowers/specs/2026-09-24-rongcloud-phase4-bridge-frontend-design.md`

---

## Global Constraints

- **Module path:** `github.com/multica-ai/multica`
- **DB import alias:** `db "github.com/multica-ai/multica/server/pkg/db/generated"`
- **Go binary:** `$env:Path = "C:\Program Files\Go\bin;$env:Path"`
- **Build:** `cd server; go build ./...`
- **Test Go:** `cd server; go test ./internal/integrations/rongcloud/... -count=1`
- **sqlc:** `cd server && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate`
- **Frontend typecheck:** `pnpm typecheck`
- **Frontend lint:** `pnpm lint`
- **Frontend test:** `pnpm test`
- **No foreign keys** (MUL-3515 Â§4); integrity enforced in application layer
- **All indexes CONCURRENTLY** in separate migration files
- **UUID PKs** with `DEFAULT gen_random_uuid()`
- **Conventional commits** scoped to `rongcloud`: `feat(rongcloud): ...`, `fix(rongcloud): ...`
- **No new DB tables** â€?Phase 4 uses existing 9 tables from Phase 2a/3

---

## Task 1: DiscussionBridge â€?New File

**Files:** `server/internal/integrations/rongcloud/discussion_bridge.go` (NEW)

**Interfaces:**
```go
type DiscussionBridge struct {
    queries *db.Queries
    logger  *slog.Logger
    agents  map[string]string // ai_type â†?executable path
}
func NewDiscussionBridge(queries *db.Queries, logger *slog.Logger) *DiscussionBridge
func (b *DiscussionBridge) ExecuteTurn(ctx context.Context, chatroomID, nodeID pgtype.UUID, prompt, model string) (string, error)
func (b *DiscussionBridge) IsServerManaged(node db.RongcloudNode) bool
func (b *DiscussionBridge) RefreshAgents()
```

### Step 1: Create `discussion_bridge.go` with struct and constructor

- [x] **Step 1:** Create `server/internal/integrations/rongcloud/discussion_bridge.go`:

```go
package rongcloud

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// knownAgentCLIs lists the agent CLI executable names that the bridge can
// discover on the server host. A node whose ai_type matches one of these
// names is considered "server-managed" when the executable is found in PATH.
var knownAgentCLIs = []string{
	"claude", "codex", "opencode", "codebuddy", "codearts", "deveco",
	"openclaw", "hermes", "pi", "omp", "cursor-agent", "kimi",
	"reasonix", "dsh", "kiro-cli", "agy", "qodercli", "qoderclicn",
	"traecli", "grok", "qwen", "qwenpaw", "mcode", "dim", "zeroclaw",
}

// DiscussionBridge executes turns for server-managed AI nodes by invoking
// locally installed agent CLIs. Nodes that are not server-managed
// (external RongCloud users) receive your_turn commands via the RongCloud
// messaging API instead.
type DiscussionBridge struct {
	queries *db.Queries
	logger  *slog.Logger
	agents  map[string]string // ai_type â†?resolved executable path
}

// NewDiscussionBridge creates a bridge that discovers available agent
// CLIs at construction time. Call RefreshAgents() later to re-probe.
func NewDiscussionBridge(queries *db.Queries, logger *slog.Logger) *DiscussionBridge {
	if logger == nil {
		logger = slog.Default()
	}
	b := &DiscussionBridge{
		queries: queries,
		logger:  logger,
		agents:  make(map[string]string),
	}
	b.RefreshAgents()
	return b
}
```

### Step 2: Implement RefreshAgents

- [x] **Step 2:** Add `RefreshAgents` method:

```go
// RefreshAgents re-probes PATH for known agent CLI executables and
// updates the internal agents map. Safe to call concurrently (callers
// should guard with external synchronization if needed).
func (b *DiscussionBridge) RefreshAgents() {
	b.agents = make(map[string]string)
	for _, name := range knownAgentCLIs {
		if path, err := exec.LookPath(name); err == nil {
			b.agents[name] = path
			b.logger.Debug("agent CLI discovered", "name", name, "path", path)
		}
	}
	if len(b.agents) == 0 {
		b.logger.Warn("no agent CLIs found on PATH; server-managed turns will fail")
	}
}
```

### Step 3: Implement IsServerManaged

- [x] **Step 3:** Add `IsServerManaged` method:

```go
// IsServerManaged returns true when the node's ai_type matches a
// discovered agent CLI on the server host. Such nodes have their turns
// executed locally via the bridge; other nodes receive your_turn
// commands through the RongCloud messaging API.
func (b *DiscussionBridge) IsServerManaged(node db.RongcloudNode) bool {
	if node.AiType == "" {
		return false
	}
	_, ok := b.agents[node.AiType]
	return ok
}
```

### Step 4: Implement ExecuteTurn

- [x] **Step 4:** Add `ExecuteTurn` method:

```go
// ExecuteTurn runs the agent CLI associated with the node, passing the
// prompt on stdin, and returns the collected stdout. The call is
// cancelled after 120 seconds. If the node's ai_type is not in the
// agents map the method returns an error immediately.
func (b *DiscussionBridge) ExecuteTurn(ctx context.Context, chatroomID, nodeID pgtype.UUID, prompt, model string) (string, error) {
	node, err := b.queries.GetRongCloudNodeByID(ctx, nodeID)
	if err != nil {
		return "", fmt.Errorf("bridge: lookup node: %w", err)
	}
	execPath, ok := b.agents[node.AiType]
	if !ok {
		return "", fmt.Errorf("bridge: agent CLI %q not available", node.AiType)
	}

	execCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(execCtx, execPath)
	if model != "" {
		cmd.Args = append(cmd.Args, "--model", model)
	}
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		b.logger.Error("agent CLI failed",
			"node_id", nodeID.String(),
			"ai_type", node.AiType,
			"error", err,
			"stderr", stderr.String(),
		)
		return "", fmt.Errorf("bridge: agent %q failed: %w (stderr: %s)", node.AiType, err, stderr.String())
	}

	b.logger.Debug("agent CLI turn completed",
		"node_id", nodeID.String(),
		"ai_type", node.AiType,
		"stdout_len", stdout.Len(),
	)
	return stdout.String(), nil
}
```

### Step 5: Build and commit

- [x] **Step 5:** Verify build and commit:

```bash
cd server
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./internal/integrations/rongcloud/...
go test ./internal/integrations/rongcloud/... -count=1
git add server/internal/integrations/rongcloud/discussion_bridge.go
git commit -m "feat(rongcloud): add DiscussionBridge for server-managed AI node turns"
```

---

## Task 2: Wire Bridge into DiscussionCoordinator

**Files:** `server/internal/integrations/rongcloud/discussion_coordinator.go` (MODIFY)

**Interfaces:**
```go
// New signature (bridge added before logger):
func NewDiscussionCoordinator(chatroomID, workspaceID pgtype.UUID, state DiscussionState, client *rongcloudAPIClient, eventStore *DiscussionEventStore, streamAssembler *StreamAssembler, bridge *DiscussionBridge, logger *slog.Logger) *DiscussionCoordinator
// New methods on DiscussionCoordinator:
func (c *DiscussionCoordinator) executeBridgeTurn(ctx context.Context, speaker SpeakerInfo, round, speakingOrder int)
func (c *DiscussionCoordinator) buildPrompt(speaker SpeakerInfo, round, speakingOrder int) string
```

### Step 1: Add bridge field and update constructor

- [x] **Step 1:** In `discussion_coordinator.go`, add `bridge *DiscussionBridge` to the struct and update `NewDiscussionCoordinator`:

Find:
```go
type DiscussionCoordinator struct {
	chatroomID      pgtype.UUID
	workspaceID     pgtype.UUID
	state           DiscussionState
	client          *rongcloudAPIClient
	eventStore      *DiscussionEventStore
	responseCh      chan NodeResponse
	streamAssembler *StreamAssembler
	logger          *slog.Logger
	mu              sync.Mutex
	paused          bool
	done            chan struct{}
}

func NewDiscussionCoordinator(
	chatroomID, workspaceID pgtype.UUID,
	state DiscussionState,
	client *rongcloudAPIClient,
	eventStore *DiscussionEventStore,
	streamAssembler *StreamAssembler,
	logger *slog.Logger,
) *DiscussionCoordinator {
```

Replace with:
```go
type DiscussionCoordinator struct {
	chatroomID      pgtype.UUID
	workspaceID     pgtype.UUID
	state           DiscussionState
	client          *rongcloudAPIClient
	eventStore      *DiscussionEventStore
	responseCh      chan NodeResponse
	streamAssembler *StreamAssembler
	bridge          *DiscussionBridge
	logger          *slog.Logger
	mu              sync.Mutex
	paused          bool
	done            chan struct{}
}

func NewDiscussionCoordinator(
	chatroomID, workspaceID pgtype.UUID,
	state DiscussionState,
	client *rongcloudAPIClient,
	eventStore *DiscussionEventStore,
	streamAssembler *StreamAssembler,
	bridge *DiscussionBridge,
	logger *slog.Logger,
) *DiscussionCoordinator {
```

In the constructor body, add `bridge: bridge,` to the struct literal.

### Step 2: Modify sendYourTurn to check bridge

- [x] **Step 2:** In `sendYourTurn`, add bridge check at the beginning. After building the speaker info but before sending via client.sendPrivateMessage, add:

```go
// If the node is server-managed, execute the turn locally via the
// bridge instead of sending a your_turn command over RongCloud.
if c.bridge != nil {
	node, err := c.queries.GetRongCloudNodeByID(ctx, speaker.NodeID)
	// NOTE: coordinator doesn't have queries field â€?need to fetch
	// node via the eventStore or pass node info differently.
```

**IMPORTANT:** The coordinator does not currently have a `queries` field. The bridge already does `GetRongCloudNodeByID` internally in `ExecuteTurn`. For `IsServerManaged`, we need the `db.RongcloudNode` struct. The simplest approach: call `bridge.ExecuteTurn` directly and let it handle the node lookup; but we still need `IsServerManaged` to decide whether to use the bridge at all.

**Solution:** Add a `queries *db.Queries` field to the coordinator (it already has `eventStore` which wraps queries, but the queries field is unexported). Instead, pass the node's `ai_type` from the `SpeakerInfo` â€?but `SpeakerInfo` doesn't have `AiType`. 

**Better solution:** Add `AiType` to `SpeakerInfo` in `discussion_types.go`, populate it during `StartDiscussion` when building speakers, and use `bridge.agents` directly. But `bridge.agents` is unexported.

**Final approach:** Add an `IsServerManagedByType(aiType string) bool` convenience method to the bridge:

In `discussion_bridge.go`, add:
```go
// IsServerManagedByType returns true when the given ai_type string
// matches a discovered agent CLI on the server host. This avoids a DB
// lookup when the caller already has the ai_type from SpeakerInfo.
func (b *DiscussionBridge) IsServerManagedByType(aiType string) bool {
	if aiType == "" {
		return false
	}
	_, ok := b.agents[aiType]
	return ok
}
```

Then, add `AiType` to `SpeakerInfo` in `discussion_types.go`:
```go
type SpeakerInfo struct {
	NodeID        pgtype.UUID
	SpeakingOrder int
	RoleName      string
	Model         string
	RongcloudUserID string
	AiType        string // added in Phase 4
}
```

Update `discussion_service.go` `StartDiscussion` to populate `AiType` when building speakers (it already calls `GetRongCloudNodeByID` â€?just add `AiType: node.AiType` to the SpeakerInfo literal).

Then in `sendYourTurn`, add at the top:
```go
if c.bridge != nil && c.bridge.IsServerManagedByType(speaker.AiType) {
	go c.executeBridgeTurn(ctx, speaker, round, speakingOrder)
	return
}
```

### Step 3: Add executeBridgeTurn and buildPrompt methods

- [x] **Step 3:** Add these methods to `discussion_coordinator.go`:

```go
// executeBridgeTurn runs a server-managed agent CLI turn in a goroutine
// and feeds the response into the coordinator's response channel.
func (c *DiscussionCoordinator) executeBridgeTurn(ctx context.Context, speaker SpeakerInfo, round, speakingOrder int) {
	prompt := c.buildPrompt(speaker, round, speakingOrder)
	result, err := c.bridge.ExecuteTurn(ctx, c.chatroomID, speaker.NodeID, prompt, speaker.Model)
	if err != nil {
		c.logger.Error("bridge turn failed, falling through to timeout",
			"node_id", speaker.NodeID.String(),
			"round", round,
			"error", err,
		)
		// Send an error response so the coordinator can advance
		select {
		case c.responseCh <- NodeResponse{
			NodeID:  speaker.NodeID.String(),
			MsgType: "stream",
			TurnID:  fmt.Sprintf("r%d_s%d", round, speakingOrder),
			Error:   err.Error(),
		}:
		case <-c.done:
		case <-ctx.Done():
		}
		return
	}
	select {
	case c.responseCh <- NodeResponse{
		NodeID:  speaker.NodeID.String(),
		MsgType: "text",
		TurnID:  fmt.Sprintf("r%d_s%d", round, speakingOrder),
		Content: result,
	}:
	case <-c.done:
	case <-ctx.Done():
	}
}

// buildPrompt constructs the prompt sent to the agent CLI for a given
// speaker's turn. The prompt includes the role name, instructions,
// model, round, and speaking order context.
func (c *DiscussionCoordinator) buildPrompt(speaker SpeakerInfo, round, speakingOrder int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("You are participating in a multi-agent discussion (round %d).\n", round))
	sb.WriteString(fmt.Sprintf("Your role: %s\n", speaker.RoleName))
	sb.WriteString(fmt.Sprintf("Speaking order: %d\n", speaker.SpeakingOrder))
	if speaker.Model != "" {
		sb.WriteString(fmt.Sprintf("Model: %s\n", speaker.Model))
	}
	sb.WriteString("\nPlease provide your contribution to the discussion.\n")
	return sb.String()
}
```

Add `"strings"` to the import block of `discussion_coordinator.go` if not already present.

### Step 4: Update discussion_types.go SpeakerInfo

- [x] **Step 4:** In `server/internal/integrations/rongcloud/discussion_types.go`, add `AiType string` field to `SpeakerInfo`:

```go
type SpeakerInfo struct {
	NodeID          pgtype.UUID
	SpeakingOrder   int
	RoleName        string
	Model           string
	RongcloudUserID string
	AiType          string
}
```

### Step 5: Update discussion_service.go to pass AiType and bridge

- [x] **Step 5:** In `server/internal/integrations/rongcloud/discussion_service.go`:

1. Add `bridge *DiscussionBridge` field to `DiscussionService` struct.
2. Update `NewDiscussionService` signature to accept `bridge *DiscussionBridge` (between `registry` and `logger`).
3. In `StartDiscussion`, when building speakers (the `GetRongCloudNodeByID` loop), add `AiType: node.AiType` to the `SpeakerInfo` literal.
4. In `StartDiscussion`, update the `NewDiscussionCoordinator` call to pass `s.bridge` before `s.logger`.

### Step 6: Build and commit

- [x] **Step 6:** Verify build and commit:

```bash
cd server
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./internal/integrations/rongcloud/...
go test ./internal/integrations/rongcloud/... -count=1
git add server/internal/integrations/rongcloud/discussion_bridge.go \
       server/internal/integrations/rongcloud/discussion_coordinator.go \
       server/internal/integrations/rongcloud/discussion_types.go \
       server/internal/integrations/rongcloud/discussion_service.go
git commit -m "feat(rongcloud): wire DiscussionBridge into coordinator for server-managed turns"
```

---

## Task 3: system_handler your_turn Action + Router Wiring

**Files:** `server/internal/integrations/rongcloud/system_handler.go` (MODIFY), `server/cmd/server/router.go` (MODIFY)

### Step 1: Add your_turn case to handleDiscussionCommand

- [x] **Step 1:** In `system_handler.go`, find `handleDiscussionCommand`. After the `case "status":` block and before `default:`, add:

```go
	case "your_turn":
		// No-op ack: the actual turn execution happens in the
		// coordinator (for server-managed nodes) or via the
		// external node's own runtime. This ack simply confirms
		// receipt.
		payload := map[string]interface{}{
			"ok":      true,
			"message": "your_turn acknowledged",
		}
		if h.client == nil {
			return
		}
		if err := h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, payload); err != nil {
			h.logger.Error("failed to send your_turn ack", "error", err)
		}
```

### Step 2: Update router.go to create bridge and pass to service

- [x] **Step 2:** In `server/cmd/server/router.go`, in the RongCloud env-gated block (around line 1213-1235):

Find:
```go
	rcRegistry := rongcloud.NewDiscussionRegistry()
```

Add before it:
```go
	rcBridge := rongcloud.NewDiscussionBridge(queries, slog.Default())
```

Find:
```go
	h.RongCloudDiscussion = rongcloud.NewDiscussionService(queries, rcClient, rcRegistry, slog.Default())
```

Replace with:
```go
	h.RongCloudDiscussion = rongcloud.NewDiscussionService(queries, rcClient, rcRegistry, rcBridge, slog.Default())
```

### Step 3: Update all newSystemHandler and NewDiscussionService callers in tests

- [x] **Step 3:** In `server/internal/integrations/rongcloud/rongcloud_test.go`, update all `NewDiscussionService` calls to include the new `bridge` parameter (pass `nil` for tests). Also check if any tests call `NewDiscussionCoordinator` directly and update those to pass `nil` for `bridge`.

### Step 4: Build, test, and commit

- [x] **Step 4:** Verify build and commit:

```bash
cd server
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./...
go test ./internal/integrations/rongcloud/... -count=1
git add server/internal/integrations/rongcloud/system_handler.go \
       server/internal/integrations/rongcloud/rongcloud_test.go \
       server/cmd/server/router.go
git commit -m "feat(rongcloud): add your_turn ack and wire DiscussionBridge in router"
```

---

## Task 4: Frontend â€?Types + Schemas

**Files:** `packages/core/types/rongcloud.ts` (NEW), `packages/core/api/schemas.ts` (MODIFY)

### Step 1: Create TypeScript types file

- [x] **Step 1:** Create `packages/core/types/rongcloud.ts`:

```typescript
/** RongCloud installation configuration. */
export interface RongCloudConfig {
  app_key: string;
  configured: boolean;
}

/** RongCloud AI node registration request. */
export interface RongCloudNodeRegisterRequest {
  name: string;
  mac_address: string;
  node_type: string;
  ai_type: string;
  capabilities?: string[];
}

/** RongCloud AI node registration result. */
export interface RongCloudNodeRegisterResult {
  node_id: string;
  token: string;
  capabilities: string[];
  device_credential_ticket: string;
  binding_version: number;
}

/** RongCloud node record. */
export interface RongCloudNode {
  id: string;
  workspace_id: string;
  owner_user_id: string;
  rongcloud_user_id: string;
  node_id: string;
  ai_type: string;
  capabilities: string[];
  deploy_status: "active" | "disabled" | string;
  binding_version: number;
  created_at: string;
  updated_at: string;
}

/** RongCloud device record. */
export interface RongCloudDevice {
  id: string;
  workspace_id: string;
  owner_user_id: string;
  node_id: string;
  device_name: string;
  device_type: string;
  credential_id: string;
  status: "active" | "disabled" | "deleted" | string;
  created_at: string;
  updated_at: string;
}

/** RongCloud chatroom record. */
export interface RongCloudChatroom {
  id: string;
  workspace_id: string;
  rongcloud_chatroom_id: string;
  owner_user_id: string;
  host_node_id: string;
  max_rounds: number;
  conversation_kind: string;
  config: Record<string, unknown>;
  status: "active" | "deleted" | string;
  created_at: string;
  updated_at: string;
}

/** RongCloud chatroom member. */
export interface RongCloudChatroomMember {
  id: string;
  chatroom_id: string;
  node_id: string;
  member_type: string;
  role_name: string;
  role_instructions: string;
  capabilities: Record<string, unknown>;
  model: string;
  speaking_order: number;
  enabled: boolean;
  discussion_model: string;
}

/** RongCloud node model catalog entry. */
export interface RongCloudNodeModelCatalog {
  id: string;
  workspace_id: string;
  node_id: string;
  model_id: string;
  provider: string;
  model_name: string;
  config: Record<string, unknown>;
}

/** RongCloud discussion event. */
export interface RongCloudDiscussionEvent {
  id: string;
  chatroom_id: string;
  workspace_id: string;
  event_type: string;
  round_number: number;
  speaking_order: number;
  node_id: string;
  content: Record<string, unknown>;
  msg_uid: string;
  created_at: string;
}

/** RongCloud discussion state snapshot. */
export interface RongCloudDiscussionState {
  chatroom_id: string;
  workspace_id: string;
  status: "idle" | "starting" | "in_progress" | "paused" | "ended" | string;
  current_round: number;
  current_speaker: string;
  host_node_id: string;
  speakers: RongCloudSpeakerInfo[];
  started_at: string;
  ended_at: string | null;
}

/** Speaker info within a discussion. */
export interface RongCloudSpeakerInfo {
  node_id: string;
  speaking_order: number;
  role_name: string;
  model: string;
  rongcloud_user_id: string;
}

/** RongCloud pairing session. */
export interface RongCloudPairingSession {
  id: string;
  workspace_id: string;
  ticket: string;
  status: "pending" | "claimed" | "expired" | string;
  client_claim_key: string;
  candidate_node_ids: string[];
  expires_at: string;
  created_at: string;
}

/** RongCloud system config entry. */
export interface RongCloudSystemConfig {
  id: string;
  workspace_id: string;
  config_key: string;
  node_id: string;
  config: Record<string, unknown>;
  config_version: number;
}

/** List response wrapper for RongCloud chatrooms. */
export interface ListRongCloudChatroomsResponse {
  chatrooms: RongCloudChatroom[];
}

/** List response wrapper for RongCloud nodes. */
export interface ListRongCloudNodesResponse {
  nodes: RongCloudNode[];
}

/** List response wrapper for RongCloud devices. */
export interface ListRongCloudDevicesResponse {
  devices: RongCloudDevice[];
}

/** List response wrapper for RongCloud discussion events. */
export interface ListRongCloudDiscussionEventsResponse {
  events: RongCloudDiscussionEvent[];
}
```

### Step 2: Add zod schemas to schemas.ts

- [x] **Step 2:** In `packages/core/api/schemas.ts`:

**Type imports** â€?after the Telegram type imports block (around line 46), add:
```typescript
import type {
  RongCloudConfig,
  RongCloudNodeRegisterRequest,
  RongCloudNodeRegisterResult,
  RongCloudNode,
  RongCloudDevice,
  RongCloudChatroom,
  RongCloudChatroomMember,
  RongCloudNodeModelCatalog,
  RongCloudDiscussionEvent,
  RongCloudDiscussionState,
  RongCloudSpeakerInfo,
  RongCloudPairingSession,
  RongCloudSystemConfig,
  ListRongCloudChatroomsResponse,
  ListRongCloudNodesResponse,
  ListRongCloudDevicesResponse,
  ListRongCloudDiscussionEventsResponse,
} from "../types/rongcloud";
```

**Schema definitions** â€?append at the end of the file (after the last schema):

```typescript
// ===== RongCloud Schemas =====

export const RongCloudConfigSchema = z
  .object({
    app_key: z.string().default(""),
    configured: z.boolean().default(false),
  })
  .loose();
export const EMPTY_RONGCLOUD_CONFIG: RongCloudConfig = { app_key: "", configured: false };

export const RongCloudNodeRegisterRequestSchema = z
  .object({
    name: z.string(),
    mac_address: z.string(),
    node_type: z.string(),
    ai_type: z.string(),
    capabilities: z.array(z.string()).default([]),
  })
  .loose();

export const RongCloudNodeRegisterResultSchema = z
  .object({
    node_id: z.string(),
    token: z.string(),
    capabilities: z.array(z.string()).default([]),
    device_credential_ticket: z.string().default(""),
    binding_version: z.number().default(0),
  })
  .loose();
export const EMPTY_RONGCLOUD_NODE_REGISTER_RESULT: RongCloudNodeRegisterResult = {
  node_id: "",
  token: "",
  capabilities: [],
  device_credential_ticket: "",
  binding_version: 0,
};

export const RongCloudNodeSchema = z
  .object({
    id: z.string().default(""),
    workspace_id: z.string().default(""),
    owner_user_id: z.string().default(""),
    rongcloud_user_id: z.string().default(""),
    node_id: z.string().default(""),
    ai_type: z.string().default(""),
    capabilities: z.array(z.string()).default([]),
    deploy_status: z.string().default(""),
    binding_version: z.number().default(0),
    created_at: z.string().default(""),
    updated_at: z.string().default(""),
  })
  .loose();
export const EMPTY_RONGCLOUD_NODE: RongCloudNode = {
  id: "",
  workspace_id: "",
  owner_user_id: "",
  rongcloud_user_id: "",
  node_id: "",
  ai_type: "",
  capabilities: [],
  deploy_status: "",
  binding_version: 0,
  created_at: "",
  updated_at: "",
};

export const RongCloudDeviceSchema = z
  .object({
    id: z.string().default(""),
    workspace_id: z.string().default(""),
    owner_user_id: z.string().default(""),
    node_id: z.string().default(""),
    device_name: z.string().default(""),
    device_type: z.string().default(""),
    credential_id: z.string().default(""),
    status: z.string().default(""),
    created_at: z.string().default(""),
    updated_at: z.string().default(""),
  })
  .loose();
export const EMPTY_RONGCLOUD_DEVICE: RongCloudDevice = {
  id: "",
  workspace_id: "",
  owner_user_id: "",
  node_id: "",
  device_name: "",
  device_type: "",
  credential_id: "",
  status: "",
  created_at: "",
  updated_at: "",
};

export const RongCloudChatroomSchema = z
  .object({
    id: z.string().default(""),
    workspace_id: z.string().default(""),
    rongcloud_chatroom_id: z.string().default(""),
    owner_user_id: z.string().default(""),
    host_node_id: z.string().default(""),
    max_rounds: z.number().default(0),
    conversation_kind: z.string().default(""),
    config: z.record(z.unknown()).default({}),
    status: z.string().default(""),
    created_at: z.string().default(""),
    updated_at: z.string().default(""),
  })
  .loose();
export const EMPTY_RONGCLOUD_CHATROOM: RongCloudChatroom = {
  id: "",
  workspace_id: "",
  rongcloud_chatroom_id: "",
  owner_user_id: "",
  host_node_id: "",
  max_rounds: 0,
  conversation_kind: "",
  config: {},
  status: "",
  created_at: "",
  updated_at: "",
};

export const RongCloudChatroomMemberSchema = z
  .object({
    id: z.string().default(""),
    chatroom_id: z.string().default(""),
    node_id: z.string().default(""),
    member_type: z.string().default(""),
    role_name: z.string().default(""),
    role_instructions: z.string().default(""),
    capabilities: z.record(z.unknown()).default({}),
    model: z.string().default(""),
    speaking_order: z.number().default(0),
    enabled: z.boolean().default(true),
    discussion_model: z.string().default(""),
  })
  .loose();

export const RongCloudNodeModelCatalogSchema = z
  .object({
    id: z.string().default(""),
    workspace_id: z.string().default(""),
    node_id: z.string().default(""),
    model_id: z.string().default(""),
    provider: z.string().default(""),
    model_name: z.string().default(""),
    config: z.record(z.unknown()).default({}),
  })
  .loose();

export const RongCloudDiscussionEventSchema = z
  .object({
    id: z.string().default(""),
    chatroom_id: z.string().default(""),
    workspace_id: z.string().default(""),
    event_type: z.string().default(""),
    round_number: z.number().default(0),
    speaking_order: z.number().default(0),
    node_id: z.string().default(""),
    content: z.record(z.unknown()).default({}),
    msg_uid: z.string().default(""),
    created_at: z.string().default(""),
  })
  .loose();

export const RongCloudSpeakerInfoSchema = z
  .object({
    node_id: z.string().default(""),
    speaking_order: z.number().default(0),
    role_name: z.string().default(""),
    model: z.string().default(""),
    rongcloud_user_id: z.string().default(""),
  })
  .loose();

export const RongCloudDiscussionStateSchema = z
  .object({
    chatroom_id: z.string().default(""),
    workspace_id: z.string().default(""),
    status: z.string().default("idle"),
    current_round: z.number().default(0),
    current_speaker: z.string().default(""),
    host_node_id: z.string().default(""),
    speakers: z.array(RongCloudSpeakerInfoSchema).default([]),
    started_at: z.string().default(""),
    ended_at: z.string().nullable().default(null),
  })
  .loose();
export const EMPTY_RONGCLOUD_DISCUSSION_STATE: RongCloudDiscussionState = {
  chatroom_id: "",
  workspace_id: "",
  status: "idle",
  current_round: 0,
  current_speaker: "",
  host_node_id: "",
  speakers: [],
  started_at: "",
  ended_at: null,
};

export const RongCloudPairingSessionSchema = z
  .object({
    id: z.string().default(""),
    workspace_id: z.string().default(""),
    ticket: z.string().default(""),
    status: z.string().default("pending"),
    client_claim_key: z.string().default(""),
    candidate_node_ids: z.array(z.string()).default([]),
    expires_at: z.string().default(""),
    created_at: z.string().default(""),
  })
  .loose();

export const RongCloudSystemConfigSchema = z
  .object({
    id: z.string().default(""),
    workspace_id: z.string().default(""),
    config_key: z.string().default(""),
    node_id: z.string().default(""),
    config: z.record(z.unknown()).default({}),
    config_version: z.number().default(0),
  })
  .loose();

export const ListRongCloudChatroomsResponseSchema = z
  .object({
    chatrooms: z.array(RongCloudChatroomSchema).default([]),
  })
  .loose();
export const EMPTY_LIST_RONGCLOUD_CHATROOMS_RESPONSE: ListRongCloudChatroomsResponse = {
  chatrooms: [],
};

export const ListRongCloudNodesResponseSchema = z
  .object({
    nodes: z.array(RongCloudNodeSchema).default([]),
  })
  .loose();
export const EMPTY_LIST_RONGCLOUD_NODES_RESPONSE: ListRongCloudNodesResponse = {
  nodes: [],
};

export const ListRongCloudDevicesResponseSchema = z
  .object({
    devices: z.array(RongCloudDeviceSchema).default([]),
  })
  .loose();
export const EMPTY_LIST_RONGCLOUD_DEVICES_RESPONSE: ListRongCloudDevicesResponse = {
  devices: [],
};

export const ListRongCloudDiscussionEventsResponseSchema = z
  .object({
    events: z.array(RongCloudDiscussionEventSchema).default([]),
  })
  .loose();
export const EMPTY_LIST_RONGCLOUD_DISCUSSION_EVENTS_RESPONSE: ListRongCloudDiscussionEventsResponse = {
  events: [],
};
```

### Step 3: Typecheck and commit

- [x] **Step 3:** Verify and commit:

```bash
pnpm typecheck
git add packages/core/types/rongcloud.ts packages/core/api/schemas.ts
git commit -m "feat(rongcloud): add TypeScript types and zod schemas for frontend"
```

---

## Task 5: Frontend â€?API Client Methods

**Files:** `packages/core/api/client.ts` (MODIFY)

### Step 1: Add type and schema imports

- [x] **Step 1:** In `packages/core/api/client.ts`:

**Type imports** â€?after the Telegram type import block (around line 202), add:
```typescript
import type {
  RongCloudConfig,
  RongCloudNodeRegisterRequest,
  RongCloudNodeRegisterResult,
  RongCloudNode,
  RongCloudDevice,
  RongCloudChatroom,
  RongCloudChatroomMember,
  RongCloudNodeModelCatalog,
  RongCloudDiscussionEvent,
  RongCloudDiscussionState,
  RongCloudPairingSession,
  RongCloudSystemConfig,
  ListRongCloudChatroomsResponse,
  ListRongCloudNodesResponse,
  ListRongCloudDevicesResponse,
  ListRongCloudDiscussionEventsResponse,
} from "../types/rongcloud";
```

**Schema imports** â€?after the Telegram schema import block (around line 374), add:
```typescript
import {
  RongCloudConfigSchema,
  EMPTY_RONGCLOUD_CONFIG,
  RongCloudNodeRegisterResultSchema,
  EMPTY_RONGCLOUD_NODE_REGISTER_RESULT,
  RongCloudNodeSchema,
  EMPTY_RONGCLOUD_NODE,
  RongCloudDeviceSchema,
  EMPTY_RONGCLOUD_DEVICE,
  RongCloudChatroomSchema,
  EMPTY_RONGCLOUD_CHATROOM,
  RongCloudNodeModelCatalogSchema,
  RongCloudDiscussionEventSchema,
  RongCloudDiscussionStateSchema,
  EMPTY_RONGCLOUD_DISCUSSION_STATE,
  RongCloudPairingSessionSchema,
  RongCloudSystemConfigSchema,
  ListRongCloudChatroomsResponseSchema,
  EMPTY_LIST_RONGCLOUD_CHATROOMS_RESPONSE,
  ListRongCloudNodesResponseSchema,
  EMPTY_LIST_RONGCLOUD_NODES_RESPONSE,
  ListRongCloudDevicesResponseSchema,
  EMPTY_LIST_RONGCLOUD_DEVICES_RESPONSE,
  ListRongCloudDiscussionEventsResponseSchema,
  EMPTY_LIST_RONGCLOUD_DISCUSSION_EVENTS_RESPONSE,
} from "./schemas";
```

### Step 2: Add API client methods

- [x] **Step 2:** Before the final closing `}` of the `ApiClient` class (around line 5063), add all RongCloud methods:

```typescript
  // ===== RongCloud =====

  async getRongCloudConfig(): Promise<RongCloudConfig> {
    const raw = await this.fetch<unknown>("/api/config/rongcloud");
    return parseWithFallback(raw, RongCloudConfigSchema, EMPTY_RONGCLOUD_CONFIG, { endpoint: "getRongCloudConfig" });
  }

  async registerRongCloudAINode(body: RongCloudNodeRegisterRequest): Promise<RongCloudNodeRegisterResult> {
    const raw = await this.fetch<unknown>("/api/ai/register", {
      method: "POST",
      body: JSON.stringify(body),
    });
    return parseWithFallback(raw, RongCloudNodeRegisterResultSchema, EMPTY_RONGCLOUD_NODE_REGISTER_RESULT, { endpoint: "registerRongCloudAINode" });
  }

  async refreshRongCloudToken(nodeId: string): Promise<{ token: string }> {
    const raw = await this.fetch<unknown>(`/api/claw/refresh-token/${encodeURIComponent(nodeId)}`, { method: "POST" });
    return raw as { token: string };
  }

  async listRongCloudChatrooms(workspaceId: string): Promise<ListRongCloudChatroomsResponse> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/chatrooms`);
    return parseWithFallback(raw, ListRongCloudChatroomsResponseSchema, EMPTY_LIST_RONGCLOUD_CHATROOMS_RESPONSE, { endpoint: "listRongCloudChatrooms" });
  }

  async getRongCloudChatroom(workspaceId: string, chatroomId: string): Promise<RongCloudChatroom> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/chatrooms/${encodeURIComponent(chatroomId)}`);
    return parseWithFallback(raw, RongCloudChatroomSchema, EMPTY_RONGCLOUD_CHATROOM, { endpoint: "getRongCloudChatroom" });
  }

  async createRongCloudChatroom(workspaceId: string, body: {
    rongcloud_chatroom_id: string;
    max_rounds?: number;
    conversation_kind?: string;
    config?: Record<string, unknown>;
  }): Promise<RongCloudChatroom> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/chatrooms`, {
      method: "POST",
      body: JSON.stringify(body),
    });
    return parseWithFallback(raw, RongCloudChatroomSchema, EMPTY_RONGCLOUD_CHATROOM, { endpoint: "createRongCloudChatroom" });
  }

  async updateRongCloudChatroom(workspaceId: string, chatroomId: string, body: {
    host_node_id?: string;
    max_rounds?: number;
    conversation_kind?: string;
    config?: Record<string, unknown>;
  }): Promise<RongCloudChatroom> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/chatrooms/${encodeURIComponent(chatroomId)}`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
    return parseWithFallback(raw, RongCloudChatroomSchema, EMPTY_RONGCLOUD_CHATROOM, { endpoint: "updateRongCloudChatroom" });
  }

  async deleteRongCloudChatroom(workspaceId: string, chatroomId: string): Promise<void> {
    await this.fetch(`/api/workspaces/${workspaceId}/rongcloud/chatrooms/${encodeURIComponent(chatroomId)}`, { method: "DELETE" });
  }

  async setRongCloudChatroomMembers(workspaceId: string, chatroomId: string, body: {
    members: Array<{
      node_id: string;
      member_type?: string;
      role_name?: string;
      role_instructions?: string;
      capabilities?: Record<string, unknown>;
      model?: string;
      speaking_order?: number;
      discussion_model?: string;
    }>;
  }): Promise<void> {
    await this.fetch(`/api/workspaces/${workspaceId}/rongcloud/chatrooms/${encodeURIComponent(chatroomId)}/members`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  async listRongCloudNodes(workspaceId: string): Promise<ListRongCloudNodesResponse> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/nodes`);
    return parseWithFallback(raw, ListRongCloudNodesResponseSchema, EMPTY_LIST_RONGCLOUD_NODES_RESPONSE, { endpoint: "listRongCloudNodes" });
  }

  async listRongCloudNodeModels(workspaceId: string, nodeId: string): Promise<RongCloudNodeModelCatalog[]> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/nodes/${encodeURIComponent(nodeId)}/models`);
    const arr = raw as unknown[];
    return (arr || []).map((item) => RongCloudNodeModelCatalogSchema.parse(item));
  }

  async addRongCloudNodeModel(workspaceId: string, nodeId: string, body: {
    model_id: string;
    provider?: string;
    model_name?: string;
    config?: Record<string, unknown>;
  }): Promise<RongCloudNodeModelCatalog> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/nodes/${encodeURIComponent(nodeId)}/models`, {
      method: "POST",
      body: JSON.stringify(body),
    });
    return RongCloudNodeModelCatalogSchema.parse(raw);
  }

  async deleteRongCloudNode(workspaceId: string, nodeId: string): Promise<void> {
    await this.fetch(`/api/workspaces/${workspaceId}/rongcloud/nodes/${encodeURIComponent(nodeId)}`, { method: "DELETE" });
  }

  async listRongCloudDevices(workspaceId: string): Promise<ListRongCloudDevicesResponse> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/devices`);
    return parseWithFallback(raw, ListRongCloudDevicesResponseSchema, EMPTY_LIST_RONGCLOUD_DEVICES_RESPONSE, { endpoint: "listRongCloudDevices" });
  }

  async createRongCloudDevice(workspaceId: string, body: {
    node_id: string;
    device_name?: string;
    device_type?: string;
  }): Promise<RongCloudDevice> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/devices`, {
      method: "POST",
      body: JSON.stringify(body),
    });
    return parseWithFallback(raw, RongCloudDeviceSchema, EMPTY_RONGCLOUD_DEVICE, { endpoint: "createRongCloudDevice" });
  }

  async deleteRongCloudDevice(workspaceId: string, deviceId: string): Promise<void> {
    await this.fetch(`/api/workspaces/${workspaceId}/rongcloud/devices/${encodeURIComponent(deviceId)}`, { method: "DELETE" });
  }

  async getRongCloudSystemHost(workspaceId: string): Promise<RongCloudSystemConfig> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/system-host`);
    return parseWithFallback(raw, RongCloudSystemConfigSchema, { id: "", workspace_id: workspaceId, config_key: "system_host", node_id: "", config: {}, config_version: 0 }, { endpoint: "getRongCloudSystemHost" });
  }

  async updateRongCloudSystemHost(workspaceId: string, body: {
    node_id: string;
    config?: Record<string, unknown>;
  }): Promise<RongCloudSystemConfig> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/system-host`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
    return parseWithFallback(raw, RongCloudSystemConfigSchema, { id: "", workspace_id: workspaceId, config_key: "system_host", node_id: "", config: {}, config_version: 0 }, { endpoint: "updateRongCloudSystemHost" });
  }

  async createRongCloudPairing(workspaceId: string, body: {
    candidate_node_ids?: string[];
    client_claim_key?: string;
    expires_in_seconds?: number;
  }): Promise<RongCloudPairingSession> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/pairing`, {
      method: "POST",
      body: JSON.stringify(body),
    });
    return RongCloudPairingSessionSchema.parse(raw);
  }

  async getRongCloudPairing(workspaceId: string, ticket: string): Promise<RongCloudPairingSession> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/pairing/${encodeURIComponent(ticket)}`);
    return RongCloudPairingSessionSchema.parse(raw);
  }

  async startRongCloudDiscussion(workspaceId: string, chatroomId: string): Promise<RongCloudDiscussionState> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/chatrooms/${encodeURIComponent(chatroomId)}/discussions`, {
      method: "POST",
    });
    return parseWithFallback(raw, RongCloudDiscussionStateSchema, EMPTY_RONGCLOUD_DISCUSSION_STATE, { endpoint: "startRongCloudDiscussion" });
  }

  async stopRongCloudDiscussion(workspaceId: string, chatroomId: string): Promise<RongCloudDiscussionState> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/chatrooms/${encodeURIComponent(chatroomId)}/discussions`, {
      method: "DELETE",
    });
    return parseWithFallback(raw, RongCloudDiscussionStateSchema, EMPTY_RONGCLOUD_DISCUSSION_STATE, { endpoint: "stopRongCloudDiscussion" });
  }

  async pauseRongCloudDiscussion(workspaceId: string, chatroomId: string): Promise<RongCloudDiscussionState> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/chatrooms/${encodeURIComponent(chatroomId)}/discussions/pause`, {
      method: "PUT",
    });
    return parseWithFallback(raw, RongCloudDiscussionStateSchema, EMPTY_RONGCLOUD_DISCUSSION_STATE, { endpoint: "pauseRongCloudDiscussion" });
  }

  async resumeRongCloudDiscussion(workspaceId: string, chatroomId: string): Promise<RongCloudDiscussionState> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/chatrooms/${encodeURIComponent(chatroomId)}/discussions/resume`, {
      method: "PUT",
    });
    return parseWithFallback(raw, RongCloudDiscussionStateSchema, EMPTY_RONGCLOUD_DISCUSSION_STATE, { endpoint: "resumeRongCloudDiscussion" });
  }

  async getRongCloudDiscussion(workspaceId: string, chatroomId: string): Promise<RongCloudDiscussionState> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/chatrooms/${encodeURIComponent(chatroomId)}/discussions`);
    return parseWithFallback(raw, RongCloudDiscussionStateSchema, EMPTY_RONGCLOUD_DISCUSSION_STATE, { endpoint: "getRongCloudDiscussion" });
  }

  async listRongCloudDiscussionEvents(workspaceId: string, chatroomId: string): Promise<ListRongCloudDiscussionEventsResponse> {
    const raw = await this.fetch<unknown>(`/api/workspaces/${workspaceId}/rongcloud/chatrooms/${encodeURIComponent(chatroomId)}/discussions/events`);
    return parseWithFallback(raw, ListRongCloudDiscussionEventsResponseSchema, EMPTY_LIST_RONGCLOUD_DISCUSSION_EVENTS_RESPONSE, { endpoint: "listRongCloudDiscussionEvents" });
  }
```

### Step 3: Typecheck and commit

- [x] **Step 3:** Verify and commit:

```bash
pnpm typecheck
git add packages/core/api/client.ts
git commit -m "feat(rongcloud): add API client methods for all RongCloud endpoints"
```

---

## Task 6: Frontend â€?React Query Hooks

**Files:** `packages/core/rongcloud/queries.ts` (NEW), `packages/core/rongcloud/index.ts` (NEW)

### Step 1: Create queries.ts

- [x] **Step 1:** Create `packages/core/rongcloud/queries.ts`:

```typescript
import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const rongcloudKeys = {
  all: (wsId: string) => ["rongcloud", wsId] as const,
  config: () => ["rongcloud", "config"] as const,
  chatrooms: (wsId: string) => [...rongcloudKeys.all(wsId), "chatrooms"] as const,
  chatroom: (wsId: string, chatroomId: string) => [...rongcloudKeys.chatrooms(wsId), chatroomId] as const,
  nodes: (wsId: string) => [...rongcloudKeys.all(wsId), "nodes"] as const,
  nodeModels: (wsId: string, nodeId: string) => [...rongcloudKeys.nodes(wsId), nodeId, "models"] as const,
  devices: (wsId: string) => [...rongcloudKeys.all(wsId), "devices"] as const,
  systemHost: (wsId: string) => [...rongcloudKeys.all(wsId), "system-host"] as const,
  discussion: (wsId: string, chatroomId: string) => [...rongcloudKeys.chatroom(wsId, chatroomId), "discussion"] as const,
  discussionEvents: (wsId: string, chatroomId: string) => [...rongcloudKeys.discussion(wsId, chatroomId), "events"] as const,
};

export const rongcloudConfigOptions = () =>
  queryOptions({
    queryKey: rongcloudKeys.config(),
    queryFn: () => api.getRongCloudConfig(),
  });

export const rongcloudChatroomsOptions = (wsId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.chatrooms(wsId),
    queryFn: () => api.listRongCloudChatrooms(wsId),
    enabled: !!wsId,
  });

export const rongcloudNodesOptions = (wsId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.nodes(wsId),
    queryFn: () => api.listRongCloudNodes(wsId),
    enabled: !!wsId,
  });

export const rongcloudDevicesOptions = (wsId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.devices(wsId),
    queryFn: () => api.listRongCloudDevices(wsId),
    enabled: !!wsId,
  });

export const rongcloudDiscussionOptions = (wsId: string, chatroomId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.discussion(wsId, chatroomId),
    queryFn: () => api.getRongCloudDiscussion(wsId, chatroomId),
    enabled: !!wsId && !!chatroomId,
    refetchInterval: 3000,
  });

export const rongcloudDiscussionEventsOptions = (wsId: string, chatroomId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.discussionEvents(wsId, chatroomId),
    queryFn: () => api.listRongCloudDiscussionEvents(wsId, chatroomId),
    enabled: !!wsId && !!chatroomId,
    refetchInterval: 3000,
  });
```

### Step 2: Create index.ts

- [x] **Step 2:** Create `packages/core/rongcloud/index.ts`:

```typescript
export {
  rongcloudKeys,
  rongcloudConfigOptions,
  rongcloudChatroomsOptions,
  rongcloudNodesOptions,
  rongcloudDevicesOptions,
  rongcloudDiscussionOptions,
  rongcloudDiscussionEventsOptions,
} from "./queries";
```

### Step 3: Typecheck and commit

- [x] **Step 3:** Verify and commit:

```bash
pnpm typecheck
git add packages/core/rongcloud/queries.ts packages/core/rongcloud/index.ts
git commit -m "feat(rongcloud): add React Query hooks for RongCloud endpoints"
```

---

## Task 7: Frontend â€?RongCloud Settings Tab

**Files:** `packages/views/settings/components/rongcloud-tab.tsx` (NEW), `packages/views/settings/components/integrations-tab.tsx` (MODIFY)

### Step 1: Create rongcloud-tab.tsx

- [x] **Step 1:** Create `packages/views/settings/components/rongcloud-tab.tsx`:

```tsx
"use client";

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@multica/ui";
import { Card, CardContent } from "@multica/ui";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { rongcloudConfigOptions, rongcloudChatroomsOptions, rongcloudNodesOptions } from "@multica/core/rongcloud";
import { api } from "@multica/core/api";
import { useT } from "../../i18n";

export function RongCloudTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const { user } = useAuthStore();

  const configQuery = useQuery({ ...rongcloudConfigOptions() });
  const chatroomsQuery = useQuery({ ...rongcloudChatroomsOptions(wsId), enabled: !!wsId });
  const nodesQuery = useQuery({ ...rongcloudNodesOptions(wsId), enabled: !!wsId });

  const configured = configQuery.data?.configured ?? false;

  return (
    <div className="space-y-4">
      <Card>
        <CardContent className="pt-6">
          <h3 className="text-title">{t(($) => $.rongcloud.status_title)}</h3>
          <p className="text-body text-muted-foreground">
            {configured
              ? t(($) => $.rongcloud.configured_description)
              : t(($) => $.rongcloud.not_configured_description)}
          </p>
          {configured && (
            <p className="text-caption text-muted-foreground">
              App Key: {configQuery.data?.app_key}
            </p>
          )}
        </CardContent>
      </Card>

      {configured && (
        <>
          <Card>
            <CardContent className="pt-6">
              <h3 className="text-title">{t(($) => $.rongcloud.chatrooms_title)}</h3>
              {chatroomsQuery.isLoading ? (
                <p className="text-body text-muted-foreground">Loading...</p>
              ) : (chatroomsQuery.data?.chatrooms?.length ?? 0) === 0 ? (
                <p className="text-body text-muted-foreground">{t(($) => $.rongcloud.no_chatrooms)}</p>
              ) : (
                <ul className="space-y-2">
                  {chatroomsQuery.data?.chatrooms?.map((cr) => (
                    <li key={cr.id} className="flex items-center justify-between">
                      <span className="text-body">{cr.rongcloud_chatroom_id}</span>
                      <span className="text-caption text-muted-foreground">{cr.status}</span>
                    </li>
                  ))}
                </ul>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardContent className="pt-6">
              <h3 className="text-title">{t(($) => $.rongcloud.nodes_title)}</h3>
              {nodesQuery.isLoading ? (
                <p className="text-body text-muted-foreground">Loading...</p>
              ) : (nodesQuery.data?.nodes?.length ?? 0) === 0 ? (
                <p className="text-body text-muted-foreground">{t(($) => $.rongcloud.no_nodes)}</p>
              ) : (
                <ul className="space-y-2">
                  {nodesQuery.data?.nodes?.map((node) => (
                    <li key={node.id} className="flex items-center justify-between">
                      <div>
                        <span className="text-body">{node.node_id}</span>
                        <span className="text-caption text-muted-foreground ml-2">{node.ai_type}</span>
                      </div>
                      <span className="text-caption text-muted-foreground">{node.deploy_status}</span>
                    </li>
                  ))}
                </ul>
              )}
            </CardContent>
          </Card>
        </>
      )}
    </div>
  );
}
```

### Step 2: Wire into integrations-tab.tsx

- [x] **Step 2:** In `packages/views/settings/components/integrations-tab.tsx`:

**Imports** â€?after the Telegram import (around line 18), add:
```typescript
import { rongcloudConfigOptions } from "@multica/core/rongcloud";
```

**Tab import** â€?after the TelegramTab import (around line 31), add:
```typescript
import { RongCloudTab } from "./rongcloud-tab";
```

**useQuery** â€?after the telegram useQuery (around line 109), add:
```typescript
  const rongcloud = useQuery({
    ...rongcloudConfigOptions(),
    enabled: canView,
    select: (data) => ({ ...data, installations: data.configured ? [{ id: "active", status: "active" }] : [] }),
  });
```

**Messaging group entry** â€?after the telegram entry (around line 195), add:
```typescript
            {
              id: "rongcloud",
              label: t(($) => $.rongcloud.section_title),
              description: t(($) => $.rongcloud.page_description),
              icon: <IntegrationChannelIcon channel="rongcloud" />,
              content: <RongCloudTab />,
              state: rongcloud,
            },
```

### Step 3: Typecheck, lint, and commit

- [x] **Step 3:** Verify and commit:

```bash
pnpm typecheck
pnpm lint
git add packages/views/settings/components/rongcloud-tab.tsx \
       packages/views/settings/components/integrations-tab.tsx
git commit -m "feat(rongcloud): add RongCloud settings tab and wire into integrations"
```

---

## Task 8: Frontend â€?Discussion Monitor View

**Files:** `packages/views/rongcloud/discussion-monitor.tsx` (NEW), `packages/views/rongcloud/chatroom-manager.tsx` (NEW)

### Step 1: Create chatroom-manager.tsx

- [x] **Step 1:** Create `packages/views/rongcloud/chatroom-manager.tsx`:

```tsx
"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button, Card, CardContent, Input, Label } from "@multica/ui";
import { useWorkspaceId } from "@multica/core/hooks";
import { rongcloudChatroomsOptions, rongcloudKeys } from "@multica/core/rongcloud";
import { api } from "@multica/core/api";
import { useT } from "../i18n";

export function ChatroomManager() {
  const { t } = useT();
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const [newChatroomId, setNewChatroomId] = useState("");

  const chatroomsQuery = useQuery({ ...rongcloudChatroomsOptions(wsId), enabled: !!wsId });

  const handleCreate = async () => {
    if (!newChatroomId.trim()) return;
    try {
      await api.createRongCloudChatroom(wsId, { rongcloud_chatroom_id: newChatroomId.trim() });
      qc.invalidateQueries({ queryKey: rongcloudKeys.chatrooms(wsId) });
      setNewChatroomId("");
      toast.success("Chatroom created");
    } catch (e) {
      toast.error("Failed to create chatroom");
    }
  };

  const handleDelete = async (chatroomId: string) => {
    try {
      await api.deleteRongCloudChatroom(wsId, chatroomId);
      qc.invalidateQueries({ queryKey: rongcloudKeys.chatrooms(wsId) });
      toast.success("Chatroom deleted");
    } catch (e) {
      toast.error("Failed to delete chatroom");
    }
  };

  return (
    <Card>
      <CardContent className="pt-6 space-y-4">
        <h3 className="text-title">Chatrooms</h3>
        <div className="flex gap-2">
          <Input
            placeholder="RongCloud Chatroom ID"
            value={newChatroomId}
            onChange={(e) => setNewChatroomId(e.target.value)}
          />
          <Button onClick={handleCreate} disabled={!newChatroomId.trim()}>Create</Button>
        </div>
        {chatroomsQuery.isLoading ? (
          <p className="text-body text-muted-foreground">Loading...</p>
        ) : (chatroomsQuery.data?.chatrooms?.length ?? 0) === 0 ? (
          <p className="text-body text-muted-foreground">No chatrooms yet.</p>
        ) : (
          <ul className="space-y-2">
            {chatroomsQuery.data?.chatrooms?.map((cr) => (
              <li key={cr.id} className="flex items-center justify-between">
                <div>
                  <span className="text-body">{cr.rongcloud_chatroom_id}</span>
                  <span className="text-caption text-muted-foreground ml-2">
                    Round {cr.max_rounds} Â· {cr.conversation_kind || "default"}
                  </span>
                </div>
                <Button variant="destructive" size="sm" onClick={() => handleDelete(cr.id)}>
                  Delete
                </Button>
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}
```

### Step 2: Create discussion-monitor.tsx

- [x] **Step 2:** Create `packages/views/rongcloud/discussion-monitor.tsx`:

```tsx
"use client";

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button, Card, CardContent, Badge } from "@multica/ui";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  rongcloudDiscussionOptions,
  rongcloudDiscussionEventsOptions,
  rongcloudKeys,
} from "@multica/core/rongcloud";
import { api } from "@multica/core/api";

export function DiscussionMonitor({ chatroomId }: { chatroomId: string }) {
  const wsId = useWorkspaceId();
  const qc = useQueryClient();

  const discussionQuery = useQuery({
    ...rongcloudDiscussionOptions(wsId, chatroomId),
    enabled: !!wsId && !!chatroomId,
  });
  const eventsQuery = useQuery({
    ...rongcloudDiscussionEventsOptions(wsId, chatroomId),
    enabled: !!wsId && !!chatroomId,
  });

  const status = discussionQuery.data?.status ?? "idle";
  const isRunning = status === "in_progress" || status === "starting";

  const handleStart = async () => {
    try {
      await api.startRongCloudDiscussion(wsId, chatroomId);
      qc.invalidateQueries({ queryKey: rongcloudKeys.discussion(wsId, chatroomId) });
      toast.success("Discussion started");
    } catch (e) {
      toast.error("Failed to start discussion");
    }
  };

  const handleStop = async () => {
    try {
      await api.stopRongCloudDiscussion(wsId, chatroomId);
      qc.invalidateQueries({ queryKey: rongcloudKeys.discussion(wsId, chatroomId) });
      toast.success("Discussion stopped");
    } catch (e) {
      toast.error("Failed to stop discussion");
    }
  };

  const handlePause = async () => {
    try {
      await api.pauseRongCloudDiscussion(wsId, chatroomId);
      qc.invalidateQueries({ queryKey: rongcloudKeys.discussion(wsId, chatroomId) });
      toast.success("Discussion paused");
    } catch (e) {
      toast.error("Failed to pause discussion");
    }
  };

  const handleResume = async () => {
    try {
      await api.resumeRongCloudDiscussion(wsId, chatroomId);
      qc.invalidateQueries({ queryKey: rongcloudKeys.discussion(wsId, chatroomId) });
      toast.success("Discussion resumed");
    } catch (e) {
      toast.error("Failed to resume discussion");
    }
  };

  return (
    <div className="space-y-4">
      <Card>
        <CardContent className="pt-6 space-y-4">
          <div className="flex items-center gap-2">
            <h3 className="text-title">Discussion</h3>
            <Badge variant={isRunning ? "default" : "secondary"}>{status}</Badge>
          </div>

          {discussionQuery.data && (
            <div className="space-y-1">
              <p className="text-caption text-muted-foreground">
                Round {discussionQuery.data.current_round} / Speaker:{" "}
                {discussionQuery.data.current_speaker || "â€?}
              </p>
              <p className="text-caption text-muted-foreground">
                Speakers: {discussionQuery.data.speakers?.length ?? 0}
              </p>
            </div>
          )}

          <div className="flex gap-2">
            {!isRunning && status !== "paused" && (
              <Button onClick={handleStart} size="sm">Start</Button>
            )}
            {isRunning && (
              <>
                <Button onClick={handlePause} size="sm" variant="outline">Pause</Button>
                <Button onClick={handleStop} size="sm" variant="destructive">Stop</Button>
              </>
            )}
            {status === "paused" && (
              <Button onClick={handleResume} size="sm">Resume</Button>
            )}
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardContent className="pt-6">
          <h3 className="text-title mb-3">Events</h3>
          {eventsQuery.isLoading ? (
            <p className="text-body text-muted-foreground">Loading...</p>
          ) : (eventsQuery.data?.events?.length ?? 0) === 0 ? (
            <p className="text-body text-muted-foreground">No events yet.</p>
          ) : (
            <ul className="space-y-1 max-h-96 overflow-y-auto">
              {eventsQuery.data?.events?.map((ev) => (
                <li key={ev.id} className="text-caption flex items-center gap-2">
                  <span className="font-mono text-xs">{ev.event_type}</span>
                  <span className="text-muted-foreground">
                    R{ev.round_number} S{ev.speaking_order}
                  </span>
                  <span className="text-muted-foreground">{ev.created_at}</span>
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
```

### Step 3: Typecheck, lint, and commit

- [x] **Step 3:** Verify and commit:

```bash
pnpm typecheck
pnpm lint
git add packages/views/rongcloud/discussion-monitor.tsx \
       packages/views/rongcloud/chatroom-manager.tsx
git commit -m "feat(rongcloud): add chatroom manager and discussion monitor views"
```

---

## Task 9: Backend Tests for Bridge

**Files:** `server/internal/integrations/rongcloud/rongcloud_test.go` (MODIFY)

### Step 1: Add bridge tests

- [x] **Step 1:** Add tests to `server/internal/integrations/rongcloud/rongcloud_test.go`:

```go
func TestDiscussionBridgeIsServerManagedByType(t *testing.T) {
	bridge := &DiscussionBridge{
		agents: map[string]string{"claude": "/usr/bin/claude"},
	}
	if !bridge.IsServerManagedByType("claude") {
		t.Error("expected claude to be server-managed")
	}
	if bridge.IsServerManagedByType("nonexistent") {
		t.Error("expected nonexistent to not be server-managed")
	}
	if bridge.IsServerManagedByType("") {
		t.Error("expected empty string to not be server-managed")
	}
}

func TestDiscussionBridgeIsServerManagedEmpty(t *testing.T) {
	bridge := &DiscussionBridge{
		agents: map[string]string{},
	}
	if bridge.IsServerManagedByType("claude") {
		t.Error("expected no agents to be server-managed")
	}
}

func TestNewDiscussionBridgeNilLogger(t *testing.T) {
	b := NewDiscussionBridge(nil, nil)
	if b == nil {
		t.Fatal("expected non-nil bridge")
	}
	if b.logger == nil {
		t.Error("expected default logger")
	}
}

func TestNewDiscussionServiceWithBridge(t *testing.T) {
	svc := NewDiscussionService(nil, nil, nil, nil, nil)
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
}

func TestHandleDiscussionCommandYourTurn(t *testing.T) {
	// your_turn with nil client should not panic
	h := &systemHandler{
		queries:  nil,
		client:   nil,
		nodeID:   "test_node",
		logger:   slog.Default(),
		registry: nil,
	}
	msg := NormalizedMessage{
		ObjectName:  objectNameCommand,
		FromUserID:  "test_user",
		Content:     `{"request_id":"req_1","service":"discussion","action":"your_turn","params":{}}`,
	}
	ctx := context.Background()
	// Should not panic even with nil client
	h.handleDiscussionCommand(ctx, msg, CommandContent{
		RequestID: "req_1",
		Service:   "discussion",
		Action:    "your_turn",
		Params:    map[string]interface{}{},
	})
}
```

### Step 2: Build, test, and commit

- [x] **Step 2:** Verify and commit:

```bash
cd server
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./...
go test ./internal/integrations/rongcloud/... -count=1
git add server/internal/integrations/rongcloud/rongcloud_test.go
git commit -m "test(rongcloud): add DiscussionBridge and your_turn ack tests"
```

---

## Task 10: Sync Plan Checkboxes + Final Verification

### Step 1: Sync Phase 4 plan checkboxes

- [x] **Step 1:** After all tasks are complete, sync all checkboxes from `- [x]` to `- [x]` in the Phase 4 plan document:

```powershell
$content = Get-Content "docs\superpowers\plans\2026-09-24-rongcloud-phase4-bridge-frontend.md" -Raw
$content = $content -replace '- \[ \]', '- [x]'
Set-Content "docs\superpowers\plans\2026-09-24-rongcloud-phase4-bridge-frontend.md" $content
```

### Step 2: Final build and test verification

- [x] **Step 2:** Run full verification:

```bash
# Backend
cd server
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./...
go test ./internal/integrations/rongcloud/... -count=1

# Frontend
cd ..
pnpm typecheck
pnpm lint
```

### Step 3: Commit

- [x] **Step 3:** Commit checkbox sync:

```bash
git add docs/superpowers/plans/2026-09-24-rongcloud-phase4-bridge-frontend.md
git commit -m "chore: sync Phase 4 plan checkboxes after completion"
```

---

## Self-Review Checklist

- [x] `go build ./...` passes from `server/`
- [x] `go test ./internal/integrations/rongcloud/... -count=1` passes
- [x] `pnpm typecheck` passes
- [x] `pnpm lint` passes
- [x] No new DB tables (Phase 4 uses existing 9 tables)
- [x] No foreign keys introduced
- [x] DiscussionBridge discovers agent CLIs via `exec.LookPath`
- [x] `IsServerManaged` checks `ai_type` against discovered CLIs
- [x] `ExecuteTurn` uses 120s timeout
- [x] Coordinator `sendYourTurn` checks bridge before falling back to RongCloud API
- [x] `system_handler.handleDiscussionCommand` handles "your_turn" with no-op ack
- [x] `NewDiscussionService` signature includes `bridge *DiscussionBridge`
- [x] `NewDiscussionCoordinator` signature includes `bridge *DiscussionBridge`
- [x] `SpeakerInfo` has `AiType` field populated from `db.RongcloudNode.AiType`
- [x] Router creates `rcBridge` and passes to `NewDiscussionService`
- [x] All test callers pass `nil` for bridge param
- [x] Frontend types file `packages/core/types/rongcloud.ts` created with 15+ interfaces
- [x] Frontend schemas added to `schemas.ts` with `.loose()` and defaults
- [x] Frontend API client methods follow `parseWithFallback` pattern
- [x] Frontend React Query hooks follow `queryOptions` pattern with `rongcloudKeys` namespace
- [x] Frontend `RongCloudTab` follows TelegramTab pattern
- [x] `integrations-tab.tsx` includes RongCloud entry in messaging group
- [x] Discussion monitor polls every 3 seconds via `refetchInterval`
- [x] All commits use `feat(rongcloud):` or `test(rongcloud):` conventional commit format
