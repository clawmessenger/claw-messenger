package main

import (
	"testing"
)

// claims_test.go 钉住 -claims 的判定核心：**唯一解才写，有歧义不写**。
//
// 归属写错比平铺更糟 —— 一个智能体会出现在别人的「我的智能体」块里，而且错得
// 看不出来。所以这里不连库、不连融云，只测纯函数 resolveClaims：判定逻辑是这套
// 工具唯一需要「想清楚」的地方，I/O 部分只是搬运。

// claimOwners 把判定结果压成 rongcloud_user_id → owner，空串表示「不写」。
func claimOwners(decisions []claimDecision) map[string]string {
	out := make(map[string]string, len(decisions))
	for _, d := range decisions {
		out[d.Node.RCUserID] = d.Owner
	}
	return out
}

// claimVerdicts 把判定结果压成 rongcloud_user_id → verdict。
func claimVerdicts(decisions []claimDecision) map[string]claimVerdict {
	out := make(map[string]claimVerdict, len(decisions))
	for _, d := range decisions {
		out[d.Node.RCUserID] = d.Verdict
	}
	return out
}

// 1. 单节点只有一个 claimant → 写这个 user。
func TestResolveClaimsSingleClaimantWins(t *testing.T) {
	nodes := []nodeRow{{RCUserID: "agent_a", MachineID: "m-1", AiType: "claude"}}
	claimants := map[string]map[string]bool{
		"agent_a": {"u1": true},
	}

	decisions := resolveClaims(nodes, claimants)
	owners := claimOwners(decisions)
	if got := owners["agent_a"]; got != "u1" {
		t.Fatalf("owner of agent_a = %q, want u1", got)
	}
	if v := claimVerdicts(decisions)["agent_a"]; v != claimVerdictSingle {
		t.Fatalf("verdict of agent_a = %q, want %q", v, claimVerdictSingle)
	}
}

// 2. 单节点两个 claimants → 不写（歧义），且这台机器只有这一行时机器级交集
// 也救不了它。
func TestResolveClaimsTwoClaimantsOnSingleNodeIsAmbiguous(t *testing.T) {
	nodes := []nodeRow{{RCUserID: "agent_a", MachineID: "m-1", AiType: "claude"}}
	claimants := map[string]map[string]bool{
		"agent_a": {"u1": true, "u2": true},
	}

	decisions := resolveClaims(nodes, claimants)
	if got := claimOwners(decisions)["agent_a"]; got != "" {
		t.Fatalf("owner of agent_a = %q, want empty (ambiguous must not be written)", got)
	}
	if v := claimVerdicts(decisions)["agent_a"]; v != claimVerdictAmbiguous {
		t.Fatalf("verdict of agent_a = %q, want %q", v, claimVerdictAmbiguous)
	}
}

// 3. 机器级：A 两个 claimants、B 一个 claimants，交集唯一 → 两个都写同一个人。
func TestResolveClaimsMachineIntersectionResolvesBoth(t *testing.T) {
	nodes := []nodeRow{
		{RCUserID: "agent_a", MachineID: "m-1", AiType: "claude"},
		{RCUserID: "agent_b", MachineID: "m-1", AiType: "gpt"},
	}
	claimants := map[string]map[string]bool{
		"agent_a": {"u1": true, "u2": true},
		"agent_b": {"u1": true},
	}

	decisions := resolveClaims(nodes, claimants)
	owners := claimOwners(decisions)
	if owners["agent_a"] != "u1" {
		t.Fatalf("owner of agent_a = %q, want u1 via machine intersection", owners["agent_a"])
	}
	if owners["agent_b"] != "u1" {
		t.Fatalf("owner of agent_b = %q, want u1", owners["agent_b"])
	}
	verdicts := claimVerdicts(decisions)
	if verdicts["agent_a"] != claimVerdictMachine {
		t.Fatalf("verdict of agent_a = %q, want %q", verdicts["agent_a"], claimVerdictMachine)
	}
	// B 第一轮就已经唯一，不能被机器级判定改写档位（值相同，档位保留 single）。
	if verdicts["agent_b"] != claimVerdictSingle {
		t.Fatalf("verdict of agent_b = %q, want %q", verdicts["agent_b"], claimVerdictSingle)
	}
}

// 4. 机器级交集为空（A={u1,u2}、B={u3}）→ 一个都不写。
func TestResolveClaimsEmptyMachineIntersectionWritesNothing(t *testing.T) {
	nodes := []nodeRow{
		{RCUserID: "agent_a", MachineID: "m-1", AiType: "claude"},
		{RCUserID: "agent_b", MachineID: "m-1", AiType: "gpt"},
	}
	claimants := map[string]map[string]bool{
		"agent_a": {"u1": true, "u2": true},
		"agent_b": {"u3": true},
	}

	decisions := resolveClaims(nodes, claimants)
	owners := claimOwners(decisions)
	if owners["agent_a"] != "" {
		t.Fatalf("owner of agent_a = %q, want empty (intersection is empty)", owners["agent_a"])
	}
	// B 自己只有一个 claimant，第一轮就唯一 —— 交集为空不影响它自己的唯一解。
	if owners["agent_b"] != "u3" {
		t.Fatalf("owner of agent_b = %q, want u3 (single claimant)", owners["agent_b"])
	}
	if v := claimVerdicts(decisions)["agent_a"]; v != claimVerdictAmbiguous {
		t.Fatalf("verdict of agent_a = %q, want %q", v, claimVerdictAmbiguous)
	}
}

// 5. 机器级交集两个人（A={u1,u2}、B={u1,u2}）→ 一个都不写。
func TestResolveClaimsTwoWayMachineIntersectionWritesNothing(t *testing.T) {
	nodes := []nodeRow{
		{RCUserID: "agent_a", MachineID: "m-1", AiType: "claude"},
		{RCUserID: "agent_b", MachineID: "m-1", AiType: "gpt"},
	}
	claimants := map[string]map[string]bool{
		"agent_a": {"u1": true, "u2": true},
		"agent_b": {"u1": true, "u2": true},
	}

	decisions := resolveClaims(nodes, claimants)
	for _, d := range decisions {
		if d.Owner != "" {
			t.Fatalf("owner of %s = %q, want empty (two-way ambiguity)", d.Node.RCUserID, d.Owner)
		}
		if d.Verdict != claimVerdictAmbiguous {
			t.Fatalf("verdict of %s = %q, want %q", d.Node.RCUserID, d.Verdict, claimVerdictAmbiguous)
		}
	}
}

// 6. 已有 claimed_by_user_id 的行不参与判定、不被覆盖 —— 即使它是唯一解，
// 也不允许改写别人已经定下的归属。
func TestResolveClaimsExistingClaimIsNeverOverwritten(t *testing.T) {
	nodes := []nodeRow{
		// 已填：好友边说 u1，但列里已经是 u9 —— 必须保留 u9、不产生写入。
		{RCUserID: "agent_a", MachineID: "m-1", AiType: "claude", Claimed: "u9"},
		// 同机器上还有一行待判，且 claimants={u1}：机器级交集会把已填行算成
		// u9 的唯一解？不会 —— 已填行根本不进分组，交集只看待判行。
		{RCUserID: "agent_b", MachineID: "m-1", AiType: "gpt"},
	}
	claimants := map[string]map[string]bool{
		"agent_a": {"u1": true},
		"agent_b": {"u2": true},
	}

	decisions := resolveClaims(nodes, claimants)
	owners := claimOwners(decisions)
	if got := owners["agent_a"]; got != "" {
		t.Fatalf("owner of agent_a = %q, want empty (row already claimed)", got)
	}
	if v := claimVerdicts(decisions)["agent_a"]; v != claimVerdictAlready {
		t.Fatalf("verdict of agent_a = %q, want %q", v, claimVerdictAlready)
	}
	// 待判行只按自己的好友边判定，不被已填行的值污染。
	if got := owners["agent_b"]; got != "u2" {
		t.Fatalf("owner of agent_b = %q, want u2", got)
	}
}

// 7. 没有任何好友边的节点 → no-edge，不写（不算歧义，也不算成功）。
func TestResolveClaimsNoEdgeIsReportedAsNoEdge(t *testing.T) {
	nodes := []nodeRow{{RCUserID: "agent_a", MachineID: "m-1", AiType: "claude"}}
	decisions := resolveClaims(nodes, map[string]map[string]bool{})

	if got := claimOwners(decisions)["agent_a"]; got != "" {
		t.Fatalf("owner of agent_a = %q, want empty", got)
	}
	if v := claimVerdicts(decisions)["agent_a"]; v != claimVerdictNoEdge {
		t.Fatalf("verdict of agent_a = %q, want %q", v, claimVerdictNoEdge)
	}
}

// 8. 隐藏节点（运维 agent / 机器主节点）跳过：即使唯一解也不写，它们不出现在
// 好友列表里。
func TestResolveClaimsSkipsHiddenNodes(t *testing.T) {
	nodes := []nodeRow{
		{RCUserID: "agent_ops", MachineID: "m-1", AiType: "ops"},
		{RCUserID: "rc_node_m-1", MachineID: "m-1", AiType: ""},
	}
	claimants := map[string]map[string]bool{
		"agent_ops":   {"u1": true},
		"rc_node_m-1": {"u1": true},
	}

	decisions := resolveClaims(nodes, claimants)
	for _, d := range decisions {
		if d.Owner != "" {
			t.Fatalf("owner of %s = %q, want empty (hidden node)", d.Node.RCUserID, d.Owner)
		}
		if d.Verdict != claimVerdictHidden {
			t.Fatalf("verdict of %s = %q, want %q", d.Node.RCUserID, d.Verdict, claimVerdictHidden)
		}
	}
}

// 9. 没有 machine_id 的行不能归组，只能靠第一轮唯一解 —— 不进机器级判定。
func TestResolveClaimsNodesWithoutMachineIDAreNotGrouped(t *testing.T) {
	nodes := []nodeRow{
		{RCUserID: "agent_a", MachineID: "", AiType: "claude"},
		{RCUserID: "agent_b", MachineID: "", AiType: "gpt"},
	}
	claimants := map[string]map[string]bool{
		"agent_a": {"u1": true, "u2": true},
		"agent_b": {"u1": true},
	}

	decisions := resolveClaims(nodes, claimants)
	owners := claimOwners(decisions)
	if owners["agent_a"] != "" {
		t.Fatalf("owner of agent_a = %q, want empty (no machine_id, ambiguous)", owners["agent_a"])
	}
	if owners["agent_b"] != "u1" {
		t.Fatalf("owner of agent_b = %q, want u1", owners["agent_b"])
	}
}

// 10. 判定必须与 claimants map 的遍历顺序无关（map 迭代顺序随机，汇总报表要稳定）。
func TestResolveClaimsIsDeterministic(t *testing.T) {
	nodes := []nodeRow{{RCUserID: "agent_a", MachineID: "m-1", AiType: "claude"}}
	claimants := map[string]map[string]bool{
		"agent_a": {"u3": true, "u1": true, "u2": true},
	}
	want := []string{"u1", "u2", "u3"}
	for i := 0; i < 20; i++ {
		got := resolveClaims(nodes, claimants)[0].Claimants
		if len(got) != len(want) {
			t.Fatalf("claimants = %v, want %v", got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("claimants = %v, want %v (must be sorted)", got, want)
			}
		}
	}
}
