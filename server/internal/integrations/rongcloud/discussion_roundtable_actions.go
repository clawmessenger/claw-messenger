package rongcloud

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 圆桌创建/推荐 action（P5）。挂在 discussionRuntime 上，由 system_handler 的
// handleDiscussionCommand 分发。三个 action 的身份域与 P4 一致：actor.RcID
//（融云侧 TEXT id）就是 create 状态机的 user_id。

// actionRecommendRoles 对齐 Python 的 recommendRoles：校验 host 配置 → 解析候选
// 设备 → 经 broker 发起真实推荐往返 → 给每个角色绑定一台设备 → 落
// rongcloud_role_recommendation（request_id 唯一约束兜底重放）→ 返回 preview。
//
// 角色数上限不是常量：它由候选设备数推出（Python：max_roles = min(8, len(devices))）。
// 固定默认值会与「一个角色一台设备」这条 C 端硬约束打架 —— 运营提示词要求 4-6 个
// 角色时，固定 3 会让设备端因超量而拒绝，前端只能看到一句笼统的
// 「角色推荐结果无效」，与真正的原因（提示词与设备数不匹配）毫无关联。
//
// 前端契约：RoundtableSetupDialog 固定发 round_policy='user_confirmed'。这条路径
// 按 round_policies 判定，未开放时诚实回 503 DISCUSSION_WORKFLOW_UNAVAILABLE，
// 不谎报支持（谎报会让前端建出跑不起来的讨论）。P7 装配好交互轮次引擎后
// roundtablePolicyAvailable(user_confirmed) 为真，此路径自然放行。
func (rt *discussionRuntime) actionRecommendRoles(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	if rt.roundtable == nil {
		return errResult(503, "discussion runtime not available")
	}
	topic := paramTrim(cmd, "topic")
	goal := paramTrim(cmd, "goal")
	if topic == "" || len(topic) > 4000 || len(goal) > 8000 {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	requested, requestedGiven, listOK := paramStringList(cmd, "node_ids", "nodeIds")
	if !listOK {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	maxRolesParam, maxRolesGiven := paramInt(cmd, "maxRoles", "max_roles")
	if maxRolesGiven && (maxRolesParam < 1 || maxRolesParam > RecommendRolesMaxRoles) {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	policy := paramTrim(cmd, "round_policy", "roundPolicy")
	if policy == "" {
		policy = RoundPolicyLegacyCapped
	}
	if policy != RoundPolicyLegacyCapped && policy != RoundPolicyUserConfirmed {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	// 讨论风格（C 端选的 free / brainstorm / debate / review / consensus）。
	// 空值或超长一律当没选：设备端会退回 goal 里的风格短语自行识别，所以这里
	// 不做 400 —— 风格只影响角色推荐的取向，不该让整次推荐失败。
	style := paramTrim(cmd, "style", "discussion_style")
	if len(style) > recommendRolesMaxStyle {
		style = ""
	}

	host, hostErr := rt.roundtable.resolveHost(ctx)
	if hostErr != "" {
		return errResult(503, hostErr)
	}
	// 推荐请求带 round_policy 时：v3 未开放就拒绝，别让流程走到 create 才炸。
	if policy == RoundPolicyUserConfirmed && !rt.roundtablePolicyAvailable(policy) {
		return errResult(503, "DISCUSSION_WORKFLOW_UNAVAILABLE")
	}

	// 候选设备 + 角色数上限。两种调用形态：
	//   · 带 node_ids（C 端）或什么都没带：按候选设备推导上限，并给每个角色分配设备；
	//   · 只给 max_roles（旧契约，无设备概念）：照旧按给定数量走，不做设备分配。
	maxRoles := maxRolesParam
	var candidates []string
	if !maxRolesGiven || requestedGiven {
		nodeIDs, candCode := rt.roundtable.roleCandidateNodesFor(ctx, actor.RcID, host, requested, requestedGiven)
		if candCode != "" {
			slog.Info("rongcloud: recommend_roles 没有可用候选设备",
				"user", actor.RcID, "requested", requested, "requested_given", requestedGiven)
			return errResult(400, candCode)
		}
		candidates = nodeIDs
		// 与 web 首推/换一批（discussion_roundtable_setup.go）同一个上限，避免
		// 同一产品口径下 web 推 5 个、小程序（传 node_ids 走本命令）推 8 个。
		maxRoles = min(recommendRoleCountCap, len(candidates))
	}

	// broker 往返（真实调设备端 agent）。等待上限由 host.TimeoutSeconds 决定。
	if rt.nodeRequests == nil {
		// 与「主持节点离线」共用同一个对外错误码，但内因是装配缺失（融云通道没
		// 把 broker 挂上来）：必须留日志，否则和真离线无法区分。
		slog.Warn("rongcloud: recommend_roles 无法下发 —— node request broker 未装配")
		return errResult(503, "SYSTEM_HOST_OFFLINE")
	}
	timeout := host.timeoutSeconds
	if timeout <= 0 {
		// 与 setup 链路同一口径（见 recommendRolesFallbackTimeoutSeconds）。
		timeout = recommendRolesFallbackTimeoutSeconds
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
		// 推荐这一轮用主持配的模型，而不是节点默认：短任务上旗舰模型与快模型
		// 差一个数量级，设备端每轮还要付一次冷启动，两个都省下来才看得见。
		Model: host.model,
	}, topic, goal, maxRoles, style)
	if code != "" {
		return errResult(503, code)
	}

	// 角色 → 设备。设备端若已自己认领（新版 CLI 读得到 candidates）就尊重它的选择，
	// 否则服务端确定性补齐。
	assignRoleNodes(payloadRoles(payload), candidates)

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
	rt.beginInteractiveSafe(ctx, result)
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
	// 重放原始 payload（canonical 结构已存 request_payload）。
	var params map[string]interface{}
	if err := json.Unmarshal(req.RequestPayload, &params); err != nil {
		return errResult(500, "CREATE_ROUNDTABLE_RECOVERABLE")
	}
	result, rErr := rt.roundtable.create(ctx, actor.RcID, params)
	if rErr != nil {
		return roundtableErrorResult(rErr)
	}
	rt.beginInteractiveSafe(ctx, result)
	return okResult(result)
}

// beginInteractiveSafe 在创建编排到达 ready 后启动 v3 轮次引擎（仅
// user_confirmed 讨论）。best-effort：引擎内部幂等（'start:<id>' 事件），创建
// 重放或由 resume 重入时不会重复开局；开局失败不回滚已建成的一室一组一讨论 ——
// 用户可以稍后 resume 重试，而把已提交的创建结果报成失败只会让前端反复重试。
func (rt *discussionRuntime) beginInteractiveSafe(ctx context.Context, result map[string]interface{}) {
	if !rt.interactiveReady() {
		return
	}
	discussionID, _ := result["discussion_id"].(string)
	if discussionID == "" {
		return
	}
	if err := rt.interactiveBegin(ctx, discussionID); err != nil {
		rt.logger.Warn("discussion: interactive begin failed", "discussionId", discussionID, "error", err)
	}
}

// ---- 辅助 ----------------------------------------------------------------

// payloadRoles 取出 broker 回包里的角色列表。返回的是回包里的同一批 map，就地修改
// 会一起落到 rongcloud_role_recommendation.roles。
func payloadRoles(payload map[string]interface{}) []map[string]interface{} {
	roles, _ := payload["roles"].([]map[string]interface{})
	return roles
}

// assignRoleNodes 给每个角色绑定一台候选设备，就地写回 role["node_id"]。
//
// C 端的校验是「每个角色的 node_id 互不相同，且都在用户所选设备内」，缺一个字段
// 就整份判无效（ROLE_RECOMMENDATION_INVALID）。Python 里这一步由设备端模型完成
// （严格模式下设备拿得到 candidates，逐角色回 node_id/model）；Go 侧还没有把这套
// 候选清单下发到设备，于是服务端按候选顺序确定性补齐 —— 设备端将来若自己回了合法
// 且未重复的分配，这里会尊重它，只补它没选到的角色。
func assignRoleNodes(roles []map[string]interface{}, candidates []string) {
	if len(roles) == 0 || len(candidates) == 0 {
		return
	}
	allowed := make(map[string]bool, len(candidates))
	for _, nodeID := range candidates {
		allowed[nodeID] = true
	}
	used := make(map[string]bool, len(candidates))
	pending := make([]int, 0, len(roles))
	for i, role := range roles {
		if nodeID, _ := role["node_id"].(string); nodeID != "" {
			if allowed[nodeID] && !used[nodeID] {
				used[nodeID] = true
				continue
			}
			// 设备端给的分配不在候选内或与前面的角色撞车：丢弃后重新分配，
			// 并把原因写进日志 —— 否则「推荐结果无效」又会变成一句没有线索的报错。
			slog.Warn("rongcloud: 忽略设备端给的角色分配",
				"node_id", nodeID, "role", role["role_name"],
				"in_candidates", allowed[nodeID])
			delete(role, "node_id")
		}
		pending = append(pending, i)
	}
	next := 0
	for _, i := range pending {
		for next < len(candidates) && used[candidates[next]] {
			next++
		}
		if next >= len(candidates) {
			// 角色数 > 候选设备数。正常路径不可达（max_roles 由候选数推出，设备端
			// 也会按 max_roles 自我校验），真发生时退化成轮转复用，让上层按
			// 「同一设备绑定多个角色」显式报错，而不是在这里凭空造一个错误码。
			slog.Warn("rongcloud: 角色数超过候选设备数，复用设备",
				"roles", len(roles), "candidates", len(candidates))
			used = make(map[string]bool, len(candidates))
			next = 0
		}
		roles[i]["node_id"] = candidates[next]
		used[candidates[next]] = true
		next++
	}
}

// roundtablePolicyAvailable 判定 round_policy 是否已开放。legacy_capped 由 P5
// 提供；user_confirmed（v3 交互轮次）自 P7 起随引擎装配一起开放 —— 引擎未装配
// （interactiveReady=false）时仍诚实返回 false，避免前端走一条死路。
func (rt *discussionRuntime) roundtablePolicyAvailable(policy string) bool {
	switch policy {
	case RoundPolicyLegacyCapped:
		return true
	case RoundPolicyUserConfirmed:
		return rt.interactiveReady()
	default:
		return false
	}
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
		"ROLE_NAME_DUPLICATED", "MODEL_NOT_AVAILABLE":
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

// actionRestartRoundtable 在原讨论组直接重开新讨论：不换房、不重新跑 LLM 角色推荐，
// 以聊天室成员表为权威重建角色板。前端语义：讨论终态（completed/cancelled/failed）
// 或死讨论（legacy_capped 停在 planning，红线：Go 侧无执行引擎）后，用户直接发言
// 即以发言内容为新 topic 重开。
func (rt *discussionRuntime) actionRestartRoundtable(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	if rt.roundtable == nil {
		return errResult(503, "discussion runtime not available")
	}
	chatroomID := paramTrim(cmd, "chatroom_id", "chatroomId")
	topic := strings.TrimSpace(getString(cmd.Params, "topic"))
	if chatroomID == "" || topic == "" {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	s := rt.roundtable
	room, err := s.queries.GetRongCloudChatroomViewByRongCloudID(ctx, chatroomID)
	if err != nil {
		return errResult(404, "ROUNDTABLE_CREATE_REQUEST_NOT_FOUND")
	}
	// 只有房主/讨论创建者可重开。
	if !room.OwnerRcUserID.Valid || room.OwnerRcUserID.String != actor.RcID {
		return errResult(403, "ROUNDTABLE_CREATE_REQUEST_NOT_FOUND")
	}
	// 并发保护：讨论真在跑（planning/running/summarizing/synthesizing）不许覆盖。
	// 两类"活跃"行放行并先收尾，正是重开的目标场景：
	//  - awaiting_user：轮次已完、等房主决定，用户直接发言=放弃旧讨论重开新局；
	//  - legacy_capped：Go 侧无执行引擎的死讨论（建出即停 planning）。
	if active, err := s.queries.GetActiveRongCloudDiscussionByChatroom(ctx, chatroomID); err == nil {
		if active.RoundPolicy == RoundPolicyUserConfirmed && active.Status != DiscussionStatusAwaitingUser {
			return errResult(409, "DISCUSSION_ALREADY_ACTIVE")
		}
		if cerr := rt.closeDiscussionForRestart(ctx, active); cerr != nil {
			return errResult(500, "CREATE_ROUNDTABLE_RECOVERABLE")
		}
	}

	// 以成员表为权威重建角色板（用户可能编辑过角色，比旧 snapshot 更可信）。
	rows, err := s.queries.ListRongCloudChatroomMembersWithNode(ctx, room.ID)
	if err != nil {
		return errResult(500, "CREATE_ROUNDTABLE_RECOVERABLE")
	}
	host, hostErr := s.resolveHost(ctx)
	if hostErr != "" {
		return errResult(503, hostErr)
	}
	roles := map[string]interface{}{}
	order := 0
	for _, row := range rows {
		if row.BusinessNodeID.Valid && row.BusinessNodeID.String == host.nodeID {
			continue // 主持节点不是角色
		}
		entry, ok := roundtableRoleEntryFromMember(row, order)
		if !ok {
			continue
		}
		roles[row.NodeRcUserID.String] = entry
		order++
	}
	if len(roles) == 0 {
		return errResult(400, "ROLE_RECOMMENDATION_INVALID")
	}
	// host 条目（含 model）与 host_config_snapshot 由同一个函数唯一地拼装，
	// 三个写入点共用 —— 见 roundtableHostSnapshots 的注释。
	roleJSON, hostSnapshot := roundtableHostSnapshots(host, roles)
	// 新讨论 ID：不能复用确定性派生（同房间会撞旧讨论的 ID），用随机 UUID。
	discussionID := uuid.NewString()
	_, err = s.queries.CreateRongCloudDiscussion(ctx, db.CreateRongCloudDiscussionParams{
		DiscussionID:       discussionID,
		ChatroomID:         chatroomID,
		WorkspaceID:        room.WorkspaceID,
		CreatedBy:          actor.RcID,
		Topic:              topic,
		Mode:               "roundtable",
		Status:             "planning",
		RoleSnapshot:       roleJSON,
		HostConfigSnapshot: hostSnapshot,
		RoundPolicy:        RoundPolicyUserConfirmed,
	})
	if err != nil {
		return errResult(500, "CREATE_ROUNDTABLE_RECOVERABLE")
	}
	rt.beginInteractiveSafe(ctx, map[string]interface{}{"discussion_id": discussionID})
	return okResult(map[string]interface{}{
		"chatroom_id":   chatroomID,
		"discussion_id": discussionID,
		"status":        "planning",
	})
}

// closeDiscussionForRestart 把等待重开的旧讨论收尾为 completed（幂等：已终态直接过）。
// awaiting_user 是用户主动放弃旧局，语义上等同于「就此结束并保留纪要」。
func (rt *discussionRuntime) closeDiscussionForRestart(ctx context.Context, active db.RongcloudDiscussion) *RoundtableError {
	if !IsLiveDiscussionStatus(active.Status) {
		return nil
	}
	d, err := rt.repo.Get(ctx, active.DiscussionID)
	if err != nil {
		return roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	// UpdateRongCloudDiscussionState 无条件写 current_round/round_revision，
	// 必须带上原值，否则终态行的轮次会被清零。
	advanced, err := rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: d.DiscussionID, ExpectedVersion: d.Version,
		Status:        DiscussionStatusCompleted,
		CurrentRound:  d.CurrentRound,
		RoundRevision: d.RoundRevision,
	})
	if err != nil {
		return roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	done, _, err := rt.interactiveEmit(ctx, advanced, interactiveEvent{
		EventType: "discussion_completed", ActorID: active.CreatedBy,
		Payload: map[string]interface{}{"reason": "restarted"},
		Key:     "restart-close:" + active.DiscussionID, StateVer: advanced.Version,
	})
	if err != nil {
		return roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	rt.interactiveBroadcast(ctx, advanced, done)
	return nil
}
