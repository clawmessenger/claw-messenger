package rongcloud

import (
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// eligibleNodes 造一个 node_id -> 节点 的候选集（只有 key 参与判断）。
func eligibleNodes(ids ...string) map[string]db.RongcloudNode {
	out := make(map[string]db.RongcloudNode, len(ids))
	for _, id := range ids {
		out[id] = db.RongcloudNode{NodeID: id}
	}
	return out
}

func TestRoleCandidateNodes(t *testing.T) {
	t.Run("keeps the caller's selection order and drops unowned ids", func(t *testing.T) {
		got, code := roleCandidateNodes(
			eligibleNodes("node_b", "node_c"),
			[]string{"node_c", "node_missing", "node_b"},
			true,
		)
		if code != "" {
			t.Fatalf("unexpected error %q", code)
		}
		// 保留调用方顺序（用户在勾选设备时的顺序），不是 map 的随机序。
		if len(got) != 2 || got[0] != "node_c" || got[1] != "node_b" {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("caps the candidate set at RecommendRolesMaxRoles", func(t *testing.T) {
		requested := []string{"n1", "n2", "n3", "n4", "n5", "n6", "n7", "n8"}
		got, code := roleCandidateNodes(eligibleNodes(requested...), requested, true)
		if code != "" && len(got) != RecommendRolesMaxRoles {
			t.Fatalf("got %d candidates (%q), want %d", len(got), code, RecommendRolesMaxRoles)
		}
		if len(got) != RecommendRolesMaxRoles {
			t.Fatalf("got %d candidates, want %d", len(got), RecommendRolesMaxRoles)
		}
	})

	t.Run("no eligible device among the requested ids", func(t *testing.T) {
		if _, code := roleCandidateNodes(eligibleNodes("node_a"), []string{"node_b"}, true); code != "NO_ELIGIBLE_USER_DEVICES" {
			t.Fatalf("got %q, want NO_ELIGIBLE_USER_DEVICES", code)
		}
	})

	t.Run("without node_ids it falls back to every eligible device, sorted", func(t *testing.T) {
		got, code := roleCandidateNodes(eligibleNodes("node_c", "node_a", "node_b"), nil, false)
		if code != "" {
			t.Fatalf("unexpected error %q", code)
		}
		if len(got) != 3 || got[0] != "node_a" || got[1] != "node_b" || got[2] != "node_c" {
			t.Fatalf("expected a deterministic sorted set, got %#v", got)
		}
	})

	t.Run("empty eligible set is reported, not silently zero roles", func(t *testing.T) {
		if _, code := roleCandidateNodes(eligibleNodes(), nil, false); code != "NO_ELIGIBLE_USER_DEVICES" {
			t.Fatalf("got %q, want NO_ELIGIBLE_USER_DEVICES", code)
		}
	})
}

func TestAssignRoleNodes(t *testing.T) {
	roles := func(names ...string) []map[string]interface{} {
		out := make([]map[string]interface{}, 0, len(names))
		for i, name := range names {
			out = append(out, map[string]interface{}{
				"role_name":      name,
				"role_prompt":    "p",
				"speaking_order": i,
			})
		}
		return out
	}

	t.Run("assigns one distinct device per role", func(t *testing.T) {
		list := roles("主持人", "质疑者", "领域专家")
		assignRoleNodes(list, []string{"node_a", "node_b", "node_c"})
		seen := map[string]bool{}
		for _, role := range list {
			nodeID, _ := role["node_id"].(string)
			if nodeID == "" {
				t.Fatalf("role %v got no device", role["role_name"])
			}
			if seen[nodeID] {
				t.Fatalf("device %q used twice: %#v", nodeID, list)
			}
			seen[nodeID] = true
		}
	})

	t.Run("keeps a device the node claimed itself", func(t *testing.T) {
		list := roles("A", "B")
		list[1]["node_id"] = "node_c"
		assignRoleNodes(list, []string{"node_a", "node_b", "node_c"})
		if list[1]["node_id"] != "node_c" {
			t.Fatalf("the node's own choice must survive: %#v", list[1])
		}
		if list[0]["node_id"] != "node_a" {
			t.Fatalf("the remaining role must take the next free candidate: %#v", list[0])
		}
	})

	t.Run("replaces an out-of-candidate or duplicated device", func(t *testing.T) {
		list := roles("A", "B")
		list[0]["node_id"] = "node_foreign"
		list[1]["node_id"] = "node_foreign"
		assignRoleNodes(list, []string{"node_a", "node_b"})
		if list[0]["node_id"] != "node_a" || list[1]["node_id"] != "node_b" {
			t.Fatalf("stale assignments must be re-assigned: %#v", list)
		}
	})

	t.Run("does nothing without candidates or roles", func(t *testing.T) {
		list := roles("A")
		assignRoleNodes(list, nil)
		if _, ok := list[0]["node_id"]; ok {
			t.Fatalf("no candidate pool means no assignment: %#v", list[0])
		}
		assignRoleNodes(nil, []string{"node_a"})
	})

	t.Run("wraps around when there are more roles than devices", func(t *testing.T) {
		list := roles("A", "B", "C")
		assignRoleNodes(list, []string{"node_a", "node_b"})
		for _, role := range list {
			if nodeID, _ := role["node_id"].(string); nodeID == "" {
				t.Fatalf("every role must still get a device: %#v", list)
			}
		}
	})
}

func TestParamStringList(t *testing.T) {
	cmd := func(value interface{}) CommandContent {
		return CommandContent{Params: map[string]interface{}{"node_ids": value}}
	}

	got, given, ok := paramStringList(cmd([]interface{}{"node_a", " node_b "}), "node_ids", "nodeIds")
	if !ok || !given || len(got) != 2 || got[0] != "node_a" || got[1] != "node_b" {
		t.Fatalf("got %#v given=%v ok=%v", got, given, ok)
	}

	if _, given, ok := paramStringList(CommandContent{Params: map[string]interface{}{}}, "node_ids"); given || !ok {
		t.Fatalf("a missing list is neither given nor malformed (given=%v ok=%v)", given, ok)
	}
	if _, given, ok := paramStringList(cmd(nil), "node_ids"); given || !ok {
		t.Fatalf("an explicit null means 'not given' (given=%v ok=%v)", given, ok)
	}
	for name, value := range map[string]interface{}{
		"empty":        []interface{}{},
		"not a list":   "node_a",
		"blank member": []interface{}{"  "},
		"non-string":   []interface{}{7},
		"too many":     []interface{}{"1", "2", "3", "4", "5", "6", "7", "8", "9"},
	} {
		if _, given, ok := paramStringList(cmd(value), "node_ids"); !given || ok {
			t.Errorf("%s: expected given=true ok=false, got given=%v ok=%v", name, given, ok)
		}
	}
}
