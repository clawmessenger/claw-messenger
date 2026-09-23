package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// systemHandler processes command messages addressed to the system node.
// It dispatches by service name to individual service handlers.
type systemHandler struct {
	client *rongcloudAPIClient
	nodeID string
	logger *slog.Logger
}

func newSystemHandler(client *rongcloudAPIClient, nodeID string, logger *slog.Logger) *systemHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &systemHandler{
		client: client,
		nodeID: nodeID,
		logger: logger,
	}
}

// handleCommand parses a command message and dispatches to the appropriate service handler.
// On parse errors or unknown services, it sends an error command_result back to the sender.
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
	default:
		h.logger.Warn("rongcloud: unknown service", "service", cmd.Service, "msgUID", msg.MsgUID)
		return h.sendError(ctx, msg, cmd.RequestID, fmt.Sprintf("unknown service: %s", cmd.Service))
	}
}

// handlePing echoes back a pong response.
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

// sendError sends an error command_result back to the sender.
func (h *systemHandler) sendError(ctx context.Context, msg NormalizedMessage, requestID, errMsg string) error {
	payload := map[string]interface{}{
		"ok":    false,
		"error": errMsg,
	}
	return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, requestID, payload)
}
