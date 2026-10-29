package main

import (
	"testing"
)

// claims_test.go 钉住 -claims 的判定核心：**唯一解才写，有歧义不写**。
//
// 归属写错比平铺更糟 —— 一个智能体会出现在别人的「我的智能体」块里，而且错得
// 看不出来。所以这里不连库、不连融云，只测纯函数 resolveClaims：判定逻辑是这套
// 工具唯一需要「想清楚」的地方，I/O 部分只是搬运。
//
// 两条最容易搞反的口径，各有用例钉住：
//   - 机器级交集只用**有好友边**的节点投票；完全没有好友边的节点不投票、也不否决，
//     但机器归属唯一时可以被这台机器的证据带上（用例 11）。
//   - 交集为空且该机器有 ≥2 个非空集合 = **冲突**（conflict）：整台机器一行都不写，
//     连第一轮的单节点唯一解也撤销（用例 4）。交集非空就不是冲突（用例 13）。

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

// 4. 同机器证据打架（A={u1,u2}、B={u3}）：交集为空且有两个非空 claimants 集合
// → 冲突。这台机器一行都不写，**连第一轮的单节点唯一解也撤销** —— B 自己只有一个
// claimant，但「同一台设备只有一个主人」意味着 A={u1,u2} 与 B={u3} 不可能都对，
// 写 B=u3 就是在赌哪套证据是真的。
func TestResolveClaimsMachineConflictRevokesSingleDecision(t *testing.T) {
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
		t.Fatalf("owner of agent_a = %q, want empty (machine conflict)", owners["agent_a"])
	}
	if owners["agent_b"] != "" {
		t.Fatalf("owner of agent_b = %q, want empty (machine conflict revokes the single decision)", owners["agent_b"])
	}
	verdicts := claimVerdicts(decisions)
	for _, id := range []string{"agent_a", "agent_b"} {
		if verdicts[id] != claimVerdictConflict {
			t.Fatalf("verdict of %s = %q, want %q", id, verdicts[id], claimVerdictConflict)
		}
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

// 11. 机器级证据补齐「没有好友边」的节点：A={u1}、B={} → 交集 {u1}，B 也能拿到
// u1。没有好友边的节点不投票、也不否决，但机器归属唯一时它可以被这台机器的证据
// 带上 —— 这正是「同一台设备只有一个主人」的用法。
func TestResolveClaimsMachineVoteFillsNoEdgeNode(t *testing.T) {
	nodes := []nodeRow{
		{RCUserID: "agent_a", MachineID: "m-1", AiType: "claude"},
		{RCUserID: "agent_b", MachineID: "m-1", AiType: "gpt"},
	}
	claimants := map[string]map[string]bool{
		"agent_a": {"u1": true},
	}

	decisions := resolveClaims(nodes, claimants)
	owners := claimOwners(decisions)
	if owners["agent_a"] != "u1" {
		t.Fatalf("owner of agent_a = %q, want u1", owners["agent_a"])
	}
	if owners["agent_b"] != "u1" {
		t.Fatalf("owner of agent_b = %q, want u1 via machine evidence (no edge of its own)", owners["agent_b"])
	}
	verdicts := claimVerdicts(decisions)
	if verdicts["agent_a"] != claimVerdictSingle {
		t.Fatalf("verdict of agent_a = %q, want %q", verdicts["agent_a"], claimVerdictSingle)
	}
	if verdicts["agent_b"] != claimVerdictMachine {
		t.Fatalf("verdict of agent_b = %q, want %q", verdicts["agent_b"], claimVerdictMachine)
	}
}

// 12. A={u1}、B={u1,u2} → 交集 {u1}：A 的单解不被撤销，B 靠机器级交集也落 u1。
// 交集非空就说明 B 的第二个候选人不是这台机器的主人。
func TestResolveClaimsMachineIntersectionKeepsSingleAndResolvesOther(t *testing.T) {
	nodes := []nodeRow{
		{RCUserID: "agent_a", MachineID: "m-1", AiType: "claude"},
		{RCUserID: "agent_b", MachineID: "m-1", AiType: "gpt"},
	}
	claimants := map[string]map[string]bool{
		"agent_a": {"u1": true},
		"agent_b": {"u1": true, "u2": true},
	}

	decisions := resolveClaims(nodes, claimants)
	owners := claimOwners(decisions)
	if owners["agent_a"] != "u1" {
		t.Fatalf("owner of agent_a = %q, want u1 (single claimant)", owners["agent_a"])
	}
	if owners["agent_b"] != "u1" {
		t.Fatalf("owner of agent_b = %q, want u1 via machine intersection", owners["agent_b"])
	}
	if v := claimVerdicts(decisions)["agent_b"]; v != claimVerdictMachine {
		t.Fatalf("verdict of agent_b = %q, want %q", v, claimVerdictMachine)
	}
}

// 13. A={u1,u2}、B={u2,u3} → 交集 {u2}：交集非空就不是冲突，两个都写 u2。
// 这条钉住「冲突只认空交集」，别把「两人都是候选人」误判成打架。
func TestResolveClaimsNonEmptyIntersectionIsNotAConflict(t *testing.T) {
	nodes := []nodeRow{
		{RCUserID: "agent_a", MachineID: "m-1", AiType: "claude"},
		{RCUserID: "agent_b", MachineID: "m-1", AiType: "gpt"},
	}
	claimants := map[string]map[string]bool{
		"agent_a": {"u1": true, "u2": true},
		"agent_b": {"u2": true, "u3": true},
	}

	decisions := resolveClaims(nodes, claimants)
	owners := claimOwners(decisions)
	for _, id := range []string{"agent_a", "agent_b"} {
		if owners[id] != "u2" {
			t.Fatalf("owner of %s = %q, want u2", id, owners[id])
		}
	}
	verdicts := claimVerdicts(decisions)
	for _, id := range []string{"agent_a", "agent_b"} {
		if verdicts[id] != claimVerdictMachine {
			t.Fatalf("verdict of %s = %q, want %q", id, verdicts[id], claimVerdictMachine)
		}
	}
}

// 14. -allow-partial 时（有账号反查失败）禁止用机器级证据去补「没有好友边」的
// 节点：它的 claims 集合可能是空的，只是因为投票人缺席。A={u1}、B={} 在全量证据下
// B 会拿 u1，部分反查时 B 必须留在 no-edge。
func TestResolveClaimsAllowPartialNeverGuessesNoEdgeNode(t *testing.T) {
	nodes := []nodeRow{
		{RCUserID: "agent_a", MachineID: "m-1", AiType: "claude"},
		{RCUserID: "agent_b", MachineID: "m-1", AiType: "gpt"},
	}
	claimants := map[string]map[string]bool{
		"agent_a": {"u1": true},
	}

	decisions := resolveClaimsWith(nodes, claimants, claimResolveOptions{AllowPartial: true})
	owners := claimOwners(decisions)
	if owners["agent_a"] != "u1" {
		t.Fatalf("owner of agent_a = %q, want u1 (has its own edge)", owners["agent_a"])
	}
	if owners["agent_b"] != "" {
		t.Fatalf("owner of agent_b = %q, want empty under -allow-partial (no edge of its own)", owners["agent_b"])
	}
	if v := claimVerdicts(decisions)["agent_b"]; v != claimVerdictNoEdge {
		t.Fatalf("verdict of agent_b = %q, want %q", v, claimVerdictNoEdge)
	}
}
