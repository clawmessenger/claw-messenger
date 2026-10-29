package rongcloud

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/internal/quota"
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
// 当前两个客户端真正会调用的动作（逐个 grep 出来的，不是猜的）：
//   - clawmessenger-web：create / update / addMember / removeMember / getMembers /
//     getMyGroups / getProfiles
//   - clawmessenger-uniapp：上面七个 + getInfo / transfer / quit / dismiss
// 也就是说 11 个动作全部有调用方。早期这里只实现了 create / update，理由是「其余
// 动作 C 端并不调用」—— 那个判断是错的：群资料页一进来就调 getInfo，成员列表调
// getMembers，结果是「群聊添加成员报错 unsupported group action: addMember」这一类
// 问题。本文件与 group_members.go 把剩下的 9 个补齐。
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

// GroupMember is one mirrored membership row, enriched with whatever human
// profile the local database could supply. Agent nodes have no claw_im_users
// row, so their nickname/portrait stay empty and the client falls back to the
// raw user id.
type GroupMember struct {
	UserID      string
	Nickname    string
	PortraitURI string
	Role        string
}

// GroupProfile is the group-level view the C-end reads back (getInfo /
// getProfiles / getMyGroups). MemberCount is derived from the membership table
// rather than the stored counter, so it can never drift from the rows.
type GroupProfile struct {
	GroupID     string
	GroupName   string
	OwnerID     string
	PortraitURI string
	Description string
	MemberCount int
	MaxRounds   int
	Stopped     bool
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
	// IsMember reports whether a RongCloud user id is in the mirrored group.
	// addMember gates on it ("只有群成员可以邀请新成员").
	IsMember func(ctx context.Context, groupID, userID string) (bool, error)
	// AddMembers inserts members idempotently and recomputes member_count.
	AddMembers func(ctx context.Context, groupID string, userIDs []string) error
	// RemoveMember deletes one member and recomputes member_count.
	RemoveMember func(ctx context.Context, groupID, userID string) error
	// ListMembers returns the mirrored member rows (getMembers fallback and the
	// nickname/portrait source for the RongCloud answer).
	ListMembers func(ctx context.Context, groupID string) ([]GroupMember, error)
	// ListAiMembers returns the group's AI speaker list in speaking order,
	// resolved against the node table (machine-host nodes with empty ai_type
	// are excluded). The group relay uses it; nil disables relayGroupMessage.
	ListAiMembers func(ctx context.Context, groupID string) ([]GroupMember, error)
	// ListGroupsByUser returns the groups a user belongs to (getMyGroups).
	ListGroupsByUser func(ctx context.Context, userID string) ([]GroupProfile, error)
	// GroupProfiles returns the mirrored profiles of the requested groups
	// (getInfo / getProfiles), skipping ids that are not mirrored.
	GroupProfiles func(ctx context.Context, groupIDs []string) ([]GroupProfile, error)
	// SetOwner rewrites the group owner and the owner/member roles (transfer).
	SetOwner func(ctx context.Context, groupID, newOwnerID string) error
	// DeleteGroup drops the group (members cascade) — the dismiss path.
	DeleteGroup func(ctx context.Context, groupID string) error
}

const (
	groupMaxRoundsMin = 1
	groupMaxRoundsMax = 100
	// defaultGroupMaxRounds matches the legacy backend's default and the web's
	// own fallback (createGroupConversation passes 20).
	defaultGroupMaxRounds = 20
	// defaultGroupRounds is what getInfo/getProfiles report when the group has no
	// stored openclaw_max_rounds — the legacy backend answered 10 in that case.
	defaultGroupRounds = 10
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
	case "addMember":
		return h.handleGroupAddMember(ctx, msg, cmd)
	case "removeMember":
		return h.handleGroupRemoveMember(ctx, msg, cmd)
	case "getMembers":
		return h.handleGroupMembers(ctx, msg, cmd)
	case "getMyGroups":
		return h.handleGroupMine(ctx, msg, cmd)
	case "getProfiles":
		return h.handleGroupProfiles(ctx, msg, cmd)
	case "getInfo":
		return h.handleGroupInfo(ctx, msg, cmd)
	case "transfer":
		return h.handleGroupTransfer(ctx, msg, cmd)
	case "quit":
		return h.handleGroupQuit(ctx, msg, cmd)
	case "dismiss":
		return h.handleGroupDismiss(ctx, msg, cmd)
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
	// 创建配额硬校验（超限：429 QUOTA_EXCEEDED{kind,limit,used}；未装配放行）。
	if qErr := h.quotaCheck(ctx, ownerID, quota.KindGroup); qErr != nil {
		h.logger.Info("rongcloud: group create blocked by quota",
			"owner_id", ownerID, "limit", qErr.Limit, "used", qErr.Used)
		return h.sendWebQuotaExceeded(ctx, msg, cmd, qErr)
	}

	memberIDs := getStringSlice(cmd.Params, "memberIds")
	// 建群头像非必填（web 的 CreateGroupDialog 初始就是空串）。空值落**群聊**默认图
	// （两人剪影），不是真人那张灰人形 —— 移动端读的就是融云群资料的 portraitUri，
	// 全平台都落同一张图的话，会话列表里一排群长得一模一样。
	portraitURI := GroupPortraitIfEmpty(GroupPortraitKindGroup, getString(cmd.Params, "portraitUri"))
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

	// 加入侧闸门（迁移 607 的 allow_group）也必须覆盖**建群**这条路径。
	//
	// 这是最容易漏的一处：addMember 有闸门，create 却把 memberIds 直接塞给
	// /group/create.json —— 于是「新建群聊」成了绕过口，勾上别人关了群聊权限的
	// agent，照样被拉进群（只是进去以后永远不吭声）。只在候选名单上做过滤等于把
	// 唯一防线放在前端：刷新一下缓存、或者换小程序就破了。闸门必须落在真正写
	// 数据的地方。
	//
	// 判据与 addMember 完全同源（decideAgentJoin），不要在这里再写一份 if：两道
	// 闸门的主人口径一旦分叉，就会出现「建群加得进去、加人加不进去」这种没法解释
	// 的组合。
	//
	// 与 addMember 唯一的差别：**不因为全员被拦就拒绝建群**。用户要的是一个群，
	// 不是一次权限报错；群照建，被拦的成员不写入，明细随成功回包带回去点名。
	perms, err := h.agentJoinPermissions(ctx, userIDs)
	if err != nil {
		// 写操作 fail-closed：group_join_gate.go 的注释点过名，「把查询故障翻译成
		// 全员无限制」是最危险的退化 —— 在这条路径上就是「一次权限库抖动，把所有
		// 人的开关都当成开的，顺手把一批不该进群的 agent 拉进来」。宁可 503 让用户
		// 重试也不建。注意这与好友列表的 fail-open 不矛盾：那边只是少显示一个人，
		// 这边是写数据。
		h.logger.Error("rongcloud: group create permission lookup", "error", err)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	joinable, rejected := decideAgentJoin(userIDs, perms, ownerID)
	// 群主必须留在名单里：融云建群要求群主本身是成员，把 owner 判掉只会建出一个
	// 没有群主的群，或者直接被融云拒掉。ownerID 能落到 rejected 的唯一情形是
	// 「群主自己就是别人名下的 agent，且那个人关了开关」—— 现实中不成立，但不能
	// 靠「现实中不成立」兜底：真出现时要保证群还建得出来，并且留一条 warn。
	if !containsUserID(joinable, ownerID) {
		h.logger.Warn("rongcloud: group create owner would be filtered by the join gate",
			"owner_id", ownerID)
		joinable = append([]string{ownerID}, joinable...)
		rejected = withoutRejection(rejected, ownerID)
	}
	userIDs = joinable

	groupID, err := h.groupMirror.AllocateID(ctx)
	if err != nil {
		h.logger.Error("rongcloud: group create allocate id", "error", err)
		return h.sendWebError(ctx, msg, cmd, 500, "failed to allocate a group id")
	}

	if err := h.client.createGroup(ctx, groupID, groupName, userIDs); err != nil {
		h.logger.Error("rongcloud: group create", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 500, "failed to create the RongCloud group")
	}

	// 融云 /group/create.json 不收 portraitUri（见 group_api.go），群头像只能建群后
	// 用 /group/refresh.json 补一次。best-effort：失败只记日志，绝不回滚已建好的群
	// —— 下一次 group/update 或 backfill 还能补。
	if err := h.client.refreshGroupInfo(ctx, groupID, "", portraitURI); err != nil {
		h.logger.Warn("rongcloud: group create portrait refresh failed",
			"group_id", groupID, "portrait_uri", portraitURI, "error", err)
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
	data := map[string]interface{}{
		"groupId":           groupID,
		"groupName":         groupName,
		"ownerId":           ownerID,
		"portraitUri":       portraitURI,
		"description":       description,
		"openclawMaxRounds": maxRounds,
	}
	// 被拦下的成员必须逐人点名，字段形状与 addMember 的 skipped 一致
	// （[]{userId, reason}）—— 前端已经按那个形状做文案映射，换个形状会静默退化成
	// 「建群成功但少了几个人」的哑巴结果。
	if len(rejected) > 0 {
		data["skipped"] = rejected
		h.logger.Info("rongcloud: group created without some members (owner permission denied)",
			"group_id", groupID, "owner_id", ownerID, "skipped", len(rejected))
	}
	return h.sendWebOK(ctx, msg, cmd, data)
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

// sendWebErrorWithData 与 sendWebError 相同，但**带上 data**。
//
// 只有「失败里必须点名到具体是哪几个」的场景才需要它：一句 message 说不清
// 「5 个好友里第 3 个的智能体被主人关掉了群聊权限」，而前端要靠 data 才能把
// 这句话拼成人话（错误回执的 data 在 web 侧是 error.data，见 services/im.ts）。
func (h *systemHandler) sendWebErrorWithData(ctx context.Context, msg NormalizedMessage, cmd CommandContent, code int, message string, data interface{}) error {
	return h.client.sendWebCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID,
		cmd.Service, cmd.Action, "error", code, data, message)
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

// containsUserID reports whether id is in the list. Used to guarantee the group
// owner survives the join gate (see handleGroupCreate).
func containsUserID(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

// withoutRejection drops the entry for id from a rejection list. Only used for
// the owner escape hatch in handleGroupCreate: if the owner is being force-kept
// in the member list, it must not also be reported as skipped.
func withoutRejection(rejected []agentJoinRejection, id string) []agentJoinRejection {
	if len(rejected) == 0 {
		return rejected
	}
	out := make([]agentJoinRejection, 0, len(rejected))
	for _, entry := range rejected {
		if entry.UserID == id {
			continue
		}
		out = append(out, entry)
	}
	return out
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
