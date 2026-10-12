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
}

func newSystemHandler(client *rongcloudAPIClient, nodeID string, logger *slog.Logger, queries *db.Queries, registry *DiscussionRegistry, nodeRequests *NodeRequestBroker, groupMirror *GroupMirror) *systemHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &systemHandler{
		queries:      queries,
		client:       client,
		nodeID:       nodeID,
		logger:       logger,
		registry:     registry,
		nodeRequests: nodeRequests,
		groupMirror:  groupMirror,
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

	switch cmd.Action {
	case "list":
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

	case "get", "info", "getInfo":
		chatroomIDStr := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
		chatroomID, err := parseUUIDParam(chatroomIDStr)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 400, "invalid or missing chatroom_id")
		}
		chatroom, err := h.queries.GetRongCloudChatroomByID(ctx, chatroomID)
		if err != nil {
			h.logger.Error("rongcloud: get chatroom", "error", err)
			return h.replyErr(ctx, msg, cmd, 404, "chatroom not found")
		}
		return h.replyOK(ctx, msg, cmd, map[string]interface{}{
			"ok":       true,
			"chatroom": chatroom,
		})

	default:
		// The 404 + 「未知的服务或操作」 wording is a contract, not cosmetics:
		// the web's isLegacyChatroomActionError matches on code===404 AND the
		// message containing both "chatroom/<action>" and "未知" to degrade
		// silently during a rolling deploy. A different phrasing makes join/
		// leave/invite/destroy throw instead of falling back.
		return h.replyErr(ctx, msg, cmd, 404, unknownServiceOrActionMessage(cmd))
	}
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
		records := make([]map[string]interface{}, 0, len(nodes))
		for _, n := range nodes {
			// Keep the same visibility contract as the HTTP device list: the
			// per-machine infrastructure node and the built-in ops maintenance
			// agent are internal and must not surface to clients.
			if isHiddenNode(n) {
				continue
			}
			aiType := n.AiType.String
			record := map[string]interface{}{
				"node_id":       n.NodeID,
				"node_type":     aiType,
				"name":          aiType,
				"rongcloud_id":  n.RongcloudUserID,
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
	if h.registry == nil {
		return h.replyErr(ctx, msg, cmd, 503, "discussion registry not available")
	}

	switch cmd.Action {
	case "status":
		chatroomIDStr := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
		chatroomID, err := parseUUIDParam(chatroomIDStr)
		if err != nil {
			return h.replyErr(ctx, msg, cmd, 400, "invalid or missing chatroom_id")
		}
		_, exists := h.registry.Get(chatroomID)
		return h.replyOK(ctx, msg, cmd, map[string]interface{}{
			"ok":     true,
			"active": exists,
		})

	case "your_turn":
		return h.replyOK(ctx, msg, cmd, map[string]interface{}{
			"ok":      true,
			"message": "your_turn acknowledged",
		})

	default:
		// Same 404 contract as chatroom: the web matches on code 404 +
		// "discussion/<action>" + "未知" to treat an action the server has not
		// learned yet as a legacy no-op instead of a hard failure.
		return h.replyErr(ctx, msg, cmd, 404, unknownServiceOrActionMessage(cmd))
	}
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
