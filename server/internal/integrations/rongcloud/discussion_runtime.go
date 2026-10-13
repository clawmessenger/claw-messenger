package rongcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// cancel_assignment_scan_floor：notifyCancelSafe 扫描事件流找活跃 assignment
// 的 target 时，从 seq=0 全量扫（讨论事件量级小，不做窗口优化）。
const cancel_assignment_scan_floor int64 = 0

// discussionRuntime 承载 service=discussion 的生命周期 action（P4）。
//
// 它与旧后端 discussion/service.py 的 DiscussionService 对位：负责讨论实例的
// 创建与推进（start/continue/finish/cancel/interject/retry/stop/getActive）、
// 权限判定、乐观锁校验与事件落库。「host AI 真正的决策执行」（收 host_decision、
// artifact_update、推进轮次）是 P6 的范围，这里只把状态机与事件流铺到位。
//
// 身份域：member_id / owner / created_by 全部使用融云侧 TEXT id（claw 用户的
// user_id 本身就是融云 id；AI 成员用节点的 rongcloud_user_id）。与 P3 的
// chatroom 命令通道同一约定。
type discussionRuntime struct {
	queries *db.Queries
	repo    *DiscussionRepository
	client  *rongcloudAPIClient
	logger  *slog.Logger
	// systemHostLoader 读管理端配置的 system host（display_name/avatar/node_id）。
	systemHostLoader func(ctx context.Context) (DiscussionSystemHostConfig, bool, error)
	// systemNodeID 是融云侧系统账号（发件人），在 channel 装配时绑定。
	systemNodeID string
	// roundtable 承载 createRoundtable 的五阶段幂等状态机（P5）。
	roundtable *roundtableCreationService
	// nodeRequests 是发给设备端的推荐请求 broker（recommendRoles 用）。
	nodeRequests *NodeRequestBroker
	// interactiveEnabled 是 P7 v3 交互轮次引擎的总开关，装配完成时由
	// registration 置为 true。它是 getWorkflowCapabilities 翻面的唯一依据：
	// 只有引擎真能推进时才对 C 端宣告 protocol_versions=[2,3]。
	interactiveEnabled bool
}

// DiscussionSystemHostConfig 是管理端 system_host 配置的运行时视图（P4）。
// 导出是因为 HTTP 层（claw_admin_discussion_host.go 的 loader 适配）要构造它。
type DiscussionSystemHostConfig struct {
	NodeID      string
	DisplayName string
	Avatar      string
	// ConfigVersion / HostPrompt / Enabled / TimeoutSeconds 供 P5 的创建编排
	//（推荐一致性校验 + host 快照落库）。Enabled=false 时按未配置处理。
	ConfigVersion  string
	HostPrompt     string
	Enabled        bool
	TimeoutSeconds int
	// RecommendationPrompt 是运营配置的「角色推荐提示词」（P5 的 recommendRoles
	// 下发给设备端，拼在推荐 prompt 最前）。
	RecommendationPrompt string
}

// discussionActor 是讨论身份域里的操作者。两个来源：
// rongcloud_user（设备/节点/安装账号）与 claw_im_users（C 端真人）。
type discussionActor struct {
	RcID       string // 融云侧 TEXT id（= claw user_id 或 rc 节点账号）
	Name       string
	Portrait   string
	IsClawUser bool
	// MirrorID / WorkspaceID：claw 用户补建的 rongcloud_user 镜像行（UUID 域），
	// P3 chatroom 写 action 拿它做 owner_user_id 外键。
	MirrorID    pgtype.UUID
	WorkspaceID pgtype.UUID
}

func newDiscussionRuntime(queries *db.Queries, repo *DiscussionRepository, client *rongcloudAPIClient, systemNodeID string, logger *slog.Logger) *discussionRuntime {
	if logger == nil {
		logger = slog.Default()
	}
	return &discussionRuntime{queries: queries, repo: repo, client: client, systemNodeID: systemNodeID, logger: logger}
}

// attachRoundtable 把 P5 的创建编排挂到运行时（channel 装配时调用）。
// nodeOwner 按「融云好友边」列一个用户归属的节点（node_claim 语义的反向查询）。
func (rt *discussionRuntime) attachRoundtable(creation *roundtableCreationService, nodeRequests *NodeRequestBroker) {
	rt.roundtable = creation
	rt.nodeRequests = nodeRequests
}

// resolveActor 把 IM 发送者的融云 id 解析成讨论身份域的 actor。先查
// rongcloud_user（节点/安装账号），miss 后查 claw_im_users（C 端真人——
// 注册时融云 id = user_id，但不写 rongcloud_user 镜像，必须兜底）。
func (rt *discussionRuntime) resolveActor(ctx context.Context, fromUserID string) (discussionActor, error) {
	if fromUserID == "" {
		return discussionActor{}, errors.New("missing sender")
	}
	if rt.queries == nil {
		return discussionActor{}, errors.New("database not available")
	}
	if u, err := rt.queries.GetRongCloudUserByRongCloudID(ctx, fromUserID); err == nil {
		return discussionActor{
			RcID:     u.RongcloudUserID,
			Name:     u.Name.String,
			Portrait: u.PortraitUri.String,
		}, nil
	}
	row, err := rt.queries.GetClawIMUserByUserID(ctx, fromUserID)
	if err != nil {
		return discussionActor{}, fmt.Errorf("rongcloud: resolve actor %s: %w", fromUserID, err)
	}
	actor := discussionActor{
		RcID:       row.UserID,
		Name:       stringOrNull(row.Nickname),
		Portrait:   stringOrNull(row.PortraitUri),
		IsClawUser: true,
	}
	// 幂等补 rongcloud_user 镜像行（与 chatroom 域共用同一实现）：P3 的 chatroom
	// 写 action（create/join 等）拿 actor 的 UUID 做 owner_user_id 外键；claw 注册
	// 链路不写镜像（clawRong 直接调融云 API），第一次进讨论/聊天室命令时补上，
	// 之后 rongcloud_user 优先命中、走快路径。workspace 用单安装归属。
	// 补建失败不阻断 actor 解析 —— 讨论域的读写只依赖融云 TEXT id，镜像只影响
	// UUID 域的外键写入。
	if mirror, mErr := mirrorClawUserToRongCloudUser(ctx, rt.queries, actor.RcID, actor.Name, actor.Portrait); mErr == nil {
		actor.MirrorID = mirror.ID
		actor.WorkspaceID = mirror.WorkspaceID
	}
	return actor, nil
}

func stringOrNull(s pgtype.Text) string {
	if s.Valid {
		return s.String
	}
	return ""
}

// isRoomMember 判定 actor（融云 id 域）是否为该聊天室的启用成员（owner 也算）。
func (rt *discussionRuntime) isRoomMember(ctx context.Context, roomID pgtype.UUID, ownerRCID, actorRCID string) bool {
	if ownerRCID != "" && ownerRCID == actorRCID {
		return true
	}
	members, err := rt.queries.ListRongCloudChatroomMembersWithNode(ctx, roomID)
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.Enabled && m.MemberID.Valid && m.MemberID.String == actorRCID {
			return true
		}
	}
	return false
}

// canControl 对齐 Python _can_control：created_by 或聊天室 owner 才能推进/取消。
func canControl(d DiscussionRecord, ownerRCID, actorRCID string) bool {
	return actorRCID != "" && (actorRCID == d.CreatedBy || (ownerRCID != "" && actorRCID == ownerRCID))
}

// ---- 快照 / 事件序列化（对齐 Python snapshot_json / event_json） ------------

func discussionSnapshotView(d DiscussionRecord, artifact *ArtifactRecord) map[string]interface{} {
	view := map[string]interface{}{
		"discussionId":  d.DiscussionID,
		"chatroomId":    d.ChatroomID,
		"createdBy":     d.CreatedBy,
		"topic":         d.Topic,
		"goal":          d.Goal,
		"maxRounds":     intPtrValue(d.MaxRounds),
		"roundPolicy":   d.RoundPolicy,
		"roundRevision": d.RoundRevision,
		"mode":          d.Mode,
		"status":        d.Status,
		"currentRound":  d.CurrentRound,
		"stateVersion":  d.Version,
		"roles":         jsonObjectValue(d.RoleSnapshot),
		"createdAt":     isoTime(d.CreatedAt),
		"updatedAt":     isoTime(d.UpdatedAt),
		"artifact":      nil,
	}
	if artifact != nil {
		view["artifact"] = artifactView(*artifact)
	}
	return view
}

func artifactView(a ArtifactRecord) map[string]interface{} {
	return map[string]interface{}{
		"artifactId":   a.ArtifactID,
		"discussionId": a.DiscussionID,
		"artifactType": a.ArtifactType,
		"title":        a.Title,
		"content":      a.Content,
		"version":      a.Version,
		"baseVersion":  a.BaseVersion,
		"createdAt":    isoTime(a.CreatedAt),
		"updatedAt":    isoTime(a.UpdatedAt),
	}
}

func intPtrValue(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

func isoTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// discussionEventView 对齐 Python event_json：v2 字段 + v3 信封展开。
//
// v3 事件是**白名单制**：前端 lib/discussion-v3.ts 的 eventData 逐类型校验，任何
// 一个不合规的 v3 事件都会让 getDiscussionEvents 整段抛错（它 map 时不让过）。
// 引擎内部仍有若干控制事件不在白名单里（assignment_created / host_turn_requested /
// control_accepted / task_timeout / retry_accepted / task_failed），它们对外必须
// 降级成 discussion_progress + 空 data —— 事件本体仍在表里（审计/重放要用），
// 只是不往 C 端暴露未定契约的内部推进。
func discussionEventView(e DiscussionEventRecord) map[string]interface{} {
	view := map[string]interface{}{
		"eventId":      eventUUIDString(e.ID),
		"discussionId": e.DiscussionID,
		"seq":          e.Seq,
		"eventType":    e.EventType,
		"actorId":      e.ActorID,
		"targetId":     e.TargetID,
		"payload":      jsonObjectValue(e.Payload),
		"createdAt":    isoTime(e.CreatedAt),
	}
	if len(e.V3Envelope) > 0 {
		var env map[string]interface{}
		if err := json.Unmarshal(e.V3Envelope, &env); err == nil {
			for k, v := range env {
				if k == "eventId" || k == "seq" || k == "eventType" || k == "createdAt" {
					continue
				}
				view[k] = v
			}
		}
		view["protocolVersion"] = 3
		if !isInteractiveV3EventType(e.EventType) {
			view["eventType"] = "discussion_progress"
			view["payload"] = map[string]interface{}{}
		}
	}
	return view
}

func eventUUIDString(u pgtype.UUID) string {
	if u.Valid {
		return u.String()
	}
	return ""
}

// jsonObjectValue 把 JSONB 原样解析成可序列化值；空/坏值给 {}。
func jsonObjectValue(raw []byte) interface{} {
	if len(raw) == 0 {
		return map[string]interface{}{}
	}
	var parsed interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return map[string]interface{}{}
	}
	return parsed
}

// ---- 参数读取 ------------------------------------------------------------

func paramTrim(cmd CommandContent, keys ...string) string {
	return strings.TrimSpace(getStringAny(cmd.Params, keys...))
}

func paramInt(cmd CommandContent, keys ...string) (int, bool) {
	for _, k := range keys {
		if v, ok := cmd.Params[k]; ok {
			switch n := v.(type) {
			case float64:
				return int(n), true
			case int:
				return n, true
			case string:
				var parsed int
				if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &parsed); err == nil {
					return parsed, true
				}
			}
		}
	}
	return 0, false
}

// paramStringList 读一个字符串数组参数（node_ids / nodeIds）。
//
// 返回值是 (值, 是否给出, 是否合法)：参数缺失（或显式 null）→ (nil,false,true)；
// 给出但形态不合法 → (nil,true,false)。「给出但是空数组」必须判非法而不是当缺失：
// C 端把「一台设备都没选」编码成省略 node_ids，一旦有客户端真的发了 []，说明它的
// 状态和我们的理解不一致，静默按「用户全部设备」去推荐只会推出用户没选的设备。
func paramStringList(cmd CommandContent, keys ...string) ([]string, bool, bool) {
	for _, k := range keys {
		raw, present := cmd.Params[k]
		if !present {
			continue
		}
		if raw == nil {
			return nil, false, true
		}
		items, ok := raw.([]interface{})
		if !ok || len(items) == 0 || len(items) > RecommendRolesMaxRoles {
			return nil, true, false
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return nil, true, false
			}
			text = strings.TrimSpace(text)
			if text == "" || len([]rune(text)) > recommendRolesMaxNodeID {
				return nil, true, false
			}
			out = append(out, text)
		}
		return out, true, true
	}
	return nil, false, true
}

// ---- action 实现 ----------------------------------------------------------

// start 对齐 Python start：owner-only，创建 planning 状态的讨论 + 两条初始事件。
// host 的实际派发（_deliver_host）在 P6；P4 先落 host_turn_requested 事件。
func (rt *discussionRuntime) actionStart(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	rcID := paramTrim(cmd, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "chatroomId 不能为空")
	}
	room, err := rt.queries.GetRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	ownerRCID := stringOrNull(room.OwnerRcUserID)
	if ownerRCID != actor.RcID {
		return errResult(403, "only the chatroom owner may start a discussion")
	}
	topic := paramTrim(cmd, "topic")
	goal := paramTrim(cmd, "goal")
	if topic == "" || len(topic) > 4000 || goal == "" || len(goal) > 8000 {
		return errResult(400, "invalid topic or goal")
	}
	maxRounds, _ := paramInt(cmd, "maxRounds", "max_rounds")
	mode := paramTrim(cmd, "mode")
	if mode == "" {
		mode = DiscussionModeModerated
	}
	if mode != DiscussionModeModerated && mode != DiscussionModeRoundtable {
		return errResult(400, "invalid mode")
	}
	roundPolicy := RoundPolicyLegacyCapped
	// 活跃讨论唯一性：一个聊天室同时只能有一个
	if _, err := rt.repo.GetActiveByChatroom(ctx, rcID); err == nil {
		return errResult(409, "chatroom already has an active discussion")
	} else if !errors.Is(err, ErrDiscussionNotFound) {
		return errResult(500, "查询活跃讨论失败")
	}

	// 角色板：启用的 AI 成员 + host 标记
	members, err := rt.queries.ListRongCloudChatroomMembersWithNode(ctx, room.ID)
	if err != nil {
		return errResult(500, "读取成员失败")
	}
	roles := map[string]interface{}{}
	for _, m := range members {
		if m.MemberType != "ai" || !m.Enabled {
			continue
		}
		if !m.NodeRcUserID.Valid || m.NodeRcUserID.String == "" {
			continue
		}
		nodeID := ""
		if m.BusinessNodeID.Valid {
			nodeID = m.BusinessNodeID.String
		}
		memberKey := m.NodeRcUserID.String
		roles[memberKey] = map[string]interface{}{
			"memberId":         memberKey,
			"nodeId":           nodeID,
			"nickname":         m.Nickname,
			"portraitUri":      m.PortraitUri,
			"roleName":         textStr(m.RoleName),
			"roleInstructions": textStr(m.RoleInstructions),
			"capabilities":     capsToInterface(m.Capabilities),
			"isHost":           room.HostNodeID.Valid && m.NodeID.Valid && room.HostNodeID.Bytes == m.NodeID.Bytes,
		}
	}
	if len(roles) == 0 {
		return errResult(400, "聊天室没有可用的 AI 成员")
	}
	var maxRoundsPtr *int
	if roundPolicy == RoundPolicyLegacyCapped {
		if maxRounds < 1 || maxRounds > 100 {
			return errResult(400, "maxRounds must be between 1 and 100")
		}
		v := maxRounds
		maxRoundsPtr = &v
	}
	discussionID := newDiscussionID()
	created, err := rt.repo.Create(ctx, CreateDiscussionParams{
		DiscussionID: discussionID,
		ChatroomID:   rcID,
		WorkspaceID:  room.WorkspaceID,
		CreatedBy:    actor.RcID,
		Topic:        topic,
		Goal:         goal,
		MaxRounds:    maxRoundsPtr,
		Mode:         mode,
		Status:       DiscussionStatusPlanning,
		RoleSnapshot: mustJSON(roles),
		RoundPolicy:  roundPolicy,
	})
	if err != nil {
		return errResult(500, "创建讨论失败")
	}
	// 初始事件：discussion_started + host_turn_requested
	events := []AppendDiscussionEventParams{
		{
			EventType: "discussion_started", ActorID: actor.RcID,
			Payload: mustJSON(map[string]interface{}{
				"topic": topic, "goal": goal, "maxRounds": intPtrValue(maxRoundsPtr), "hostMemberId": "",
			}),
			IdempotencyKey: "start:" + discussionID,
		},
	}
	_, evErr := rt.appendEventsSequential(ctx, created, events)
	if evErr != nil {
		return errResult(500, "写入讨论事件失败")
	}
	return okResult(discussionSnapshotView(created, nil))
}

// getActive 对齐 Python：成员可见；带最新产物；无活跃时 data=null。
func (rt *discussionRuntime) actionGetActive(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	rcID := paramTrim(cmd, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "chatroomId 不能为空")
	}
	room, err := rt.queries.GetRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	if !rt.isRoomMember(ctx, room.ID, stringOrNull(room.OwnerRcUserID), actor.RcID) {
		return errResult(403, "不是聊天室成员")
	}
	// discussionId 显式指定时校验归属
	discussionID := paramTrim(cmd, "discussionId", "discussion_id")
	if discussionID != "" {
		d, err := rt.repo.Get(ctx, discussionID)
		if err != nil || d.ChatroomID != rcID {
			return errResult(404, "讨论不存在")
		}
		return okResult(discussionSnapshotView(d, rt.latestArtifactPtr(ctx, discussionID)))
	}
	d, err := rt.repo.GetActiveByChatroom(ctx, rcID)
	if errors.Is(err, ErrDiscussionNotFound) {
		return okResult(nil)
	}
	if err != nil {
		return errResult(500, "查询活跃讨论失败")
	}
	return okResult(discussionSnapshotView(d, rt.latestArtifactPtr(ctx, d.DiscussionID)))
}

// stop 对齐 Python stop：无版本参数；roundtable → cancel，moderated → owner 强停。
func (rt *discussionRuntime) actionStop(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	discussionID := paramTrim(cmd, "discussionId", "discussion_id")
	if discussionID == "" {
		return errResult(400, "discussionId 不能为空")
	}
	d, err := rt.repo.Get(ctx, discussionID)
	if errors.Is(err, ErrDiscussionNotFound) {
		return errResult(404, "讨论不存在")
	}
	if err != nil {
		return errResult(500, "查询讨论失败")
	}
	room, err := rt.queries.GetRongCloudChatroomViewByRongCloudID(ctx, d.ChatroomID)
	if err != nil {
		return errResult(500, "查询聊天室失败")
	}
	ownerRCID := stringOrNull(room.OwnerRcUserID)
	if !canControl(d, ownerRCID, actor.RcID) {
		return errResult(403, "only the chatroom owner may stop a discussion")
	}
	if !IsLiveDiscussionStatus(d.Status) {
		return okResult(discussionSnapshotView(d, rt.latestArtifactPtr(ctx, d.DiscussionID)))
	}
	changed, advErr := rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: d.DiscussionID, ExpectedVersion: d.Version,
		Status: DiscussionStatusCancelled, RoundRevision: d.RoundRevision,
	})
	if advErr != nil {
		return rt.advanceErrorResult(ctx, advErr, d.DiscussionID)
	}
	_, _ = rt.appendEventsSequential(ctx, changed, []AppendDiscussionEventParams{{
		EventType: "discussion_cancelled", ActorID: actor.RcID,
		Payload:        mustJSON(map[string]interface{}{"reason": "stopped by owner"}),
		IdempotencyKey: fmt.Sprintf("stop:%s:%d", d.DiscussionID, d.Version+1),
	}})
	rt.notifyCancelSafe(ctx, changed)
	return okResult(discussionSnapshotView(changed, rt.latestArtifactPtr(ctx, changed.DiscussionID)))
}

// cancel 对齐 Python cancel：owner-only + 版本校验 + cancelled 事件。
func (rt *discussionRuntime) actionCancel(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	discussionID := paramTrim(cmd, "discussionId", "discussion_id")
	if discussionID == "" {
		return errResult(400, "discussionId 不能为空")
	}
	version, ok := paramInt(cmd, "stateVersion", "state_version", "version")
	if !ok || version < 1 {
		return errResult(400, "stateVersion 必须是正整数")
	}
	d, err := rt.repo.Get(ctx, discussionID)
	if errors.Is(err, ErrDiscussionNotFound) {
		return errResult(404, "讨论不存在")
	}
	if err != nil {
		return errResult(500, "查询讨论失败")
	}
	room, err := rt.queries.GetRongCloudChatroomViewByRongCloudID(ctx, d.ChatroomID)
	if err != nil {
		return errResult(500, "查询聊天室失败")
	}
	if !canControl(d, stringOrNull(room.OwnerRcUserID), actor.RcID) {
		return errResult(403, "only the discussion owner may cancel")
	}
	if !IsLiveDiscussionStatus(d.Status) {
		return okResult(discussionSnapshotView(d, rt.latestArtifactPtr(ctx, d.DiscussionID)))
	}
	if d.Version != version {
		return rt.versionConflictResult(ctx, d.DiscussionID)
	}
	changed, advErr := rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: d.DiscussionID, ExpectedVersion: d.Version,
		Status: DiscussionStatusCancelled, RoundRevision: d.RoundRevision,
	})
	if advErr != nil {
		return rt.advanceErrorResult(ctx, advErr, d.DiscussionID)
	}
	_, _ = rt.appendEventsSequential(ctx, changed, []AppendDiscussionEventParams{{
		EventType: "discussion_cancelled", ActorID: actor.RcID,
		Payload:        mustJSON(map[string]interface{}{"reason": paramTrim(cmd, "reason")}),
		IdempotencyKey: fmt.Sprintf("cancel:%s:%d", d.DiscussionID, d.Version+1),
	}})
	rt.notifyCancelSafe(ctx, changed)
	return okResult(discussionSnapshotView(changed, rt.latestArtifactPtr(ctx, changed.DiscussionID)))
}

// interject 对齐 Python interject：owner 或启用的人类成员；幂等 by messageUid。
func (rt *discussionRuntime) actionInterject(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	discussionID := paramTrim(cmd, "discussionId", "discussion_id")
	content := paramTrim(cmd, "content")
	messageUID := paramTrim(cmd, "messageUid", "message_uid")
	if discussionID == "" {
		return errResult(400, "discussionId 不能为空")
	}
	version, ok := paramInt(cmd, "stateVersion", "state_version", "version")
	if !ok || version < 1 {
		return errResult(400, "stateVersion 必须是正整数")
	}
	d, err := rt.repo.Get(ctx, discussionID)
	if errors.Is(err, ErrDiscussionNotFound) {
		return errResult(404, "讨论不存在")
	}
	if err != nil {
		return errResult(500, "查询讨论失败")
	}
	if d.Version != version {
		return rt.versionConflictResult(ctx, d.DiscussionID)
	}
	if !IsLiveDiscussionStatus(d.Status) {
		return errResult(409, "discussion is no longer active")
	}
	room, err := rt.queries.GetRongCloudChatroomViewByRongCloudID(ctx, d.ChatroomID)
	if err != nil {
		return errResult(500, "查询聊天室失败")
	}
	ownerRCID := stringOrNull(room.OwnerRcUserID)
	if !canControl(d, ownerRCID, actor.RcID) && !rt.isHumanMember(ctx, room.ID, actor.RcID) {
		return errResult(403, "only enabled human members may interject")
	}
	if content == "" || len(content) > 8000 || messageUID == "" || len(messageUID) > 128 {
		return errResult(400, "invalid interjection")
	}
	// 幂等：同一 messageUid 只落一条
	idemKey := "interjection:" + discussionID + ":" + messageUID
	existing, _ := rt.repo.ListEventsAfter(ctx, discussionID, 0)
	for _, e := range existing {
		if e.IdempotencyKey == idemKey {
			return okResult(discussionSnapshotView(d, rt.latestArtifactPtr(ctx, discussionID)))
		}
	}
	_, appErr := rt.appendEventsSequential(ctx, d, []AppendDiscussionEventParams{{
		EventType: "user_interjection", ActorID: actor.RcID,
		Payload: mustJSON(map[string]interface{}{
			"messageUid": messageUID, "memberId": actor.RcID, "content": content,
			"createdAt": time.Now().UnixMilli(), "consumedFromRound": d.CurrentRound + 1,
		}),
		IdempotencyKey: idemKey,
	}})
	if appErr != nil {
		return errResult(500, "写入插话事件失败")
	}
	return okResult(discussionSnapshotView(d, rt.latestArtifactPtr(ctx, discussionID)))
}

// retry 对齐 Python retry：owner-only；重投递最近一次未完成的 host_turn /
// assignment（重新私发给目标）。P4 不改变状态机，只做安全重发。
func (rt *discussionRuntime) actionRetry(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	discussionID := paramTrim(cmd, "discussionId", "discussion_id")
	if discussionID == "" {
		return errResult(400, "discussionId 不能为空")
	}
	version, ok := paramInt(cmd, "stateVersion", "state_version", "version")
	if !ok || version < 1 {
		return errResult(400, "stateVersion 必须是正整数")
	}
	d, err := rt.repo.Get(ctx, discussionID)
	if errors.Is(err, ErrDiscussionNotFound) {
		return errResult(404, "讨论不存在")
	}
	if err != nil {
		return errResult(500, "查询讨论失败")
	}
	room, err := rt.queries.GetRongCloudChatroomViewByRongCloudID(ctx, d.ChatroomID)
	if err != nil {
		return errResult(500, "查询聊天室失败")
	}
	if !canControl(d, stringOrNull(room.OwnerRcUserID), actor.RcID) {
		return errResult(403, "only the discussion owner may retry")
	}
	if !IsLiveDiscussionStatus(d.Status) {
		return errResult(409, "discussion is no longer active")
	}
	if d.Status == DiscussionStatusAwaitingUser {
		return errResult(409, "discussion is awaiting a user decision")
	}
	if d.Version != version {
		return rt.versionConflictResult(ctx, d.DiscussionID)
	}
	// 找最近一次 host_turn_requested / assignment_created
	events, err := rt.repo.ListEventsAfter(ctx, discussionID, 0)
	if err != nil {
		return errResult(500, "读取事件失败")
	}
	var last *DiscussionEventRecord
	for i := len(events) - 1; i >= 0; i-- {
		t := events[i].EventType
		if t == "host_turn_requested" || t == "assignment_created" {
			last = &events[i]
			break
		}
	}
	if last == nil {
		return errResult(409, "no persisted discussion work to retry")
	}
	// 重发（best-effort）：目标 = target_id（融云账号）
	if rt.client != nil && last.TargetID != "" {
		content, _ := json.Marshal(jsonObjectValue(last.Payload))
		if sendErr := rt.sendPrivateJSON(ctx, last.TargetID, content); sendErr != nil {
			rt.logger.Warn("discussion retry redeliver failed", "error", sendErr, "discussionId", discussionID)
		}
	}
	return okResult(discussionSnapshotView(d, rt.latestArtifactPtr(ctx, d.DiscussionID)))
}

// continue 对齐 Python continue_round（legacy_capped 分支）：roundtable 模式且
// awaiting_user 时开下一轮；opinion 可选。
func (rt *discussionRuntime) actionContinue(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	discussionID := paramTrim(cmd, "discussionId", "discussion_id")
	if discussionID == "" {
		return errResult(400, "discussionId 不能为空")
	}
	version, ok := paramInt(cmd, "stateVersion", "state_version", "version")
	if !ok || version < 1 {
		return errResult(400, "stateVersion 必须是正整数")
	}
	opinion := paramTrim(cmd, "opinion")
	if len(opinion) > 8000 {
		return errResult(400, "opinion exceeds 8000 characters")
	}
	d, err := rt.repo.Get(ctx, discussionID)
	if errors.Is(err, ErrDiscussionNotFound) {
		return errResult(404, "讨论不存在")
	}
	if err != nil {
		return errResult(500, "查询讨论失败")
	}
	room, err := rt.queries.GetRongCloudChatroomViewByRongCloudID(ctx, d.ChatroomID)
	if err != nil {
		return errResult(500, "查询聊天室失败")
	}
	if !canControl(d, stringOrNull(room.OwnerRcUserID), actor.RcID) {
		return errResult(403, "only the discussion owner may continue")
	}
	if d.Mode != DiscussionModeRoundtable || d.Status != DiscussionStatusAwaitingUser {
		return errResult(409, "discussion is not awaiting a user decision")
	}
	if d.Version != version {
		return rt.versionConflictResult(ctx, d.DiscussionID)
	}
	nextRound := d.CurrentRound + 1
	if d.MaxRounds != nil && nextRound > *d.MaxRounds {
		return errResult(409, "maximum rounds reached")
	}
	events := []AppendDiscussionEventParams{}
	if opinion != "" {
		events = append(events, AppendDiscussionEventParams{
			EventType: "user_interjection", ActorID: actor.RcID,
			Payload: mustJSON(map[string]interface{}{
				"messageUid": fmt.Sprintf("continue:%s:%d", discussionID, d.Version+1),
				"memberId":   actor.RcID, "content": opinion,
				"createdAt": time.Now().UnixMilli(), "consumedFromRound": nextRound,
			}),
			IdempotencyKey: fmt.Sprintf("continue-opinion:%s:%d", discussionID, d.Version+1),
		})
	}
	events = append(events, AppendDiscussionEventParams{
		EventType: "round_started", ActorID: actor.RcID,
		Payload:        mustJSON(map[string]interface{}{"round": nextRound}),
		IdempotencyKey: fmt.Sprintf("round-start:%s:%d", discussionID, nextRound),
	})
	changed, advErr := rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: d.DiscussionID, ExpectedVersion: d.Version,
		Status: DiscussionStatusRunning, CurrentRound: nextRound, RoundRevision: d.RoundRevision,
	})
	if advErr != nil {
		return rt.advanceErrorResult(ctx, advErr, d.DiscussionID)
	}
	_, _ = rt.appendEventsSequential(ctx, changed, events)
	return okResult(discussionSnapshotView(changed, rt.latestArtifactPtr(ctx, changed.DiscussionID)))
}

// finish 对齐 Python finish（legacy_capped 分支）：roundtable 且 awaiting_user 时
// 进入 synthesizing，落 final_synthesis_started 事件。
func (rt *discussionRuntime) actionFinish(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	discussionID := paramTrim(cmd, "discussionId", "discussion_id")
	if discussionID == "" {
		return errResult(400, "discussionId 不能为空")
	}
	version, ok := paramInt(cmd, "stateVersion", "state_version", "version")
	if !ok || version < 1 {
		return errResult(400, "stateVersion 必须是正整数")
	}
	d, err := rt.repo.Get(ctx, discussionID)
	if errors.Is(err, ErrDiscussionNotFound) {
		return errResult(404, "讨论不存在")
	}
	if err != nil {
		return errResult(500, "查询讨论失败")
	}
	room, err := rt.queries.GetRongCloudChatroomViewByRongCloudID(ctx, d.ChatroomID)
	if err != nil {
		return errResult(500, "查询聊天室失败")
	}
	if !canControl(d, stringOrNull(room.OwnerRcUserID), actor.RcID) {
		return errResult(403, "only the discussion owner may finish")
	}
	if d.Mode != DiscussionModeRoundtable || d.Status != DiscussionStatusAwaitingUser {
		return errResult(409, "discussion is not awaiting a user decision")
	}
	if d.Version != version {
		return rt.versionConflictResult(ctx, d.DiscussionID)
	}
	changed, advErr := rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: d.DiscussionID, ExpectedVersion: d.Version,
		Status: DiscussionStatusSynthesizing, RoundRevision: d.RoundRevision,
	})
	if advErr != nil {
		return rt.advanceErrorResult(ctx, advErr, d.DiscussionID)
	}
	_, _ = rt.appendEventsSequential(ctx, changed, []AppendDiscussionEventParams{{
		EventType: "final_synthesis_started", ActorID: actor.RcID,
		Payload:        mustJSON(map[string]interface{}{"round": d.CurrentRound}),
		IdempotencyKey: fmt.Sprintf("final-synthesis:%s:%d", discussionID, d.Version+1),
	}})
	return okResult(discussionSnapshotView(changed, rt.latestArtifactPtr(ctx, changed.DiscussionID)))
}

// ---- system host / workflow capabilities ---------------------------------

// actionGetSystemHostStatus 对齐 Python：system_host 未配置时 reason=
// SYSTEM_HOST_NOT_CONFIGURED（code 200, data 里带空 display_name/avatar）。
// Go 侧存储在 rongcloud_system_config 的 config_key='system_host' 行。
func (rt *discussionRuntime) actionGetSystemHostStatus(ctx context.Context) commandResult {
	if rt.systemHostLoader != nil {
		if cfg, found, err := rt.systemHostLoader(ctx); err == nil && found {
			data := map[string]interface{}{
				"display_name": cfg.DisplayName,
				"avatar":       cfg.Avatar,
				"reason":       nil,
			}
			if cfg.NodeID == "" {
				data["reason"] = "SYSTEM_HOST_NOT_CONFIGURED"
			}
			return okResult(data)
		}
	}
	return okResult(map[string]interface{}{
		"display_name": "", "avatar": "", "reason": "SYSTEM_HOST_NOT_CONFIGURED",
	})
}

// actionGetWorkflowCapabilities 对齐 Python workflow_capabilities.py 的
// get_workflow_capabilities：v2 / legacy_capped 恒可；v3 / user_confirmed 只在
// 交互轮次引擎（P7）装配就绪时宣告。
//
// 这条回包是前端 RoundtableSetupDialog 决定「能否建 user_confirmed 圆桌」的唯一
// 依据，所以宁可少报也不能多报：引擎缺失（interactiveReady=false）时仍只回 [2]，
// 前端就会走 legacy 路径，而不是建出一个跑不起来的讨论。
func (rt *discussionRuntime) actionGetWorkflowCapabilities(cmd CommandContent) commandResult {
	if len(cmd.Params) != 0 {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	versions := []int{2}
	policies := []string{RoundPolicyLegacyCapped}
	if rt.interactiveReady() {
		versions = append(versions, 3)
		policies = append(policies, RoundPolicyUserConfirmed)
	}
	return okResult(map[string]interface{}{
		"protocol_versions": versions,
		"round_policies":    policies,
	})
}

// ---- 辅助 ----------------------------------------------------------------

// appendEventsSequential 在无并发时序要求时顺序落事件（每次调 NextEventSeq+1）。
// 严格并发安全的事件序号分配在 P6 的运行时事务里做。
func (rt *discussionRuntime) appendEventsSequential(ctx context.Context, d DiscussionRecord, events []AppendDiscussionEventParams) ([]DiscussionEventRecord, error) {
	out := make([]DiscussionEventRecord, 0, len(events))
	for _, e := range events {
		if e.DiscussionID == "" {
			e.DiscussionID = d.DiscussionID
		}
		if e.Seq == 0 {
			seq, err := rt.repo.NextEventSeq(ctx, e.DiscussionID)
			if err != nil {
				return out, err
			}
			e.Seq = seq
		}
		rec, err := rt.repo.AppendEvent(ctx, e)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// latestArtifactPtr 返回最新产物（无则 nil）。
func (rt *discussionRuntime) latestArtifactPtr(ctx context.Context, discussionID string) *ArtifactRecord {
	a, err := rt.repo.LatestArtifact(ctx, discussionID)
	if err != nil {
		return nil
	}
	return &a
}

// versionConflictResult 回 409 + 最新快照（前端 conflictSnapshot 消费
// data.latestSnapshot / data.snapshot）。
func (rt *discussionRuntime) versionConflictResult(ctx context.Context, discussionID string) commandResult {
	d, err := rt.repo.Get(ctx, discussionID)
	if err != nil {
		return errResult(409, "discussion version mismatch")
	}
	return commandResult{
		code: 409, message: "discussion version mismatch",
		data: map[string]interface{}{"latestSnapshot": discussionSnapshotView(d, nil)},
	}
}

// advanceErrorResult 把 AdvanceState 的错误翻译成回包。
func (rt *discussionRuntime) advanceErrorResult(ctx context.Context, err error, discussionID string) commandResult {
	if errors.Is(err, ErrDiscussionVersionConflict) {
		return rt.versionConflictResult(ctx, discussionID)
	}
	if errors.Is(err, ErrDiscussionNotFound) {
		return errResult(404, "讨论不存在")
	}
	return errResult(500, "推进讨论状态失败")
}

// isHumanMember 判定 actor（融云 id）是否为该聊天室启用的人类成员。
func (rt *discussionRuntime) isHumanMember(ctx context.Context, roomID pgtype.UUID, actorRCID string) bool {
	members, err := rt.queries.ListRongCloudChatroomMembersWithNode(ctx, roomID)
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.Enabled && m.MemberType == "user" && m.MemberID.Valid && m.MemberID.String == actorRCID {
			return true
		}
	}
	return false
}

// notifyCancelSafe 给 host 与活跃 assignment 目标私发 discussion_cancel（v2 信封）。
// best-effort：融云侧失败不影响状态机已提交的结果。
func (rt *discussionRuntime) notifyCancelSafe(ctx context.Context, d DiscussionRecord) {
	if rt.client == nil {
		return
	}
	payload := map[string]interface{}{
		"msg_type": "discussion_cancel", "protocolVersion": 2,
		"discussionId": d.DiscussionID, "chatroomId": d.ChatroomID,
		"requestId": newDiscussionID(), "stateVersion": d.Version,
		"round": d.CurrentRound, "timestamp": time.Now().UnixMilli(),
		"reason": "cancelled",
	}
	content, _ := json.Marshal(payload)
	targets := map[string]bool{}
	// host（角色板 isHost=true 的成员）
	var roles map[string]interface{}
	if err := json.Unmarshal(d.RoleSnapshot, &roles); err == nil {
		for _, v := range roles {
			r, ok := v.(map[string]interface{})
			if !ok || !boolValue(r["isHost"]) {
				continue
			}
			if mid, ok := r["memberId"].(string); ok && mid != "" {
				targets[mid] = true
			}
		}
	}
	// 活跃 assignment 的 target（事件流里最后一条 assignment_created/host_turn_requested）
	events, _ := rt.repo.ListEventsAfter(ctx, d.DiscussionID, cancel_assignment_scan_floor)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].EventType == "assignment_created" || events[i].EventType == "host_turn_requested" {
			if events[i].TargetID != "" {
				targets[events[i].TargetID] = true
			}
			break
		}
	}
	for target := range targets {
		if err := rt.sendPrivateJSON(ctx, target, content); err != nil {
			rt.logger.Warn("discussion cancel notify failed", "error", err, "target", target)
		}
	}
}

// sendPrivateJSON 用系统账号经融云私发一条 command 消息（isPersisted=1）。
func (rt *discussionRuntime) sendPrivateJSON(ctx context.Context, toUserID string, content []byte) error {
	if rt.client == nil {
		return errors.New("rongcloud client not available")
	}
	systemNodeID := rt.systemNodeID
	if systemNodeID == "" {
		return errors.New("system node id not bound")
	}
	_, err := rt.client.sendPrivateMessage(ctx, systemNodeID, toUserID, objectNameCommand, string(content))
	return err
}

func boolValue(v interface{}) bool {
	b, _ := v.(bool)
	return b
}

// newDiscussionID 生成本次讨论/请求用的 UUID。
func newDiscussionID() string {
	return uuid.NewString()
}

// ---- 导出的序列化入口（HTTP 层扁平 REST 用） -------------------------------

// DiscussionSnapshotView 序列化讨论快照（camelCase，对齐 Python snapshot_json）。
func DiscussionSnapshotView(d DiscussionRecord, artifact *ArtifactRecord) map[string]interface{} {
	return discussionSnapshotView(d, artifact)
}

// DiscussionEventView 序列化事件（含 v3 信封展开）。
func DiscussionEventView(e DiscussionEventRecord) map[string]interface{} {
	return discussionEventView(e)
}

// ArtifactView 序列化产物（camelCase，对齐 Python artifact_json）。
func ArtifactView(a ArtifactRecord) map[string]interface{} {
	return artifactView(a)
}

// RoundDocumentView 序列化逐轮纪要（**snake_case**，对齐 Python document_json ——
// C 端 services/discussion.ts 的 documentMetadata 直接读 snake_case 键，不能改成
// camelCase）。includeContent=false 时省掉正文（列表接口只回元数据）。
func RoundDocumentView(d RoundDocumentRecord, includeContent bool) map[string]interface{} {
	view := map[string]interface{}{
		"document_id":       d.DocumentID,
		"discussion_id":     d.DiscussionID,
		"round":             d.Round,
		"revision":          d.Revision,
		"source_request_id": d.SourceRequestID,
		"title":             d.Title,
		"created_at":        isoTime(d.CreatedAt),
	}
	if includeContent {
		view["content"] = d.Content
	}
	return view
}

// ---- C 端 REST 的成员资格校验（P4） ---------------------------------------

// DiscussionRoomAccess 按 Python _discussion_room_access 的语义校验：chatroomID
// 存在且 userID（融云侧 id）是它的启用成员或 owner。HTTP 层的扁平 REST 用。
type DiscussionRoomAccess struct {
	queries *db.Queries
}

func NewDiscussionRoomAccess(queries *db.Queries) *DiscussionRoomAccess {
	return &DiscussionRoomAccess{queries: queries}
}

// CheckMember 返回用户是否可访问该聊天室的讨论资源。
func (a *DiscussionRoomAccess) CheckMember(ctx context.Context, chatroomID, userID string) (bool, error) {
	if a == nil || a.queries == nil {
		return false, errors.New("rongcloud: room access checker not configured")
	}
	room, err := a.queries.GetRongCloudChatroomViewByRongCloudID(ctx, chatroomID)
	if err != nil {
		return false, nil // 房间不存在 → 不可访问（不区分 404/403，对齐 Python）
	}
	ownerRCID := stringOrNull(room.OwnerRcUserID)
	if ownerRCID != "" && ownerRCID == userID {
		return true, nil
	}
	members, err := a.queries.ListRongCloudChatroomMembersWithNode(ctx, room.ID)
	if err != nil {
		return false, err
	}
	for _, m := range members {
		if m.Enabled && m.MemberID.Valid && m.MemberID.String == userID {
			return true, nil
		}
	}
	return false, nil
}
