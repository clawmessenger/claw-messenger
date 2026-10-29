package rongcloud

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// confirm 侧的权限闸门（discussion_join_gate.go 接到 actionConfirmRoundtableSetup）。
//
// 为什么要 DB 用例：纯函数绿只证明「判据对」，证明不了「confirm 真的按它拦、
// 而且拦的时候报的是权限错误码而不是笼统的『设备不可用』」。这两件事坏掉的
// 表现完全不同 —— 前者是越权，后者是用户被引去查设备。
//
// 库不可达时依赖 setupSetupFixture 的 Skip（与同目录其余 DB 用例同一约定）。

// confirmWithNode 走完 shell → topic → confirm，角色绑在给定节点上。
//
// 返回 confirm 的 commandResult 与讨论行 id（用于断言成员表）。
func (f *setupFixture) confirmWithNode(t *testing.T, node db.RongcloudNode) (commandResult, string) {
	t.Helper()
	ctx := context.Background()
	chatroomID, discussionID := f.createShell(t, "req-"+uniqueID("t"))
	if r := f.rt.actionSubmitSetupTopic(ctx, CommandContent{
		Service: "discussion", Action: "submitSetupTopic",
		Params: map[string]interface{}{"chatroom_id": chatroomID, "topic": "权限评审", "goal": "得出结论"},
	}, f.actor); r.code != 200 {
		t.Fatalf("submitSetupTopic: %d %s", r.code, r.message)
	}

	// 覆盖 fixture 的 nodeOwner：它捕获的是 setup 时那个 memberNode 副本，
	// 本用例要换一台「别人的、权限关着」的节点。
	f.rt.roundtable.nodeOwner = func(context.Context, string) ([]db.RongcloudNode, error) {
		return []db.RongcloudNode{node}, nil
	}

	rolesJSON, _ := json.Marshal([]map[string]interface{}{{
		"role_id": "role_x", "role_name": "分析师", "role_prompt": "分析数据",
		"node_id": node.NodeID,
	}})
	recID := "rec_" + uniqueID("t")
	if _, err := f.queries.InsertRongCloudRoleRecommendation(ctx, db.InsertRongCloudRoleRecommendationParams{
		RecommendationID: recID,
		RequestID:        "c2c_" + recID,
		UserID:           f.ownerRCID,
		HostNodeID:       f.hostNode.NodeID,
		HostAiUserID:     f.hostNode.RongcloudUserID,
		ConfigVersion:    "1",
		Topic:            "权限评审",
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

	result := f.rt.actionConfirmRoundtableSetup(ctx, CommandContent{
		Service: "discussion", Action: "confirmRoundtableSetup",
		Params: map[string]interface{}{
			"chatroom_id":       chatroomID,
			"recommendation_id": recID,
			"roles": []interface{}{map[string]interface{}{
				"role_id": "role_x", "role_name": "分析师", "role_prompt": "分析数据",
				"node_id": node.NodeID,
			}},
		},
	}, f.actor)
	return result, discussionID
}

func (f *setupFixture) aiMemberCount(t *testing.T, discussionID string) int {
	t.Helper()
	ctx := context.Background()
	row, err := f.queries.GetRongCloudDiscussion(ctx, discussionID)
	if err != nil {
		t.Fatalf("读讨论行: %v", err)
	}
	room, err := f.queries.GetRongCloudChatroomViewByRongCloudID(ctx, row.ChatroomID)
	if err != nil {
		t.Fatalf("读房间: %v", err)
	}
	members, err := f.queries.ListRongCloudChatroomMembersWithNode(ctx, room.ID)
	if err != nil {
		t.Fatalf("读成员: %v", err)
	}
	count := 0
	for _, m := range members {
		if m.MemberType == "ai" {
			count++
		}
	}
	return count
}

func TestSetupConfirmRejectsAgentWhoseOwnerClosedDiscussion(t *testing.T) {
	f := setupSetupFixture(t)
	node := f.memberNode
	node.ClaimedByUserID = pgtype.Text{String: "rc-someone-else", Valid: true}
	node.AllowDiscussion = false

	result, discussionID := f.confirmWithNode(t, node)
	if result.code != 400 {
		t.Fatalf("code = %d (%s), want 400", result.code, result.message)
	}
	if result.message != "ROLE_DEVICE_PERMISSION_DENIED" {
		// 报 ROLE_DEVICE_UNAVAILABLE 就等于把用户引去「检查设备是否在线」，
		// 而真正要做的是让主人在权限面板里打开讨论组开关。
		t.Fatalf("message = %q, want ROLE_DEVICE_PERMISSION_DENIED", result.message)
	}
	data, _ := result.data.(map[string]interface{})
	if idx, ok := data["role_index"].(int); !ok || idx != 0 {
		t.Fatalf("data.role_index = %#v, want 0（前端靠它定位是哪一张角色卡）", data["role_index"])
	}
	// 被拒时不能留下半套成员（host 之外不该有 AI 成员）。
	if n := f.aiMemberCount(t, discussionID); n != 1 {
		t.Fatalf("AI 成员数 = %d, want 1（只有 host），被拒的角色不得落成员表", n)
	}
}

func TestSetupConfirmAllowsOwnerOwnAgentEvenWhenSwitchOff(t *testing.T) {
	f := setupSetupFixture(t)
	node := f.memberNode
	// 认领者**就是**发起人，且开关关着：主人自己建的讨论组不受自己的开关限制
	// （与派发侧 actorRCID 主人豁免同源）。
	node.ClaimedByUserID = pgtype.Text{String: f.ownerRCID, Valid: true}
	node.AllowDiscussion = false

	result, _ := f.confirmWithNode(t, node)
	if result.code != 200 {
		t.Fatalf("code = %d (%s), want 200：主人豁免不该被闸门误伤", result.code, result.message)
	}
}

func TestSetupConfirmAllowsSharedAgentWithPermission(t *testing.T) {
	f := setupSetupFixture(t)
	node := f.memberNode
	node.ClaimedByUserID = pgtype.Text{String: "rc-someone-else", Valid: true}
	node.AllowDiscussion = true

	result, _ := f.confirmWithNode(t, node)
	if result.code != 200 {
		t.Fatalf("code = %d (%s), want 200：授权过的 agent 必须能用", result.code, result.message)
	}
}
