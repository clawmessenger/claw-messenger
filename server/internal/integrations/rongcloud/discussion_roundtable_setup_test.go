package rongcloud

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/quota"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 引导式创建（configuring 链路）测试。
//
// 覆盖：
//   - createRoundtableShell 全链路（configuring 讨论行 + setup_prompt(topic) 事件 + 幂等重放）
//   - submitSetupTopic 的 CAS 语义（推进断点 / 同 topic 幂等 / 不同 topic 409）
//   - resumeSetup 断点快照 / touchSetup 心跳
//   - 配额硬校验（429 QUOTA_EXCEEDED）
//   - 用户主动 cancel（user_abandoned，回收房间释放配额）与 configuring 守卫（其余 action 409）
//   - GC 回收（abandoned，幂等）
//   - GetActiveByChatroom 纳入 configuring（与唯一索引口径一致）
//   - 旧五阶段 ensureDiscussion(initialStatus=planning) 回归
//
// 全部 DB 用例在数据库不可达时 Skip（与既有 fixture 同一约定）。

// setupFixture 是引导式链路的测试夹具：workspace + claw 真人 owner + host 节点 +
// 成员节点 + 挂了创建编排的 runtime（无融云客户端，presence 回落 deploy_status）。
type setupFixture struct {
	rt         *discussionRuntime
	pool       *pgxpool.Pool
	queries    *db.Queries
	wsID       pgtype.UUID
	ownerRCID  string
	actor      discussionActor
	hostNode   db.RongcloudNode
	memberNode db.RongcloudNode
}

func setupSetupFixture(t *testing.T) *setupFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("skipping: cannot build pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping: db unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	queries := db.New(pool)
	wsID := seedWorkspaceRow(t, pool)

	// claw 真人 owner（注册链路融云 id = user_id，不写镜像，resolveActor 兜底补建）
	ownerRCID := uniqueID("u")
	_, err = pool.Exec(ctx, `
		INSERT INTO claw_im_users (user_id, username, nickname, password_hash, status)
		VALUES ($1, $2, $3, 'x', 'active') ON CONFLICT DO NOTHING`,
		ownerRCID, "owner-"+ownerRCID, "Owner "+ownerRCID)
	if err != nil {
		t.Fatalf("seed claw user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM claw_im_users WHERE user_id=$1", ownerRCID)
	})

	hostNode := seedNode(t, queries, wsID, "host-node-"+uniqueID("t"),
		`["discussion_host","discussion_roundtable","discussion_model_routing","discussion_role_recommendation"]`)
	memberNode := seedNode(t, queries, wsID, "member-node-"+uniqueID("t"),
		`["discussion_participant","discussion_roundtable","discussion_model_routing"]`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM rongcloud_node WHERE id IN ($1,$2)", hostNode.ID, memberNode.ID)
	})

	rt := newDiscussionRuntime(queries, NewDiscussionRepository(queries, testLogger()), nil, "rc_test_sys", testLogger())
	rt.interactiveEnabled = true
	creation := newRoundtableCreationService(queries, nil)
	creation.hostLoader = func(context.Context) (DiscussionSystemHostConfig, bool, error) {
		return DiscussionSystemHostConfig{
			NodeID: hostNode.ID.String(), DisplayName: "主持人", Enabled: true, ConfigVersion: "1",
		}, true, nil
	}
	creation.nodeOwner = func(context.Context, string) ([]db.RongcloudNode, error) {
		return []db.RongcloudNode{memberNode}, nil
	}
	rt.attachRoundtable(creation, nil)

	actor, err := rt.resolveActor(ctx, ownerRCID)
	if err != nil {
		t.Fatalf("resolve actor: %v", err)
	}
	return &setupFixture{
		rt: rt, pool: pool, queries: queries, wsID: wsID,
		ownerRCID: ownerRCID, actor: actor, hostNode: hostNode, memberNode: memberNode,
	}
}

// createShell 走一遍空组直建并断言基础不变量，返回 (chatroomID, discussionID)。
func (f *setupFixture) createShell(t *testing.T, clientRequestID string) (string, string) {
	t.Helper()
	r := f.rt.actionCreateRoundtableShell(context.Background(), CommandContent{
		Service: "discussion", Action: "createRoundtableShell",
		Params: map[string]interface{}{"client_request_id": clientRequestID, "name": "引导测试组"},
	}, f.actor)
	if r.code != 200 {
		t.Fatalf("createRoundtableShell: %d %s", r.code, r.message)
	}
	data, _ := r.data.(map[string]interface{})
	chatroomID, _ := data["chatroom_id"].(string)
	discussionID, _ := data["discussion_id"].(string)
	if chatroomID == "" || discussionID == "" {
		t.Fatalf("createRoundtableShell 回包缺 id: %#v", data)
	}
	if data["status"] != DiscussionStatusConfiguring {
		t.Fatalf("status = %v, want configuring", data["status"])
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_discussion_event WHERE discussion_id=$1", discussionID)
		_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_discussion WHERE discussion_id=$1", discussionID)
		_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_chatroom_member WHERE chatroom_id=(SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1)", chatroomID)
		_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1", chatroomID)
	})
	return chatroomID, discussionID
}

// eventTypes 列出讨论当前全部事件的类型（按 seq 升序）。
func (f *setupFixture) eventTypes(t *testing.T, discussionID string) []string {
	t.Helper()
	events, err := f.rt.repo.ListEventsAfter(context.Background(), discussionID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.EventType)
	}
	return out
}

func hasEvent(types []string, want string) bool {
	for _, typ := range types {
		if typ == want {
			return true
		}
	}
	return false
}

// assertRoomDeleted 断言房间已软删（status='deleted'，配额计数随之释放）。
// 注意 GetRongCloudChatroomViewByRongCloudID 不过滤 status，必须直查列。
func (f *setupFixture) assertRoomDeleted(t *testing.T, chatroomID string) {
	t.Helper()
	var status string
	if err := f.pool.QueryRow(context.Background(),
		"SELECT status FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1", chatroomID).Scan(&status); err != nil {
		t.Fatalf("读房间 status: %v", err)
	}
	if status != "deleted" {
		t.Fatalf("房间 status = %q, want deleted", status)
	}
}

// waitEvent 轮询直到某类事件出现（异步推荐管线落事件有调度延迟）。
func (f *setupFixture) waitEvent(t *testing.T, discussionID, eventType string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hasEvent(f.eventTypes(t, discussionID), eventType) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("事件 %s 在 5s 内未出现；当前事件: %v", eventType, f.eventTypes(t, discussionID))
}

func TestSetupShellCreatesConfiguringDiscussion(t *testing.T) {
	f := setupSetupFixture(t)
	ctx := context.Background()
	chatroomID, discussionID := f.createShell(t, "req-"+uniqueID("t"))

	row, err := f.queries.GetConfiguringRongCloudDiscussionByChatroom(ctx, chatroomID)
	if err != nil {
		t.Fatalf("configuring 讨论行不存在: %v", err)
	}
	if row.SetupStep != setupStepTopic {
		t.Fatalf("setup_step = %q, want topic", row.SetupStep)
	}
	if !row.LastInteractionAt.Valid {
		t.Fatalf("last_interaction_at 未初始化")
	}
	// 房间：讨论组语义（conversation_kind=discussion）且活跃（计入配额）。
	room, err := f.queries.GetRongCloudChatroomViewByRongCloudID(ctx, chatroomID)
	if err != nil {
		t.Fatalf("房间不存在: %v", err)
	}
	if !room.ConversationKind.Valid || room.ConversationKind.String != "discussion" {
		t.Fatalf("conversation_kind = %#v, want discussion", room.ConversationKind)
	}
	// S1 欢迎卡已落库（幂等键 setup-prompt:topic:<id>）。
	if !hasEvent(f.eventTypes(t, discussionID), setupEventPrompt) {
		t.Fatalf("缺 setup_prompt(topic) 事件: %v", f.eventTypes(t, discussionID))
	}
	// GetActiveByChatroom 必须把 configuring 视为活跃（与唯一索引口径一致）：
	// 同一聊天室不能再起旧链路讨论，cancel 也要能定位到它。
	active, err := f.rt.repo.GetActiveByChatroom(ctx, chatroomID)
	if err != nil || active.DiscussionID != discussionID {
		t.Fatalf("GetActiveByChatroom = %+v, %v; want configuring 讨论", active, err)
	}
}

func TestSetupShellIdempotentReplay(t *testing.T) {
	f := setupSetupFixture(t)
	ctx := context.Background()
	reqID := "req-" + uniqueID("t")
	chatroomID, discussionID := f.createShell(t, reqID)

	// 同 client_request_id 重放：返回既有半成品，不重复建房/建讨论/落事件。
	r := f.rt.actionCreateRoundtableShell(ctx, CommandContent{
		Service: "discussion", Action: "createRoundtableShell",
		Params: map[string]interface{}{"client_request_id": reqID},
	}, f.actor)
	if r.code != 200 {
		t.Fatalf("重放: %d %s", r.code, r.message)
	}
	data, _ := r.data.(map[string]interface{})
	if data["chatroom_id"] != chatroomID || data["discussion_id"] != discussionID {
		t.Fatalf("重放返回了不同 id: %#v", data)
	}
	if n := len(f.eventTypes(t, discussionID)); n != 1 {
		t.Fatalf("重放落了新事件，事件数 = %d, want 1", n)
	}
}

func TestSetupShellQuotaExceeded(t *testing.T) {
	f := setupSetupFixture(t)
	ctx := context.Background()
	// 配额服务挂上真实库，把该用户的讨论组上限压成 0（禁止）。
	svc := quota.New(f.pool)
	zero := 0
	if err := svc.UpsertOverride(ctx, f.ownerRCID, quota.OverrideLimits{DiscussionLimit: &zero}, "test-admin"); err != nil {
		t.Fatalf("upsert override: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM claw_im_user_quota WHERE user_id=$1", f.ownerRCID)
	})
	f.rt.quota = svc

	r := f.rt.actionCreateRoundtableShell(ctx, CommandContent{
		Service: "discussion", Action: "createRoundtableShell",
		Params: map[string]interface{}{"client_request_id": "req-" + uniqueID("t")},
	}, f.actor)
	if r.code != 429 || r.message != quota.CodeQuotaExceeded {
		t.Fatalf("quota 拦截 = %d %s, want 429 QUOTA_EXCEEDED", r.code, r.message)
	}
	data, _ := r.data.(map[string]interface{})
	if data["kind"] != string(quota.KindDiscussion) {
		t.Fatalf("quota data = %#v, want kind=discussion", data)
	}
}

func TestSetupSubmitTopicCAS(t *testing.T) {
	f := setupSetupFixture(t)
	ctx := context.Background()
	chatroomID, discussionID := f.createShell(t, "req-"+uniqueID("t"))

	submit := func(topic string) commandResult {
		return f.rt.actionSubmitSetupTopic(ctx, CommandContent{
			Service: "discussion", Action: "submitSetupTopic",
			Params: map[string]interface{}{"chatroom_id": chatroomID, "topic": topic},
		}, f.actor)
	}
	if r := submit("讨论季度规划"); r.code != 200 {
		t.Fatalf("submitSetupTopic: %d %s", r.code, r.message)
	}
	row, err := f.queries.GetConfiguringRongCloudDiscussionByChatroom(ctx, chatroomID)
	if err != nil {
		t.Fatalf("读讨论行: %v", err)
	}
	if row.SetupStep != setupStepRoles || row.Topic != "讨论季度规划" {
		t.Fatalf("断点/议题 = %q/%q, want roles/讨论季度规划", row.SetupStep, row.Topic)
	}
	// 同 topic 重复提交按幂等成功接受；S2 回上一步改题（roles 步内重提）也允许
	// —— 向导「上一步」回 S1 改完再提交就是这条路，会触发新一轮推荐。
	if r := submit("讨论季度规划"); r.code != 200 {
		t.Fatalf("同 topic 幂等接受: %d %s", r.code, r.message)
	}
	if r := submit("换个议题"); r.code != 200 {
		t.Fatalf("roles 步内改题应接受: %d %s", r.code, r.message)
	}
	row, err = f.queries.GetConfiguringRongCloudDiscussionByChatroom(ctx, chatroomID)
	if err != nil {
		t.Fatalf("读讨论行: %v", err)
	}
	if row.Topic != "换个议题" {
		t.Fatalf("改题未生效: topic = %q", row.Topic)
	}
	// 断点推进到 confirm 后（已勾选完待确认）再提交议题是乱序：CAS 拒绝 → 409。
	if _, err := f.pool.Exec(ctx,
		"UPDATE rongcloud_discussion SET setup_step='confirm' WHERE discussion_id=$1", discussionID); err != nil {
		t.Fatalf("推进断点: %v", err)
	}
	if r := submit("又改题"); r.code != 409 {
		t.Fatalf("confirm 步提交议题应 409, got %d %s", r.code, r.message)
	}
	// nodeRequests 未装配 → 异步推荐落 setup_recommend_failed（前端据以展示重试）。
	f.waitEvent(t, discussionID, setupEventRecommendFailed)
}

func TestSetupResumeAndTouch(t *testing.T) {
	f := setupSetupFixture(t)
	ctx := context.Background()
	chatroomID, discussionID := f.createShell(t, "req-"+uniqueID("t"))

	resume := f.rt.actionResumeSetup(ctx, CommandContent{
		Service: "discussion", Action: "resumeSetup",
		Params: map[string]interface{}{"chatroom_id": chatroomID},
	}, f.actor)
	if resume.code != 200 {
		t.Fatalf("resumeSetup: %d %s", resume.code, resume.message)
	}
	data, _ := resume.data.(map[string]interface{})
	if data["step"] != setupStepTopic || data["status"] != DiscussionStatusConfiguring {
		t.Fatalf("resume 快照 = %#v, want step=topic status=configuring", data)
	}

	// touchSetup：把交互时间往回拨，再心跳，应被刷回接近当前时间。
	old := time.Now().Add(-9 * time.Minute)
	if _, err := f.pool.Exec(ctx,
		"UPDATE rongcloud_discussion SET last_interaction_at=$1 WHERE discussion_id=$2",
		old, discussionID); err != nil {
		t.Fatalf("回拨 last_interaction_at: %v", err)
	}
	touch := f.rt.actionTouchSetup(ctx, CommandContent{
		Service: "discussion", Action: "touchSetup",
		Params: map[string]interface{}{"chatroom_id": chatroomID},
	}, f.actor)
	if touch.code != 200 {
		t.Fatalf("touchSetup: %d %s", touch.code, touch.message)
	}
	row, err := f.queries.GetConfiguringRongCloudDiscussionByChatroom(ctx, chatroomID)
	if err != nil {
		t.Fatalf("读讨论行: %v", err)
	}
	if !row.LastInteractionAt.Time.After(old) {
		t.Fatalf("touch 未刷新 last_interaction_at: %v", row.LastInteractionAt.Time)
	}
}

func TestSetupUserCancelReclaimsRoom(t *testing.T) {
	f := setupSetupFixture(t)
	ctx := context.Background()
	chatroomID, discussionID := f.createShell(t, "req-"+uniqueID("t"))

	// configuring 守卫：continue/finish/interject/retry/stop 对半成品一律 409。
	for _, action := range []string{"continue", "finish", "interject", "retry", "stop"} {
		r, handled := f.rt.interactiveLifecycleAction(ctx, action, CommandContent{
			Service: "discussion", Action: action,
			Params: map[string]interface{}{"chatroom_id": chatroomID},
		}, f.actor)
		if !handled || r.code != 409 {
			t.Fatalf("configuring 下 %s = handled:%v code:%d, want handled+409", action, handled, r.code)
		}
	}
	// cancel 放行：cancelled(user_abandoned) + 房间删除（配额释放）+ 事件广播。
	r, handled := f.rt.interactiveLifecycleAction(ctx, "cancel", CommandContent{
		Service: "discussion", Action: "cancel",
		Params: map[string]interface{}{"chatroom_id": chatroomID},
	}, f.actor)
	if !handled || r.code != 200 {
		t.Fatalf("cancel = handled:%v code:%d %s", handled, r.code, r.message)
	}
	d, err := f.rt.repo.Get(ctx, discussionID)
	if err != nil {
		t.Fatalf("读讨论: %v", err)
	}
	if d.Status != DiscussionStatusCancelled {
		t.Fatalf("status = %q, want cancelled", d.Status)
	}
	row, err := f.queries.GetRongCloudDiscussion(ctx, discussionID)
	if err != nil {
		t.Fatalf("读讨论行: %v", err)
	}
	if row.CancelReason != setupUserAbandonReason {
		t.Fatalf("cancel_reason = %q, want user_abandoned", row.CancelReason)
	}
	if !hasEvent(f.eventTypes(t, discussionID), "discussion_cancelled") {
		t.Fatalf("缺 discussion_cancelled 事件: %v", f.eventTypes(t, discussionID))
	}
	f.assertRoomDeleted(t, chatroomID)
	// 幂等：再 cancel 一次不应报错（讨论已非活跃，走通用 cancel 的已终态分支）。
	r2, _ := f.rt.interactiveLifecycleAction(ctx, "cancel", CommandContent{
		Service: "discussion", Action: "cancel",
		Params: map[string]interface{}{"chatroom_id": chatroomID, "discussion_id": discussionID},
	}, f.actor)
	if r2.code >= 500 {
		t.Fatalf("重复 cancel = %d %s", r2.code, r2.message)
	}
}

func TestSetupGCReclaimsAbandoned(t *testing.T) {
	f := setupSetupFixture(t)
	ctx := context.Background()
	chatroomID, discussionID := f.createShell(t, "req-"+uniqueID("t"))

	// 未超时：不该被扫到。
	f.rt.sweepAbandonedSetups(ctx)
	if row, err := f.queries.GetConfiguringRongCloudDiscussionByChatroom(ctx, chatroomID); err != nil || row.SetupStep == "" {
		t.Fatalf("未超时的半成品被误回收: row=%+v err=%v", row, err)
	}

	// 超时（11 分钟无交互）：GC 回收 → cancelled(abandoned) + 删房 + 事件。
	if _, err := f.pool.Exec(ctx,
		"UPDATE rongcloud_discussion SET last_interaction_at=$1 WHERE discussion_id=$2",
		time.Now().Add(-11*time.Minute), discussionID); err != nil {
		t.Fatalf("回拨 last_interaction_at: %v", err)
	}
	f.rt.sweepAbandonedSetups(ctx)
	row, err := f.queries.GetRongCloudDiscussion(ctx, discussionID)
	if err != nil {
		t.Fatalf("读讨论行: %v", err)
	}
	if row.Status != DiscussionStatusCancelled {
		t.Fatalf("status = %q, want cancelled", row.Status)
	}
	if row.CancelReason != setupAbandonReason {
		t.Fatalf("cancel_reason = %q, want abandoned", row.CancelReason)
	}
	if _, err := f.queries.GetRongCloudChatroomViewByRongCloudID(ctx, chatroomID); err == nil {
		// 视图不过滤 status（历史讨论仍可访问房间），软删断言走 status 列。
		f.assertRoomDeleted(t, chatroomID)
	}
	types := f.eventTypes(t, discussionID)
	if !hasEvent(types, "discussion_cancelled") {
		t.Fatalf("缺 discussion_cancelled 事件: %v", types)
	}
	// 幂等：再扫一轮不报错、不重复落事件。
	f.rt.sweepAbandonedSetups(ctx)
	if n := len(f.eventTypes(t, discussionID)); n != len(types) {
		t.Fatalf("GC 重复落事件: %d → %d", len(types), n)
	}
}

func TestSetupConfirmFullFlow(t *testing.T) {
	f := setupSetupFixture(t)
	ctx := context.Background()
	chatroomID, discussionID := f.createShell(t, "req-"+uniqueID("t"))
	if r := f.rt.actionSubmitSetupTopic(ctx, CommandContent{
		Service: "discussion", Action: "submitSetupTopic",
		Params: map[string]interface{}{"chatroom_id": chatroomID, "topic": "发布计划评审", "goal": "得出结论"},
	}, f.actor); r.code != 200 {
		t.Fatalf("submitSetupTopic: %d %s", r.code, r.message)
	}

	// 直接落一条推荐（绕开 broker 往返，推荐管线本身由 fail 路径用例覆盖）。
	rolesJSON, _ := json.Marshal([]map[string]interface{}{{
		"role_id": "role_x", "role_name": "分析师", "role_prompt": "分析数据",
		"node_id": f.memberNode.NodeID,
	}})
	recID := "rec_" + uniqueID("t")
	if _, err := f.queries.InsertRongCloudRoleRecommendation(ctx, db.InsertRongCloudRoleRecommendationParams{
		RecommendationID: recID,
		RequestID:        "c2c_" + recID,
		UserID:           f.ownerRCID,
		HostNodeID:       f.hostNode.NodeID,
		HostAiUserID:     f.hostNode.RongcloudUserID,
		ConfigVersion:    "1",
		Topic:            "发布计划评审",
		Goal:             "得出结论",
		Roles:            rolesJSON,
		ExpiresAt:        pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("insert recommendation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM rongcloud_role_recommendation WHERE recommendation_id=$1", recID)
	})

	confirm := f.rt.actionConfirmRoundtableSetup(ctx, CommandContent{
		Service: "discussion", Action: "confirmRoundtableSetup",
		Params: map[string]interface{}{
			"chatroom_id":       chatroomID,
			"recommendation_id": recID,
			"roles": []interface{}{map[string]interface{}{
				"role_id": "role_x", "role_name": "分析师", "role_prompt": "分析数据",
				"node_id": f.memberNode.NodeID,
			}},
		},
	}, f.actor)
	if confirm.code != 200 {
		t.Fatalf("confirmRoundtableSetup: %d %s", confirm.code, confirm.message)
	}
	data, _ := confirm.data.(map[string]interface{})
	if data["status"] != DiscussionStatusPlanning {
		t.Fatalf("confirm 回包 status = %v, want planning", data["status"])
	}
	row, err := f.queries.GetRongCloudDiscussion(ctx, discussionID)
	if err != nil {
		t.Fatalf("读讨论行: %v", err)
	}
	// CAS 已推进出 configuring；beginInteractiveSafe 会立刻把 v3 引擎跑起来，
	// 所以读到 planning（引擎尚未推进）或 running（引擎已开局）都正常。
	if row.Status != DiscussionStatusPlanning && row.Status != DiscussionStatusRunning {
		t.Fatalf("status = %q, want planning/running", row.Status)
	}
	if len(row.RoleSnapshot) == 0 || len(row.HostConfigSnapshot) == 0 {
		t.Fatalf("角色板/主持快照未落库")
	}
	if !hasEvent(f.eventTypes(t, discussionID), setupEventStepCompleted) {
		t.Fatalf("缺 setup_step_completed 事件: %v", f.eventTypes(t, discussionID))
	}
	// 角色已落成 AI 成员行。
	room, err := f.queries.GetRongCloudChatroomViewByRongCloudID(ctx, chatroomID)
	if err != nil {
		t.Fatalf("读房间: %v", err)
	}
	members, err := f.queries.ListRongCloudChatroomMembersWithNode(ctx, room.ID)
	if err != nil {
		t.Fatalf("读成员: %v", err)
	}
	aiCount := 0
	for _, m := range members {
		if m.MemberType == "ai" {
			aiCount++
		}
	}
	if aiCount != 2 { // host + 分析师
		t.Fatalf("AI 成员数 = %d, want 2 (host+role)", aiCount)
	}

	// 并发确认重放（同 recommendation_id）：幂等接受，仍回 planning。
	replay := f.rt.actionConfirmRoundtableSetup(ctx, CommandContent{
		Service: "discussion", Action: "confirmRoundtableSetup",
		Params: map[string]interface{}{
			"chatroom_id":       chatroomID,
			"recommendation_id": recID,
			"roles": []interface{}{map[string]interface{}{
				"role_id": "role_x", "role_name": "分析师", "role_prompt": "分析数据",
				"node_id": f.memberNode.NodeID,
			}},
		},
	}, f.actor)
	// 已确认后 findConfiguringSetup 查不到 configuring 行 → 回 404 语义错误码，
	// 或 CAS 分支幂等接受；两者都不得是 5xx。
	if replay.code >= 500 {
		t.Fatalf("确认重放 = %d %s", replay.code, replay.message)
	}
}

// TestSetupLegacyPlanningRegression 旧五阶段回归：ensureDiscussion 传 planning
// 时行为与改造前一致（initialStatus 非 configuring 一律回落 planning）。
func TestSetupLegacyPlanningRegression(t *testing.T) {
	f := setupSetupFixture(t)
	ctx := context.Background()
	chatroomID, _ := f.createShell(t, "req-"+uniqueID("t"))
	// 手工把这个半成品推进 cancelled（腾出一室一活跃的名额），再验证旧链路
	// ensureDiscussion 落 planning 行。
	if _, err := f.pool.Exec(ctx,
		"UPDATE rongcloud_discussion SET status='cancelled', cancel_reason='user_abandoned' WHERE chatroom_id=$1",
		chatroomID); err != nil {
		t.Fatalf("清理半成品: %v", err)
	}

	host, hostErr := f.rt.roundtable.resolveHost(ctx)
	if hostErr != "" {
		t.Fatalf("resolveHost: %s", hostErr)
	}
	canonical := roundtableCanonical{
		ClientRequestID: "req-" + uniqueID("t"), ChatroomName: "旧链路",
		Topic: "t", Goal: "g", RoundPolicy: RoundPolicyUserConfirmed,
	}
	validated := roundtableValidated{host: host, nodes: map[string]db.RongcloudNode{}}
	newChatroomID := "rc_legacy_" + uniqueID("t")
	newDiscussionID := "disc-" + uniqueID("t")
	if cerr := f.rt.roundtable.ensureChatroom(ctx, db.RongcloudRoundtableCreateRequest{}, newChatroomID, canonical, validated, f.ownerRCID); cerr != nil {
		t.Fatalf("ensureChatroom: %+v", cerr)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_discussion_event WHERE discussion_id=$1", newDiscussionID)
		_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_discussion WHERE discussion_id=$1", newDiscussionID)
		_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_chatroom_member WHERE chatroom_id=(SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1)", newChatroomID)
		_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1", newChatroomID)
	})
	if _, cerr := f.rt.roundtable.ensureDiscussion(ctx, db.RongcloudRoundtableCreateRequest{}, newChatroomID, newDiscussionID, canonical, validated, f.ownerRCID, DiscussionStatusPlanning); cerr != nil {
		t.Fatalf("ensureDiscussion: %+v", cerr)
	}
	row, err := f.queries.GetRongCloudDiscussion(ctx, newDiscussionID)
	if err != nil {
		t.Fatalf("读讨论行: %v", err)
	}
	if row.Status != DiscussionStatusPlanning {
		t.Fatalf("旧链路 status = %q, want planning", row.Status)
	}
	if row.SetupStep != "" {
		t.Fatalf("旧链路不应写 setup_step, got %q", row.SetupStep)
	}
	// 非法 initialStatus 一律回落 planning（防调用方传花）。
	if _, cerr := f.rt.roundtable.ensureDiscussion(ctx, db.RongcloudRoundtableCreateRequest{}, newChatroomID, newDiscussionID, canonical, validated, f.ownerRCID, "bogus"); cerr != nil {
		t.Fatalf("ensureDiscussion 幂等重放: %+v", cerr)
	}
}

// ---- 纯单元（无 DB） --------------------------------------------------------

func TestParseSetupConfirmRoles(t *testing.T) {
	t.Run("snake/camel 双读 + speaking_order 按数组序重排", func(t *testing.T) {
		roles, rErr := parseSetupConfirmRoles(CommandContent{Params: map[string]interface{}{
			"roles": []interface{}{
				map[string]interface{}{"role_id": "r1", "role_name": "分析师", "role_prompt": "p1", "node_id": "n1"},
				map[string]interface{}{"roleId": "r2", "roleName": "写手", "roleInstructions": "p2", "nodeId": "n2"},
			},
		}})
		if rErr != nil {
			t.Fatalf("parse: %+v", rErr)
		}
		if len(roles) != 2 || roles[0].SpeakingOrder != 0 || roles[1].SpeakingOrder != 1 {
			t.Fatalf("speaking_order 未按数组序: %+v", roles)
		}
		if roles[1].RolePrompt != "p2" {
			t.Fatalf("camelCase roleInstructions 未读到: %+v", roles[1])
		}
	})
	t.Run("同设备两个角色 → ROLE_DEVICE_DUPLICATED", func(t *testing.T) {
		_, rErr := parseSetupConfirmRoles(CommandContent{Params: map[string]interface{}{
			"roles": []interface{}{
				map[string]interface{}{"role_id": "r1", "role_name": "a", "role_prompt": "p", "node_id": "n1"},
				map[string]interface{}{"role_id": "r2", "role_name": "b", "role_prompt": "p", "node_id": "n1"},
			},
		}})
		if rErr == nil || rErr.Code != "ROLE_DEVICE_DUPLICATED" {
			t.Fatalf("want ROLE_DEVICE_DUPLICATED, got %+v", rErr)
		}
	})
	t.Run("空角色数组 / 非法 role_id 拒绝", func(t *testing.T) {
		if _, rErr := parseSetupConfirmRoles(CommandContent{Params: map[string]interface{}{
			"roles": []interface{}{},
		}}); rErr == nil {
			t.Fatalf("空数组应拒绝")
		}
		if _, rErr := parseSetupConfirmRoles(CommandContent{Params: map[string]interface{}{
			"roles": []interface{}{
				map[string]interface{}{"role_id": "bad id!", "role_name": "a", "role_prompt": "p", "node_id": "n1"},
			},
		}}); rErr == nil {
			t.Fatalf("非法 role_id 应拒绝")
		}
	})
}

func TestSetupPromptPayloadShape(t *testing.T) {
	p := setupPromptPayload("topic", "请输入议题")
	if p["step"] != "topic" || p["prompt"] != "请输入议题" {
		t.Fatalf("payload 形状错: %#v", p)
	}
}
