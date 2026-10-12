package rongcloud

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// 群组系统命令（service=group）。
//
// C 端 web 的「建群」走的就是这条通道：sendSystemRequest('group', 'create',
// {groupName, ownerId, memberIds, portraitUri, openclawMaxRounds})，紧接着再发一条
// ('group','update',{groupId, openclawMaxRounds}) 兜底（见
// clawmessenger-web/src/services/im.ts 的 createGroupConversation）。
// Go 的 systemHandler 此前只认 ping/chatroom/device/discussion/ai，group 落进
// unknown service 分支，所以建群一直是坏的。
//
// 这里只补 web 真正会调用的两个动作：create 与 update。其余动作（getMyGroups /
// getInfo / getMembers / join / quit / dismiss）旧后端有，但当前 C 端并不调用 ——
// 会话列表由融云 SDK 客户端侧维护，管理端的群组列表则直接读本地镜像。
//
// 职责切分：融云 API 调用留在本包（group_api.go），数据库归 HTTP 层，两者通过
// GroupMirror 这组回调对接。

// GroupRecord is one RongCloud group as it is mirrored locally.
type GroupRecord struct {
	GroupID     string
	GroupName   string
	OwnerID     string
	PortraitURI string
	Description string
	MemberIDs   []string
	MaxRounds   int
}

// GroupMirror is the local projection of RongCloud groups. Every callback is
// provided by the HTTP layer (which owns the database); a nil field disables the
// corresponding step rather than failing the command, so a deployment without the
// group feature keeps working for everything else.
type GroupMirror struct {
	// AllocateID mints the platform's numeric group id (same style as user and
	// node ids), unique across the existing groups.
	AllocateID func(ctx context.Context) (string, error)
	// SaveGroup upserts the group and replaces its member list.
	SaveGroup func(ctx context.Context, group GroupRecord) error
	// OwnerOf returns the group's owner (a RongCloud user id) and whether the
	// group is mirrored at all.
	OwnerOf func(ctx context.Context, groupID string) (string, bool, error)
	// UpdateMeta applies a partial settings change (empty strings / nil pointers
	// mean "leave as is").
	UpdateMeta func(ctx context.Context, groupID, groupName, portraitURI, description string, maxRounds *int, stopped *bool) error
	// FallbackAgent returns an RongCloud user id of a live agent node, used to
	// seed a group with an AI member when the creator picked none.
	FallbackAgent func(ctx context.Context) (string, bool, error)
}

const (
	groupMaxRoundsMin = 1
	groupMaxRoundsMax = 100
	// defaultGroupMaxRounds matches the legacy backend's default and the web's
	// own fallback (createGroupConversation passes 20).
	defaultGroupMaxRounds = 20
)

func (h *systemHandler) handleGroupCommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	if h.groupMirror == nil {
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	switch cmd.Action {
	case "create":
		return h.handleGroupCreate(ctx, msg, cmd)
	case "update":
		return h.handleGroupUpdate(ctx, msg, cmd)
	default:
		return h.sendWebError(ctx, msg, cmd, 400, fmt.Sprintf("unsupported group action: %s", cmd.Action))
	}
}

func (h *systemHandler) handleGroupCreate(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	groupName := strings.TrimSpace(getString(cmd.Params, "groupName"))
	if groupName == "" {
		return h.sendWebError(ctx, msg, cmd, 400, "groupName is required")
	}
	// 群主以融云消息发送者为准：客户端上报的 ownerId 不作数（旧后端同样处理），
	// 否则任何人都能把群挂到别人名下。
	ownerID := strings.TrimSpace(msg.FromUserID)
	if ownerID == "" || ownerID == h.nodeID {
		return h.sendWebError(ctx, msg, cmd, 400, "group owner cannot be resolved")
	}
	if h.groupMirror.AllocateID == nil || h.groupMirror.SaveGroup == nil {
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}

	memberIDs := getStringSlice(cmd.Params, "memberIds")
	portraitURI := strings.TrimSpace(getString(cmd.Params, "portraitUri"))
	description := strings.TrimSpace(getString(cmd.Params, "description"))
	maxRounds := groupMaxRounds(cmd.Params["openclawMaxRounds"])

	userIDs := dedupeUserIDs(append([]string{ownerID}, memberIDs...))
	// 本地部署的 AI 通过自己的融云账号收群消息。创建者没选 AI 时补一个，
	// 否则这个群只有人类成员能收到消息（沿用旧后端语义）。
	if !containsAgentNode(userIDs) && h.groupMirror.FallbackAgent != nil {
		if fallback, ok, err := h.groupMirror.FallbackAgent(ctx); err == nil && ok {
			userIDs = append(userIDs, fallback)
		} else if err != nil {
			h.logger.Warn("rongcloud: group create fallback agent lookup", "error", err)
		}
	}

	groupID, err := h.groupMirror.AllocateID(ctx)
	if err != nil {
		h.logger.Error("rongcloud: group create allocate id", "error", err)
		return h.sendWebError(ctx, msg, cmd, 500, "failed to allocate a group id")
	}

	if err := h.client.createGroup(ctx, groupID, groupName, userIDs); err != nil {
		h.logger.Error("rongcloud: group create", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 500, "failed to create the RongCloud group")
	}

	// 融云侧已经建好，本地镜像写失败不回滚（镜像只是管理端的读模型，
	// 丢一次可以在下次 update 时补回来），但必须留下日志。
	if err := h.groupMirror.SaveGroup(ctx, GroupRecord{
		GroupID:     groupID,
		GroupName:   groupName,
		OwnerID:     ownerID,
		PortraitURI: portraitURI,
		Description: description,
		MemberIDs:   userIDs,
		MaxRounds:   maxRounds,
	}); err != nil {
		h.logger.Error("rongcloud: group mirror save", "error", err, "group_id", groupID)
	}

	h.logger.Info("rongcloud: group created",
		"group_id", groupID, "owner_id", ownerID, "members", len(userIDs))
	return h.sendWebOK(ctx, msg, cmd, map[string]interface{}{
		"groupId":           groupID,
		"groupName":         groupName,
		"ownerId":           ownerID,
		"portraitUri":       portraitURI,
		"description":       description,
		"openclawMaxRounds": maxRounds,
	})
}

func (h *systemHandler) handleGroupUpdate(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	groupID := strings.TrimSpace(getString(cmd.Params, "groupId"))
	if groupID == "" {
		return h.sendWebError(ctx, msg, cmd, 400, "groupId is required")
	}
	if h.groupMirror.OwnerOf == nil || h.groupMirror.UpdateMeta == nil {
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}

	ownerID, found, err := h.groupMirror.OwnerOf(ctx, groupID)
	if err != nil {
		h.logger.Error("rongcloud: group update owner lookup", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	if !found {
		return h.sendWebError(ctx, msg, cmd, 404, "group not found")
	}
	if ownerID != strings.TrimSpace(msg.FromUserID) {
		return h.sendWebError(ctx, msg, cmd, 403, "only the group owner can change its settings")
	}

	groupName := strings.TrimSpace(getString(cmd.Params, "groupName"))
	portraitURI := strings.TrimSpace(getString(cmd.Params, "portraitUri"))
	description := strings.TrimSpace(getString(cmd.Params, "description"))

	// 名字/头像属于融云群资料：必须先刷融云，否则本端看着是新的、其他成员
	// 读到的还是旧资料（旧后端同样先刷融云再落本地）。
	if groupName != "" || portraitURI != "" {
		if err := h.client.refreshGroupInfo(ctx, groupID, groupName, portraitURI); err != nil {
			h.logger.Error("rongcloud: group refresh", "error", err, "group_id", groupID)
			return h.sendWebError(ctx, msg, cmd, 500, "failed to update the RongCloud group profile")
		}
	}

	maxRounds, _ := groupMaxRoundsPtr(cmd.Params["openclawMaxRounds"])
	stopped, _ := groupBoolPtr(cmd.Params["openclawStopped"])
	if err := h.groupMirror.UpdateMeta(ctx, groupID, groupName, portraitURI, description, maxRounds, stopped); err != nil {
		h.logger.Error("rongcloud: group mirror update", "error", err, "group_id", groupID)
	}

	return h.sendWebOK(ctx, msg, cmd, map[string]interface{}{
		"groupId":     groupID,
		"groupName":   groupName,
		"portraitUri": portraitURI,
		"description": description,
	})
}

func (h *systemHandler) sendWebOK(ctx context.Context, msg NormalizedMessage, cmd CommandContent, data interface{}) error {
	return h.client.sendWebCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID,
		cmd.Service, cmd.Action, "success", 200, data, "ok")
}

func (h *systemHandler) sendWebError(ctx context.Context, msg NormalizedMessage, cmd CommandContent, code int, message string) error {
	return h.client.sendWebCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID,
		cmd.Service, cmd.Action, "error", code, nil, message)
}

// getStringSlice reads a JSON array of strings, tolerating a single string or a
// comma-separated list (the web has sent both shapes historically).
func getStringSlice(params map[string]interface{}, key string) []string {
	value, ok := params[key]
	if !ok || value == nil {
		return nil
	}
	out := make([]string, 0, 4)
	switch typed := value.(type) {
	case []interface{}:
		for _, item := range typed {
			if s, ok := item.(string); ok {
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					out = append(out, trimmed)
				}
			}
		}
	case []string:
		for _, item := range typed {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				out = append(out, trimmed)
			}
		}
	case string:
		for _, part := range strings.Split(typed, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
	}
	return out
}

// dedupeUserIDs removes blanks and duplicates, keeping the first occurrence, so
// the owner stays first and 融云 does not receive the same userId twice.
func dedupeUserIDs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

// containsAgentNode reports whether the member list already includes an agent
// node. Node RongCloud ids are namespaced rc_node_<machineKey>[_<agent>] (see
// node_service.go), so the prefix is the cheap, DB-free check the create path
// needs.
func containsAgentNode(userIDs []string) bool {
	for _, id := range userIDs {
		if strings.HasPrefix(id, "rc_node_") {
			return true
		}
	}
	return false
}

// groupMaxRounds clamps the round limit into the range the runtime accepts.
func groupMaxRounds(value interface{}) int {
	if rounds, ok := groupMaxRoundsPtr(value); ok && rounds != nil {
		return *rounds
	}
	return defaultGroupMaxRounds
}

// groupMaxRoundsPtr returns a clamped value, or (nil,false) when absent/unusable
// so a partial update does not clobber the stored setting.
func groupMaxRoundsPtr(value interface{}) (*int, bool) {
	var number int
	switch typed := value.(type) {
	case float64:
		if typed != float64(int(typed)) {
			return nil, false
		}
		number = int(typed)
	case int:
		number = typed
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return nil, false
		}
		number = parsed
	default:
		return nil, false
	}
	if number < groupMaxRoundsMin {
		number = groupMaxRoundsMin
	}
	if number > groupMaxRoundsMax {
		number = groupMaxRoundsMax
	}
	return &number, true
}

func groupBoolPtr(value interface{}) (*bool, bool) {
	switch typed := value.(type) {
	case bool:
		return &typed, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1":
			result := true
			return &result, true
		case "false", "0":
			result := false
			return &result, true
		}
	}
	return nil, false
}
