package rongcloud

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// discussionFixture bundles everything a P4 runtime test needs.
type discussionFixture struct {
	rt         *discussionRuntime
	pool       *pgxpool.Pool
	queries    *db.Queries
	wsID       pgtype.UUID
	ownerRCID  string // claw 用户的融云 id（= user_id）
	roomRCID   string
	memberNode db.RongcloudNode
}

// setupDiscussionFixture 建 workspace + claw 真人 owner + AI 成员 + 聊天室。
// owner 用 claw_im_users 路径（这是 P4 新补的身份域）。
func setupDiscussionFixture(t *testing.T) *discussionFixture {
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

	// claw 真人 owner：直接插 claw_im_users（注册链路融云 id = user_id）
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

	// AI 成员节点
	memberNode := seedNode(t, queries, wsID, "ai-node-"+uniqueID("t"), `["discussion_participant"]`)

	// 真实形态：claw 注册不写镜像。这里先空跑一次 resolveActor 让它补建
	// owner 的镜像行（拿到 owner_user_id 的 UUID），聊天室用它做 owner。
	rt0 := newDiscussionRuntime(queries, nil, nil, "rc_test_sys", testLogger())
	firstActor, err := rt0.resolveActor(ctx, ownerRCID)
	if err != nil {
		t.Fatalf("prime mirror row: %v", err)
	}
	if firstActor.IsClawUser != true || !firstActor.MirrorID.Valid {
		t.Fatalf("expected claw-domain actor with mirror row, got %+v", firstActor)
	}
	roomRCID := "rc_disc_" + uniqueID("t")
	_, err = pool.Exec(ctx, `
		INSERT INTO rongcloud_chatroom (workspace_id, rongcloud_chatroom_id, owner_user_id, host_node_id, max_rounds, conversation_kind, config, status, name)
		VALUES ($1, $2, $3, $4, 10, 'discussion', '{}', 'active', $5)`,
		wsID, roomRCID, firstActor.MirrorID, memberNode.ID, "test-room")
	if err != nil {
		t.Fatalf("seed chatroom: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM rongcloud_discussion_event WHERE discussion_id IN (SELECT discussion_id FROM rongcloud_discussion WHERE chatroom_id=$1)", roomRCID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM rongcloud_discussion WHERE chatroom_id=$1", roomRCID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM rongcloud_chatroom_member WHERE chatroom_id=(SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1)", roomRCID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1", roomRCID)
	})
	// owner 作为人类成员加入
	_, _ = pool.Exec(ctx, `
		INSERT INTO rongcloud_chatroom_member (chatroom_id, node_id, member_type, member_id, nickname, capabilities, enabled)
		VALUES ((SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1), NULL, 'user', $2, $3, '[]', TRUE)`,
		roomRCID, ownerRCID, "Owner")
	// AI 成员
	_, _ = pool.Exec(ctx, `
		INSERT INTO rongcloud_chatroom_member (chatroom_id, node_id, member_type, member_id, nickname, role_name, capabilities, enabled)
		VALUES ((SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1), $2, 'ai', $3, $4, 'analyst', '["discussion_participant"]', TRUE)`,
		roomRCID, memberNode.ID, memberNode.RongcloudUserID, "AI Member")

	rt := newDiscussionRuntime(queries, NewDiscussionRepository(queries, testLogger()), nil, "rc_test_sys", testLogger())
	return &discussionFixture{
		rt: rt, pool: pool, queries: queries, wsID: wsID,
		ownerRCID: ownerRCID, roomRCID: roomRCID, memberNode: memberNode,
	}
}

func discussionParams(kv ...interface{}) map[string]interface{} {
	m := map[string]interface{}{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// TestDiscussionStartLifecycle 验证 claw 真人 owner 的 start → getActive →
// interject → cancel 全链路（P4 核心）。
func TestDiscussionStartLifecycle(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	actor, err := f.rt.resolveActor(ctx, f.ownerRCID)
	if err != nil {
		t.Fatalf("resolveActor: %v", err)
	}
	// RcID 一致即可：fixture 已预建镜像行（prime 步骤），此时 rongcloud_user
	// 优先命中、IsClawUser=false 是线上第二次之后的正常形态。
	if actor.RcID != f.ownerRCID {
		t.Fatalf("actor rc id mismatch: %v", actor.RcID)
	}

	// start（moderated）
	start := f.rt.actionStart(ctx, CommandContent{
		Service: "discussion", Action: "start", WebEnvelope: true,
		Params: discussionParams("chatroomId", f.roomRCID, "topic", "T", "goal", "G", "maxRounds", 3),
	}, actor)
	if start.code != 200 {
		t.Fatalf("start: %d %s", start.code, start.message)
	}
	snap, _ := start.data.(map[string]interface{})
	discussionID, _ := snap["discussionId"].(string)
	if discussionID == "" {
		t.Fatal("start returned no discussionId")
	}
	if snap["status"] != DiscussionStatusPlanning || snap["stateVersion"] != 1 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if snap["roundPolicy"] != RoundPolicyLegacyCapped {
		t.Fatalf("roundPolicy = %v", snap["roundPolicy"])
	}
	roles, _ := snap["roles"].(map[string]interface{})
	if len(roles) != 1 {
		t.Fatalf("roles = %v (expected 1 AI member)", roles)
	}

	// getActive
	active := f.rt.actionGetActive(ctx, CommandContent{
		Params: discussionParams("chatroomId", f.roomRCID),
	}, actor)
	if active.code != 200 {
		t.Fatalf("getActive: %d %s", active.code, active.message)
	}
	activeSnap, _ := active.data.(map[string]interface{})
	if activeSnap["discussionId"] != discussionID {
		t.Fatalf("getActive mismatch: %v", activeSnap["discussionId"])
	}

	// interject（幂等 by messageUid）
	ij := f.rt.actionInterject(ctx, CommandContent{
		Params: discussionParams("discussionId", discussionID, "stateVersion", 1, "content", "hi", "messageUid", "muid-1"),
	}, actor)
	if ij.code != 200 {
		t.Fatalf("interject: %d %s", ij.code, ij.message)
	}
	ij2 := f.rt.actionInterject(ctx, CommandContent{
		Params: discussionParams("discussionId", discussionID, "stateVersion", 1, "content", "hi", "messageUid", "muid-1"),
	}, actor)
	if ij2.code != 200 {
		t.Fatalf("interject replay: %d %s", ij2.code, ij2.message)
	}
	events, err := f.rt.repo.ListEventsAfter(ctx, discussionID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 { // started + interjection（重放被幂等挡掉）
		t.Fatalf("events = %d (expected 2, idempotency broken?)", len(events))
	}

	// cancel（错误版本 → 409 + latestSnapshot）
	badVer := f.rt.actionCancel(ctx, CommandContent{
		Params: discussionParams("discussionId", discussionID, "stateVersion", 99, "reason", "x"),
	}, actor)
	if badVer.code != 409 {
		t.Fatalf("cancel bad version: %d", badVer.code)
	}
	if badVer.data == nil {
		t.Fatal("409 must carry latestSnapshot")
	}
	// cancel（正确版本）
	cancel := f.rt.actionCancel(ctx, CommandContent{
		Params: discussionParams("discussionId", discussionID, "stateVersion", 1, "reason", "用户强制取消"),
	}, actor)
	if cancel.code != 200 {
		t.Fatalf("cancel: %d %s", cancel.code, cancel.message)
	}
	if cancelSnap, _ := cancel.data.(map[string]interface{}); cancelSnap["status"] != DiscussionStatusCancelled {
		t.Fatalf("cancel status: %v", cancelSnap["status"])
	}
	// getActive after cancel → null
	active2 := f.rt.actionGetActive(ctx, CommandContent{
		Params: discussionParams("chatroomId", f.roomRCID),
	}, actor)
	if active2.code != 200 || active2.data != nil {
		t.Fatalf("getActive after cancel: %d %v", active2.code, active2.data)
	}
}

// TestDiscussionPermissionGuards 验证非 owner 被拒。
func TestDiscussionPermissionGuards(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	owner, _ := f.rt.resolveActor(ctx, f.ownerRCID)
	// 另一个 claw 用户（非成员、非 owner）
	stranger := uniqueID("s")
	_, _ = f.pool.Exec(ctx, `
		INSERT INTO claw_im_users (user_id, username, nickname, password_hash, status)
		VALUES ($1, $2, 'S', 'x', 'active') ON CONFLICT DO NOTHING`, stranger, "stranger-"+stranger)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM claw_im_users WHERE user_id=$1", stranger)
	})
	strangerActor, err := f.rt.resolveActor(ctx, stranger)
	if err != nil {
		t.Fatalf("resolve stranger: %v", err)
	}

	start := f.rt.actionStart(ctx, CommandContent{
		Params: discussionParams("chatroomId", f.roomRCID, "topic", "T", "goal", "G", "maxRounds", 3),
	}, strangerActor)
	if start.code != 403 {
		t.Fatalf("stranger start: %d (expected 403)", start.code)
	}

	startOK := f.rt.actionStart(ctx, CommandContent{
		Params: discussionParams("chatroomId", f.roomRCID, "topic", "T", "goal", "G", "maxRounds", 3),
	}, owner)
	if startOK.code != 200 {
		t.Fatalf("owner start: %d %s", startOK.code, startOK.message)
	}
	// getActive 对陌生人 403
	ga := f.rt.actionGetActive(ctx, CommandContent{
		Params: discussionParams("chatroomId", f.roomRCID),
	}, strangerActor)
	if ga.code != 403 {
		t.Fatalf("stranger getActive: %d (expected 403)", ga.code)
	}
}

// TestDiscussionWorkflowCapabilities 验证能力查询：引擎未装配时只报 v2 /
// legacy_capped，装配后就绪时报 [2,3] / [legacy_capped,user_confirmed]。
// 「不谎报 v3」是这条回包的硬约束（前端据此决定能否建 user_confirmed 圆桌）。
func TestDiscussionWorkflowCapabilities(t *testing.T) {
	rt := &discussionRuntime{}
	if r := rt.actionGetWorkflowCapabilities(CommandContent{Params: map[string]interface{}{}}); r.code != 200 {
		t.Fatalf("caps: %d", r.code)
	}
	if r := rt.actionGetWorkflowCapabilities(CommandContent{Params: discussionParams("x", "1")}); r.code != 400 {
		t.Fatalf("caps with params: %d (expected 400)", r.code)
	}
	rt2 := &discussionRuntime{}
	res := rt2.actionGetWorkflowCapabilities(CommandContent{Params: map[string]interface{}{}})
	data := res.data.(map[string]interface{})
	pv, _ := data["protocol_versions"].([]int)
	if len(pv) != 1 || pv[0] != 2 {
		t.Fatalf("protocol_versions = %v", data["protocol_versions"])
	}
	policies, _ := data["round_policies"].([]string)
	if len(policies) != 1 || policies[0] != RoundPolicyLegacyCapped {
		t.Fatalf("round_policies = %v", data["round_policies"])
	}

	// 引擎装配就绪（P7）→ 宣告 v3。缺任何一项（repo/queries）都必须退回 v2。
	ready := &discussionRuntime{
		interactiveEnabled: true,
		repo:               &DiscussionRepository{},
		queries:            nil,
	}
	readyData := ready.actionGetWorkflowCapabilities(CommandContent{Params: map[string]interface{}{}}).data.(map[string]interface{})
	if pv, _ := readyData["protocol_versions"].([]int); len(pv) != 1 {
		t.Fatalf("engine without queries must report v2 only, got %v", pv)
	}
}

// TestDiscussionSystemHostStatusExposesTimeout 钉住 getSystemHostStatus 回包里的
// timeout_seconds：C 端要靠它算「推荐等多久算可疑」的本地窗口，口径必须与
// discussion_roundtable_setup.go 的 `WithTimeout(timeout+5s)` 一致。
//
// 不下发这个字段时前端只能拍死值：管理端把超时配到 120s 以上，前端就会先于
// 服务端判死并诱导用户「重试」—— 一次多余的完整设备端 LLM 回合。
func TestDiscussionSystemHostStatusExposesTimeout(t *testing.T) {
	// 未配置：仍回 200，timeout_seconds 回 0 由前端按默认值兜底。
	bare := &discussionRuntime{}
	bareRes := bare.actionGetSystemHostStatus(context.Background())
	if bareRes.code != 200 {
		t.Fatalf("unconfigured status: %d", bareRes.code)
	}
	bareData := bareRes.data.(map[string]interface{})
	if got, ok := bareData["timeout_seconds"]; !ok || got != 0 {
		t.Fatalf("unconfigured timeout_seconds = %v (want 0 present)", bareData["timeout_seconds"])
	}
	if bareData["reason"] != "SYSTEM_HOST_NOT_CONFIGURED" {
		t.Fatalf("unconfigured reason = %v", bareData["reason"])
	}

	// 已配置：原样下发管理端配的秒数（含 120 以上的值）。
	for _, seconds := range []int{60, 120, 600} {
		rt := &discussionRuntime{}
		rt.systemHostLoader = func(context.Context) (DiscussionSystemHostConfig, bool, error) {
			return DiscussionSystemHostConfig{
				NodeID: "node-host", DisplayName: "系统主持人", Avatar: "a.png",
				TimeoutSeconds: seconds,
			}, true, nil
		}
		res := rt.actionGetSystemHostStatus(context.Background())
		if res.code != 200 {
			t.Fatalf("status: %d", res.code)
		}
		data := res.data.(map[string]interface{})
		if got := data["timeout_seconds"]; got != seconds {
			t.Fatalf("timeout_seconds = %v (want %d)", got, seconds)
		}
		if data["display_name"] != "系统主持人" || data["reason"] != nil {
			t.Fatalf("status payload changed: %v", data)
		}
	}
}

// TestResolveActorUnknownSender 验证未知发送者报错。
func TestResolveActorUnknownSender(t *testing.T) {
	f := setupDiscussionFixture(t)
	if _, err := f.rt.resolveActor(context.Background(), "no-such-user-xyz"); err == nil {
		t.Fatal("expected error for unknown sender")
	}
	if _, err := f.rt.resolveActor(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty sender")
	}
}

// TestDiscussionSnapshotViewShape 抽查快照序列化的 camelCase 字段。
func TestDiscussionSnapshotViewShape(t *testing.T) {
	v := discussionSnapshotView(DiscussionRecord{
		DiscussionID: "d1", ChatroomID: "c1", CreatedBy: "u1",
		Topic: "t", Goal: "g", Mode: "moderated", Status: "planning",
		Version: 2, RoundPolicy: "legacy_capped", RoleSnapshot: []byte(`{"k":1}`),
	}, nil)
	for _, key := range []string{"discussionId", "chatroomId", "createdBy", "topic", "goal", "stateVersion", "roundPolicy", "roles", "status", "mode"} {
		if _, ok := v[key]; !ok {
			t.Errorf("snapshot missing key %s", key)
		}
	}
	if !strings.Contains(string(mustJSON(v)), `"stateVersion":2`) {
		t.Error("stateVersion not serialized")
	}
}

// TestDiscussionRoomAccessCheck 校验成员资格判定。
func TestDiscussionRoomAccessCheck(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	access := NewDiscussionRoomAccess(f.queries)
	ok, err := access.CheckMember(ctx, f.roomRCID, f.ownerRCID)
	if err != nil || !ok {
		t.Fatalf("owner should have access: %v %v", ok, err)
	}
	ok, _ = access.CheckMember(ctx, f.roomRCID, "ghost-user")
	if ok {
		t.Fatal("ghost should not have access")
	}
	ok, _ = access.CheckMember(ctx, "no-such-room", f.ownerRCID)
	if ok {
		t.Fatal("nonexistent room should not grant access")
	}
}
