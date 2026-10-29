package rongcloud

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ---- decideAgentJoin：纯判定 --------------------------------------------------

func TestDecideAgentJoinNeverBlocksHumans(t *testing.T) {
	// perms 里没有行的 id 是真人好友（不是 agent 节点）。把真人拦下来是纯回归 ——
	// 这个开关管的是智能体，通讯录里绝大多数条目都是人。
	allowed, rejected := decideAgentJoin([]string{"user-8", "user-9"}, nil, "user-7")
	if len(rejected) != 0 {
		t.Fatalf("rejected = %v, want none", rejected)
	}
	if len(allowed) != 2 || allowed[0] != "user-8" || allowed[1] != "user-9" {
		t.Fatalf("allowed = %v, want the two ids in their original order", allowed)
	}
}

func TestDecideAgentJoinAllowsUnclaimedAgents(t *testing.T) {
	// 未认领 = 没有主人 = 没有可点的开关。按「关」处理会把整机主节点和没走认领
	// 流程的 agent 永久踢出所有群聊，而用户手上没有任何自救入口（与 allows 同口径）。
	perms := map[string]agentJoinPermission{"rc_node_machine1": {claimedBy: "", allowGroup: false}}
	allowed, rejected := decideAgentJoin([]string{"rc_node_machine1"}, perms, "user-7")
	if len(rejected) != 0 {
		t.Fatalf("rejected = %v, want none", rejected)
	}
	if len(allowed) != 1 {
		t.Fatalf("allowed = %v, want the unclaimed node", allowed)
	}
}

func TestDecideAgentJoinLetsTheOwnerPlaceTheirOwnAgent(t *testing.T) {
	// 开关只约束**别的账号**：主人自己把 agent 拉进群，不该被自己关掉的开关挡住。
	// 这条是加入侧与派发侧口径对齐的关键 —— 两边不一致就会出现
	// 「加不进去但派发侧本来会放行」的自相矛盾。
	perms := map[string]agentJoinPermission{"agent-a": {claimedBy: "owner-a", allowGroup: false}}
	allowed, rejected := decideAgentJoin([]string{"agent-a"}, perms, "owner-a")
	if len(rejected) != 0 {
		t.Fatalf("rejected = %v, want none (the inviter is the owner)", rejected)
	}
	if len(allowed) != 1 {
		t.Fatalf("allowed = %v, want the owner's own agent", allowed)
	}
}

func TestDecideAgentJoinBlocksSomeoneElsesAgentWithTheSwitchOff(t *testing.T) {
	params := []struct {
		name   string
		perm   agentJoinPermission
		reason string
	}{
		{
			name:   "别人的 agent 且开关关着",
			perm:   agentJoinPermission{claimedBy: "owner-a", allowGroup: false},
			reason: agentJoinDeniedReason,
		},
		{
			// inviter 为空表示拿不到发起人。缺参数不是放行理由（fail-closed）。
			name:   "拿不到发起人时不豁免",
			perm:   agentJoinPermission{claimedBy: "owner-a", allowGroup: false},
			reason: agentJoinDeniedReason,
		},
	}
	for _, param := range params {
		inviter := "user-7"
		if param.name == "拿不到发起人时不豁免" {
			inviter = ""
		}
		allowed, rejected := decideAgentJoin([]string{"agent-a"}, map[string]agentJoinPermission{"agent-a": param.perm}, inviter)
		if len(allowed) != 0 {
			t.Fatalf("%s: allowed = %v, want none", param.name, allowed)
		}
		if len(rejected) != 1 || rejected[0].UserID != "agent-a" || rejected[0].Reason != param.reason {
			t.Fatalf("%s: rejected = %v", param.name, rejected)
		}
	}
}

func TestDecideAgentJoinAllowsSomeoneElsesAgentWhenTheSwitchIsOn(t *testing.T) {
	perms := map[string]agentJoinPermission{"agent-a": {claimedBy: "owner-a", allowGroup: true}}
	allowed, rejected := decideAgentJoin([]string{"agent-a"}, perms, "user-7")
	if len(rejected) != 0 {
		t.Fatalf("rejected = %v, want none", rejected)
	}
	if len(allowed) != 1 {
		t.Fatalf("allowed = %v, want the agent", allowed)
	}
}

func TestDecideAgentJoinSplitsOneMixedBatchInOrder(t *testing.T) {
	// 真实的勾选是混批的：真人 + 自己的 agent + 别人的 agent。顺序必须原样保留
	// （添加顺序决定成员列表里的先后），被拦的要点名到具体 id。
	perms := map[string]agentJoinPermission{
		"agent-mine":  {claimedBy: "user-7", allowGroup: false},
		"agent-other": {claimedBy: "owner-a", allowGroup: false},
		"agent-open":  {claimedBy: "owner-b", allowGroup: true},
	}
	ids := []string{"user-8", "agent-other", "agent-mine", "agent-open"}
	allowed, rejected := decideAgentJoin(ids, perms, "user-7")

	wantAllowed := []string{"user-8", "agent-mine", "agent-open"}
	if len(allowed) != len(wantAllowed) {
		t.Fatalf("allowed = %v, want %v", allowed, wantAllowed)
	}
	for i := range wantAllowed {
		if allowed[i] != wantAllowed[i] {
			t.Fatalf("allowed = %v, want %v (顺序必须与勾选顺序一致)", allowed, wantAllowed)
		}
	}
	if len(rejected) != 1 || rejected[0].UserID != "agent-other" {
		t.Fatalf("rejected = %v, want only agent-other", rejected)
	}
}

func TestDecideAgentJoinTrimsTheInviterID(t *testing.T) {
	// FromUserID 是协议字段，历史上有过带空白的值。不 Trim 会让主人豁免静默失效，
	// 表现是「我自己拉的自己的 agent 却被拦」。
	perms := map[string]agentJoinPermission{"agent-a": {claimedBy: "owner-a", allowGroup: false}}
	allowed, _ := decideAgentJoin([]string{"agent-a"}, perms, "  owner-a ")
	if len(allowed) != 1 {
		t.Fatal("a padded inviter id must still be recognised as the owner")
	}
}

// ---- 接线：闸门真的在 addMember 里 --------------------------------------------

// joinPermRow 造一条权限行。claimedBy 为空串时留 NULL（Valid=false），
// 与真实数据里「没被认领」的形态一致。
func joinPermRow(rcID, claimedBy string, allowGroup bool) db.ListRongCloudNodePermissionsByRcUserIDsRow {
	row := db.ListRongCloudNodePermissionsByRcUserIDsRow{
		RongcloudUserID: rcID,
		AllowGroup:      allowGroup,
	}
	if claimedBy != "" {
		row.ClaimedByUserID = pgtype.Text{String: claimedBy, Valid: true}
	}
	return row
}

// newJoinGateHandler 装配一个带假权限来源的群命令处理器。
//
// 权限来源注入的是**行**（systemHandler.joinPermissionRows），这样「闸门有没有
// 接进 addMember」和「行 → 判定输入的映射」走的是同一条真实调用链，而不用连真库。
func newJoinGateHandler(t *testing.T, rec *groupTestServer, srv *httptest.Server, mirror *GroupMirror, rows []db.ListRongCloudNodePermissionsByRcUserIDsRow) *systemHandler {
	t.Helper()
	handler := newGroupCommandHandler(t, rec, srv, mirror)
	handler.joinPermissionRows = func(context.Context, []string) ([]db.ListRongCloudNodePermissionsByRcUserIDsRow, error) {
		return rows, nil
	}
	return handler
}

// ---- 行 → 判定输入 ------------------------------------------------------------

func TestJoinPermissionsFromRowsHandlesEveryClaimedShape(t *testing.T) {
	// 认领者这一列在库里可能是 NULL、空串、带空白的串三种形态。三种都要归成
	// 「没有主人」—— 认错一个，主人豁免和「未认领放行」两条分支就会有一条失效。
	rows := []db.ListRongCloudNodePermissionsByRcUserIDsRow{
		joinPermRow("agent-claimed", "owner-a", false),
		joinPermRow("agent-null", "", true),
		{RongcloudUserID: "agent-empty", ClaimedByUserID: pgtype.Text{String: "", Valid: true}},
		{RongcloudUserID: "agent-blank", ClaimedByUserID: pgtype.Text{String: "   ", Valid: true}},
	}

	perms := joinPermissionsFromRows(rows)

	if perm := perms["agent-claimed"]; perm.claimedBy != "owner-a" || perm.allowGroup {
		t.Fatalf("agent-claimed = %#v", perm)
	}
	if perm := perms["agent-null"]; perm.claimedBy != "" {
		t.Fatalf("agent-null claimedBy = %q, want empty", perm.claimedBy)
	}
	if perm, ok := perms["agent-empty"]; !ok || perm.claimedBy != "" {
		t.Fatalf("agent-empty = %#v, want an entry with no owner", perm)
	}
	if perm, ok := perms["agent-blank"]; !ok || perm.claimedBy != "" {
		t.Fatalf("agent-blank = %#v, want the whitespace trimmed to no owner", perm)
	}
}

func TestGroupAddMemberSkipsTheAgentWhoseOwnerSaidNo(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.member("100001", "user-7", "member")
	perms := []db.ListRongCloudNodePermissionsByRcUserIDsRow{
		joinPermRow("agent-blocked", "owner-a", false),
		joinPermRow("agent-open", "owner-b", true),
	}
	handler := newJoinGateHandler(t, rec, srv, stub.mirror(), perms)

	msg := groupCommand("addMember", "user-7", `{"groupId":"100001","userIds":["user-8","agent-blocked","agent-open"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	// 被拦的那个绝不能发给融云：先加再踢会给他推送一遍群消息。
	joins := rec.calls("/group/join.json")
	if len(joins) != 1 {
		t.Fatalf("join calls = %d, want 1", len(joins))
	}
	if joins[0]["userId"] != "user-8,agent-open" {
		t.Errorf("userId = %q, want only the joinable members", joins[0]["userId"])
	}
	if len(stub.added) != 2 {
		t.Errorf("mirror added = %v, want only the joinable members", stub.added)
	}

	data, ok := groupResultData(t, rec).(map[string]interface{})
	if !ok {
		t.Fatalf("data is not an object: %#v", rec.lastCommandResult(t)["data"])
	}
	if data["addedCount"] != float64(2) {
		t.Errorf("addedCount = %#v, want 2", data["addedCount"])
	}
	// 被拦的必须点名，否则用户点开成员列表发现少了一个，既不知道是谁也不知道为什么。
	skipped, ok := data["skipped"].([]interface{})
	if !ok || len(skipped) != 1 {
		t.Fatalf("skipped = %#v, want exactly one entry", data["skipped"])
	}
	entry, ok := skipped[0].(map[string]interface{})
	if !ok || entry["userId"] != "agent-blocked" || entry["reason"] != agentJoinDeniedReason {
		t.Fatalf("skipped[0] = %#v, want {userId: agent-blocked, reason: %s}", skipped[0], agentJoinDeniedReason)
	}
}

func TestGroupAddMemberFailsWholeWhenEveryoneIsBlocked(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.member("100001", "user-7", "member")
	rows := []db.ListRongCloudNodePermissionsByRcUserIDsRow{joinPermRow("agent-blocked", "owner-a", false)}
	handler := newJoinGateHandler(t, rec, srv, stub.mirror(), rows)

	msg := groupCommand("addMember", "user-7", `{"groupId":"100001","userIds":["agent-blocked"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	// 一个人都没加进去，就不能回 200 + addedCount:0 —— 那等于告诉前端「加成功了」。
	rec.onlyReply(t)
	result := rec.lastCommandResult(t)
	if result["status"] != "error" || result["code"] != float64(403) {
		t.Fatalf("expected 403, got %#v", result)
	}
	data, ok := result["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("403 也必须带 data，否则前端无法点名: %#v", result["data"])
	}
	skipped, ok := data["skipped"].([]interface{})
	if !ok || len(skipped) != 1 {
		t.Fatalf("skipped = %#v, want one entry", data["skipped"])
	}
}

func TestGroupAddMemberFailsClosedWhenPermissionLookupBreaks(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.member("100001", "user-7", "member")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())
	handler.joinPermissionRows = func(context.Context, []string) ([]db.ListRongCloudNodePermissionsByRcUserIDsRow, error) {
		return nil, errors.New("permission store down")
	}

	msg := groupCommand("addMember", "user-7", `{"groupId":"100001","userIds":["user-8"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	// 读不到权限位就不知道能不能加：宁可 503 让用户重试，也不静默放行。
	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(503) {
		t.Fatalf("expected 503, got %#v", result)
	}
	if len(stub.added) != 0 {
		t.Errorf("mirror added = %v, want none", stub.added)
	}
}

func TestGroupAddMemberDoesNotConsultTheGateTwiceOnOrdering(t *testing.T) {
	// 回归护栏：判定的入参必须是**用户勾选的原顺序**，且只查一次权限。
	// 查两次（比如先算能不能加、加完再算一遍）会让 cache 外的实现多一次往返，
	// 也让「部分放行」在两次判定之间出现不一致的可能。
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.member("100001", "user-7", "member")
	calls := 0
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())
	handler.joinPermissionRows = func(_ context.Context, ids []string) ([]db.ListRongCloudNodePermissionsByRcUserIDsRow, error) {
		calls++
		if len(ids) != 2 || ids[0] != "user-8" || ids[1] != "user-9" {
			t.Errorf("lookup ids = %v, want the requested order", ids)
		}
		return nil, nil
	}

	msg := groupCommand("addMember", "user-7", `{"groupId":"100001","userIds":["user-8","user-9"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	if calls != 1 {
		t.Fatalf("permission lookups = %d, want exactly 1", calls)
	}
}
