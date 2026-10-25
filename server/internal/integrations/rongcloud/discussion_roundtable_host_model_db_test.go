package rongcloud

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 主持人模型**真的落到库里**的集成测试（task 2026-10-25/task-001）。
//
// 与 `discussion_roundtable_host_model_test.go` 的分工：那份钉的是纯函数口径，
// 这份钉的是「三条链路确实都调用了它」。变异验证（把纯函数里写 model 的那行注
// 释掉）只能证明口径被覆盖；只有真的走一遍 createRoundtable / ensureDiscussion
// 再看落库的 role_snapshot，才能证明**调用点**没被绕过。
//
// 真库用例：连不上就 Skip（与 discussion_roundtable_setup_test.go 同一套约定）。

const hostModelTestRoute = "openai/gpt-5.6-sol"

// withHostModel 把 fixture 的 hostLoader 换成「配置了模型」的版本。
// 其余字段与 setupSetupFixture 里的默认值保持一致。
func withHostModel(f *setupFixture, model string) {
	nodeID := f.hostNode.ID.String()
	f.rt.roundtable.hostLoader = func(context.Context) (DiscussionSystemHostConfig, bool, error) {
		return DiscussionSystemHostConfig{
			NodeID:        nodeID,
			DisplayName:   "主持人",
			Enabled:       true,
			ConfigVersion: "1",
			Model:         model,
		}, true, nil
	}
}

// assertPersistedHostModel 断言三段持久化结果都带上了模型：
// role_snapshot 的 host 条目、host_config_snapshot，以及 v3 引擎读出来的值。
func assertPersistedHostModel(t *testing.T, roleSnapshot, hostSnapshot []byte, hostAIUserID, want string) {
	t.Helper()
	var roles map[string]map[string]interface{}
	if err := json.Unmarshal(roleSnapshot, &roles); err != nil {
		t.Fatalf("role_snapshot 不是 JSON: %v (%s)", err, roleSnapshot)
	}
	host, ok := roles[hostAIUserID]
	if !ok {
		t.Fatalf("role_snapshot 里没有 host 条目: %s", roleSnapshot)
	}
	if host["model"] != want {
		t.Fatalf("持久化的 host model = %#v, want %q", host["model"], want)
	}

	var snapshot map[string]interface{}
	if err := json.Unmarshal(hostSnapshot, &snapshot); err != nil {
		t.Fatalf("host_config_snapshot 不是 JSON: %v (%s)", err, hostSnapshot)
	}
	if snapshot["model"] != want {
		t.Fatalf("host_config_snapshot model = %#v, want %q", snapshot["model"], want)
	}

	// 派发侧真实读取口径：v3 引擎只从角色板取 model，再随 your_turn 下发。
	_, parsedHost, _ := interactiveRoles(DiscussionRecord{RoleSnapshot: roleSnapshot})
	if parsedHost.Model != want {
		t.Fatalf("interactiveRole.Model = %q, want %q", parsedHost.Model, want)
	}
}

// TestPersistedRoleSnapshotCarriesHostModel 走两条真正写库的链路
// （stages 的 ensureDiscussion / actions 的 actionRestartRoundtable），
// 断言 role_snapshot 与 host_config_snapshot 都持久化了 model。
func TestPersistedRoleSnapshotCarriesHostModel(t *testing.T) {
	t.Run("setup：actionConfirmRoundtableSetup", func(t *testing.T) {
		f := setupSetupFixture(t)
		withHostModel(f, hostModelTestRoute)
		ctx := context.Background()

		chatroomID, discussionID := f.createShell(t, "req-"+uniqueID("t"))
		if r := f.rt.actionSubmitSetupTopic(ctx, CommandContent{
			Service: "discussion", Action: "submitSetupTopic",
			Params: map[string]interface{}{"chatroom_id": chatroomID, "topic": "发布计划评审", "goal": "得出结论"},
		}, f.actor); r.code != 200 {
			t.Fatalf("submitSetupTopic: %d %s", r.code, r.message)
		}

		// 直接落一条推荐，绕开 broker 往返（推荐管线不由这份用例覆盖）。
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
			_, _ = f.pool.Exec(context.Background(),
				"DELETE FROM rongcloud_role_recommendation WHERE recommendation_id=$1", recID)
		})

		if r := f.rt.actionConfirmRoundtableSetup(ctx, CommandContent{
			Service: "discussion", Action: "confirmRoundtableSetup",
			Params: map[string]interface{}{
				"chatroom_id":       chatroomID,
				"recommendation_id": recID,
				"roles": []interface{}{map[string]interface{}{
					"role_id": "role_x", "role_name": "分析师", "role_prompt": "分析数据",
					"node_id": f.memberNode.NodeID,
				}},
			},
		}, f.actor); r.code != 200 {
			t.Fatalf("confirmRoundtableSetup: %d %s", r.code, r.message)
		}

		row, err := f.queries.GetRongCloudDiscussion(ctx, discussionID)
		if err != nil {
			t.Fatalf("读讨论行: %v", err)
		}
		assertPersistedHostModel(t, row.RoleSnapshot, row.HostConfigSnapshot, f.hostNode.RongcloudUserID, hostModelTestRoute)
	})

	t.Run("stages：ensureDiscussion", func(t *testing.T) {
		f := setupSetupFixture(t)
		withHostModel(f, hostModelTestRoute)
		ctx := context.Background()

		host, hostErr := f.rt.roundtable.resolveHost(ctx)
		if hostErr != "" {
			t.Fatalf("resolveHost: %s", hostErr)
		}
		if host.model != hostModelTestRoute {
			t.Fatalf("host.model = %q, want %q", host.model, hostModelTestRoute)
		}

		canonical := roundtableCanonical{
			ClientRequestID: "req-" + uniqueID("t"), ChatroomName: "模型落库",
			Topic: "t", Goal: "g", RoundPolicy: RoundPolicyUserConfirmed,
		}
		validated := roundtableValidated{host: host, nodes: map[string]db.RongcloudNode{}}
		chatroomID := "rc_model_" + uniqueID("t")
		discussionID := "disc-" + uniqueID("t")
		if cerr := f.rt.roundtable.ensureChatroom(
			ctx, db.RongcloudRoundtableCreateRequest{}, chatroomID, canonical, validated, f.ownerRCID); cerr != nil {
			t.Fatalf("ensureChatroom: %+v", cerr)
		}
		t.Cleanup(func() {
			ctx := context.Background()
			_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_discussion_event WHERE discussion_id=$1", discussionID)
			_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_discussion WHERE discussion_id=$1", discussionID)
			_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_chatroom_member WHERE chatroom_id=(SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1)", chatroomID)
			_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1", chatroomID)
		})
		if _, cerr := f.rt.roundtable.ensureDiscussion(
			ctx, db.RongcloudRoundtableCreateRequest{}, chatroomID, discussionID,
			canonical, validated, f.ownerRCID, DiscussionStatusPlanning); cerr != nil {
			t.Fatalf("ensureDiscussion: %+v", cerr)
		}

		row, err := f.queries.GetRongCloudDiscussion(ctx, discussionID)
		if err != nil {
			t.Fatalf("读讨论行: %v", err)
		}
		assertPersistedHostModel(t, row.RoleSnapshot, row.HostConfigSnapshot, host.aiUserID, hostModelTestRoute)
	})

	t.Run("actions：actionRestartRoundtable", func(t *testing.T) {
		f := setupSetupFixture(t)
		withHostModel(f, hostModelTestRoute)
		ctx := context.Background()

		chatroomID, shellDiscussionID := f.createShell(t, "req-"+uniqueID("t"))
		// 直建/重开要求房间里没有仍在跑的讨论：把引导期的半成品收尾。
		if _, err := f.pool.Exec(ctx, `
			UPDATE rongcloud_discussion SET status='cancelled', cancel_reason='user_abandoned'
			WHERE discussion_id=$1`, shellDiscussionID); err != nil {
			t.Fatalf("收尾半成品: %v", err)
		}
		room, err := f.queries.GetRongCloudChatroomViewByRongCloudID(ctx, chatroomID)
		if err != nil {
			t.Fatalf("读房间: %v", err)
		}
		// 一个普通 AI 角色：actionRestartRoundtable 从成员表重建角色板。
		if _, err := f.queries.UpsertRongCloudChatroomMemberByMemberID(ctx,
			db.UpsertRongCloudChatroomMemberByMemberIDParams{
				ChatroomID:       room.ID,
				MemberType:       "ai",
				NodeID:           f.memberNode.ID,
				MemberID:         pgtype.Text{String: f.memberNode.RongcloudUserID, Valid: true},
				Nickname:         "分析师",
				RoleName:         pgtype.Text{String: "分析师", Valid: true},
				RoleInstructions: pgtype.Text{String: "分析数据", Valid: true},
				Capabilities:     []byte("[]"),
				Enabled:          true,
				PortraitUri:      DefaultUserPortrait(),
			}); err != nil {
			t.Fatalf("落 AI 成员: %v", err)
		}

		r := f.rt.actionRestartRoundtable(ctx, CommandContent{
			Service: "discussion", Action: "restartRoundtable",
			Params: map[string]interface{}{"chatroom_id": chatroomID, "topic": "模型落库"},
		}, f.actor)
		if r.code != 200 {
			t.Fatalf("actionRestartRoundtable = %d %s", r.code, r.message)
		}
		data, _ := r.data.(map[string]interface{})
		discussionID, _ := data["discussion_id"].(string)
		if discussionID == "" {
			t.Fatalf("回包缺 discussion_id: %#v", data)
		}
		t.Cleanup(func() {
			ctx := context.Background()
			_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_discussion_event WHERE discussion_id=$1", discussionID)
			_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_discussion WHERE discussion_id=$1", discussionID)
		})

		row, err := f.queries.GetRongCloudDiscussion(ctx, discussionID)
		if err != nil {
			t.Fatalf("读讨论行: %v", err)
		}
		host, hostErr := f.rt.roundtable.resolveHost(ctx)
		if hostErr != "" {
			t.Fatalf("resolveHost: %s", hostErr)
		}
		assertPersistedHostModel(t, row.RoleSnapshot, row.HostConfigSnapshot, host.aiUserID, hostModelTestRoute)
	})
}
