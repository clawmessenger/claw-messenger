package rongcloud

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 圆桌创建编排的 ensure 三阶段（P5）。与 discussion_roundtable.go 的状态机
// 主体配套：每个阶段幂等（重放只补缺，不重复创建）。

// roundtableHostRoleName / roundtableHostInstructions 对齐 Python 的
// ROUNDTABLE_HOST_ROLE / ROUNDTABLE_HOST_INSTRUCTIONS。
const (
	roundtableHostRoleName     = "中立主持人"
	roundtableHostInstructions = "保持中立，控制讨论节奏；每轮汇总共识、分歧与开放问题，不替普通成员表达立场。"
)

// roundtableNewChatroomID 对齐 _new_chatroom_id：确定性 uuid5。
// 同一 (user, client_request_id) 重放生成同一个 chatroom id。
func roundtableNewChatroomID(userID, clientRequestID string) string {
	return "rt_" + uuid.NewSHA1(uuid.NameSpaceURL, []byte("roundtable:"+userID+":"+clientRequestID)).String()
}

// roundtableNewDiscussionID 对齐 _new_discussion_id。
func roundtableNewDiscussionID(chatroomID string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("discussion:"+chatroomID)).String()
}

// roundtableReadyResponse 对齐 _ready_response。
func roundtableReadyResponse(request db.RongcloudRoundtableCreateRequest) map[string]interface{} {
	return map[string]interface{}{
		"client_request_id": request.ClientRequestID,
		"recommendation_id": textStr(request.RecommendationID),
		"chatroom_id":       textStr(request.ChatroomID),
		"discussion_id":     textStr(request.DiscussionID),
		"status":            "ready",
	}
}

// ownedAndEligibleNodes 对齐 _owned_and_eligible_nodes。归属按融云好友边。
func (s *roundtableCreationService) ownedAndEligibleNodes(ctx context.Context, userID string, host roundtableHostConfig) (map[string]bool, map[string]db.RongcloudNode) {
	owned := map[string]bool{}
	eligible := map[string]db.RongcloudNode{}
	if s.nodeOwner == nil {
		return owned, eligible
	}
	nodes, err := s.nodeOwner(ctx, userID)
	if err != nil {
		return owned, eligible
	}
	for _, node := range nodes {
		if node.NodeID == "" {
			continue
		}
		owned[node.NodeID] = true
		// 在线判定走融云 presence（见 node_presence.go），不能读 deploy_status：
		// 那列在 Go 侧无人写入，恒为 offline，会让可选设备永远为空。
		if nodeIsOnline(ctx, s.client, node) &&
			len(missingCapabilities(node.Capabilities, roundtableRequiredRoleCaps)) == 0 &&
			node.NodeID != host.nodeID {
			eligible[node.NodeID] = node
		}
	}
	return owned, eligible
}

// roleCandidateNodes 解析本轮角色推荐的候选设备，对齐 Python
// _handle_discussion_recommendRoles 的两句话：
//
//	devices = eligible(requester) ∩ requested_node_ids
//	max_roles = min(8, len(devices))
//
// 候选集同时也是角色分配的设备池：C 端（RoundtableSetupDialog / uniapp flow.js）
// 在拿到推荐后硬校验「角色数 ≤ min(8, 所选设备数)」「每个角色的 node_id 互不相同
// 且都在所选设备内」，所以角色数上限必须由候选数推出，而不是某个固定默认值 ——
// 固定 3 会让「运营提示词要求 4-6 个角色」的部署必然产出超量角色而整份被否。
//
// 与 Python 的一处有意差别：Python 的候选集不过滤在线（只过滤 enabled /
// system_reserved / 能力位），在线性只在下发模型目录时用到。Go 侧没有服务端模型
// 目录，于是直接复用创建阶段那套 eligible（在线 + 能力齐备 + 非 host），好处是
// 推荐出来的角色一定能通过 create 的校验，不会「推荐成功、创建必失败」。
func roleCandidateNodes(eligible map[string]db.RongcloudNode, requested []string, requestedGiven bool) ([]string, string) {
	if requestedGiven {
		out := make([]string, 0, len(requested))
		seen := make(map[string]bool, len(requested))
		for _, nodeID := range requested {
			if seen[nodeID] {
				continue
			}
			seen[nodeID] = true
			if _, ok := eligible[nodeID]; !ok {
				continue
			}
			out = append(out, nodeID)
			if len(out) >= RecommendRolesMaxRoles {
				break
			}
		}
		if len(out) == 0 {
			return nil, "NO_ELIGIBLE_USER_DEVICES"
		}
		return out, ""
	}
	ids := make([]string, 0, len(eligible))
	for nodeID := range eligible {
		ids = append(ids, nodeID)
	}
	if len(ids) == 0 {
		return nil, "NO_ELIGIBLE_USER_DEVICES"
	}
	// 没给 node_ids 时集合来自 map，必须排序：同一份配置两次调用要给出同一份推荐。
	sort.Strings(ids)
	if len(ids) > RecommendRolesMaxRoles {
		ids = ids[:RecommendRolesMaxRoles]
	}
	return ids, ""
}

// roleCandidateNodesFor 取候选设备的运行时封装（查归属 + 在线判定）。
func (s *roundtableCreationService) roleCandidateNodesFor(ctx context.Context, userID string, host roundtableHostConfig, requested []string, requestedGiven bool) ([]string, string) {
	_, eligible := s.ownedAndEligibleNodes(ctx, userID, host)
	return roleCandidateNodes(eligible, requested, requestedGiven)
}

// ensureChatroom 对齐 _ensure_chatroom：融云侧建房 + DB 行 + owner/host 成员行。
func (s *roundtableCreationService) ensureChatroom(ctx context.Context, request db.RongcloudRoundtableCreateRequest, chatroomID string, canonical roundtableCanonical, validated roundtableValidated, userID string) *RoundtableError {
	if s.client != nil {
		if err := s.client.createChatroom(ctx, chatroomID); err != nil {
			return roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
		}
	}
	ownerMirror, err := s.queries.GetRongCloudUserByRongCloudID(ctx, userID)
	if err != nil {
		return roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	hostNode, err := s.queries.GetRongCloudNodeByNodeID(ctx, validated.host.nodeID)
	if err != nil {
		return roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	maxRounds := int32(0)
	if canonical.MaxRounds != nil {
		maxRounds = int32(*canonical.MaxRounds)
	}
	room, err := s.queries.CreateRongCloudChatroom(ctx, db.CreateRongCloudChatroomParams{
		WorkspaceID:         ownerMirror.WorkspaceID,
		RongcloudChatroomID: chatroomID,
		OwnerUserID:         ownerMirror.ID,
		HostNodeID:          hostNode.ID,
		MaxRounds:           maxRounds,
		ConversationKind:    pgtype.Text{String: "discussion", Valid: true},
		Config:              []byte("{}"),
		Status:              "active",
		Name:                canonical.ChatroomName,
		// 融云聊天室 API 没有头像字段，讨论组头像纯本地数据；不写就会恒为空串，
		// 各端只能各显各的兜底图。
		PortraitUri: DefaultUserPortrait(),
	})
	if err != nil {
		// 重复到达（chatroom 行已存在）→ 幂等继续；融云侧房间无需重建。
		if existing, gErr := s.queries.GetRongCloudChatroomViewByRongCloudID(ctx, chatroomID); gErr == nil && existing.ID.Valid {
			room = db.RongcloudChatroom{ID: existing.ID, WorkspaceID: existing.WorkspaceID}
		} else {
			if s.client != nil {
				_ = s.client.destroyChatroom(ctx, chatroomID)
			}
			return roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
		}
	}
	nickname := ownerMirror.Name.String
	if !ownerMirror.Name.Valid || nickname == "" {
		nickname = userID
	}
	_, _ = s.queries.UpsertRongCloudChatroomMemberByMemberID(ctx, db.UpsertRongCloudChatroomMemberByMemberIDParams{
		ChatroomID:   room.ID,
		MemberType:   "user",
		Capabilities: []byte("[]"),
		Enabled:      true,
		MemberID:     pgtype.Text{String: userID, Valid: true},
		Nickname:     nickname,
		// 所有者头像取镜像行的实际值，空则统一默认图（成员列表直接读这一列）。
		PortraitUri: DefaultPortraitIfEmpty(ownerMirror.PortraitUri.String),
	})
	hostCapsJSON, _ := json.Marshal(roundtableRequiredHostCaps)
	_, _ = s.queries.UpsertRongCloudChatroomMemberByMemberID(ctx, db.UpsertRongCloudChatroomMemberByMemberIDParams{
		ChatroomID:       room.ID,
		MemberType:       "ai",
		NodeID:           hostNode.ID,
		Capabilities:     hostCapsJSON,
		Enabled:          true,
		MemberID:         pgtype.Text{String: validated.host.aiUserID, Valid: true},
		Nickname:         validated.host.displayName,
		RoleName:         pgtype.Text{String: roundtableHostRoleName, Valid: true},
		RoleInstructions: pgtype.Text{String: roundtableHostInstructions, Valid: true},
		PortraitUri:      DefaultUserPortrait(),
	})
	return nil
}

// ensureMembers 对齐 _ensure_members：把推荐的角色落成 AI 成员行。
func (s *roundtableCreationService) ensureMembers(ctx context.Context, chatroomID string, canonical roundtableCanonical, validated roundtableValidated) *RoundtableError {
	room, err := s.queries.GetRongCloudChatroomViewByRongCloudID(ctx, chatroomID)
	if err != nil {
		return roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	memberCapsJSON, _ := json.Marshal(roundtableRequiredRoleCaps)
	for _, role := range canonical.Roles {
		node, ok := validated.nodes[role.NodeID]
		if !ok {
			return roundtableRoleErr("ROLE_DEVICE_UNAVAILABLE", role.SpeakingOrder)
		}
		var model pgtype.Text
		if role.Model != nil && *role.Model != "" {
			model = pgtype.Text{String: *role.Model, Valid: true}
		}
		_, err := s.queries.UpsertRongCloudChatroomMemberByMemberID(ctx, db.UpsertRongCloudChatroomMemberByMemberIDParams{
			ChatroomID:       room.ID,
			MemberType:       "ai",
			MemberID:         pgtype.Text{String: node.RongcloudUserID, Valid: true},
			NodeID:           node.ID,
			Nickname:         role.RoleName,
			RoleName:         pgtype.Text{String: role.RoleName, Valid: true},
			RoleInstructions: pgtype.Text{String: role.RolePrompt, Valid: true},
			Capabilities:     memberCapsJSON,
			Enabled:          true,
			DiscussionModel:  model,
			SpeakingOrder:    pgtype.Int4{Int32: int32(role.SpeakingOrder), Valid: true},
			PortraitUri:      DefaultUserPortrait(),
		})
		if err != nil {
			return roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
		}
	}
	return nil
}

// ensureDiscussion 对齐 _ensure_started：落讨论实例（幂等 by discussion_id）。
// initialStatus 允许引导式链路落 configuring（draft 语义）；旧五阶段链路恒传
// planning，行为与改造前逐字一致。
func (s *roundtableCreationService) ensureDiscussion(ctx context.Context, request db.RongcloudRoundtableCreateRequest, chatroomID, discussionID string, canonical roundtableCanonical, validated roundtableValidated, userID string, initialStatus string) (string, *RoundtableError) {
	if initialStatus != DiscussionStatusConfiguring {
		initialStatus = DiscussionStatusPlanning
	}
	// 引导式链路的断点从 topic 起步；旧链路恒为空（列默认语义）。
	setupStep := ""
	if initialStatus == DiscussionStatusConfiguring {
		setupStep = setupStepTopic
	}
	room, err := s.queries.GetRongCloudChatroomViewByRongCloudID(ctx, chatroomID)
	if err != nil {
		return "", roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	if existing, err := s.queries.GetRongCloudDiscussion(ctx, discussionID); err == nil {
		if existing.ChatroomID != chatroomID || existing.CreatedBy != userID {
			return "", roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
		}
		return discussionID, nil
	}
	roles := map[string]interface{}{}
	for _, role := range canonical.Roles {
		node := validated.nodes[role.NodeID]
		roles[node.RongcloudUserID] = map[string]interface{}{
			"memberId":         node.RongcloudUserID,
			"nodeId":           role.NodeID,
			"nickname":         role.RoleName,
			"portraitUri":      "",
			"roleName":         role.RoleName,
			"roleInstructions": role.RolePrompt,
			"capabilities":     roundtableRequiredRoleCaps,
			"isHost":           false,
			// speakingOrder 是 v3 轮次引擎（P7）的派发顺序来源：interactiveRoles
			// 按它排序依次点名。缺了它所有角色会归到 0 号位，发言顺序退化成
			// map 遍历顺序（不确定）。legacy_capped 不读它，但两模式共用同一份
			// 快照，所以这里无条件写入。
			"speakingOrder": role.SpeakingOrder,
		}
	}
	// host 条目（含 model）与 host_config_snapshot 由同一个函数唯一地拼装，
	// 三个写入点共用 —— 见 roundtableHostSnapshots 的注释。
	roleJSON, hostSnapshot := roundtableHostSnapshots(validated.host, roles)
	var maxRounds pgtype.Int4
	if canonical.MaxRounds != nil {
		maxRounds = pgtype.Int4{Int32: int32(*canonical.MaxRounds), Valid: true}
	}
	_, err = s.queries.CreateRongCloudDiscussion(ctx, db.CreateRongCloudDiscussionParams{
		DiscussionID:       discussionID,
		ChatroomID:         chatroomID,
		WorkspaceID:        room.WorkspaceID,
		CreatedBy:          userID,
		Topic:              canonical.Topic,
		Goal:               canonical.Goal,
		MaxRounds:          maxRounds,
		Mode:               "roundtable",
		Status:             initialStatus,
		RoleSnapshot:       roleJSON,
		HostConfigSnapshot: hostSnapshot,
		RecommendationID:   textOrNull(canonical.RecommendationID),
		ClientRequestID:    textOrNull(canonical.ClientRequestID),
		RoundPolicy:        canonical.RoundPolicy,
		SetupStep:          setupStep,
	})
	if err != nil {
		// 并发/重放：行已存在 → 幂等接受。
		if _, gErr := s.queries.GetRongCloudDiscussion(ctx, discussionID); gErr == nil {
			return discussionID, nil
		}
		return "", roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	return discussionID, nil
}

// recordError 对齐 record_error：错误落表 + 返回公共错误码。
// P5 阶段做最小实现（错误码直接透传）；完整恢复由 P6 的 outbox worker 接管。
func (s *roundtableCreationService) recordError(ctx context.Context, userID, clientRequestID string, request db.RongcloudRoundtableCreateRequest, cause *RoundtableError, chatroomID, discussionID string) *RoundtableError {
	return cause
}
