package rongcloud

// group_create_join_gate_test.go 钉住「建群也要过加入侧闸门」。
//
// 背景：加入侧闸门（迁移 607 的 allow_group）最初只落在 addMember 上，而
// handleGroupCreate 把 cmd.Params.memberIds 直接塞进 /group/create.json —— 于是
// 「新建群聊」成了绕过口：勾上别人关了群聊权限的 agent，照样被拉进群，只是进去
// 以后永远不吭声。只在候选名单上过滤等于把唯一防线放在前端，刷新缓存或换小程序
// 就破了，所以闸门必须落在真正写数据的地方（这里）。
//
// 三条硬契约（与 addMember 的差别只有第一条）：
//   1. **不因为全员被拦就拒绝建群** —— 用户要的是一个群，不是一次权限报错；
//      群照建，被拦的成员不写入，明细随成功回包点名（skipped 形状与 addMember 一致）。
//   2. 主人豁免照旧：我自己关掉开关的 agent，我自己建群时仍然拉得进来。
//   3. 权限查询失败回 503 且**不建群**（写操作 fail-closed）—— 与好友列表的
//      fail-open 刻意不同，那边只是少显示一个人。

import (
	"context"
	"errors"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// createdUserIDs 取 /group/create.json 收到的 userId 列表（逗号串拆开）。
func createdUserIDs(t *testing.T, rec *groupTestServer) []string {
	t.Helper()
	creates := rec.calls("/group/create.json")
	if len(creates) != 1 {
		t.Fatalf("create calls = %d, want 1", len(creates))
	}
	raw := creates[0]["userId"]
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func skippedEntries(t *testing.T, rec *groupTestServer) []interface{} {
	t.Helper()
	data, ok := groupResultData(t, rec).(map[string]interface{})
	if !ok {
		raw := rec.lastCommandResult(t)["data"]
		t.Fatalf("data is not an object: %#v", raw)
	}
	skipped, ok := data["skipped"].([]interface{})
	if !ok {
		return nil
	}
	return skipped
}

// TestGroupCreateSkipsTheAgentWhoseOwnerSaidNo 是这条绕过口的正面用例：
// 别人关掉群聊权限的 agent 既不能进 createGroup 的 userIDs，也不能进本地镜像，
// 但**必须**在 skipped 里被点名。
func TestGroupCreateSkipsTheAgentWhoseOwnerSaidNo(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	rows := []db.ListRongCloudNodePermissionsByRcUserIDsRow{
		joinPermRow("agent-blocked", "owner-a", false),
		joinPermRow("agent-open", "owner-b", true),
		// 我自己名下的 agent，我自己关了开关 —— 主人豁免，照常进。
		joinPermRow("agent-mine", "user-7", false),
	}
	handler := newJoinGateHandler(t, rec, srv, stub.mirror(), rows)

	msg := groupCommand("create", "user-7", `{"groupName":"产品讨论","memberIds":[
		"user-8","agent-blocked","agent-open","agent-mine"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	// 群照建：不能因为有一个成员被拦就整单失败。
	got := createdUserIDs(t, rec)
	want := []string{"user-7", "user-8", "agent-open", "agent-mine"}
	if len(got) != len(want) {
		t.Fatalf("create userIds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("create userIds = %v, want %v (顺序 = 勾选顺序，群主在最前)", got, want)
		}
	}

	// 本地镜像必须与融云侧一致：镜像多一个人，管理端就会显示一个其实没进群的人。
	if len(stub.saved) != 1 {
		t.Fatalf("mirror saved %d groups, want 1", len(stub.saved))
	}
	if len(stub.saved[0].MemberIDs) != len(want) {
		t.Errorf("mirror members = %v, want %v", stub.saved[0].MemberIDs, want)
	}

	skipped := skippedEntries(t, rec)
	if len(skipped) != 1 {
		t.Fatalf("skipped = %#v, want exactly one entry", skipped)
	}
	entry, ok := skipped[0].(map[string]interface{})
	if !ok || entry["userId"] != "agent-blocked" || entry["reason"] != agentJoinDeniedReason {
		t.Fatalf("skipped[0] = %#v, want {userId: agent-blocked, reason: %s}", skipped[0], agentJoinDeniedReason)
	}
}

// TestGroupCreateStillCreatesWhenEveryMemberIsBlocked：全员被拦也要把群建出来，
// 成员只剩群主，明细照旧点名。
//
// 反过来做（403 + 一个群都不建）看似「更安全」，实际是把权限问题变成了功能不可
// 用：用户勾了三个 agent，得到一句「创建失败」，既不知道删掉谁，也没有任何入口
// 能改别人的开关。
func TestGroupCreateStillCreatesWhenEveryMemberIsBlocked(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	rows := []db.ListRongCloudNodePermissionsByRcUserIDsRow{
		joinPermRow("agent-blocked", "owner-a", false),
		joinPermRow("agent-blocked-2", "owner-c", false),
	}
	handler := newJoinGateHandler(t, rec, srv, stub.mirror(), rows)

	msg := groupCommand("create", "user-7", `{"groupName":"只剩我","memberIds":["agent-blocked","agent-blocked-2"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	got := createdUserIDs(t, rec)
	if len(got) != 1 || got[0] != "user-7" {
		t.Fatalf("create userIds = %v, want only the owner", got)
	}
	result := rec.lastCommandResult(t)
	if result["status"] == "error" {
		t.Fatalf("group create must not fail when everyone is blocked: %#v", result)
	}
	if skipped := skippedEntries(t, rec); len(skipped) != 2 {
		t.Fatalf("skipped = %#v, want both blocked members named", skipped)
	}
}

// TestGroupCreateFailsClosedWhenPermissionLookupBreaks：写操作 fail-closed。
// 不建融云群、不写镜像、回 503，让用户重试。
func TestGroupCreateFailsClosedWhenPermissionLookupBreaks(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())
	handler.joinPermissionRows = func(context.Context, []string) ([]db.ListRongCloudNodePermissionsByRcUserIDsRow, error) {
		return nil, errors.New("permission store down")
	}

	msg := groupCommand("create", "user-7", `{"groupName":"抖动","memberIds":["user-8","agent-x"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(503) {
		t.Fatalf("expected 503, got %#v", result)
	}
	if len(rec.calls("/group/create.json")) != 0 {
		t.Error("a permission lookup failure must not create the RongCloud group")
	}
	if len(stub.saved) != 0 {
		t.Errorf("mirror saved = %v, want none", stub.saved)
	}
}

// TestGroupCreateLetsHumansAndUnclaimedNodesThrough：闸门只管「别人名下的
// agent」。真人没有节点行、未认领节点没有主人（也就没有可点的开关），两类都必须
// 照常进群。
//
// 未认领单独列一条是因为它最容易被写成「allow_group=false 就拦」：NOT NULL
// DEFAULT false 意味着一个从没被认领过的整机主节点，这一列读出来就是 false，
// 按值拦会把整机智能体永久踢出所有新群，而用户手上没有任何自救入口。
func TestGroupCreateLetsHumansAndUnclaimedNodesThrough(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	rows := []db.ListRongCloudNodePermissionsByRcUserIDsRow{
		// 未认领：claimed_by_user_id 为 NULL，allow_group 取列默认值 false。
		joinPermRow("rc_node_machine1", "", false),
		joinPermRow("agent-blocked", "owner-a", false),
	}
	handler := newJoinGateHandler(t, rec, srv, stub.mirror(), rows)

	msg := groupCommand("create", "user-7",
		`{"groupName":"混合名单","memberIds":["user-8","rc_node_machine1","agent-blocked"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	got := createdUserIDs(t, rec)
	want := []string{"user-7", "user-8", "rc_node_machine1"}
	if len(got) != len(want) {
		t.Fatalf("create userIds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("create userIds = %v, want %v", got, want)
		}
	}
	skipped := skippedEntries(t, rec)
	if len(skipped) != 1 {
		t.Fatalf("skipped = %#v, want only the claimed-and-disabled agent", skipped)
	}
	if entry, ok := skipped[0].(map[string]interface{}); !ok || entry["userId"] != "agent-blocked" {
		t.Fatalf("skipped[0] = %#v, want agent-blocked only", skipped[0])
	}
}

// TestGroupCreateKeepsTheOwnerEvenIfTheGateWouldFilterIt：群主被判定拦下时仍要
// 留在成员里（融云建群要求群主是成员），并且不能再出现在 skipped 里 —— 一边
// 把人塞进去、一边告诉前端「这个人没进去」是自相矛盾的。
//
// 现实中「群主自己是别人名下的 agent」不成立，这条钉的是兜底路径本身。
func TestGroupCreateKeepsTheOwnerEvenIfTheGateWouldFilterIt(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	rows := []db.ListRongCloudNodePermissionsByRcUserIDsRow{
		joinPermRow("user-7", "owner-a", false),
	}
	handler := newJoinGateHandler(t, rec, srv, stub.mirror(), rows)

	msg := groupCommand("create", "user-7", `{"groupName":"兜底","memberIds":[]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	got := createdUserIDs(t, rec)
	if len(got) != 1 || got[0] != "user-7" {
		t.Fatalf("create userIds = %v, want the owner force-kept", got)
	}
	if skipped := skippedEntries(t, rec); len(skipped) != 0 {
		t.Fatalf("skipped = %#v, want none — the owner is in the group, so it must not be reported as skipped", skipped)
	}
}

// TestGroupCreateDoesNotGateTwice 回归护栏：一次建群只查一次权限（入参是
// 组装完成、去重后的最终名单，顺序群主最前）。
func TestGroupCreateDoesNotGateTwice(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	calls := 0
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())
	handler.joinPermissionRows = func(_ context.Context, ids []string) ([]db.ListRongCloudNodePermissionsByRcUserIDsRow, error) {
		calls++
		if len(ids) != 3 || ids[0] != "user-7" || ids[1] != "user-8" || ids[2] != "user-9" {
			t.Errorf("lookup ids = %v, want the deduped final list with the owner first", ids)
		}
		return nil, nil
	}

	msg := groupCommand("create", "user-7", `{"groupName":"x","memberIds":["user-8","user-7","user-9"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	if calls != 1 {
		t.Fatalf("permission lookups = %d, want exactly 1", calls)
	}
}
