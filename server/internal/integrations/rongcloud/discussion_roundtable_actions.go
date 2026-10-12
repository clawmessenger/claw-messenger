package rongcloud

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 圆桌创建/推荐 action（P5）。挂在 discussionRuntime 上，由 system_handler 的
// handleDiscussionCommand 分发。三个 action 的身份域与 P4 一致：actor.RcID
//（融云侧 TEXT id）就是 create 状态机的 user_id。

// actionRecommendRoles 对齐 Python 的 recommendRoles：校验 host 配置 → 经
// broker 发起真实推荐往返 → 落 rongcloud_role_recommendation（request_id 唯一
// 约束兜底重放）→ 返回 preview。
//
// 前端契约：RoundtableSetupDialog 固定发 round_policy='user_confirmed'，而 v3
// 运行时（interactive rounds）在 P7 才落地 —— 这条路径按 round_policies 判定，
// 未开放时诚实回 503 DISCUSSION_WORKFLOW_UNAVAILABLE，不谎报支持（谎报会让
// 前端建出跑不起来的讨论）。
func (rt *discussionRuntime) actionRecommendRoles(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	if rt.roundtable == nil {
		return errResult(503, "discussion runtime not available")
	}
	topic := paramTrim(cmd, "topic")
	goal := paramTrim(cmd, "goal")
	if topic == "" || len(topic) > 4000 || len(goal) > 8000 {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	maxRoles := 3
	if v, ok := paramInt(cmd, "maxRoles", "max_roles"); ok {
		maxRoles = v
	}
	if maxRoles < 1 || maxRoles > RecommendRolesMaxRoles {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	policy := paramTrim(cmd, "round_policy", "roundPolicy")
	if policy == "" {
		policy = RoundPolicyLegacyCapped
	}
	if policy != RoundPolicyLegacyCapped && policy != RoundPolicyUserConfirmed {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}

	host, hostErr := rt.roundtable.resolveHost(ctx)
	if hostErr != "" {
		return errResult(503, hostErr)
	}
	// 推荐请求带 round_policy 时：v3 未开放就拒绝，别让流程走到 create 才炸。
	if policy == RoundPolicyUserConfirmed && !rt.roundtablePolicyAvailable(policy) {
		return errResult(503, "DISCUSSION_WORKFLOW_UNAVAILABLE")
	}

	// broker 往返（真实调设备端 agent）。等待上限由 host.TimeoutSeconds 决定。
	if rt.nodeRequests == nil {
		return errResult(503, "SYSTEM_HOST_OFFLINE")
	}
	timeout := host.timeoutSeconds
	if timeout <= 0 {
		timeout = 60
	}
	bctx, cancel := context.WithTimeout(ctx, time.Duration(timeout+5)*time.Second)
	defer cancel()
	payload, code := rt.nodeRequests.RecommendRoles(bctx, NodeRequestHost{
		AIUserID:             host.aiUserID,
		DisplayName:          host.displayName,
		Avatar:               host.avatar,
		RecommendationPrompt: rt.roundtable.recommendationPrompt(ctx),
		TimeoutSeconds:       timeout,
		ConfigVersion:        configVersionInt(host.configVersion),
	}, topic, goal, maxRoles)
	if code != "" {
		return errResult(503, code)
	}

	// 落库：recommendation_id 由 broker 确定性派生；user 域用 actor.RcID
	//（claw 用户 user_id / 节点 rc 账号），与 create 状态机的推荐校验同键。
	recID, _ := payload["recommendation_id"].(string)
	requestID := "c2c_" + recID
	rolesJSON, _ := json.Marshal(payload["roles"])
	expiresAt := time.Now().Add(recommendRolesTTL)
	if _, err := rt.queries.InsertRongCloudRoleRecommendation(ctx, db.InsertRongCloudRoleRecommendationParams{
		RecommendationID: recID,
		RequestID:        requestID,
		UserID:           actor.RcID,
		HostNodeID:       host.nodeID,
		HostAiUserID:     host.aiUserID,
		ConfigVersion:    host.configVersion,
		Topic:            topic,
		Goal:             goal,
		Roles:            rolesJSON,
		ExpiresAt:        pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		// 幂等：request_id 冲突 → 读回既存行（同一推荐重放）。
		if existing, gErr := rt.queries.GetRongCloudRoleRecommendationByRequest(ctx, requestID); gErr == nil {
			payload["recommendation_id"] = existing.RecommendationID
		}
	}
	return okResult(payload)
}

// actionCreateRoundtable 对齐 Python createRoundtable：进入五阶段幂等状态机。
func (rt *discussionRuntime) actionCreateRoundtable(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	if rt.roundtable == nil {
		return errResult(503, "discussion runtime not available")
	}
	result, rErr := rt.roundtable.create(ctx, actor.RcID, cmd.Params)
	if rErr != nil {
		return roundtableErrorResult(rErr)
	}
	return okResult(result)
}

// actionResumeRoundtable 对齐 Python resumeRoundtable：按 chatroomId 找回
// 未完成的 create 请求并重入状态机（不重新 claim —— 幂等键仍在原 user 名下）。
func (rt *discussionRuntime) actionResumeRoundtable(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	if rt.roundtable == nil {
		return errResult(503, "discussion runtime not available")
	}
	chatroomID := paramTrim(cmd, "chatroom_id", "chatroomId")
	if chatroomID == "" {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	req, err := rt.queries.GetRoundtableCreateRequestByChatroom(ctx, pgtype.Text{String: chatroomID, Valid: true})
	if err != nil {
		return errResult(404, "ROUNDTABLE_CREATE_REQUEST_NOT_FOUND")
	}
	if req.UserID != actor.RcID {
		return errResult(403, "ROUNDTABLE_CREATE_REQUEST_NOT_FOUND")
	}
	if req.Status == "ready" {
		return okResult(roundtableReadyResponse(req))
	}
	// 重放原始 payload（canonical 结构已存 request_payload）。
	var params map[string]interface{}
	if err := json.Unmarshal(req.RequestPayload, &params); err != nil {
		return errResult(500, "CREATE_ROUNDTABLE_RECOVERABLE")
	}
	result, rErr := rt.roundtable.create(ctx, actor.RcID, params)
	if rErr != nil {
		return roundtableErrorResult(rErr)
	}
	return okResult(result)
}

// ---- 辅助 ----------------------------------------------------------------

// roundtablePolicyAvailable 判定 round_policy 是否已开放（P5 只有 legacy_capped）。
func (rt *discussionRuntime) roundtablePolicyAvailable(policy string) bool {
	return policy == RoundPolicyLegacyCapped
}

// roundtableErrorResult 把 RoundtableError 翻译成 commandResult。错误码语义：
// 4xx 客户端可修改重试；503 环境问题；CREATE_ROUNDTABLE_RECOVERABLE 可重试。
func roundtableErrorResult(e *RoundtableError) commandResult {
	if e == nil {
		return errResult(500, "CREATE_ROUNDTABLE_RECOVERABLE")
	}
	switch e.Code {
	case "INVALID_ROUNDTABLE_REQUEST", "CREATE_ROUNDTABLE_CONFLICT",
		"ROLE_DEVICE_DUPLICATED", "ROLE_DEVICE_NOT_OWNED", "ROLE_DEVICE_UNAVAILABLE",
		"MODEL_NOT_AVAILABLE":
		return commandResult{code: 400, message: e.Code, data: e.Data}
	case "RECOMMENDATION_EXPIRED":
		return commandResult{code: 410, message: e.Code, data: e.Data}
	case "SYSTEM_HOST_NOT_CONFIGURED", "SYSTEM_HOST_DISABLED", "SYSTEM_HOST_OFFLINE",
		"DISCUSSION_WORKFLOW_UNAVAILABLE":
		return commandResult{code: 503, message: e.Code, data: e.Data}
	default:
		return commandResult{code: 503, message: e.Code, data: e.Data}
	}
}

// recommendationPrompt 读运营配置的角色推荐提示词（hostLoader 的同一配置源，
// 前端「角色推荐提示词」写进 system_host.role_recommendation_prompt）。
// DiscussionSystemHostConfig 目前不带该字段，读 loader 拿不到时为空串。
func (s *roundtableCreationService) recommendationPrompt(ctx context.Context) string {
	if s.hostLoader == nil {
		return ""
	}
	cfg, found, err := s.hostLoader(ctx)
	if err != nil || !found {
		return ""
	}
	return cfg.RecommendationPrompt
}

// configVersionInt 把 string 版本号转 int（broker 的 NodeRequestHost 要 int）。
func configVersionInt(v string) int {
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
