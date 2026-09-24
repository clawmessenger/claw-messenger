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
	queries  *db.Queries
	client   *rongcloudAPIClient
	nodeID   string
	logger   *slog.Logger
	registry *DiscussionRegistry
}

func newSystemHandler(client *rongcloudAPIClient, nodeID string, logger *slog.Logger, queries *db.Queries, registry *DiscussionRegistry) *systemHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &systemHandler{
		queries:  queries,
		client:   client,
		nodeID:   nodeID,
		logger:   logger,
		registry: registry,
	}
}

func (h *systemHandler) handleCommand(ctx context.Context, msg NormalizedMessage) error {
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
	default:
		h.logger.Warn("rongcloud: unknown service", "service", cmd.Service, "msgUID", msg.MsgUID)
		return h.sendError(ctx, msg, cmd.RequestID, fmt.Sprintf("unknown service: %s", cmd.Service))
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
		return h.sendError(ctx, msg, cmd.RequestID, "database not available")
	}

	switch cmd.Action {
	case "list":
		wsIDStr := getString(cmd.Params, "workspace_id")
		wsID, err := parseUUIDParam(wsIDStr)
		if err != nil {
			return h.sendError(ctx, msg, cmd.RequestID, "invalid or missing workspace_id")
		}
		chatrooms, err := h.queries.ListRongCloudChatroomsByWorkspace(ctx, wsID)
		if err != nil {
			h.logger.Error("rongcloud: list chatrooms", "error", err)
			return h.sendError(ctx, msg, cmd.RequestID, "failed to list chatrooms")
		}
		return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, map[string]interface{}{
			"ok":        true,
			"chatrooms": chatrooms,
		})

	case "get", "info":
		chatroomIDStr := getString(cmd.Params, "chatroom_id")
		chatroomID, err := parseUUIDParam(chatroomIDStr)
		if err != nil {
			return h.sendError(ctx, msg, cmd.RequestID, "invalid or missing chatroom_id")
		}
		chatroom, err := h.queries.GetRongCloudChatroomByID(ctx, chatroomID)
		if err != nil {
			h.logger.Error("rongcloud: get chatroom", "error", err)
			return h.sendError(ctx, msg, cmd.RequestID, "chatroom not found")
		}
		return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, map[string]interface{}{
			"ok":       true,
			"chatroom": chatroom,
		})

	default:
		return h.sendError(ctx, msg, cmd.RequestID, fmt.Sprintf("unsupported chatroom action: %s", cmd.Action))
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
		return h.sendError(ctx, msg, cmd.RequestID, "discussion registry not available")
	}

	switch cmd.Action {
	case "status":
		chatroomIDStr := getString(cmd.Params, "chatroom_id")
		chatroomID, err := parseUUIDParam(chatroomIDStr)
		if err != nil {
			return h.sendError(ctx, msg, cmd.RequestID, "invalid or missing chatroom_id")
		}
		_, exists := h.registry.Get(chatroomID)
		return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, map[string]interface{}{
			"ok":     true,
			"active": exists,
		})

	case "your_turn":
		return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, map[string]interface{}{
			"ok":      true,
			"message": "your_turn acknowledged",
		})

	default:
		return h.sendError(ctx, msg, cmd.RequestID, fmt.Sprintf("unsupported discussion action: %s", cmd.Action))
	}
}
