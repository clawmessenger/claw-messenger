package rongcloud

import (
	"encoding/json"
	"reflect"
	"testing"
)

// 续推（append）这一轮的候选设备怎么算，是本次修复的核心。
//
// 背景：一轮推荐给 3-5 个角色、每个角色绑一台设备，而用户每轮只采纳 1 个角色。
// 若续推时无条件按**整行历史所有批次**的角色设备排除，上一批没被采纳的 4 台设备
// 会被永久排除，几轮之后候选清零，用户看到「没有可用于讨论的在线设备」而设备都在线。
//
// 修法：前端显式给了 node_ids（它传的就是「剩余未参与」的准确集合）时不再做历史
// 排除；只有没给时才保留整行排除兜底（那条路径候选由服务端自己挑，不排除可能挑到
// 已被占用的设备，confirm 会以 ROLE_DEVICE_DUPLICATED 整单拒绝）。
func TestAppendCandidateNodes(t *testing.T) {
	roles := func(nodeIDs ...string) []byte {
		type role struct {
			NodeID string `json:"node_id"`
		}
		out := make([]role, 0, len(nodeIDs))
		for _, nodeID := range nodeIDs {
			out = append(out, role{NodeID: nodeID})
		}
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal roles: %v", err)
		}
		return raw
	}

	// 上一批推荐了 5 个角色 → 5 台设备被「历史占用」，用户只采纳了 node-a。
	historyRoles := roles("node-a", "node-b", "node-c", "node-d", "node-e")

	t.Run("node_ids 已给出时不排除历史角色设备", func(t *testing.T) {
		// 前端算出的「剩余未参与」集合：上一批未采纳的 node-b..e 都还在里面，
		// 加上本批新出现的 node-f。这些必须原样全部保留。
		candidates := []string{"node-b", "node-c", "node-d", "node-e", "node-f"}
		got := appendCandidateNodes(candidates, historyRoles, true)
		if !reflect.DeepEqual(got, candidates) {
			t.Fatalf("给了 node_ids 时不应排除历史角色设备：got %v want %v", got, candidates)
		}
	})

	t.Run("node_ids 给出但只剩历史设备也照样保留", func(t *testing.T) {
		// 极端形态：唯一剩下的在线设备都出现在历史角色里，也要能推出去 ——
		// 否则这就是用户看到「没有可用设备」的那条死路。
		candidates := []string{"node-b"}
		got := appendCandidateNodes(candidates, historyRoles, true)
		if !reflect.DeepEqual(got, []string{"node-b"}) {
			t.Fatalf("got %v want [node-b]", got)
		}
	})

	t.Run("未给 node_ids 时仍排除历史角色设备", func(t *testing.T) {
		candidates := []string{"node-a", "node-b", "node-f", "node-g"}
		got := appendCandidateNodes(candidates, historyRoles, false)
		want := []string{"node-f", "node-g"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("没给 node_ids 时必须沿用整行排除兜底：got %v want %v", got, want)
		}
	})

	t.Run("未给 node_ids 且全部被占用时返回空集", func(t *testing.T) {
		// 空集是调用方 fail("NO_ELIGIBLE_USER_DEVICES") 的触发条件，必须如实返回。
		got := appendCandidateNodes([]string{"node-a", "node-b"}, historyRoles, false)
		if len(got) != 0 {
			t.Fatalf("全部被占用时应返回空集：got %v", got)
		}
	})

	t.Run("角色行没有 node_id 时两种路径都原样返回", func(t *testing.T) {
		empty := roles()
		candidates := []string{"node-a", "node-b"}
		for _, trust := range []bool{true, false} {
			got := appendCandidateNodes(candidates, empty, trust)
			if !reflect.DeepEqual(got, candidates) {
				t.Fatalf("trustNodeIDs=%v：got %v want %v", trust, got, candidates)
			}
		}
	})

	t.Run("角色行非法 JSON 时不影响候选", func(t *testing.T) {
		candidates := []string{"node-a", "node-b"}
		got := appendCandidateNodes(candidates, []byte("not-json"), false)
		if !reflect.DeepEqual(got, candidates) {
			t.Fatalf("非法 JSON 应退化成「没有历史占用」：got %v want %v", got, candidates)
		}
	})
}
