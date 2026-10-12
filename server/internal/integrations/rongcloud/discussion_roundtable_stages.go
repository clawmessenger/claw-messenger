package rongcloud

import (
	"context"
	"encoding/json"

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
		if node.DeployStatus == "online" &&
			len(missingCapabilities(node.Capabilities, roundtableRequiredRoleCaps)) == 0 &&
			node.NodeID != host.nodeID {
			eligible[node.NodeID] = node
		}
	}
	return owned, eligible
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
		})
		if err != nil {
			return roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
		}
	}
	return nil
}

// ensureDiscussion 对齐 _ensure_started：落讨论实例（幂等 by discussion_id）。
func (s *roundtableCreationService) ensureDiscussion(ctx context.Context, request db.RongcloudRoundtableCreateRequest, chatroomID, discussionID string, canonical roundtableCanonical, validated roundtableValidated, userID string) (string, *RoundtableError) {
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
		}
	}
	roles[validated.host.aiUserID] = map[string]interface{}{
		"memberId":         validated.host.aiUserID,
		"nodeId":           validated.host.nodeID,
		"nickname":         validated.host.displayName,
		"portraitUri":      validated.host.avatar,
		"roleName":         roundtableHostRoleName,
		"roleInstructions": "",
		"capabilities":     roundtableRequiredHostCaps,
		"isHost":           true,
	}
	roleJSON, _ := json.Marshal(roles)
	hostSnapshot, _ := json.Marshal(map[string]interface{}{
		"node_id":                validated.host.nodeID,
		"ai_user_id":             validated.host.aiUserID,
		"display_name":           validated.host.displayName,
		"avatar":                 validated.host.avatar,
		"discussion_host_prompt": validated.host.hostPrompt,
		"config_version":         validated.host.configVersion,
	})
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
		Status:             "planning",
		RoleSnapshot:       roleJSON,
		HostConfigSnapshot: hostSnapshot,
		RecommendationID:   textOrNull(canonical.RecommendationID),
		ClientRequestID:    textOrNull(canonical.ClientRequestID),
		RoundPolicy:        canonical.RoundPolicy,
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
