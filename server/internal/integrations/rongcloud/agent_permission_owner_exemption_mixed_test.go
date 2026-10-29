package rongcloud

// 主人豁免的**混合场景**回归：一个群里有 N 个 agent、分属 N 个不同主人。
//
// 为什么单独一条：其余主人豁免用例都是「一个 agent + 一个主人」的对照，
// 只能证明「主人放行 / 别人拒绝」这条单点判据。真实的群是多人共处的 ——
// 豁免必须是**按 agent 各自的认领者**判，而不是「本轮发起人是谁 → 整个群放行」。
// 后者是一种很容易写出来的错误实现（在 filter 外面判一次 actor），单 agent 用例
// 发现不了，因为它和正确实现在单 agent 下等价。
//
// 本用例用 3 个不同主人的 agent 同场：A 发起时只有 A 的 agent 留下，B/C 的仍被
// 各自主人关掉的开关拦住。

import (
	"context"
	"testing"
)

func TestFilterMembersByPermission_MixedOwnersOnlyActorOwnNodeKept(t *testing.T) {
	env := newPermEnv(t)
	const (
		ownerA  = "rc_owner_a"
		ownerB  = "rc_owner_b"
		ownerC  = "rc_owner_c"
		strange = "rc_stranger"
	)
	// 三个 agent 的群聊开关全关（各自主人关掉的），认领者互不相同。
	nodeA := env.seedAgent(t, "perm-mix-a-"+uniqueID("t"), ownerA, permFlags{Group: false}, "")
	nodeB := env.seedAgent(t, "perm-mix-b-"+uniqueID("t"), ownerB, permFlags{Group: false}, "")
	nodeC := env.seedAgent(t, "perm-mix-c-"+uniqueID("t"), ownerC, permFlags{Group: false}, "")
	resetAgentPermissionCache()

	rt := permRelayRuntime(env.queries, testLogger())
	aiMembers := []GroupMember{
		{UserID: nodeA.RongcloudUserID},
		{UserID: nodeB.RongcloudUserID},
		{UserID: nodeC.RongcloudUserID},
	}
	members := relayMembersFromGroup(aiMembers)
	perms := rt.agentPermissions(context.Background(), relayMemberIDs(members))

	cases := []struct {
		name     string
		actor    string
		wantRCID string // "" 表示应当一个都不剩
	}{
		{name: "A 发起：只留 A 的 agent", actor: ownerA, wantRCID: nodeA.RongcloudUserID},
		{name: "B 发起：只留 B 的 agent", actor: ownerB, wantRCID: nodeB.RongcloudUserID},
		{name: "C 发起：只留 C 的 agent", actor: ownerC, wantRCID: nodeC.RongcloudUserID},
		{name: "陌生人发起：一个都不留", actor: strange, wantRCID: ""},
		{name: "拿不到发起人：一个都不留", actor: "", wantRCID: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kept := filterMembersByPermission(members, perms, sceneGroup, tc.actor)
			if tc.wantRCID == "" {
				if len(kept) != 0 {
					t.Fatalf("kept = %+v, want empty（actor %q 不是任何人的主人）", kept, tc.actor)
				}
				return
			}
			if len(kept) != 1 || kept[0].rcID != tc.wantRCID {
				t.Fatalf("kept = %+v, want exactly %q（豁免必须按各 agent 自己的认领者判，不能整群放行）",
					kept, tc.wantRCID)
			}
		})
	}
}

// TestGroupRelayPermissionGate_MixedOwners 是上面那条的**入口级**对照：
// 走完整的 StartGroupRelay（成员校验 → 权限过滤 → 计数），证明接线后行为一致。
//
// 每次都用新 hub：同一个 hub 第二次调用会因为「上一条中继还在跑」走排队分支返回
// 200，让陌生人那条的 409 断言假绿。
func TestGroupRelayPermissionGate_MixedOwners(t *testing.T) {
	env := newPermEnv(t)
	const (
		ownerA  = "rc_owner_a2"
		ownerB  = "rc_owner_b2"
		strange = "rc_stranger2"
	)
	nodeA := env.seedAgent(t, "perm-mix2-a-"+uniqueID("t"), ownerA, permFlags{Group: false}, "")
	nodeB := env.seedAgent(t, "perm-mix2-b-"+uniqueID("t"), ownerB, permFlags{Group: false}, "")
	resetAgentPermissionCache()

	aiMembers := []GroupMember{{UserID: nodeA.RongcloudUserID}, {UserID: nodeB.RongcloudUserID}}
	allMembers := append([]GroupMember{
		{UserID: ownerA}, {UserID: ownerB}, {UserID: strange},
	}, aiMembers...)

	t.Run("A 发起：只剩 A 的 agent", func(t *testing.T) {
		hub := newPermHub(permRelayRuntime(env.queries, testLogger()))
		result := hub.StartGroupRelay(context.Background(), "g-mix-a-"+uniqueID("t"),
			ownerA, "大家好", allMembers, aiMembers)
		if result.code != 200 {
			t.Fatalf("code = %d (%s), want 200", result.code, result.message)
		}
		data, _ := result.data.(map[string]interface{})
		if speakers, _ := data["speakers"].(int); speakers != 1 {
			t.Fatalf("speakers = %v, want 1（只有 A 的 agent 被豁免）", data["speakers"])
		}
	})

	t.Run("陌生人发起：全部被拦 → 409", func(t *testing.T) {
		hub := newPermHub(permRelayRuntime(env.queries, testLogger()))
		result := hub.StartGroupRelay(context.Background(), "g-mix-s-"+uniqueID("t"),
			strange, "大家好", allMembers, aiMembers)
		if result.code != 409 {
			t.Fatalf("code = %d (%s), want 409（陌生人不能借豁免放行）", result.code, result.message)
		}
	})
}
