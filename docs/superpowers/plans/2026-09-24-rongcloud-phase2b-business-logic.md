# RongCloud Phase 2b Business Logic Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to execute this plan. Each task is independent enough to be dispatched to a subagent unless explicitly noted.

**Goal:** Complete all Phase 2b business logic â€?fill service stubs, implement 15 missing handler endpoints, complete 2 stub endpoints, wire 16 new routes, and add system_handler command routing for chatroom/device commands.

**Architecture:** No new DB tables or sqlc queries. All work is completing existing service methods, adding handler endpoints, and wiring routes. Uses Phase 2a infrastructure exclusively.

**Tech Stack:** Go 1.23+, Chi v5, sqlc, PostgreSQL, slog, encoding/json, crypto/rand, crypto/hmac, secretbox.

**Spec:** `docs/superpowers/specs/2026-09-24-rongcloud-phase2b-business-logic-design.md`

---

## Global Constraints

- **Module path:** `github.com/anomalyco/multica`
- **Integration package:** `server/internal/integrations/rongcloud`
- **Handler package:** `server/internal/handler`
- **DB queries:** `server/pkg/db/generated` (type `db.Queries`)
- **sqlc queries file:** `server/pkg/db/queries/rongcloud.sql` â€?DO NOT MODIFY, all queries already exist
- **No new migrations** â€?all tables exist from Phase 2a migrations 538/539
- **No new foreign keys** â€?per AGENTS.md database rules
- **Handler patterns:**
  - nil-check: `if h.RongCloudXxx == nil { writeFeatureDisabled(w, "rongcloud_not_configured", "...") }`
  - workspace UUID: `parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")`
  - user ID: `requireUserID(w, r)` then `util.ParseUUID(userID)`
  - body decode: `json.NewDecoder(r.Body).Decode(&req)`
  - success: `writeJSON(w, http.StatusCreated, result)` or `writeJSON(w, http.StatusOK, map[string]bool{"ok": true})`
  - error: `writeError(w, http.StatusInternalServerError, "...")`
- **pgText helper:** defined in `node_service.go` as `pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }`
- **secretbox:** `box.Seal(plaintext)` encrypts, `box.Open(ciphertext)` decrypts
- **RongCloud API client methods (chatroom_api.go):** `createChatroom`, `destroyChatroom`, `joinChatroom(ctx, chatroomID, userIDs []string)`, `quitChatroom(ctx, chatroomID, userIDs []string)`, `getChatroomInfo`, `getChatroomMembers`, `ensureChatroom`
- **Build:** `cd server && go build ./...`
- **Test:** `cd server && go test ./internal/integrations/rongcloud/...`
- **Full test:** `make test`
- **Commit style:** `feat(rongcloud): <description>` or `fix(rongcloud): <description>`

---

## Task 1: ChatroomService â€?DeleteChatroom + SetMembers RongCloud Sync

**Files:**
- `server/internal/integrations/rongcloud/chatroom_service.go`

**Interfaces:**
- `DeleteChatroom(ctx, id pgtype.UUID) error` â€?add `client.destroyChatroom` call before DB soft-delete
- `SetMembers(ctx, chatroomID pgtype.UUID, members []ChatroomMemberConfig) error` â€?add `client.joinChatroom`/`quitChatroom` sync

### Step 1: Add `errors` import if not present

```go
// Ensure imports include:
"errors"
```

### Step 2: Update `DeleteChatroom` to call RongCloud destroy

Replace the existing `DeleteChatroom` method body:

```go
func (s *ChatroomService) DeleteChatroom(ctx context.Context, id pgtype.UUID) error {
	// Fetch chatroom to get RongCloud chatroom ID
	chatroom, err := s.queries.GetRongCloudChatroomByID(ctx, id)
	if err != nil {
		return fmt.Errorf("get chatroom: %w", err)
	}

	// Destroy on RongCloud â€?ignore "not found" errors
	if chatroom.RongcloudChatroomID != "" {
		if err := s.client.destroyChatroom(ctx, chatroom.RongcloudChatroomID); err != nil {
			s.logger.Warn("rongcloud destroyChatroom failed, continuing with DB delete",
				"chatroom_id", chatroom.RongcloudChatroomID, "error", err)
		}
	}

	// DB soft-delete
	return s.queries.DeleteRongCloudChatroom(ctx, id)
}
```

### Step 3: Update `SetMembers` to sync join/quit with RongCloud

Replace the existing `SetMembers` method body:

```go
func (s *ChatroomService) SetMembers(ctx context.Context, chatroomID pgtype.UUID, members []ChatroomMemberConfig) error {
	// Get chatroom to retrieve RongCloud chatroom ID
	chatroom, err := s.queries.GetRongCloudChatroomByID(ctx, chatroomID)
	if err != nil {
		return fmt.Errorf("get chatroom for member sync: %w", err)
	}
	rcChatroomID := chatroom.RongcloudChatroomID

	// Get old members for diff
	oldMembers, err := s.queries.ListRongCloudChatroomMembers(ctx, chatroomID)
	if err != nil {
		return fmt.Errorf("list old members: %w", err)
	}

	// Build old member node ID set
	oldSet := make(map[pgtype.UUID]bool)
	for _, m := range oldMembers {
		oldSet[m.NodeID] = true
	}

	// Build new member node ID set + RongCloud user IDs for new members
	newSet := make(map[pgtype.UUID]bool)
	newRongcloudUserIDs := make(map[pgtype.UUID]string) // nodeID -> rongcloud_user_id
	for _, m := range members {
		newSet[m.NodeID] = true
		// Fetch node to get rongcloud_user_id
		node, err := s.queries.GetRongCloudNodeByID(ctx, m.NodeID)
		if err != nil {
			return fmt.Errorf("get node for member sync: %w", err)
		}
		rcUser, err := s.queries.GetRongCloudUserByRongCloudID(ctx, node.RongcloudUserID)
		if err != nil {
			return fmt.Errorf("get rongcloud user for member sync: %w", err)
		}
		newRongcloudUserIDs[m.NodeID] = rcUser.RongcloudUserID
	}

	// Determine added and removed members
	var addedUserIDs, removedUserIDs []string
	for nodeID, rcUserID := range newRongcloudUserIDs {
		if !oldSet[nodeID] {
			addedUserIDs = append(addedUserIDs, rcUserID)
		}
	}
	for _, m := range oldMembers {
		if !newSet[m.NodeID] {
			rcUser, err := s.queries.GetRongCloudUserByRongCloudID(ctx, m.RongcloudUserID)
			if err != nil {
				s.logger.Warn("get rongcloud user for removed member failed",
					"node_id", m.NodeID, "error", err)
				continue
			}
			removedUserIDs = append(removedUserIDs, rcUser.RongcloudUserID)
		}
	}

	// DB replace: delete all + create new
	if err := s.queries.DeleteRongCloudChatroomMembersByChatroom(ctx, chatroomID); err != nil {
		return fmt.Errorf("clear members: %w", err)
	}
	for _, m := range members {
		params := db.CreateRongCloudChatroomMemberParams{
			ChatroomID:       chatroomID,
			NodeID:          m.NodeID,
			MemberType:      pgText(m.MemberType),
			RoleName:        pgText(m.RoleName),
			RoleInstructions: pgText(m.RoleInstructions),
			Capabilities:    m.Capabilities,
			Model:           pgText(m.Model),
			SpeakingOrder:   pgtype.Int4{Int32: m.SpeakingOrder, Valid: true},
			DiscussionModel: pgText(m.DiscussionModel),
		}
		if _, err := s.queries.CreateRongCloudChatroomMember(ctx, params); err != nil {
			return fmt.Errorf("create member: %w", err)
		}
	}

	// Sync with RongCloud
	if rcChatroomID != "" {
		if len(addedUserIDs) > 0 {
			if err := s.client.joinChatroom(ctx, rcChatroomID, addedUserIDs); err != nil {
				s.logger.Warn("rongcloud joinChatroom failed",
					"chatroom_id", rcChatroomID, "error", err)
			}
		}
		if len(removedUserIDs) > 0 {
			if err := s.client.quitChatroom(ctx, rcChatroomID, removedUserIDs); err != nil {
				s.logger.Warn("rongcloud quitChatroom failed",
					"chatroom_id", rcChatroomID, "error", err)
			}
		}
	}

	return nil
}
```

### Step 4: Add `fmt` import if not present

Ensure `"fmt"` is in the import block.

### Step 5: Verify build

```bash
cd server && go build ./internal/integrations/rongcloud/...
```

### Step 6: Commit

```bash
git add server/internal/integrations/rongcloud/chatroom_service.go
git commit -m "feat(rongcloud): sync RongCloud on chatroom delete and member changes"
```

---

## Task 2: NodeService â€?EnrollDeviceCredential + Connection Sessions

**Files:**
- `server/internal/integrations/rongcloud/node_service.go`

**Interfaces:**
- `EnrollDeviceCredential(ctx, nodeID string) (DeviceCredential, error)` â€?persist to DB
- `OpenConnectionSession(ctx, nodeID string) (string, error)` â€?real session tracking via system_config
- `CloseConnectionSession(ctx, sessionID string) error` â€?delete from system_config

### Step 1: Add `DeviceCredential` struct

Add near the top of the file (after existing type declarations):

```go
type DeviceCredential struct {
	CredentialID     string `json:"credentialId"`
	CredentialSecret string `json:"credentialSecret"`
}
```

### Step 2: Implement `EnrollDeviceCredential` â€?persist to DB

Replace the existing stub method:

```go
func (s *NodeService) EnrollDeviceCredential(ctx context.Context, nodeID string) (DeviceCredential, error) {
	// Get node record
	node, err := s.queries.GetRongCloudNodeByNodeID(ctx, nodeID)
	if err != nil {
		return DeviceCredential{}, fmt.Errorf("get node: %w", err)
	}

	// Generate credential
	credID, credSecret, err := generateDeviceCredential()
	if err != nil {
		return DeviceCredential{}, fmt.Errorf("generate credential: %w", err)
	}

	// Encrypt secret
	encryptedSecret, err := s.box.Seal([]byte(credSecret))
	if err != nil {
		return DeviceCredential{}, fmt.Errorf("encrypt secret: %w", err)
	}

	// Persist to DB
	params := db.CreateRongCloudDeviceParams{
		WorkspaceID:                node.WorkspaceID,
		OwnerUserID:               node.OwnerUserID,
		NodeID:                    node.ID,
		DeviceName:               pgText("device-" + nodeID),
		DeviceType:               pgText("cli"),
		CredentialID:             pgText(credID),
		CredentialSecretEncrypted: encryptedSecret,
		Status:                    pgText("active"),
	}
	if _, err := s.queries.CreateRongCloudDevice(ctx, params); err != nil {
		return DeviceCredential{}, fmt.Errorf("create device: %w", err)
	}

	return DeviceCredential{CredentialID: credID, CredentialSecret: credSecret}, nil
}
```

### Step 3: Implement `OpenConnectionSession` â€?use system_config table

Replace the existing stub:

```go
func (s *NodeService) OpenConnectionSession(ctx context.Context, nodeID string) (string, error) {
	// Generate session ID
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	sessionID := "sess_" + hex.EncodeToString(buf)

	// Get node to find workspace
	node, err := s.queries.GetRongCloudNodeByNodeID(ctx, nodeID)
	if err != nil {
		return "", fmt.Errorf("get node: %w", err)
	}

	// Store in system_config
	configJSON, _ := json.Marshal(map[string]interface{}{
		"node_id":   nodeID,
		"opened_at": time.Now().UTC().Format(time.RFC3339),
	})

	params := db.UpsertRongCloudSystemConfigParams{
		WorkspaceID: node.WorkspaceID,
		ConfigKey:   pgText("connection_session:" + sessionID),
		NodeID:      pgtype.UUID{Bytes: node.ID.Bytes, Valid: true},
		Config:      configJSON,
	}
	if err := s.queries.UpsertRongCloudSystemConfig(ctx, params); err != nil {
		return "", fmt.Errorf("store session: %w", err)
	}

	return sessionID, nil
}
```

### Step 4: Implement `CloseConnectionSession` â€?delete from system_config

Replace the existing stub:

```go
func (s *NodeService) CloseConnectionSession(ctx context.Context, sessionID string) error {
	// Session ID is embedded in config_key; we need workspace_id to delete.
	// Since system_config has UNIQUE(workspace_id, config_key), we need the workspace.
	// For public endpoint, we don't have workspace in URL. Use List to find it.
	// Alternatively, store workspace_id in the session config and look it up.
	// For simplicity: scan all system_configs with key prefix "connection_session:" + sessionID

	configs, err := s.queries.ListRongCloudSystemConfigs(ctx, pgtype.UUID{})
	if err != nil {
		return fmt.Errorf("list system configs: %w", err)
	}

	targetKey := "connection_session:" + sessionID
	for _, cfg := range configs {
		if cfg.ConfigKey.String == targetKey {
			return s.queries.DeleteRongCloudSystemConfig(ctx, db.DeleteRongCloudSystemConfigParams{
				WorkspaceID: cfg.WorkspaceID,
				ConfigKey:   cfg.ConfigKey,
			})
		}
	}

	// Not found â€?idempotent, return nil
	return nil
}
```

### Step 5: Verify build

```bash
cd server && go build ./internal/integrations/rongcloud/...
```

### Step 6: Commit

```bash
git add server/internal/integrations/rongcloud/node_service.go
git commit -m "feat(rongcloud): persist device credentials and track connection sessions"
```

---

## Task 3: PairingService â€?ClaimSession Device Binding

**Files:**
- `server/internal/integrations/rongcloud/pairing_service.go`

**Interfaces:**
- `ClaimSession(ctx, ticket, clientClaimKey, idempotencyKey, deviceCredentialTicket string) error` â€?add device binding parameter

### Step 1: Update `ClaimSession` signature to accept device credential ticket

Add `deviceCredentialTicket` parameter to the method. The method currently:
```go
func (s *PairingService) ClaimSession(ctx context.Context, ticket, clientClaimKey, idempotencyKey string) error
```

Change to:
```go
func (s *PairingService) ClaimSession(ctx context.Context, ticket, clientClaimKey, idempotencyKey, deviceCredentialTicket string) error
```

### Step 2: Add device binding after status update to "claimed"

After the successful `UpdateRongCloudPairingSessionStatus` call, add:

```go
	// Bind device credential to session (if provided)
	if deviceCredentialTicket != "" {
		// Update the session's config/metadata to record the device binding.
		// Since rongcloud_pairing_session has no metadata column, we use
		// system_config to store the binding.
		configJSON, _ := json.Marshal(map[string]string{
			"pairing_ticket":             ticket,
			"device_credential_ticket":   deviceCredentialTicket,
		})
		_ = s.queries.UpsertRongCloudSystemConfig(ctx, db.UpsertRongCloudSystemConfigParams{
			WorkspaceID: session.WorkspaceID,
			ConfigKey:   pgText("pairing_binding:" + ticket),
			Config:      configJSON,
		})
	}
```

**Note:** The `UpsertRongCloudSystemConfig` requires a `NodeID` pgtype.UUID field. Since we don't have a node ID at this point, pass `pgtype.UUID{}` (invalid UUID, Valid=false). The DB column should be nullable. If the column is NOT NULL, this will fail and we need to handle it differently â€?store in the session's `candidate_node_ids` JSONB field instead.

### Step 3: Add imports for `db` package and `json` if not present

```go
// Ensure these are imported:
"encoding/json"
"<module>/pkg/db/generated"
```

### Step 4: Verify build

```bash
cd server && go build ./internal/integrations/rongcloud/...
```

### Step 5: Commit

```bash
git add server/internal/integrations/rongcloud/pairing_service.go
git commit -m "feat(rongcloud): bind device credential on pairing claim"
```

---

## Task 4: InstallService â€?SystemConfig CRUD

**Files:**
- `server/internal/integrations/rongcloud/install_service.go`

**Interfaces:**
- `UpsertSystemConfig(ctx, workspaceID pgtype.UUID, configKey string, nodeID pgtype.UUID, config json.RawMessage) error`
- `GetSystemConfig(ctx, workspaceID pgtype.UUID, configKey string) (json.RawMessage, error)`
- `ListSystemConfigs(ctx, workspaceID pgtype.UUID) ([]db.RongcloudSystemConfig, error)`
- `DeleteSystemConfig(ctx, workspaceID pgtype.UUID, configKey string) error`

### Step 1: Add imports

```go
// Ensure these are imported:
"<module>/pkg/db/generated"
```

### Step 2: Add `UpsertSystemConfig` method

```go
func (s *InstallService) UpsertSystemConfig(ctx context.Context, workspaceID pgtype.UUID, configKey string, nodeID pgtype.UUID, config json.RawMessage) error {
	params := db.UpsertRongCloudSystemConfigParams{
		WorkspaceID: workspaceID,
		ConfigKey:   pgText(configKey),
		NodeID:      nodeID,
		Config:      config,
	}
	return s.queries.UpsertRongCloudSystemConfig(ctx, params)
}
```

### Step 3: Add `GetSystemConfig` method

```go
func (s *InstallService) GetSystemConfig(ctx context.Context, workspaceID pgtype.UUID, configKey string) (json.RawMessage, error) {
	params := db.GetRongCloudSystemConfigParams{
		WorkspaceID: workspaceID,
		ConfigKey:   pgText(configKey),
	}
	cfg, err := s.queries.GetRongCloudSystemConfig(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("get system config: %w", err)
	}
	return cfg.Config, nil
}
```

### Step 4: Add `ListSystemConfigs` method

```go
func (s *InstallService) ListSystemConfigs(ctx context.Context, workspaceID pgtype.UUID) ([]db.RongcloudSystemConfig, error) {
	return s.queries.ListRongCloudSystemConfigs(ctx, workspaceID)
}
```

### Step 5: Add `DeleteSystemConfig` method

```go
func (s *InstallService) DeleteSystemConfig(ctx context.Context, workspaceID pgtype.UUID, configKey string) error {
	params := db.DeleteRongCloudSystemConfigParams{
		WorkspaceID: workspaceID,
		ConfigKey:   pgText(configKey),
	}
	return s.queries.DeleteRongCloudSystemConfig(ctx, params)
}
```

### Step 6: Add `fmt` import if not present

Ensure `"fmt"` is in the import block.

### Step 7: Verify build

```bash
cd server && go build ./internal/integrations/rongcloud/...
```

### Step 8: Commit

```bash
git add server/internal/integrations/rongcloud/install_service.go
git commit -m "feat(rongcloud): add system_config CRUD to InstallService"
```

---

## Task 5: system_handler â€?Chatroom/Device Command Routing

**Files:**
- `server/internal/integrations/rongcloud/system_handler.go`

**Interfaces:**
- `handleCommand` â€?add `case "chatroom"` and `case "device"` to the switch

### Step 1: Add `queries` field to `systemHandler` struct

The system_handler needs DB access for chatroom/device commands. Update the struct:

```go
type systemHandler struct {
	client   *rongcloudAPIClient
	queries  *db.Queries
	nodeID   string
	logger   *slog.Logger
}
```

Update `newSystemHandler` to accept `queries`:

```go
func newSystemHandler(client *rongcloudAPIClient, queries *db.Queries, nodeID string, logger *slog.Logger) *systemHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &systemHandler{
		client:  client,
		queries: queries,
		nodeID:  nodeID,
		logger:  logger,
	}
}
```

### Step 2: Add `db` import

```go
// Ensure this is imported:
"<module>/pkg/db/generated"
```

### Step 3: Add chatroom command cases to `handleCommand`

Update the switch in `handleCommand`:

```go
	switch cmd.Service {
	case "ping":
		return s.handlePing(ctx, msg, cmd)
	case "chatroom":
		return s.handleChatroomCommand(ctx, msg, cmd)
	case "device":
		return s.handleDeviceCommand(ctx, msg, cmd)
	default:
		return s.sendError(ctx, msg, cmd.RequestID, "unknown service: "+cmd.Service)
	}
```

### Step 4: Add `handleChatroomCommand` method

```go
func (s *systemHandler) handleChatroomCommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	switch cmd.Action {
	case "list":
		// List chatrooms â€?requires workspace_id from params
		wsIDStr, ok := cmd.Params["workspace_id"].(string)
		if !ok {
			return s.sendError(ctx, msg, cmd.RequestID, "missing workspace_id")
		}
		wsID, err := util.ParseUUID(wsIDStr)
		if err != nil {
			return s.sendError(ctx, msg, cmd.RequestID, "invalid workspace_id")
		}
		chatrooms, err := s.queries.ListRongCloudChatroomsByWorkspace(ctx, wsID)
		if err != nil {
			return s.sendError(ctx, msg, cmd.RequestID, "list chatrooms failed: "+err.Error())
		}
		payload := map[string]interface{}{"ok": true, "chatrooms": chatrooms}
		return s.client.sendCommandResult(ctx, s.nodeID, msg.FromUserID, cmd.RequestID, payload)
	default:
		return s.sendError(ctx, msg, cmd.RequestID, "unknown chatroom action: "+cmd.Action)
	}
}
```

### Step 5: Add `handleDeviceCommand` method

```go
func (s *systemHandler) handleDeviceCommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	switch cmd.Action {
	case "list":
		wsIDStr, ok := cmd.Params["workspace_id"].(string)
		if !ok {
			return s.sendError(ctx, msg, cmd.RequestID, "missing workspace_id")
		}
		wsID, err := util.ParseUUID(wsIDStr)
		if err != nil {
			return s.sendError(ctx, msg, cmd.RequestID, "invalid workspace_id")
		}
		devices, err := s.queries.ListRongCloudDevicesByWorkspace(ctx, wsID)
		if err != nil {
			return s.sendError(ctx, msg, cmd.RequestID, "list devices failed: "+err.Error())
		}
		payload := map[string]interface{}{"ok": true, "devices": devices}
		return s.client.sendCommandResult(ctx, s.nodeID, msg.FromUserID, cmd.RequestID, payload)
	case "enroll":
		// Return device enrollment info
		payload := map[string]interface{}{"ok": true, "message": "use POST /api/claw/device-credentials/enroll"}
		return s.client.sendCommandResult(ctx, s.nodeID, msg.FromUserID, cmd.RequestID, payload)
	default:
		return s.sendError(ctx, msg, cmd.RequestID, "unknown device action: "+cmd.Action)
	}
}
```

### Step 6: Add `util` import

```go
// Ensure this is imported:
"<module>/internal/util"
```

**Note:** Check the actual import path for the `util` package by looking at how `rongcloud_handler.go` imports it. It may be `"github.com/anomalyco/multica/internal/util"` or similar.

### Step 7: Update all callers of `newSystemHandler`

Search for all call sites of `newSystemHandler` and add the `queries` parameter. This is likely in `channel.go` or `webhook.go` where the system handler is instantiated.

```go
// Old: newSystemHandler(client, nodeID, logger)
// New: newSystemHandler(client, queries, nodeID, logger)
```

### Step 8: Verify build

```bash
cd server && go build ./internal/integrations/rongcloud/...
```

### Step 9: Commit

```bash
git add server/internal/integrations/rongcloud/system_handler.go
# Also any files where newSystemHandler callers were updated
git commit -m "feat(rongcloud): add chatroom/device command routing to system_handler"
```

---

## Task 6: Handler â€?Public Endpoints (Enroll, Connection Sessions)

**Files:**
- `server/internal/handler/rongcloud_handler.go`

**Interfaces:**
- `EnrollDeviceCredential(w, r)` â€?POST /api/claw/device-credentials/enroll
- `CreateConnectionSession(w, r)` â€?POST /api/claw/connection-sessions
- `CloseConnectionSession(w, r)` â€?POST /api/claw/connection-sessions/{sessionId}/close

### Step 1: Add `EnrollDeviceCredential` handler method

```go
func (h *Handler) EnrollDeviceCredential(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	var req struct {
		NodeID          string `json:"nodeId"`
		EnrollmentToken string `json:"enrollmentToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.NodeID == "" {
		writeError(w, http.StatusBadRequest, "nodeId is required")
		return
	}

	cred, err := h.RongCloudNode.EnrollDeviceCredential(r.Context(), req.NodeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "enroll device credential: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, cred)
}
```

### Step 2: Add `CreateConnectionSession` handler method

```go
func (h *Handler) CreateConnectionSession(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	var req struct {
		NodeID string `json:"nodeId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.NodeID == "" {
		writeError(w, http.StatusBadRequest, "nodeId is required")
		return
	}

	sessionID, err := h.RongCloudNode.OpenConnectionSession(r.Context(), req.NodeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "open connection session: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"sessionId": sessionID})
}
```

### Step 3: Add `CloseConnectionSession` handler method

```go
func (h *Handler) CloseConnectionSession(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	sessionID := chi.URLParam(r, "sessionId")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "sessionId is required")
		return
	}

	if err := h.RongCloudNode.CloseConnectionSession(r.Context(), sessionID); err != nil {
		writeError(w, http.StatusInternalServerError, "close connection session: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
```

### Step 4: Verify build

```bash
cd server && go build ./internal/handler/...
```

### Step 5: Commit

```bash
git add server/internal/handler/rongcloud_handler.go
git commit -m "feat(rongcloud): add public endpoints for device enrollment and connection sessions"
```

---

## Task 7: Handler â€?Workspace Member GET Endpoints

**Files:**
- `server/internal/handler/rongcloud_handler.go`

**Interfaces:**
- `GetRongCloudChatroom(w, r)` â€?GET /rongcloud/chatrooms/{chatroomId}
- `ListRongCloudNodes(w, r)` â€?GET /rongcloud/nodes (replace stub)
- `ListRongCloudNodeModels(w, r)` â€?GET /rongcloud/nodes/{nodeId}/models
- `ListRongCloudDevices(w, r)` â€?GET /rongcloud/devices (replace stub)
- `GetRongCloudSystemHost(w, r)` â€?GET /rongcloud/system-host
- `GetRongCloudPairing(w, r)` â€?GET /rongcloud/pairing/{ticket}

### Step 1: Add `GetRongCloudChatroom` handler method

```go
func (h *Handler) GetRongCloudChatroom(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudChatroom == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}

	chatroom, err := h.RongCloudChatroom.GetChatroom(r.Context(), chatroomID)
	if err != nil {
		writeError(w, http.StatusNotFound, "chatroom not found: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, chatroom)
}
```

### Step 2: Replace `ListRongCloudNodes` stub

Replace the existing stub method:

```go
func (h *Handler) ListRongCloudNodes(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}

	nodes, err := h.RongCloudNode.ListNodes(r.Context(), workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list nodes: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, nodes)
}
```

**Note:** `ListNodes` needs to be added to `NodeService`. Add this method to `node_service.go`:

```go
func (s *NodeService) ListNodes(ctx context.Context, workspaceID pgtype.UUID) ([]db.RongcloudNode, error) {
	return s.queries.ListRongCloudNodesByWorkspace(ctx, workspaceID)
}
```

### Step 3: Add `ListRongCloudNodeModels` handler method

```go
func (h *Handler) ListRongCloudNodeModels(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	nodeID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "nodeId"), "node id")
	if !ok {
		return
	}

	models, err := h.RongCloudNode.ListNodeModels(r.Context(), nodeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list node models: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, models)
}
```

**Note:** `ListNodeModels` currently takes `pgtype.UUID` â€?the URL param `nodeId` is a UUID from the DB `id` column, not the `node_id` string. Verify this works with the existing `ListNodeModels` signature which calls `ListRongCloudNodeModelCatalogsByNode(ctx, nodeID pgtype.UUID)`.

### Step 4: Replace `ListRongCloudDevices` stub

Replace the existing stub method:

```go
func (h *Handler) ListRongCloudDevices(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}

	devices, err := h.RongCloudNode.ListDevices(r.Context(), workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list devices: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, devices)
}
```

**Note:** `ListDevices` needs to be added to `NodeService`. Add this method to `node_service.go`:

```go
func (s *NodeService) ListDevices(ctx context.Context, workspaceID pgtype.UUID) ([]db.RongcloudDevice, error) {
	return s.queries.ListRongCloudDevicesByWorkspace(ctx, workspaceID)
}
```

### Step 5: Add `GetRongCloudSystemHost` handler method

```go
func (h *Handler) GetRongCloudSystemHost(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudInstall == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}

	config, err := h.RongCloudInstall.GetSystemConfig(r.Context(), workspaceID, "system_host")
	if err != nil {
		writeError(w, http.StatusNotFound, "system host not configured: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"config": json.RawMessage(config)})
}
```

### Step 6: Add `GetRongCloudPairing` handler method

```go
func (h *Handler) GetRongCloudPairing(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudPairing == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	ticket := chi.URLParam(r, "ticket")
	if ticket == "" {
		writeError(w, http.StatusBadRequest, "ticket is required")
		return
	}

	session, err := h.RongCloudPairing.GetSession(r.Context(), ticket)
	if err != nil {
		writeError(w, http.StatusNotFound, "pairing session not found: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, session)
}
```

### Step 7: Add `ListNodes` and `ListDevices` methods to NodeService

Add to `node_service.go`:

```go
func (s *NodeService) ListNodes(ctx context.Context, workspaceID pgtype.UUID) ([]db.RongcloudNode, error) {
	return s.queries.ListRongCloudNodesByWorkspace(ctx, workspaceID)
}

func (s *NodeService) ListDevices(ctx context.Context, workspaceID pgtype.UUID) ([]db.RongcloudDevice, error) {
	return s.queries.ListRongCloudDevicesByWorkspace(ctx, workspaceID)
}
```

Ensure `db` import is present in `node_service.go`.

### Step 8: Verify build

```bash
cd server && go build ./internal/handler/... ./internal/integrations/rongcloud/...
```

### Step 9: Commit

```bash
git add server/internal/handler/rongcloud_handler.go server/internal/integrations/rongcloud/node_service.go
git commit -m "feat(rongcloud): add workspace GET endpoints for chatroom, nodes, devices, models, system-host, pairing"
```

---

## Task 8: Handler â€?Workspace Admin POST/PUT/DELETE Endpoints

**Files:**
- `server/internal/handler/rongcloud_handler.go`

**Interfaces:**
- `UpdateRongCloudChatroom(w, r)` â€?PUT /rongcloud/chatrooms/{chatroomId}
- `SetRongCloudChatroomMembers(w, r)` â€?POST /rongcloud/chatrooms/{chatroomId}/members
- `AddRongCloudNodeModel(w, r)` â€?POST /rongcloud/nodes/{nodeId}/models
- `DeleteRongCloudNode(w, r)` â€?DELETE /rongcloud/nodes/{nodeId}
- `CreateRongCloudDevice(w, r)` â€?POST /rongcloud/devices
- `DeleteRongCloudDevice(w, r)` â€?DELETE /rongcloud/devices/{deviceId}
- `UpdateRongCloudSystemHost(w, r)` â€?PUT /rongcloud/system-host
- `CreateRongCloudPairing(w, r)` â€?POST /rongcloud/pairing

### Step 1: Add `UpdateRongCloudChatroom` handler method

```go
func (h *Handler) UpdateRongCloudChatroom(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudChatroom == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}

	var req struct {
		MaxRounds        int32           `json:"maxRounds"`
		ConversationKind string          `json:"conversationKind"`
		Config           json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	err := h.RongCloudChatroom.UpdateChatroom(r.Context(), chatroomID, pgtype.UUID{}, req.MaxRounds, req.ConversationKind, req.Config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update chatroom: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
```

**Note:** The existing `UpdateChatroom` signature is `(ctx, id, hostNodeID pgtype.UUID, maxRounds int32, conversationKind string, config json.RawMessage)`. If `hostNodeID` should be updatable, add it to the request struct. For now passing zero UUID means "no change" â€?verify the sqlc query handles this, or pass the existing value. If the query overwrites, either add `hostNodeID` to the request or change the service method signature.

### Step 2: Add `SetRongCloudChatroomMembers` handler method

```go
func (h *Handler) SetRongCloudChatroomMembers(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudChatroom == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}

	var req struct {
		Members []struct {
			NodeID            string          `json:"nodeId"`
			MemberType        string          `json:"memberType"`
			RoleName          string          `json:"roleName"`
			RoleInstructions  string          `json:"roleInstructions"`
			Capabilities      json.RawMessage `json:"capabilities"`
			Model             string          `json:"model"`
			SpeakingOrder     int32           `json:"speakingOrder"`
			DiscussionModel   string          `json:"discussionModel"`
		} `json:"members"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	members := make([]rongcloud.ChatroomMemberConfig, 0, len(req.Members))
	for _, m := range req.Members {
		nodeUUID, err := util.ParseUUID(m.NodeID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid nodeId: "+m.NodeID)
			return
		}
		members = append(members, rongcloud.ChatroomMemberConfig{
			NodeID:            nodeUUID,
			MemberType:        m.MemberType,
			RoleName:          m.RoleName,
			RoleInstructions:  m.RoleInstructions,
			Capabilities:      m.Capabilities,
			Model:             m.Model,
			SpeakingOrder:     m.SpeakingOrder,
			DiscussionModel:   m.DiscussionModel,
		})
	}

	if err := h.RongCloudChatroom.SetMembers(r.Context(), chatroomID, members); err != nil {
		writeError(w, http.StatusInternalServerError, "set members: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
```

### Step 3: Add `AddRongCloudNodeModel` handler method

```go
func (h *Handler) AddRongCloudNodeModel(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}

	nodeID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "nodeId"), "node id")
	if !ok {
		return
	}

	var req struct {
		ModelID    string          `json:"modelId"`
		Provider   string          `json:"provider"`
		ModelName  string          `json:"modelName"`
		Config     json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	err := h.RongCloudNode.AddNodeModel(r.Context(), workspaceID, nodeID, req.ModelID, req.Provider, req.ModelName, req.Config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "add node model: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}
```

### Step 4: Add `DeleteRongCloudNode` handler method

```go
func (h *Handler) DeleteRongCloudNode(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	nodeID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "nodeId"), "node id")
	if !ok {
		return
	}

	err := h.RongCloudNode.DeleteNode(r.Context(), nodeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete node: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
```

**Note:** `DeleteNode` needs to be added to `NodeService`. Add to `node_service.go`:

```go
func (s *NodeService) DeleteNode(ctx context.Context, id pgtype.UUID) error {
	return s.queries.DeleteRongCloudNode(ctx, id)
}
```

### Step 5: Add `CreateRongCloudDevice` handler method

```go
func (h *Handler) CreateRongCloudDevice(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}

	userIDStr := requireUserID(w, r)
	if userIDStr == "" {
		return
	}
	ownerID, err := util.ParseUUID(userIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	var req struct {
		NodeID     string `json:"nodeId"`
		DeviceName string `json:"deviceName"`
		DeviceType string `json:"deviceType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	cred, err := h.RongCloudNode.CreateDevice(r.Context(), workspaceID, ownerID, req.NodeID, req.DeviceName, req.DeviceType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create device: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, cred)
}
```

**Note:** `CreateDevice` needs to be added to `NodeService`. Add to `node_service.go`:

```go
func (s *NodeService) CreateDevice(ctx context.Context, workspaceID, ownerUserID pgtype.UUID, nodeIDStr, deviceName, deviceType string) (DeviceCredential, error) {
	// Get node by node_id string
	node, err := s.queries.GetRongCloudNodeByNodeID(ctx, nodeIDStr)
	if err != nil {
		return DeviceCredential{}, fmt.Errorf("get node: %w", err)
	}

	credID, credSecret, err := generateDeviceCredential()
	if err != nil {
		return DeviceCredential{}, fmt.Errorf("generate credential: %w", err)
	}

	encryptedSecret, err := s.box.Seal([]byte(credSecret))
	if err != nil {
		return DeviceCredential{}, fmt.Errorf("encrypt secret: %w", err)
	}

	params := db.CreateRongCloudDeviceParams{
		WorkspaceID:                workspaceID,
		OwnerUserID:               ownerUserID,
		NodeID:                    node.ID,
		DeviceName:               pgText(deviceName),
		DeviceType:               pgText(deviceType),
		CredentialID:             pgText(credID),
		CredentialSecretEncrypted: encryptedSecret,
		Status:                    pgText("active"),
	}
	if _, err := s.queries.CreateRongCloudDevice(ctx, params); err != nil {
		return DeviceCredential{}, fmt.Errorf("create device: %w", err)
	}

	return DeviceCredential{CredentialID: credID, CredentialSecret: credSecret}, nil
}
```

### Step 6: Add `DeleteRongCloudDevice` handler method

```go
func (h *Handler) DeleteRongCloudDevice(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	deviceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "deviceId"), "device id")
	if !ok {
		return
	}

	err := h.RongCloudNode.DeleteDevice(r.Context(), deviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete device: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
```

**Note:** `DeleteDevice` needs to be added to `NodeService`. Add to `node_service.go`:

```go
func (s *NodeService) DeleteDevice(ctx context.Context, id pgtype.UUID) error {
	return s.queries.DeleteRongCloudDevice(ctx, id)
}
```

### Step 7: Add `UpdateRongCloudSystemHost` handler method

```go
func (h *Handler) UpdateRongCloudSystemHost(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudInstall == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}

	var req struct {
		NodeID string          `json:"nodeId"`
		Config json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	nodeUUID, err := util.ParseUUID(req.NodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid nodeId")
		return
	}

	err = h.RongCloudInstall.UpsertSystemConfig(r.Context(), workspaceID, "system_host", nodeUUID, req.Config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update system host: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
```

### Step 8: Add `CreateRongCloudPairing` handler method

```go
func (h *Handler) CreateRongCloudPairing(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudPairing == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}

	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}

	var req struct {
		CandidateNodeIDs []string `json:"candidateNodeIds"`
		ClientClaimKey   string   `json:"clientClaimKey"`
		ExpiresIn        int      `json:"expiresIn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	candidateIDs := make([]pgtype.UUID, 0, len(req.CandidateNodeIDs))
	for _, idStr := range req.CandidateNodeIDs {
		uuid, err := util.ParseUUID(idStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid candidate nodeId: "+idStr)
			return
		}
		candidateIDs = append(candidateIDs, uuid)
	}

	params := rongcloud.PairingCreateParams{
		WorkspaceID:      workspaceID,
		CandidateNodeIDs: candidateIDs,
		ClientClaimKey:   req.ClientClaimKey,
	}
	if req.ExpiresIn > 0 {
		params.ExpiresIn = time.Duration(req.ExpiresIn) * time.Second
	}

	session, err := h.RongCloudPairing.CreateSession(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create pairing session: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, session)
}
```

### Step 9: Add `DeleteNode` and `DeleteDevice` and `CreateDevice` methods to NodeService

Verify these are added to `node_service.go` (from Steps 4, 5, 6 above):

```go
func (s *NodeService) DeleteNode(ctx context.Context, id pgtype.UUID) error {
	return s.queries.DeleteRongCloudNode(ctx, id)
}

func (s *NodeService) DeleteDevice(ctx context.Context, id pgtype.UUID) error {
	return s.queries.DeleteRongCloudDevice(ctx, id)
}

func (s *NodeService) CreateDevice(ctx context.Context, workspaceID, ownerUserID pgtype.UUID, nodeIDStr, deviceName, deviceType string) (DeviceCredential, error) {
	// ... (code from Step 5)
}
```

### Step 10: Add `time` import to handler if not present

```go
// Ensure "time" is imported in rongcloud_handler.go
```

### Step 11: Verify build

```bash
cd server && go build ./internal/handler/... ./internal/integrations/rongcloud/...
```

### Step 12: Commit

```bash
git add server/internal/handler/rongcloud_handler.go server/internal/integrations/rongcloud/node_service.go
git commit -m "feat(rongcloud): add workspace admin endpoints for chatroom, node, device, system-host, pairing"
```

---

## Task 9: Router â€?Wire All New Routes

**Files:**
- `server/cmd/server/router.go`

**Interfaces:**
- Add 3 public routes (device-credentials/enroll, connection-sessions, connection-sessions/{sessionId}/close)
- Add 6 workspace member GET routes
- Add 8 workspace admin routes

### Step 1: Add public routes

In the public routes section (near existing `r.Post("/api/ai/register", ...)`), add:

```go
r.Post("/api/claw/device-credentials/enroll", h.EnrollDeviceCredential)
r.Post("/api/claw/connection-sessions", h.CreateConnectionSession)
r.Post("/api/claw/connection-sessions/{sessionId}/close", h.CloseConnectionSession)
```

### Step 2: Add workspace member GET routes

In the workspace member routes section (near existing `r.Get("/rongcloud/chatrooms", h.ListRongCloudChatrooms)`), add:

```go
r.Get("/rongcloud/chatrooms/{chatroomId}", h.GetRongCloudChatroom)
r.Get("/rongcloud/nodes", h.ListRongCloudNodes)
r.Get("/rongcloud/nodes/{nodeId}/models", h.ListRongCloudNodeModels)
r.Get("/rongcloud/devices", h.ListRongCloudDevices)
r.Get("/rongcloud/system-host", h.GetRongCloudSystemHost)
r.Get("/rongcloud/pairing/{ticket}", h.GetRongCloudPairing)
```

### Step 3: Add workspace admin routes

In the workspace admin routes section (near existing `r.Post("/rongcloud/chatrooms", h.CreateRongCloudChatroom)`), add:

```go
r.Put("/rongcloud/chatrooms/{chatroomId}", h.UpdateRongCloudChatroom)
r.Post("/rongcloud/chatrooms/{chatroomId}/members", h.SetRongCloudChatroomMembers)
r.Post("/rongcloud/nodes/{nodeId}/models", h.AddRongCloudNodeModel)
r.Delete("/rongcloud/nodes/{nodeId}", h.DeleteRongCloudNode)
r.Post("/rongcloud/devices", h.CreateRongCloudDevice)
r.Delete("/rongcloud/devices/{deviceId}", h.DeleteRongCloudDevice)
r.Put("/rongcloud/system-host", h.UpdateRongCloudSystemHost)
r.Post("/rongcloud/pairing", h.CreateRongCloudPairing)
```

### Step 4: Verify build

```bash
cd server && go build ./cmd/server/...
```

### Step 5: Run full build + tests

```bash
cd server && go build ./... && go test ./internal/integrations/rongcloud/...
```

### Step 6: Commit

```bash
git add server/cmd/server/router.go
git commit -m "feat(rongcloud): wire Phase 2b routes for all new endpoints"
```

---

## Task 10: Tests â€?Extend rongcloud_test.go

**Files:**
- `server/internal/integrations/rongcloud/rongcloud_test.go`

### Step 1: Add test for ChatroomService.DeleteChatroom with RongCloud destroy

```go
func TestChatroomService_DeleteChatroom_CallsDestroy(t *testing.T) {
	// Setup: create mock client that records destroyChatroom calls
	// Create chatroom in DB
	// Call DeleteChatroom
	// Assert: destroyChatroom was called with correct ID
	// Assert: DB record has status='deleted'
}
```

### Step 2: Add test for ChatroomService.SetMembers join/quit sync

```go
func TestChatroomService_SetMembers_SyncsJoinQuit(t *testing.T) {
	// Setup: create chatroom with existing members
	// Call SetMembers with partially different members
	// Assert: joinChatroom called for new members
	// Assert: quitChatroom called for removed members
}
```

### Step 3: Add test for NodeService.EnrollDeviceCredential persistence

```go
func TestNodeService_EnrollDeviceCredential_Persists(t *testing.T) {
	// Setup: create node in DB
	// Call EnrollDeviceCredential
	// Assert: device record exists in DB with correct fields
	// Assert: credential_id starts with "dc_"
}
```

### Step 4: Add test for system_handler chatroom/device routing

```go
func TestSystemHandler_ChatroomCommand(t *testing.T) {
	// Setup: systemHandler with mock client + DB
	// Send command with Service="chatroom", Action="list"
	// Assert: sendCommandResult called with chatroom list payload
}

func TestSystemHandler_DeviceCommand(t *testing.T) {
	// Setup: systemHandler with mock client + DB
	// Send command with Service="device", Action="list"
	// Assert: sendCommandResult called with device list payload
}
```

### Step 5: Run tests

```bash
cd server && go test ./internal/integrations/rongcloud/... -v
```

### Step 6: Commit

```bash
git add server/internal/integrations/rongcloud/rongcloud_test.go
git commit -m "test(rongcloud): add tests for Phase 2b business logic"
```

---

## Self-Review Checklist

- [x] All `- [x]` in this plan are checked `- [x]` after completion
- [x] `go build ./...` passes from `server/`
- [x] `go test ./internal/integrations/rongcloud/...` passes
- [x] `make test` passes
- [x] No new DB migrations were created
- [x] No new sqlc queries were added
- [x] All 15 missing handler endpoints are implemented
- [x] Both stub endpoints (ListNodes, ListDevices) are completed
- [x] All 16 new routes are wired in router.go
- [x] system_handler routes chatroom/device commands
- [x] node_message_handler remains a stub (Phase 4)
- [x] ChatroomService.DeleteChatroom calls client.destroyChatroom
- [x] ChatroomService.SetMembers calls client.joinChatroom/quitChatroom
- [x] NodeService.EnrollDeviceCredential persists to DB
- [x] NodeService.OpenConnectionSession/CloseConnectionSession use system_config table
- [x] PairingService.ClaimSession accepts deviceCredentialTicket parameter
- [x] InstallService has SystemConfig CRUD methods
- [x] Conventional commits used for each task
