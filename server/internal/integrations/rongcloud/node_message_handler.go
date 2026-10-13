package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

func (h *systemHandler) handleNodeMessage(ctx context.Context, msg NormalizedMessage) {
	h.logger.Info("rongcloud: node message received",
		"msgUID", msg.MsgUID,
		"fromUserID", msg.FromUserID,
		"objectName", msg.ObjectName,
	)

	// A command_result may answer a server-initiated request (the admin
	// role-recommendation preview) rather than a discussion turn. Offer it to the
	// broker first: it returns true only for a request id it issued, so real
	// discussion turns still fall through to the coordinator below.
	if msg.ObjectName == objectNameCommand && h.nodeRequests != nil &&
		h.nodeRequests.AcceptResponse(msg.FromUserID, msg.Content) {
		return
	}

	switch msg.ObjectName {
	case objectNameCommand:
		h.handleCommandResult(ctx, msg)
	case objectNameStream:
		h.handleStreamMessage(ctx, msg)
	case objectNameText:
		h.handleTextResponse(ctx, msg)
	default:
		h.logger.Debug("rongcloud: unhandled node message type", "objectName", msg.ObjectName)
	}
}

func (h *systemHandler) handleCommandResult(ctx context.Context, msg NormalizedMessage) {
	var result CommandResultContent
	if err := json.Unmarshal([]byte(msg.Content), &result); err != nil {
		h.logger.Warn("rongcloud: parse command result", "error", err, "msgUID", msg.MsgUID)
		return
	}

	// P7：先给 v3 交互轮次引擎一次机会。设备对 your_turn 的回包也走这条
	// command_result 通道，引擎按 requestId 在 outbox 里查得到派发才算它的
	// （返回 false 时这条消息属于别的消费方，继续往下走旧协调器）。
	if h.discussionRuntime != nil {
		content := ""
		if s, ok := result.Payload["content"].(string); ok {
			content = s
		}
		kind, _ := result.Payload["kind"].(string)
		errMsg, _ := result.Payload["error"].(string)
		if h.discussionRuntime.interactiveAcceptTurn(ctx, msg.FromUserID, result.RequestID,
			content, kind == "error", errMsg) {
			return
		}
	}

	resp := NodeResponse{
		NodeID:    msg.FromUserID,
		MsgType:   "command_result",
		RequestID: result.RequestID,
		TurnID:    result.RequestID,
	}

	if payload, ok := result.Payload["content"]; ok {
		if s, ok := payload.(string); ok {
			resp.Content = s
		}
	}

	h.dispatchToCoordinator(ctx, msg.TargetID, resp)
}

func (h *systemHandler) handleStreamMessage(ctx context.Context, msg NormalizedMessage) {
	var stream map[string]interface{}
	if err := json.Unmarshal([]byte(msg.Content), &stream); err != nil {
		h.logger.Warn("rongcloud: parse stream message", "error", err, "msgUID", msg.MsgUID)
		return
	}

	resp := NodeResponse{
		NodeID:  msg.FromUserID,
		MsgType: "stream",
	}

	if v, ok := stream["turn_id"]; ok {
		resp.TurnID = fmt.Sprintf("%v", v)
	}
	if v, ok := stream["stream_type"]; ok {
		resp.StreamType = fmt.Sprintf("%v", v)
	}
	if v, ok := stream["content"]; ok {
		resp.Content = fmt.Sprintf("%v", v)
	}
	if v, ok := stream["chunk_index"]; ok {
		if i, ok := v.(float64); ok {
			resp.ChunkIndex = int(i)
		}
	}
	if v, ok := stream["error"]; ok {
		resp.Error = fmt.Sprintf("%v", v)
	}

	h.dispatchToCoordinator(ctx, msg.TargetID, resp)
}

func (h *systemHandler) handleTextResponse(ctx context.Context, msg NormalizedMessage) {
	var tc textContent
	if err := json.Unmarshal([]byte(msg.Content), &tc); err == nil {
		msg.Content = tc.Content
	}

	resp := NodeResponse{
		NodeID:  msg.FromUserID,
		MsgType: "text",
		Content: msg.Content,
		TurnID:  msg.MsgUID,
	}

	h.dispatchToCoordinator(ctx, msg.TargetID, resp)
}

func (h *systemHandler) dispatchToCoordinator(ctx context.Context, targetID string, resp NodeResponse) {
	if h.registry == nil {
		h.logger.Debug("rongcloud: no registry configured, dropping node response")
		return
	}

	chatroomID, err := parseUUIDParam(targetID)
	if err != nil {
		h.logger.Warn("rongcloud: cannot parse targetID as chatroom UUID", "targetID", targetID, "error", err)
		return
	}

	if !h.registry.SendResponse(chatroomID, resp) {
		h.logger.Warn("rongcloud: no active discussion or channel full", "chatroomID", targetID)
	}
}

func isCommandResult(msg NormalizedMessage) bool {
	if msg.ObjectName != objectNameCommand {
		return false
	}
	var peek struct {
		MsgType string `json:"msg_type"`
	}
	if err := json.Unmarshal([]byte(msg.Content), &peek); err != nil {
		return false
	}
	return peek.MsgType == "command_result"
}

func isStreamMessage(objectName string) bool {
	return objectName == objectNameStream
}

var _ = pgtype.UUID{}
