package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type systemHandler struct {
	queries      *db.Queries
	client       *rongcloudAPIClient
	nodeID       string
	logger       *slog.Logger
	registry     *DiscussionRegistry
	nodeRequests *NodeRequestBroker
	groupMirror  *GroupMirror
	// discussionRuntime 承载 service=discussion 的生命周期 action（P4）。
	discussionRuntime *discussionRuntime
}

func newSystemHandler(client *rongcloudAPIClient, nodeID string, logger *slog.Logger, queries *db.Queries, registry *DiscussionRegistry, nodeRequests *NodeRequestBroker, groupMirror *GroupMirror, discussionRuntime *discussionRuntime) *systemHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &systemHandler{
		queries:           queries,
		client:            client,
		nodeID:            nodeID,
		logger:            logger,
		registry:          registry,
		nodeRequests:      nodeRequests,
		groupMirror:       groupMirror,
		discussionRuntime: discussionRuntime,
	}
}

func (h *systemHandler) handleCommand(ctx context.Context, msg NormalizedMessage) error {
	// 忽略服务端自己发出的消息：融云会把本端发出的 command 回执也回灌 webhook，
	// 不拦住就会被当成新请求再回一次（旧后端同样有这道 guard）。
	if h.nodeID != "" && msg.FromUserID == h.nodeID {
		h.logger.Debug("rongcloud: ignoring command from the system node itself", "msgUID", msg.MsgUID)
		return nil
	}

	var cmd CommandContent
	if err := json.Unmarshal([]byte(msg.Content), &cmd); err != nil {
		h.logger.Error("rongcloud: parse command content", "error", err, "msgUID", msg.MsgUID)
		return h.sendError(ctx, msg, "", "invalid command content")
	}

	if cmd.RequestID == "" {
		h.logger.Warn("rongcloud: command missing request_id", "msgUID", msg.MsgUID)
		return nil
	}

	switch cmd.Service {
	case "ping":
		return h.handlePing(ctx, msg, cmd)
	case "chatroom":
		return h.handleChatroomCommand(ctx, msg, cmd)
	case "device":
		return h.handleDeviceCommand(ctx, msg, cmd)
	case "discussion":
		return h.handleDiscussionCommand(ctx, msg, cmd)
	case "ai":
		return h.handleAICommand(ctx, msg, cmd)
	case "group":
		return h.handleGroupCommand(ctx, msg, cmd)
	default:
		h.logger.Warn("rongcloud: unknown service", "service", cmd.Service, "msgUID", msg.MsgUID)
		return h.replyErr(ctx, msg, cmd, 404, unknownServiceOrActionMessage(cmd))
	}
}

func (h *systemHandler) handlePing(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	payload := map[string]interface{}{
		"ok":      true,
		"message": "pong",
	}
	if echo, ok := cmd.Params["echo"]; ok {
		payload["echo"] = echo
	}
	return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, payload)
}

func (h *systemHandler) handleChatroomCommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	if h.queries == nil {
		return h.replyErr(ctx, msg, cmd, 503, "database not available")
	}

	// Actions that mutate or check ownership need the actor. The read-only
	// actions (getList/getInfo/getMembers) do not — the web calls them before
	// the user has a stable member row, and requiring a registered rongcloud_user
	// would break the hydration flow.
	switch cmd.Action {
	case "list":
		// Legacy workspace-scoped list (device CLI). Kept for back-compat;
		// the web uses getList.
		wsIDStr := getStringAny(cmd.Params, "workspace_id", "workspaceId")
		wsID, err := parseUUIDParam(wsIDStr)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 400, "invalid or missing workspace_id")
		}
		chatrooms, err := h.queries.ListRongCloudChatroomsByWorkspace(ctx, wsID)
		if err != nil {
			h.logger.Error("rongcloud: list chatrooms", "error", err)
			return h.replyErr(ctx, msg, cmd, 500, "failed to list chatrooms")
		}
		return h.replyOK(ctx, msg, cmd, map[string]interface{}{
			"ok":        true,
			"chatrooms": chatrooms,
		})

	case "getList":
		actor, err := h.resolveActor(ctx, msg.FromUserID)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
		}
		return h.commitResult(ctx, msg, cmd, h.actionChatroomGetList(ctx, cmd, actor))

	case "get", "info", "getInfo":
		return h.commitResult(ctx, msg, cmd, h.actionChatroomGetInfo(ctx, cmd))

	case "update":
		actor, err := h.resolveActor(ctx, msg.FromUserID)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
		}
		return h.commitResult(ctx, msg, cmd, h.actionChatroomUpdate(ctx, cmd, actor))

	case "destroy":
		actor, err := h.resolveActor(ctx, msg.FromUserID)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
		}
		return h.commitResult(ctx, msg, cmd, h.actionChatroomDestroy(ctx, cmd, actor))

	case "join":
		actor, err := h.resolveActor(ctx, msg.FromUserID)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
		}
		return h.commitResult(ctx, msg, cmd, h.actionChatroomJoin(ctx, cmd, actor))

	case "leave":
		actor, err := h.resolveActor(ctx, msg.FromUserID)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
		}
		return h.commitResult(ctx, msg, cmd, h.actionChatroomLeave(ctx, cmd, actor))

	case "invite":
		actor, err := h.resolveActor(ctx, msg.FromUserID)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
		}
		return h.commitResult(ctx, msg, cmd, h.actionChatroomInvite(ctx, cmd, actor))

	case "removeMember":
		actor, err := h.resolveActor(ctx, msg.FromUserID)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
		}
		return h.commitResult(ctx, msg, cmd, h.actionChatroomRemoveMember(ctx, cmd, actor))

	case "getMembers":
		return h.commitResult(ctx, msg, cmd, h.actionChatroomGetMembers(ctx, cmd))

	case "create":
		actor, err := h.resolveActor(ctx, msg.FromUserID)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
		}
		return h.commitResult(ctx, msg, cmd, h.actionChatroomCreate(ctx, cmd, actor))

	case "setHost":
		actor, err := h.resolveActor(ctx, msg.FromUserID)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
		}
		return h.commitResult(ctx, msg, cmd, h.actionChatroomSetHost(ctx, cmd, actor))

	case "setMemberRole":
		actor, err := h.resolveActor(ctx, msg.FromUserID)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
		}
		return h.commitResult(ctx, msg, cmd, h.actionChatroomSetMemberRole(ctx, cmd, actor))

	case "setDiscussionTeam":
		actor, err := h.resolveActor(ctx, msg.FromUserID)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
		}
		return h.commitResult(ctx, msg, cmd, h.actionChatroomSetDiscussionTeam(ctx, cmd, actor))

	default:
		// The 404 + 「未知的服务或操作」 wording is a contract, not cosmetics:
		// the web's isLegacyChatroomActionError matches on code===404 AND the
		// message containing both "chatroom/<action>" and "未知" to degrade
		// silently during a rolling deploy. A different phrasing makes join/
		// leave/invite/destroy throw instead of falling back.
		return h.replyErr(ctx, msg, cmd, 404, unknownServiceOrActionMessage(cmd))
	}
}

// commitResult turns a commandResult into the right reply shape.
func (h *systemHandler) commitResult(ctx context.Context, msg NormalizedMessage, cmd CommandContent, r commandResult) error {
	if r.code == 200 {
		return h.replyOK(ctx, msg, cmd, r.data)
	}
	return h.replyErr(ctx, msg, cmd, r.code, r.message)
}

func (h *systemHandler) handleAICommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	if h.queries == nil {
		return h.sendError(ctx, msg, cmd.RequestID, "database not available")
	}

	switch cmd.Action {
	case "getNodes":
		// Resolve the installation workspace through the system user this
		// handler is connected as (local single-install semantics).
		sysUser, err := h.queries.GetRongCloudUserByRongCloudID(ctx, h.nodeID)
		if err != nil {
			h.logger.Error("rongcloud: ai getNodes resolve system user", "error", err)
			return h.sendError(ctx, msg, cmd.RequestID, "failed to resolve workspace")
		}
		nodes, err := h.queries.ListRongCloudNodesByWorkspace(ctx, sysUser.WorkspaceID)
		if err != nil {
			h.logger.Error("rongcloud: ai getNodes list nodes", "error", err)
			return h.sendError(ctx, msg, cmd.RequestID, "failed to list nodes")
		}
		// Ownership filter + binding_role: the mini-program gates discussion
		// participation on binding_role === 'owner' over this list, and the
		// sender is the claw user (its user_id doubles as the rc account id),
		// so the friend edge of msg.FromUserID is exactly their bound nodes.
		ownedRC := map[string]bool{}
		if h.client != nil {
			friends, err := h.client.FriendIDs(ctx, msg.FromUserID)
			if err != nil {
				h.logger.Warn("rongcloud: ai getNodes friend lookup failed, listing workspace-wide", "from", msg.FromUserID, "error", err)
				ownedRC = nil
			} else {
				for _, friend := range friends {
					ownedRC[friend] = true
				}
			}
		} else {
			ownedRC = nil
		}
		records := make([]map[string]interface{}, 0, len(nodes))
		for _, n := range nodes {
			// Keep the same visibility contract as the HTTP device list: the
			// per-machine infrastructure node and the built-in ops maintenance
			// agent are internal and must not surface to clients.
			if isHiddenNode(n) {
				continue
			}
			if ownedRC != nil && !ownedRC[n.RongcloudUserID] {
				continue
			}
			aiType := n.AiType.String
			record := map[string]interface{}{
				"node_id":       n.NodeID,
				"node_type":     aiType,
				"name":          aiType,
				"rongcloud_id":  n.RongcloudUserID,
				"binding_role":  "owner",
				"deploy_status": n.DeployStatus,
				"status":        "active",
				"created_at":    n.CreatedAt.Time,
				"updated_at":    n.UpdatedAt.Time,
			}
			if len(n.Capabilities) > 0 {
				record["capabilities"] = json.RawMessage(n.Capabilities)
			}
			if u, err := h.queries.GetRongCloudUserByRongCloudID(ctx, n.RongcloudUserID); err == nil && u.Name.Valid && u.Name.String != "" {
				record["name"] = u.Name.String
				record["nickname"] = u.Name.String
			}
			records = append(records, record)
		}
		return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, map[string]interface{}{
			"ok":    true,
			"nodes": records,
		})

	default:
		return h.sendError(ctx, msg, cmd.RequestID, fmt.Sprintf("unsupported ai action: %s", cmd.Action))
	}
}

func (h *systemHandler) handleDeviceCommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	if h.queries == nil {
		return h.sendError(ctx, msg, cmd.RequestID, "database not available")
	}

	switch cmd.Action {
	case "list":
		wsIDStr := getString(cmd.Params, "workspace_id")
		wsID, err := parseUUIDParam(wsIDStr)
		if err != nil {
			return h.sendError(ctx, msg, cmd.RequestID, "invalid or missing workspace_id")
		}
		devices, err := h.queries.ListRongCloudDevicesByWorkspace(ctx, wsID)
		if err != nil {
			h.logger.Error("rongcloud: list devices", "error", err)
			return h.sendError(ctx, msg, cmd.RequestID, "failed to list devices")
		}
		return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, map[string]interface{}{
			"ok":      true,
			"devices": devices,
		})

	case "get", "info":
		deviceIDStr := getString(cmd.Params, "device_id")
		deviceID, err := parseUUIDParam(deviceIDStr)
		if err != nil {
			return h.sendError(ctx, msg, cmd.RequestID, "invalid or missing device_id")
		}
		device, err := h.queries.GetRongCloudDeviceByID(ctx, deviceID)
		if err != nil {
			h.logger.Error("rongcloud: get device", "error", err)
			return h.sendError(ctx, msg, cmd.RequestID, "device not found")
		}
		return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, map[string]interface{}{
			"ok":     true,
			"device": device,
		})

	default:
		return h.sendError(ctx, msg, cmd.RequestID, fmt.Sprintf("unsupported device action: %s", cmd.Action))
	}
}

func (h *systemHandler) sendError(ctx context.Context, msg NormalizedMessage, requestID, errMsg string) error {
	payload := map[string]interface{}{
		"ok":    false,
		"error": errMsg,
	}
	return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, requestID, payload)
}

func parseUUIDParam(s string) (pgtype.UUID, error) {
	if s == "" {
		return pgtype.UUID{}, fmt.Errorf("empty uuid")
	}
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return pgtype.UUID{}, err
	}
	return u, nil
}

func (h *systemHandler) handleDiscussionCommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	// 只读 action 不需要 actor
	switch cmd.Action {
	case "getWorkflowCapabilities":
		if h.discussionRuntime == nil {
			return h.replyErr(ctx, msg, cmd, 503, "discussion runtime not available")
		}
		return h.replyCommandResult(ctx, msg, cmd, h.discussionRuntime.actionGetWorkflowCapabilities(cmd))
	case "getSystemHostStatus":
		if h.discussionRuntime == nil {
			return h.replyErr(ctx, msg, cmd, 503, "discussion runtime not available")
		}
		return h.replyCommandResult(ctx, msg, cmd, h.discussionRuntime.actionGetSystemHostStatus(ctx))
	}

	// P4：service=discussion 的生命周期 action。resolveActor 兼容 C 端真人
	// （claw_im_users）与节点/安装账号（rongcloud_user）两个身份域。
	// P4/P5 已注册动作。未注册动作走 default 的 404 契约 —— 与「runtime 未装配」
	// 的语义区分开：未装配只影响已注册的动作（503），未注册的动作永远 404 降级。
	known := map[string]bool{
		"start": true, "getActive": true, "continue": true, "finish": true,
		"cancel": true, "interject": true, "retry": true, "stop": true,
		"recommendRoles": true, "createRoundtable": true, "resumeRoundtable": true,
	}
	if !known[cmd.Action] {
		return h.replyErr(ctx, msg, cmd, 404, unknownServiceOrActionMessage(cmd))
	}
	if h.discussionRuntime == nil {
		return h.replyErr(ctx, msg, cmd, 503, "discussion runtime not available")
	}

	actor, err := h.discussionRuntime.resolveActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 400, "AUTHENTICATED_USER_NOT_FOUND")
	}

	var result commandResult
	switch cmd.Action {
	case "start":
		result = h.discussionRuntime.actionStart(ctx, cmd, actor)
	case "getActive":
		result = h.discussionRuntime.actionGetActive(ctx, cmd, actor)
	case "continue":
		result = h.discussionRuntime.actionContinue(ctx, cmd, actor)
	case "finish":
		result = h.discussionRuntime.actionFinish(ctx, cmd, actor)
	case "cancel":
		result = h.discussionRuntime.actionCancel(ctx, cmd, actor)
	case "interject":
		result = h.discussionRuntime.actionInterject(ctx, cmd, actor)
	case "retry":
		result = h.discussionRuntime.actionRetry(ctx, cmd, actor)
	case "stop":
		result = h.discussionRuntime.actionStop(ctx, cmd, actor)
	case "recommendRoles":
		result = h.discussionRuntime.actionRecommendRoles(ctx, cmd, actor)
	case "createRoundtable":
		result = h.discussionRuntime.actionCreateRoundtable(ctx, cmd, actor)
	case "resumeRoundtable":
		result = h.discussionRuntime.actionResumeRoundtable(ctx, cmd, actor)
	default:
		return h.replyErr(ctx, msg, cmd, 404, unknownServiceOrActionMessage(cmd))
	}
	return h.replyCommandResult(ctx, msg, cmd, result)
}

// unknownServiceOrActionMessage mirrors the legacy backend's handler-not-found
// error (command_handler.py: "未知的服务或操作: <service>/<action>"). The exact
// wording is matched by the C-end web (isLegacyChatroomActionError), so it must
// not be reworded.
func unknownServiceOrActionMessage(cmd CommandContent) string {
	return fmt.Sprintf("未知的服务或操作: %s/%s", cmd.Service, cmd.Action)
}

// replyOK answers a request in the dialect it arrived in.
//
// The chatroom/discussion services are spoken to by BOTH clients: the web
// browses rooms over them, and the device CLI replies to discussion turns. The
// web parses a flat object carrying a string `status`
// (rongcloudAPIClient.sendWebCommandResult); the CLI parses the device-shaped
// {request_id, msg_type, payload} (sendCommandResult). Encoding every reply the
// device way left the web unable to find a `status`, so its requests hung until
// timeout. The request's own envelope decides the reply shape.
func (h *systemHandler) replyOK(ctx context.Context, msg NormalizedMessage, cmd CommandContent, data interface{}) error {
	if cmd.WebEnvelope {
		return h.client.sendWebCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID,
			cmd.Service, cmd.Action, "success", 200, data, "ok")
	}
	payload := map[string]interface{}{"ok": true}
	if fields, ok := data.(map[string]interface{}); ok {
		for k, v := range fields {
			payload[k] = v
		}
	} else if data != nil {
		payload["data"] = data
	}
	return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, payload)
}

// replyErr is replyOK's error twin. code carries the same meaning in both
// dialects (404/400/403/409/503), so the legacy-404 tolerance keeps working.
// replyCommandResult 把 commandResult（P3 chatroom / P4 discussion action 的
// 内部返回形状）翻译成按方言自适应的回包。409 时 data 里带 latestSnapshot ——
// 前端 conflictSnapshot 读 error.data.latestSnapshot 做重同步。
func (h *systemHandler) replyCommandResult(ctx context.Context, msg NormalizedMessage, cmd CommandContent, result commandResult) error {
	if result.code == 200 {
		return h.replyOK(ctx, msg, cmd, result.data)
	}
	if cmd.WebEnvelope {
		return h.client.sendWebCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID,
			cmd.Service, cmd.Action, "error", result.code, result.data, result.message)
	}
	return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, map[string]interface{}{
		"ok":    false,
		"error": result.message,
	})
}

func (h *systemHandler) replyErr(ctx context.Context, msg NormalizedMessage, cmd CommandContent, code int, errMsg string) error {
	if cmd.WebEnvelope {
		return h.client.sendWebCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID,
			cmd.Service, cmd.Action, "error", code, nil, errMsg)
	}
	return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, map[string]interface{}{
		"ok":    false,
		"error": errMsg,
	})
}
