package rongcloud

import (
	"context"
	"fmt"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// chatroom_moderation.go 补全 service=chatroom 的拉黑 / 禁言 / 白名单三族动作
// （block / unblock / getBlocklist / mute / unmute / getGaglist / addWhitelist /
// removeWhitelist / getWhitelist）。
//
// 缺口由来：handleChatroomCommand 此前只实现房间生命周期与成员管理，这九个动作
// 全部落进 default 的 404 分支。移动端（clawmessenger-uniapp 的
// subPackages/chatroom/pages/chatroom/members.vue 拉黑/禁言、settings.vue 管理
// 三个名单）是唯一调用方；web 不调用。
//
// 契约对齐旧后端 rongcloud/command_handler.py 的 _handle_chatroom_*：
//   - 写动作（block/unblock/mute/unmute/addWhitelist/removeWhitelist）要求调用者是
//     聊天室创建者（owner），否则 403；房间不存在则 404（与 destroy/update 一致）。
//   - 三个列表是只读、不校验 owner，与 getMembers 一样不强制 actor 注册。
//
// 参数方言：移动端拉黑/禁言发的是单数 userId（字符串），白名单发的是复数
// userIds（数组）；旧后端只认复数，导致移动端的拉黑/禁言在 Python 侧同样是坏的。
// 这里两种写法都收（见 moderationUserIDs），真正把客户端修通。

// moderationUserIDs 从参数里取目标用户列表，兼容单数 userId / user_id（字符串）
// 与复数 userIds / user_ids / memberIds（数组）两种写法，去重并去空。
func moderationUserIDs(params map[string]interface{}) []string {
	if list := getStringList(params["userIds"]); len(list) > 0 {
		return dedupeStrings(list)
	}
	if list := getStringList(params["user_ids"]); len(list) > 0 {
		return dedupeStrings(list)
	}
	if list := getStringList(params["memberIds"]); len(list) > 0 {
		return dedupeStrings(list)
	}
	if single := getStringAny(params, "userId", "user_id", "memberId", "member_id"); single != "" {
		return []string{single}
	}
	return nil
}

// dedupeStrings 去重并去掉空白项，保持出现顺序。
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// moderationMinute 取禁言/拉黑时长（分钟），缺省 0（永久），负数按 0 处理。
func moderationMinute(params map[string]interface{}) int {
	switch v := params["minute"].(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// resolveOwnedChatroom 取房间并校验调用者是创建者。房间不存在 404、非创建者 403。
// 供所有「管理名单」的写动作复用。client 缺失时诚实回 503，不假装成功。
func (h *systemHandler) resolveOwnedChatroom(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) (string, commandResult, bool) {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	if rcID == "" {
		return "", errResult(400, "聊天室 ID 不能为空"), false
	}
	if h.client == nil {
		return "", errResult(503, "rongcloud client not available"), false
	}
	room, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return "", errResult(404, "聊天室不存在"), false
	}
	if room.OwnerUserID != actor.ID {
		return "", errResult(403, "只有聊天室创建者可以管理名单"), false
	}
	return rcID, commandResult{}, true
}

// moderationListResult 把融云 users 列表包成前端期望的 {"users":[...]}。
func moderationListResult(users []map[string]interface{}) commandResult {
	if users == nil {
		users = []map[string]interface{}{}
	}
	return okResult(map[string]interface{}{"users": users})
}

// ---- 黑名单 ----------------------------------------------------------------

func (h *systemHandler) actionChatroomBlock(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID, res, ok := h.resolveOwnedChatroom(ctx, cmd, actor)
	if !ok {
		return res
	}
	userIDs := moderationUserIDs(cmd.Params)
	if len(userIDs) == 0 {
		return errResult(400, "参数不完整")
	}
	if err := h.client.blockChatroomMember(ctx, rcID, userIDs, moderationMinute(cmd.Params)); err != nil {
		h.logger.Error("rongcloud: chatroom block", "error", err, "chatroom_id", rcID)
		return errResult(500, "拉黑用户失败")
	}
	return okResult(map[string]interface{}{"ok": true})
}

func (h *systemHandler) actionChatroomUnblock(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID, res, ok := h.resolveOwnedChatroom(ctx, cmd, actor)
	if !ok {
		return res
	}
	userIDs := moderationUserIDs(cmd.Params)
	if len(userIDs) == 0 {
		return errResult(400, "参数不完整")
	}
	if err := h.client.unblockChatroomMember(ctx, rcID, userIDs); err != nil {
		h.logger.Error("rongcloud: chatroom unblock", "error", err, "chatroom_id", rcID)
		return errResult(500, "移除黑名单失败")
	}
	return okResult(map[string]interface{}{"ok": true})
}

func (h *systemHandler) actionChatroomGetBlocklist(ctx context.Context, cmd CommandContent) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "聊天室 ID 不能为空")
	}
	if h.client == nil {
		return errResult(503, "rongcloud client not available")
	}
	users, err := h.client.listChatroomBlocklist(ctx, rcID)
	if err != nil {
		h.logger.Error("rongcloud: chatroom getBlocklist", "error", err, "chatroom_id", rcID)
		return errResult(500, "获取黑名单失败")
	}
	return moderationListResult(users)
}

// ---- 禁言 ------------------------------------------------------------------

func (h *systemHandler) actionChatroomMute(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID, res, ok := h.resolveOwnedChatroom(ctx, cmd, actor)
	if !ok {
		return res
	}
	userIDs := moderationUserIDs(cmd.Params)
	if len(userIDs) == 0 {
		return errResult(400, "参数不完整")
	}
	if err := h.client.gagChatroomMember(ctx, rcID, userIDs, moderationMinute(cmd.Params)); err != nil {
		h.logger.Error("rongcloud: chatroom mute", "error", err, "chatroom_id", rcID)
		return errResult(500, "禁言用户失败")
	}
	return okResult(map[string]interface{}{"ok": true})
}

func (h *systemHandler) actionChatroomUnmute(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID, res, ok := h.resolveOwnedChatroom(ctx, cmd, actor)
	if !ok {
		return res
	}
	userIDs := moderationUserIDs(cmd.Params)
	if len(userIDs) == 0 {
		return errResult(400, "参数不完整")
	}
	if err := h.client.ungagChatroomMember(ctx, rcID, userIDs); err != nil {
		h.logger.Error("rongcloud: chatroom unmute", "error", err, "chatroom_id", rcID)
		return errResult(500, "解除禁言失败")
	}
	return okResult(map[string]interface{}{"ok": true})
}

func (h *systemHandler) actionChatroomGetGaglist(ctx context.Context, cmd CommandContent) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "聊天室 ID 不能为空")
	}
	if h.client == nil {
		return errResult(503, "rongcloud client not available")
	}
	users, err := h.client.listChatroomGaglist(ctx, rcID)
	if err != nil {
		h.logger.Error("rongcloud: chatroom getGaglist", "error", err, "chatroom_id", rcID)
		return errResult(500, "获取禁言列表失败")
	}
	return moderationListResult(users)
}

// ---- 白名单 ----------------------------------------------------------------

func (h *systemHandler) actionChatroomAddWhitelist(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID, res, ok := h.resolveOwnedChatroom(ctx, cmd, actor)
	if !ok {
		return res
	}
	userIDs := moderationUserIDs(cmd.Params)
	if len(userIDs) == 0 {
		return errResult(400, "参数不完整")
	}
	if err := h.client.addChatroomWhitelist(ctx, rcID, userIDs); err != nil {
		h.logger.Error("rongcloud: chatroom addWhitelist", "error", err, "chatroom_id", rcID)
		return errResult(500, "添加白名单失败")
	}
	return okResult(map[string]interface{}{"ok": true})
}

func (h *systemHandler) actionChatroomRemoveWhitelist(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID, res, ok := h.resolveOwnedChatroom(ctx, cmd, actor)
	if !ok {
		return res
	}
	userIDs := moderationUserIDs(cmd.Params)
	if len(userIDs) == 0 {
		return errResult(400, "参数不完整")
	}
	if err := h.client.removeChatroomWhitelist(ctx, rcID, userIDs); err != nil {
		h.logger.Error("rongcloud: chatroom removeWhitelist", "error", err, "chatroom_id", rcID)
		return errResult(500, "移除白名单失败")
	}
	return okResult(map[string]interface{}{"ok": true})
}

func (h *systemHandler) actionChatroomGetWhitelist(ctx context.Context, cmd CommandContent) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "聊天室 ID 不能为空")
	}
	if h.client == nil {
		return errResult(503, "rongcloud client not available")
	}
	users, err := h.client.listChatroomWhitelist(ctx, rcID)
	if err != nil {
		h.logger.Error("rongcloud: chatroom getWhitelist", "error", err, "chatroom_id", rcID)
		return errResult(500, "获取白名单失败")
	}
	return moderationListResult(users)
}
