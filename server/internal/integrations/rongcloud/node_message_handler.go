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
	//
	// 模型目录响应先于 command_result 判定：它**不是** command_result（自带
	// msg_type，见 node_request_broker.go 的 AcceptCatalogResponse），但同样
	// 只能由 broker 消费，不能落进命令分发器。
	if msg.ObjectName == objectNameCommand && h.nodeRequests != nil {
		if h.nodeRequests.AcceptCatalogResponse(msg.FromUserID, msg.Content) {
			return
		}
		if h.nodeRequests.AcceptResponse(msg.FromUserID, msg.Content) {
			return
		}
	}

	switch msg.ObjectName {
	case objectNameCommand:
		h.handleCommandResult(ctx, msg)
	case objectNameStream:
		// 这里是 v1 的 RC:StreamMsg（老圆桌协议），拿不到中继轮次的
		// request_id —— 中继续期只走私聊心跳（relay_ping_ 前缀的
		// command_result，见 handleCommandResult）。曾经在这里按
		// msg.FromUserID 调 NotifyStreamActivity，但那个函数要的是
		// request_id、传用户 id 恒为 false，等于没做事还会误导排查。
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
		// 流式心跳（设备端 relay_ping_<requestId>）只做续期，绝不当回包。
		// 必须排在 interactiveAcceptTurn 与 relayHub 之前：
		//   · 讨论组的 requestId 是 UUID，但 relayHub.NotifyStreamActivity 对
		//     **任何** relay_ping_ 前缀都恒返回 true（未知轮次也消化），放在
		//     它后面会让讨论心跳永远被吞掉；
		//   · 讨论组此前完全收不到心跳 → 单轮只有派发时定死的墙钟，设备明明
		//     还在流式产出也照样被判超时：第 N 轮「响应超时」→ 重试 → 跳过 →
		//     主持人汇总同样超时 → summary_failed（线上 2026-10-22）。
		if turnID, ok := parseStreamPing(result.RequestID); ok {
			if h.discussionRuntime.interactiveNotifyStreamActivity(ctx, turnID) {
				return
			}
			if h.relayHub != nil && h.relayHub.NotifyStreamActivity(result.RequestID) {
				return
			}
		}
		if h.discussionRuntime.interactiveAcceptTurn(ctx, msg.FromUserID, result.RequestID,
			content, kind == "error", errMsg) {
			return
		}
		// 讨论组闲聊中继：relay_ 前缀的 your_turn 回包在这里消化，不进旧协调器。
		if h.relayHub != nil && h.relayHub.AcceptResponse(ctx, msg.FromUserID, result.RequestID,
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

// isModelCatalogResponse 识别设备对 `discussion_model_catalog_request` 的回包
// （管理端「供应商 → 模型」选择器要的那份目录）。
//
// 它走 command 通道，但**不是** command_result —— 设备端按 web 的协议直接回
// `{msg_type:"discussion_model_catalog_response", protocolVersion:2, …}`
// （packages/xiachat-cli/src/model-catalog.ts 的 buildModelCatalogResponse）。
// 不在这里单独识别的话，它会掉进 dispatchCommandAsync 被当成一条未知命令，
// 于是等目录的那一侧只能等到超时。
func isModelCatalogResponse(msg NormalizedMessage) bool {
	if msg.ObjectName != objectNameCommand {
		return false
	}
	var peek struct {
		MsgType string `json:"msg_type"`
	}
	if err := json.Unmarshal([]byte(msg.Content), &peek); err != nil {
		return false
	}
	return peek.MsgType == catalogResponseMsgType
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

// isDiscussionEventBroadcast 识别服务端代发的 v3 讨论事件广播。融云会把本端
// 发出的消息回灌 webhook；广播以发言节点身份发送后 fromUserId 不再是系统账号，
// handleCommand 的自环守卫拦不住，必须在路由层按内容特征丢弃——它不是命令
// 也不是回包，进入命令分发只会白占去重表并朝节点回一条莫名其妙的 404。
func isDiscussionEventBroadcast(msg NormalizedMessage) bool {
	if msg.ObjectName != objectNameCommand {
		return false
	}
	var peek struct {
		MsgType string `json:"msg_type"`
	}
	if err := json.Unmarshal([]byte(msg.Content), &peek); err != nil {
		return false
	}
	return peek.MsgType == "discussion_event"
}

func isStreamMessage(objectName string) bool {
	return objectName == objectNameStream
}

var _ = pgtype.UUID{}
