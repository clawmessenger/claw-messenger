package rongcloud

import (
	"context"
	"strings"
)

// 群组系统命令里除 create/update 之外的动作（service=group）。
//
// 这些动作此前全部落进 handleGroupCommand 的 default 分支，回一句
// "unsupported group action: addMember" —— 也就是用户报的那个错。而两个客户端
// （clawmessenger-web / clawmessenger-uniapp）把它们都当正常接口在用：群资料页
// 调 getInfo，成员列表调 getMembers，加人调 addMember。所以这里按旧后端
// clawmessenger-server/rongcloud/command_handler.py 的语义逐个补齐，包括各自的
// 权限判定 —— 权限放宽是最容易在补接口时顺手引入的安全问题：
//
//	addMember    只有**群成员**可以邀请新成员（不是只有群主）
//	removeMember 只有**群主**可以移除成员
//	transfer     只有群主可转让，且新群主必须已在群里
//	quit         群主不能退出（必须先转让或解散）
//	dismiss      只有群主可以解散
//	getInfo/getProfiles/getMembers/getMyGroups 不校验身份，只读
//
// 一致性策略（沿用旧后端）：
//   - getInfo / getProfiles / getMyGroups 只读本地镜像 —— 融云没有「批量取群资料」
//     的接口，本地镜像就是权威投影。
//   - getMembers 以融云为权威成员来源（融云才是成员关系的拥有者），本地镜像负责
//     补昵称/头像，并在融云查询失败时兜底。
//   - addMember / removeMember / transfer 先落融云再改镜像，融云拒绝就不改本地
//     —— 否则本端以为加进去了、别的成员读到的还是旧名单。
//   - quit / dismiss 反过来：先改本地（C 端读的就是本地），融云失败只记日志。
//     这是旧后端自己的取舍，改动它会让「退出后会话还在」之类的体验回归。

// ---- addMember ---------------------------------------------------------------

func (h *systemHandler) handleGroupAddMember(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	groupID := strings.TrimSpace(getString(cmd.Params, "groupId"))
	if groupID == "" {
		return h.sendWebError(ctx, msg, cmd, 400, "groupId is required")
	}
	// 两个客户端都传 userIds；memberIds 是历史别名，一并收下免得整套流程失联。
	memberIDs := dedupeUserIDs(getStringSlice(cmd.Params, "userIds"))
	if len(memberIDs) == 0 {
		memberIDs = dedupeUserIDs(getStringSlice(cmd.Params, "memberIds"))
	}
	if len(memberIDs) == 0 {
		return h.sendWebError(ctx, msg, cmd, 400, "userIds is required")
	}
	if h.groupMirror.IsMember == nil || h.groupMirror.AddMembers == nil {
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}

	senderID := strings.TrimSpace(msg.FromUserID)
	member, err := h.groupMirror.IsMember(ctx, groupID, senderID)
	if err != nil {
		h.logger.Error("rongcloud: group add member permission lookup", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	if !member {
		return h.sendWebError(ctx, msg, cmd, 403, "only a group member can invite new members")
	}

	// 加入侧闸门（迁移 607 的 allow_group）：主人关掉这个开关表达的是「别把我
	// 的智能体拉进别人的群」，而此前它只在派发侧生效 —— agent 照样被拉进去，
	// 只是永远不吭声，用户看到的是「这群里有个哑巴」。
	//
	// 逐人判、部分放行：一次勾 5 个人，只为其中 1 个 agent 被拦就整单失败，
	// 用户只会反复重试；而被拦的必须回给前端点名，否则他会以为加成功了。
	perms, err := h.agentJoinPermissions(ctx, memberIDs)
	if err != nil {
		// 读不到权限位就不知道能不能加。宁可 503 让用户重试，也不静默放行
		// （与派发侧 "dispatch denied (no-permission-info)" 同一取舍方向）。
		h.logger.Error("rongcloud: group add member permission lookup", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	joinable, rejected := decideAgentJoin(memberIDs, perms, senderID)
	if len(joinable) == 0 {
		h.logger.Info("rongcloud: group add member denied by owner permission",
			"group_id", groupID, "inviter", senderID, "denied", len(rejected))
		return h.sendWebErrorWithData(ctx, msg, cmd, 403,
			"the owners of these agents disabled group chat for them",
			map[string]interface{}{"groupId": groupID, "skipped": rejected})
	}

	if err := h.client.joinGroup(ctx, groupID, joinable); err != nil {
		h.logger.Error("rongcloud: group join", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 500, "failed to add the members to the RongCloud group")
	}

	// 融云已经加进去了；镜像写失败不回滚（镜像只是读模型，丢一次可由下次
	// getMembers 的融云结果遮住），但必须留日志。
	if err := h.groupMirror.AddMembers(ctx, groupID, joinable); err != nil {
		h.logger.Error("rongcloud: group mirror add members", "error", err, "group_id", groupID)
	}

	data := map[string]interface{}{
		"groupId":    groupID,
		"userIds":    joinable,
		"addedCount": len(joinable),
	}
	if len(rejected) > 0 {
		data["skipped"] = rejected
		h.logger.Info("rongcloud: group add member partially denied",
			"group_id", groupID, "added", len(joinable), "denied", len(rejected))
	}

	h.logger.Info("rongcloud: group members added", "group_id", groupID, "count", len(joinable))
	return h.sendWebOK(ctx, msg, cmd, data)
}

// ---- removeMember ------------------------------------------------------------

func (h *systemHandler) handleGroupRemoveMember(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	groupID := strings.TrimSpace(getString(cmd.Params, "groupId"))
	// web 传 userId；移动端同一份值同时放 userId 与 memberId。
	memberID := strings.TrimSpace(getStringAny(cmd.Params, "userId", "memberId"))
	if groupID == "" || memberID == "" {
		return h.sendWebError(ctx, msg, cmd, 400, "groupId and userId are required")
	}
	if h.groupMirror.OwnerOf == nil || h.groupMirror.RemoveMember == nil {
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}

	ownerID, found, err := h.groupMirror.OwnerOf(ctx, groupID)
	if err != nil {
		h.logger.Error("rongcloud: group remove member owner lookup", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	if !found {
		return h.sendWebError(ctx, msg, cmd, 404, "group not found")
	}
	if ownerID != strings.TrimSpace(msg.FromUserID) {
		return h.sendWebError(ctx, msg, cmd, 403, "only the group owner can remove members")
	}
	// 群主把自己移出群要走 quit/transfer，否则融云拒绝后镜像与融云会长期不一致。
	if memberID == ownerID {
		return h.sendWebError(ctx, msg, cmd, 400, "the group owner cannot be removed")
	}

	if err := h.client.quitGroup(ctx, groupID, []string{memberID}); err != nil {
		h.logger.Error("rongcloud: group quit", "error", err, "group_id", groupID, "user_id", memberID)
		return h.sendWebError(ctx, msg, cmd, 500, "failed to remove the member from the RongCloud group")
	}
	if err := h.groupMirror.RemoveMember(ctx, groupID, memberID); err != nil {
		h.logger.Error("rongcloud: group mirror remove member", "error", err, "group_id", groupID, "user_id", memberID)
	}

	h.logger.Info("rongcloud: group member removed", "group_id", groupID, "user_id", memberID)
	return h.sendWebOK(ctx, msg, cmd, map[string]interface{}{
		"groupId": groupID,
		"userId":  memberID,
		"removed": true,
	})
}

// ---- getMembers --------------------------------------------------------------

func (h *systemHandler) handleGroupMembers(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	groupID := strings.TrimSpace(getString(cmd.Params, "groupId"))
	if groupID == "" {
		return h.sendWebError(ctx, msg, cmd, 400, "groupId is required")
	}

	// 融云是成员关系的权威来源。
	ids, rongErr := h.client.queryGroupMembers(ctx, groupID)
	if rongErr != nil {
		h.logger.Warn("rongcloud: group member query", "error", rongErr, "group_id", groupID)
	}

	// 本地镜像：补昵称/头像/角色，也是融云查询失败时的成员来源。
	var mirrored []GroupMember
	if h.groupMirror.ListMembers != nil {
		list, err := h.groupMirror.ListMembers(ctx, groupID)
		if err != nil {
			h.logger.Error("rongcloud: group mirror members", "error", err, "group_id", groupID)
		} else {
			mirrored = list
		}
	}

	if len(ids) == 0 {
		if len(mirrored) == 0 {
			if rongErr != nil {
				return h.sendWebError(ctx, msg, cmd, 500, "failed to load the group members")
			}
			// 融云查得到，群里确实没人：正常的空列表，不是错误。
			return h.sendWebOK(ctx, msg, cmd, []interface{}{})
		}
		// 取舍：融云「成功但零成员」也回退镜像。刚建好的群在融云侧会有短暂的
		// 可见性延迟，此刻若直接信空结果，用户点开成员列表会看到「暂无成员」；
		// 而镜像此刻一定是对的（create 刚写完）。真正被清空的群会在下一次有人
		// 进出时与镜像重新对齐。
		return h.sendWebOK(ctx, msg, cmd, groupMemberViews(nil, mirrored))
	}
	return h.sendWebOK(ctx, msg, cmd, groupMemberViews(ids, mirrored))
}

// groupMemberViews 把「融云给出的成员 id」与「本地镜像的补充信息」合成 C 端契约的
// 成员数组。
//
// ids 为 nil 时以镜像行为准（融云查询失败的兜底）；否则以 ids 为准，镜像只用来补
// 字段 —— 融云里已经踢掉、但镜像还没来得及删的人不会出现在列表里。
func groupMemberViews(ids []string, mirrored []GroupMember) []map[string]interface{} {
	byID := make(map[string]GroupMember, len(mirrored))
	for _, member := range mirrored {
		byID[member.UserID] = member
	}

	ordered := ids
	if len(ordered) == 0 {
		ordered = make([]string, 0, len(mirrored))
		for _, member := range mirrored {
			ordered = append(ordered, member.UserID)
		}
	}

	views := make([]map[string]interface{}, 0, len(ordered))
	for _, id := range ordered {
		member := byID[id]
		nickname := member.Nickname
		if nickname == "" {
			nickname = id
		}
		role := member.Role
		if role == "" {
			role = "member"
		}
		views = append(views, map[string]interface{}{
			"userId":        id,
			"username":      id,
			"nickname":      nickname,
			"groupNickname": "",
			"portraitUri":   member.PortraitURI,
			"role":          role,
		})
	}
	return views
}

// ---- getMyGroups -------------------------------------------------------------

func (h *systemHandler) handleGroupMine(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	// 只查调用者自己的群列表：payload 里的 userId 不认（旧后端同），否则任何人
	// 都能枚举别人的群。
	senderID := strings.TrimSpace(msg.FromUserID)
	if senderID == "" {
		return h.sendWebError(ctx, msg, cmd, 400, "userId is required")
	}
	if h.groupMirror.ListGroupsByUser == nil {
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}

	groups, err := h.groupMirror.ListGroupsByUser(ctx, senderID)
	if err != nil {
		h.logger.Error("rongcloud: group list by user", "error", err)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	return h.sendWebOK(ctx, msg, cmd, groupProfileViews(groups, senderID))
}

// ---- getProfiles / getInfo ---------------------------------------------------

func (h *systemHandler) handleGroupProfiles(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	// 移动端可能传 groupIds（数组）或 groupId（单个）。
	groupIDs := dedupeUserIDs(getStringSlice(cmd.Params, "groupIds"))
	if single := strings.TrimSpace(getString(cmd.Params, "groupId")); single != "" {
		groupIDs = dedupeUserIDs(append(groupIDs, single))
	}
	if len(groupIDs) == 0 {
		return h.sendWebError(ctx, msg, cmd, 400, "groupIds is required")
	}
	if h.groupMirror.GroupProfiles == nil {
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}

	profiles, err := h.groupMirror.GroupProfiles(ctx, groupIDs)
	if err != nil {
		h.logger.Error("rongcloud: group profiles", "error", err)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	return h.sendWebOK(ctx, msg, cmd, groupProfileViews(profiles, strings.TrimSpace(msg.FromUserID)))
}

func (h *systemHandler) handleGroupInfo(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	groupID := strings.TrimSpace(getString(cmd.Params, "groupId"))
	if groupID == "" {
		return h.sendWebError(ctx, msg, cmd, 400, "groupId is required")
	}
	if h.groupMirror.GroupProfiles == nil {
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}

	profiles, err := h.groupMirror.GroupProfiles(ctx, []string{groupID})
	if err != nil {
		h.logger.Error("rongcloud: group info", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	if len(profiles) == 0 {
		return h.sendWebError(ctx, msg, cmd, 404, "group not found")
	}
	return h.sendWebOK(ctx, msg, cmd, groupProfileView(profiles[0], strings.TrimSpace(msg.FromUserID)))
}

// ownerViewpoint 判定「观察者本人是否就是这个会话的群主 / 房主」。
//
// 群聊（getInfo/getProfiles/getMyGroups）与讨论组/聊天室（getList/getInfo/update/
// create/setHost/setDiscussionTeam）都下发 isOwner，判定必须只有这一处实现 ——
// 两个地方各写一遍，早晚会有一边漏掉空值判断。
//
// fail-closed 三条：
//   - owner 为空 → false。老群/老聊天室的 owner 列可能是 NULL，取出来是空串；
//     此时**不能**说「谁都不是群主」，但更**不能**说「谁都是」。客户端拿不到
//     true 只是少一个入口（还能走服务端 403 兜底），给错了是把管理入口白送。
//   - viewer 为空 → false。未登录、内部调用、取不到发送者时没有观察者，无从判定。
//   - 两侧都空 → false。这一条单列出来是因为 `"" == ""` 天然成立：少了前两条的
//     任意一条，都会把「空 owner + 空 viewer」判成 true，正好是最坏的情况。
//
// 两侧都先 TrimSpace：镜像列与消息头都可能带空白，一个空格就会让真群主变成
// 非群主 —— 那正是我们要修的那类「功能看起来没了」。
func ownerViewpoint(ownerID, viewerID string) bool {
	owner := strings.TrimSpace(ownerID)
	viewer := strings.TrimSpace(viewerID)
	return owner != "" && viewer != "" && owner == viewer
}

// groupProfileViews 组装 getProfiles/getMyGroups 的数组。
//
// viewerID 是**观察者**（本条命令的发送者）：isOwner 是「相对视角」的字段，同一条
// 群资料对不同调用者答案不同，所以必须逐条按同一个 viewer 判定，不能缓存也不能
// 用群自己的 owner 字段顶替。
func groupProfileViews(profiles []GroupProfile, viewerID string) []map[string]interface{} {
	views := make([]map[string]interface{}, 0, len(profiles))
	for _, profile := range profiles {
		views = append(views, groupProfileView(profile, viewerID))
	}
	return views
}

// groupProfileView 是 getInfo/getProfiles/getMyGroups 共用的单条群资料。
//
// 同时给 groupName 与 name、portraitUri 与 avatar 两组别名：前端各处历史写法不
// 一致（web 读 groupName/portraitUri，移动端 getProfiles 读 groupId|id 与
// portraitUri|avatar），别名一份省得某一端显示空白。
//
// 为什么要下发 isOwner（而不是让客户端拿 ownerId 自己比）：
//   - 群镜像的 owner_id 对老群可能是空串 —— 历史群、从旧后端迁移来的群、早期
//     版本建出来的群都没有回填过这个字段。客户端一旦改成「ownerId === 我」的
//     字符串比对，这些群上真正的群主会被判成非群主，于是「移除成员」入口整块
//     消失，而「添加成员」（不判权限）还在，用户看到的就是「功能没了」。
//   - 客户端本地还有两套 id 空间（平台 userId 与融云 imUserId），比对时得同时
//     试两个，这本身就是服务端有确定答案的事情，不该让每个端各猜一遍。
//
// 重要边界：isOwner 只用于**显示侧**的开关（决定要不要把入口画出来）。真正的写
// 操作仍由 handleGroupRemoveMember 在服务端校验（ownerID != msg.FromUserID 即
// 403），前端不得把 isOwner 当成授权凭据 —— 它是投影，不是令牌。
//
// viewerID 为空（未登录、取不到发送者）时一律 isOwner=false：fail-closed。绝不
// 能因为 owner 为空而把「没有观察者」当成「观察者就是群主」。
func groupProfileView(profile GroupProfile, viewerID string) map[string]interface{} {
	rounds := profile.MaxRounds
	if rounds <= 0 {
		rounds = defaultGroupRounds
	}
	// 判定只走 ownerViewpoint（群聊与聊天室共用一处实现，见其注释）。
	isOwner := ownerViewpoint(profile.OwnerID, viewerID)
	return map[string]interface{}{
		"id":                profile.GroupID,
		"groupId":           profile.GroupID,
		"groupName":         profile.GroupName,
		"name":              profile.GroupName,
		// ownerId 保留镜像原值（可能是空串），不因为 isOwner 的判定而改写：
		// 别的端/老客户端还靠它显示「群主：xxx」，抹成空会让它们更困惑。
		"ownerId":           profile.OwnerID,
		"isOwner":           isOwner,
		"memberCount":       profile.MemberCount,
		"portraitUri":       profile.PortraitURI,
		"avatar":            profile.PortraitURI,
		"description":       profile.Description,
		"openclawMaxRounds": rounds,
		"openclawStopped":   profile.Stopped,
	}
}

// ---- transfer ----------------------------------------------------------------

func (h *systemHandler) handleGroupTransfer(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	groupID := strings.TrimSpace(getString(cmd.Params, "groupId"))
	newOwnerID := strings.TrimSpace(getStringAny(cmd.Params, "newOwnerId", "newOwnerUserId"))
	if groupID == "" || newOwnerID == "" {
		return h.sendWebError(ctx, msg, cmd, 400, "groupId and newOwnerId are required")
	}
	if h.groupMirror.OwnerOf == nil || h.groupMirror.IsMember == nil || h.groupMirror.SetOwner == nil {
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}

	ownerID, found, err := h.groupMirror.OwnerOf(ctx, groupID)
	if err != nil {
		h.logger.Error("rongcloud: group transfer owner lookup", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	if !found {
		return h.sendWebError(ctx, msg, cmd, 404, "group not found")
	}
	if ownerID != strings.TrimSpace(msg.FromUserID) {
		return h.sendWebError(ctx, msg, cmd, 403, "only the group owner can transfer ownership")
	}
	member, err := h.groupMirror.IsMember(ctx, groupID, newOwnerID)
	if err != nil {
		h.logger.Error("rongcloud: group transfer member lookup", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	if !member {
		return h.sendWebError(ctx, msg, cmd, 400, "the new owner must be a group member")
	}

	if err := h.client.transferGroup(ctx, groupID, newOwnerID); err != nil {
		h.logger.Error("rongcloud: group transfer", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 500, "failed to transfer the RongCloud group")
	}
	if err := h.groupMirror.SetOwner(ctx, groupID, newOwnerID); err != nil {
		h.logger.Error("rongcloud: group mirror transfer", "error", err, "group_id", groupID)
	}

	h.logger.Info("rongcloud: group ownership transferred", "group_id", groupID, "new_owner_id", newOwnerID)
	return h.sendWebOK(ctx, msg, cmd, map[string]interface{}{
		"groupId":     groupID,
		"ownerId":     newOwnerID,
		"transferred": true,
	})
}

// ---- quit --------------------------------------------------------------------

func (h *systemHandler) handleGroupQuit(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	groupID := strings.TrimSpace(getString(cmd.Params, "groupId"))
	if groupID == "" {
		return h.sendWebError(ctx, msg, cmd, 400, "groupId is required")
	}
	// 退群的人永远是发送者本人。
	senderID := strings.TrimSpace(msg.FromUserID)
	if senderID == "" {
		return h.sendWebError(ctx, msg, cmd, 400, "userId is required")
	}
	if h.groupMirror.OwnerOf == nil || h.groupMirror.RemoveMember == nil {
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}

	// 群主退群会把群变成无主群，融云也不接受。
	if ownerID, found, err := h.groupMirror.OwnerOf(ctx, groupID); err != nil {
		h.logger.Error("rongcloud: group quit owner lookup", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	} else if found && ownerID == senderID {
		return h.sendWebError(ctx, msg, cmd, 400, "the group owner must transfer ownership or dismiss the group first")
	}

	// 先改本地：C 端读的是镜像，先落本地能让「退出」立刻生效（旧后端同样顺序）。
	if err := h.groupMirror.RemoveMember(ctx, groupID, senderID); err != nil {
		h.logger.Error("rongcloud: group mirror quit", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	if err := h.client.quitGroup(ctx, groupID, []string{senderID}); err != nil {
		h.logger.Warn("rongcloud: group quit", "error", err, "group_id", groupID, "user_id", senderID)
	}

	h.logger.Info("rongcloud: group quit", "group_id", groupID, "user_id", senderID)
	return h.sendWebOK(ctx, msg, cmd, map[string]interface{}{
		"groupId": groupID,
		"userId":  senderID,
		"left":    true,
	})
}

// ---- dismiss -----------------------------------------------------------------

func (h *systemHandler) handleGroupDismiss(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	groupID := strings.TrimSpace(getString(cmd.Params, "groupId"))
	if groupID == "" {
		return h.sendWebError(ctx, msg, cmd, 400, "groupId is required")
	}
	if h.groupMirror.OwnerOf == nil || h.groupMirror.DeleteGroup == nil {
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}

	senderID := strings.TrimSpace(msg.FromUserID)
	ownerID, found, err := h.groupMirror.OwnerOf(ctx, groupID)
	if err != nil {
		h.logger.Error("rongcloud: group dismiss owner lookup", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	if !found {
		return h.sendWebError(ctx, msg, cmd, 404, "group not found")
	}
	if ownerID != senderID {
		return h.sendWebError(ctx, msg, cmd, 403, "only the group owner can dismiss the group")
	}

	// 同 quit：先删本地，融云失败只记日志（旧后端的取舍，本地才是 C 端数据源）。
	if err := h.groupMirror.DeleteGroup(ctx, groupID); err != nil {
		h.logger.Error("rongcloud: group mirror dismiss", "error", err, "group_id", groupID)
		return h.sendWebError(ctx, msg, cmd, 503, "group service is unavailable")
	}
	if err := h.client.dismissGroup(ctx, groupID, senderID); err != nil {
		h.logger.Warn("rongcloud: group dismiss", "error", err, "group_id", groupID)
	}

	h.logger.Info("rongcloud: group dismissed", "group_id", groupID, "owner_id", senderID)
	return h.sendWebOK(ctx, msg, cmd, map[string]interface{}{
		"groupId":   groupID,
		"dismissed": true,
	})
}
