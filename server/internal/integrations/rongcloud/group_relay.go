package rongcloud

import (
	"context"
	"strings"
)

// 普通群组（融云 group，conversationType=3）的「按序发言」中继。
//
// 背景：群里的 AI 节点都是群成员，用户发一条群消息每个节点都会收到。
// 设备端 CLI 旧版对任何 RC:TxtMsg 都直接跑 agent 并私聊回复（不看
// conversationType），于是 N 个节点同时抢答。CLI 修复后群广播只观察不抢答，
// 由 web 在发出群消息后调用 discussion/relayGroupMessage，服务端按群成员
// 的加入顺序依次给每个 AI 节点私发 your_turn，回包以该节点自己的融云账号
// 经 /message/group/publish.json 广播回群（正常群聊气泡），再派下一位。
//
// 与讨论组中继（chatroom_relay.go）共享同一个 chatroomRelayHub 状态机：
// session.isGroup=true 切换广播端点与提示词。群没有角色板，顺序即成员行
// created_at 顺序，AI 身份由 rongcloud_node 表反查（ai_type 非空；机器主
// 节点 ai_type 为空天然排除 —— 见红线 #6）。

// StartGroupRelay 是 web 走 IM 命令（discussion/relayGroupMessage）进来的
// 入口：校验发起者是群成员，取 AI 节点列表，然后按加入顺序依次派发。
// allMembers 用于发起者校验（真人/节点都算群成员）；aiMembers 是经节点表
// ai_type 过滤后的发言列表（机器主节点天然排除，现行 <agent>_ 与旧 rc_node_
// 两代 id 都不受前缀影响 —— 前缀判定在这里不可靠）。
func (h *chatroomRelayHub) StartGroupRelay(ctx context.Context, groupID, requesterRCID, text string, allMembers, aiMembers []GroupMember) commandResult {
	if h.rt == nil || !h.rt.interactiveReady() {
		return errRelayResult(503, "讨论服务尚未初始化")
	}
	if strings.TrimSpace(text) == "" {
		return errRelayResult(400, "消息内容不能为空")
	}
	if len([]rune(text)) > relayMaxTextLength {
		return errRelayResult(400, "消息内容过长")
	}
	if !groupHasMember(allMembers, requesterRCID) {
		return errRelayResult(403, "仅群成员可以发起对话")
	}
	speakers := relayMembersFromGroup(aiMembers)
	// 权限闸门（迁移 607）：allow_group=false 的 agent 不参与群聊接力。
	// 一次批量查询就够（按 rc id），不按成员逐个查 —— 群成员数 × 每条消息都查
	// 一次会把「有人说话」变成一串 N+1。
	//
	// 全被过滤时走下面现成的 409：群还在、成员也在，只是没人有权限说话。
	//
	// actor = requesterRCID（发这条群消息的人）：群聊闸门同样有主人豁免 —— 主人
	// 在自己发的消息里不该被自己关掉的开关挡住，与设备端私聊的 owner 口径一致。
	perms := h.rt.agentPermissions(ctx, relayMemberIDs(speakers))
	speakers = applyBehaviorPrompts(filterMembersByPermission(speakers, perms, sceneGroup, requesterRCID), perms)
	if len(speakers) == 0 {
		return errRelayResult(409, "该群没有可发言的 AI 成员")
	}
	bgCtx := context.WithoutCancel(ctx)
	started, queued := h.StartGroupOrQueue(bgCtx, groupID, speakers, text, requesterRCID, h.groupRelayRounds(ctx, groupID))
	if !started && !queued {
		return errRelayResult(409, "当前无法发起对话，请稍后重试")
	}
	return commandResult{code: 200, data: map[string]interface{}{
		"ok":       true,
		"groupId":  groupID,
		"speakers": len(speakers),
		// queued=true：上一条消息的中继还在跑，这条已排队，跑完自动接上
		// （web 不用特别处理，但要能区分「已开始」和「已排队」）。
		"queued": queued,
	}}
}

// groupRelayRounds 读群设置里的「AI 对话轮数」（rongcloud_group.
// openclaw_max_rounds）：用户发一条消息后，AI 成员之间最多接力讨论几轮。
// 群镜像没配或查不到时回落默认值 —— 这个设置项取不到不该让整次发言失败。
func (h *chatroomRelayHub) groupRelayRounds(ctx context.Context, groupID string) int {
	if h.groupMirror == nil || h.groupMirror.GroupProfiles == nil {
		return defaultGroupRelayRounds
	}
	profiles, err := h.groupMirror.GroupProfiles(ctx, []string{groupID})
	if err != nil {
		h.rt.logger.Warn("relay: group rounds lookup",
			"group", groupID, "error", err)
		return defaultGroupRelayRounds
	}
	for _, profile := range profiles {
		if profile.GroupID == groupID && profile.MaxRounds > 0 {
			return profile.MaxRounds
		}
	}
	return defaultGroupRelayRounds
}

// StartGroup 发起一次群中继（复用 hub 的 sessions 表，键即群 id）。
// rounds 是该群的「AI 对话轮数」设置：一次中继要跑几轮全员发言。
func (h *chatroomRelayHub) StartGroup(ctx context.Context, groupID string, members []relayMember, userText, requesterRCID string, rounds int) bool {
	started, _ := h.StartGroupOrQueue(ctx, groupID, members, userText, requesterRCID, rounds)
	return started
}

// StartGroupOrQueue 与 StartGroup 相同，差别只在群已有中继在跑时不拒绝，
// 而是把用户消息排进当前会话的待办（pump 收尾后自动接着跑）。见
// chatroomRelayHub.StartOrQueue 的说明。
func (h *chatroomRelayHub) StartGroupOrQueue(ctx context.Context, groupID string, members []relayMember, userText, requesterRCID string, rounds int) (bool, bool) {
	if len(members) == 0 {
		return false, false
	}
	total := normalizeGroupRelayRounds(rounds)
	h.mu.Lock()
	if existing, busy := h.sessions[groupID]; busy {
		enqueueLocked(existing, userText, requesterRCID, total, normalizeContextRounds(rounds))
		h.mu.Unlock()
		return false, true
	}
	session := &relaySession{
		chatroomRCID:  groupID,
		members:       members,
		userText:      userText,
		isGroup:       true,
		requesterRCID: requesterRCID,
		contextRounds: normalizeContextRounds(rounds),
		totalRounds:   total,
	}
	h.attachContextLocked(session)
	h.sessions[groupID] = session
	h.mu.Unlock()

	go h.pump(ctx, session)
	return true, false
}

// groupHasMember 报告 requesterRCID 是否在群成员行里。
func groupHasMember(members []GroupMember, requesterRCID string) bool {
	for _, m := range members {
		if m.UserID == requesterRCID {
			return true
		}
	}
	return false
}

// relayMembersFromGroup 把（已按节点表 ai_type 过滤的）群 AI 成员行折叠成
// 按序发言列表。顺序即成员行 created_at 顺序；不做 id 前缀判定 —— 现行
// agent 节点是 <agent>_<machineKey>，机器主节点是 rc_node_<machineKey>，
// 前缀无法区分，AI 身份只能信 rongcloud_node.ai_type。
func relayMembersFromGroup(members []GroupMember) []relayMember {
	out := make([]relayMember, 0, len(members))
	order := 0
	for _, m := range members {
		out = append(out, relayMember{
			rcID:     m.UserID,
			roleName: groupSpeakerName(m),
			order:    order,
		})
		order++
	}
	return out
}

// groupSpeakerName 展示名：成员行昵称 > user id。
func groupSpeakerName(m GroupMember) string {
	if m.Nickname != "" {
		return m.Nickname
	}
	return m.UserID
}
