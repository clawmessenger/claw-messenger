package rongcloud

// 配置期权限校验（迁移 607）的回归用例。
//
// 派发侧的 fail-closed 只保证「不说话」，用户看到的是「人拉进来了、图标也在，
// 就是不吭声」—— 无从判断是权限还是故障。所以把「聊天室与讨论组权限都关着」
// 的 agent 挡在建组/换人这一步，并给出可操作文案（让主人去设备名片里开）。
//
// 三条口径必须一起钉住，写反任何一条都会让功能不可用：
//   - 已认领 + 两个都关 → 422；
//   - 已认领 + 只开其中一个 → 放行（配置期是并集，见
//     TestChatroomCreate_PermissionGateIsChatroomOrDiscussionUnion）；
//   - 未认领 → **放行**（没有主人就没有能点开关的人）。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type configEnv struct {
	*permEnv
	owner db.RongcloudUser
	h     *systemHandler
}

func newConfigEnv(t *testing.T) *configEnv {
	t.Helper()
	env := newPermEnv(t)
	owner := seedUser(t, env.queries, env.wsID, "perm-owner-"+uniqueID("t"))
	h := &systemHandler{queries: env.queries, nodeID: "rc_test_sys", logger: testLogger()}
	return &configEnv{permEnv: env, owner: owner, h: h}
}

// createRoom 用给定成员节点建一个讨论房间，并登记清理。返回回包与房间的融云 id。
func (c *configEnv) createRoom(t *testing.T, hostNodeID string, members ...string) (commandResult, string) {
	t.Helper()
	roomID := "rc_perm_room_" + uniqueID("t")
	aiMembers := []interface{}{map[string]interface{}{"nodeId": hostNodeID, "roleName": "host"}}
	for i, nodeID := range members {
		aiMembers = append(aiMembers, map[string]interface{}{
			"nodeId": nodeID, "roleName": fmt.Sprintf("member-%d", i),
		})
	}
	result := c.h.actionChatroomCreate(context.Background(), CommandContent{
		Service: "chatroom", Action: "create", WebEnvelope: true,
		Params: map[string]interface{}{
			"chatroomId":   roomID,
			"chatroomName": "perm-room",
			"hostNodeId":   hostNodeID,
			"aiMembers":    aiMembers,
		},
	}, c.owner)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = c.pool.Exec(ctx, `DELETE FROM rongcloud_chatroom_member
			WHERE chatroom_id = (SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id = $1)`, roomID)
		_, _ = c.pool.Exec(ctx, `DELETE FROM rongcloud_chatroom WHERE rongcloud_chatroom_id = $1`, roomID)
	})
	return result, roomID
}

// TestChatroomCreate_RejectsClaimedAgentWithoutDiscussionPermission 钉住配置期
// 422：核心不是状态码，而是**文案必须点名场景与节点**并在里面说「未开启」。
func TestChatroomCreate_RejectsClaimedAgentWithoutDiscussionPermission(t *testing.T) {
	env := newConfigEnv(t)
	host := env.seedAgent(t, "perm-cfg-host-"+uniqueID("t"), "", permFlags{Discussion: true}, "")
	denied := env.seedAgent(t, "perm-cfg-denied-"+uniqueID("t"), "owner-1", permFlags{Discussion: false}, "")
	resetAgentPermissionCache()

	result, _ := env.createRoom(t, host.NodeID, denied.NodeID)
	if result.code != 422 {
		t.Fatalf("code = %d (%s), want 422", result.code, result.message)
	}
	if !strings.Contains(result.message, "未开启") {
		t.Errorf("message must tell the user the switch is off: %q", result.message)
	}
	if !strings.Contains(result.message, denied.NodeID) {
		t.Errorf("message must name the offending node %q: %q", denied.NodeID, result.message)
	}
}

// TestChatroomCreate_AllowsAgentWithAllPermissions 是正控：开关都开着时必须能建
// 房间。没有它，一个「一律 422」的实现也能让上一条全绿。
func TestChatroomCreate_AllowsAgentWithAllPermissions(t *testing.T) {
	env := newConfigEnv(t)
	all := permFlags{Private: true, Group: true, Chatroom: true, Discussion: true, FS: true}
	host := env.seedAgent(t, "perm-cfg-host-ok-"+uniqueID("t"), "", all, "")
	member := env.seedAgent(t, "perm-cfg-member-ok-"+uniqueID("t"), "owner-1", all, "")
	resetAgentPermissionCache()

	result, _ := env.createRoom(t, host.NodeID, member.NodeID)
	if result.code != 200 {
		t.Fatalf("code = %d (%s), want 200", result.code, result.message)
	}
}

// TestChatroomCreate_AllowsUnclaimedAgent 钉住 §7.2 的口径：未认领节点放行。
//
// 写反成「未认领也 422」会让历史节点（以及绑定未完成的中间态）永久锁在讨论组
// 之外，而用户手上没有任何入口能解锁 —— 这是本次唯一一处「宁可放行」的地方，
// 所以必须有正向用例守着。
func TestChatroomCreate_AllowsUnclaimedAgent(t *testing.T) {
	env := newConfigEnv(t)
	host := env.seedAgent(t, "perm-cfg-host-unclaimed-"+uniqueID("t"), "", permFlags{}, "")
	member := env.seedAgent(t, "perm-cfg-member-unclaimed-"+uniqueID("t"), "", permFlags{}, "")
	resetAgentPermissionCache()

	result, _ := env.createRoom(t, host.NodeID, member.NodeID)
	if result.code != 200 {
		t.Fatalf("unclaimed node must pass config-time validation, got %d (%s)", result.code, result.message)
	}
}

// TestChatroomCreate_PermissionGateIsChatroomOrDiscussionUnion 钉住配置期的判据
// 是 **chatroom ∪ discussion 至少开一个**（队长裁定）。
//
// 为什么是并集：加人时无法预知这个房间以后会不会变成讨论组，只看
// allow_discussion 会把「只开了聊天室」的 agent 挡在纯聊天室之外；只看
// allow_chatroom 则会做出「能建不能跑」的讨论组。两个都关才是真的「拉进来也
// 永远不会吭声」，那一种才拒绝。
//
// 三个分支必须都钉住：并集只要一侧还能过，另一侧的缺失就会被掩盖成假绿。
func TestChatroomCreate_PermissionGateIsChatroomOrDiscussionUnion(t *testing.T) {
	cases := []struct {
		name    string
		flags   permFlags
		want    int
		wantMsg string
	}{
		{name: "only-chatroom", flags: permFlags{Chatroom: true, Discussion: false}, want: 200},
		{name: "only-discussion", flags: permFlags{Chatroom: false, Discussion: true}, want: 200},
		{name: "neither", flags: permFlags{}, want: 422, wantMsg: "未开启"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newConfigEnv(t)
			host := env.seedAgent(t, "perm-cfg-host-"+uniqueID("t"), "", permFlags{}, "")
			member := env.seedAgent(t, "perm-cfg-member-"+uniqueID("t"), "owner-1", tc.flags, "")
			resetAgentPermissionCache()

			result, _ := env.createRoom(t, host.NodeID, member.NodeID)
			if result.code != tc.want {
				t.Fatalf("code = %d (%s), want %d", result.code, result.message, tc.want)
			}
			if tc.wantMsg != "" {
				if !strings.Contains(result.message, tc.wantMsg) {
					t.Errorf("message must tell the user the switch is off: %q", result.message)
				}
				if !strings.Contains(result.message, member.NodeID) {
					t.Errorf("message must name the offending node %q: %q", member.NodeID, result.message)
				}
			}
		})
	}
}

// TestSetDiscussionTeam_RejectsClaimedAgentWithoutDiscussionPermission 覆盖第二条
// 配置入口（换人）：房间已经建好之后，把没开权限的 agent 换进来同样要 422。
func TestSetDiscussionTeam_RejectsClaimedAgentWithoutDiscussionPermission(t *testing.T) {
	env := newConfigEnv(t)
	host := env.seedAgent(t, "perm-team-host-"+uniqueID("t"), "", permFlags{Discussion: true}, "")
	allowed := env.seedAgent(t, "perm-team-member-"+uniqueID("t"), "owner-1", permFlags{Discussion: true}, "")
	denied := env.seedAgent(t, "perm-team-denied-"+uniqueID("t"), "owner-1", permFlags{Discussion: false}, "")
	resetAgentPermissionCache()

	result, roomID := env.createRoom(t, host.NodeID, allowed.NodeID)
	if result.code != 200 {
		t.Fatalf("seed room failed: %d (%s)", result.code, result.message)
	}

	result = env.h.actionChatroomSetDiscussionTeam(context.Background(), CommandContent{
		Service: "chatroom", Action: "setDiscussionTeam", WebEnvelope: true,
		Params: map[string]interface{}{
			"chatroomId": roomID,
			"aiMembers": []interface{}{
				map[string]interface{}{"nodeId": denied.NodeID, "roleName": "member-0"},
			},
		},
	}, env.owner)
	if result.code != 422 {
		t.Fatalf("code = %d (%s), want 422", result.code, result.message)
	}
	if !strings.Contains(result.message, "未开启") || !strings.Contains(result.message, denied.NodeID) {
		t.Fatalf("message must name the switch and the node: %q", result.message)
	}
}
