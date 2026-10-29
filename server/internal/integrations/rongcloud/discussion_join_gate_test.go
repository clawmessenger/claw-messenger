package rongcloud

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 讨论组加入侧闸门（discussion_join_gate.go）的测试。
//
// 覆盖两层：
//  1. filterDiscussionJoinNodes 的判据（纯函数，不碰库）；
//  2. roleCandidateNodesFor 真的把它接进了候选池 —— 只测纯函数的话，
//     「闸门写了但没接线」这种改动会全绿通过。

func gateNode(nodeID, claimedBy string, allowDiscussion bool) db.RongcloudNode {
	node := db.RongcloudNode{
		NodeID:          nodeID,
		RongcloudUserID: "rc_" + nodeID,
		DeployStatus:    "online",
		// 角色候选要求的三项能力（roundtableRequiredRoleCaps）。
		Capabilities:    []byte(`["discussion_participant","discussion_roundtable","discussion_model_routing"]`),
		AllowDiscussion: allowDiscussion,
	}
	if claimedBy != "" {
		node.ClaimedByUserID = pgtype.Text{String: claimedBy, Valid: true}
	}
	return node
}

func TestFilterDiscussionJoinNodes(t *testing.T) {
	const owner = "rc-owner"
	const stranger = "rc-stranger"

	cases := []struct {
		name   string
		node   db.RongcloudNode
		viewer string
		want   bool
		why    string
	}{
		{
			name:   "未认领放行",
			node:   gateNode("n1", "", false),
			viewer: owner,
			want:   true,
			why:    "没有主人就没有能点开关的人，拦下等于永久锁死历史节点",
		},
		{
			name:   "主人自己的 agent 即使关着也放行",
			node:   gateNode("n2", owner, false),
			viewer: owner,
			want:   true,
			why:    "开关只约束别的账号，与派发侧 actorRCID 主人豁免同源",
		},
		{
			name:   "别人的 agent 关着讨论组权限 → 拦",
			node:   gateNode("n3", stranger, false),
			viewer: owner,
			want:   false,
			why:    "这正是本闸门要挡的那一半：好友分享过来的 agent 不能默认进组",
		},
		{
			name:   "别人的 agent 开着讨论组权限 → 放行",
			node:   gateNode("n4", stranger, true),
			viewer: owner,
			want:   true,
			why:    "授权过就该能用，闸门不是「禁止别人的 agent」",
		},
		{
			name:   "认领者有前后空白仍算主人",
			node:   gateNode("n5", "  "+owner+"  ", false),
			viewer: owner,
			want:   true,
			why:    "claim 列是 TEXT，历史数据可能带空白；不 Trim 会把自己的 agent 判成别人的",
		},
		{
			name:   "viewer 为空不豁免",
			node:   gateNode("n6", owner, false),
			viewer: "",
			want:   false,
			why:    "拿不到发起人时无从判断「就是他本人」，fail-closed（与群聊侧同源）",
		},
		{
			name:   "viewer 为空但开关开着仍放行",
			node:   gateNode("n7", owner, true),
			viewer: "",
			want:   true,
			why:    "豁免只是锦上添花，开关开着的节点任何发起人都能用",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, blocked := filterDiscussionJoinNodes(
				map[string]db.RongcloudNode{tc.node.NodeID: tc.node}, tc.viewer,
			)
			_, inAllowed := allowed[tc.node.NodeID]
			_, inBlocked := blocked[tc.node.NodeID]
			if inAllowed != tc.want || inBlocked == tc.want {
				t.Fatalf("allowed=%v blocked=%v, want allowed=%v (%s)",
					inAllowed, inBlocked, tc.want, tc.why)
			}
			// 两个 map 必须互补：漏一个既不在 allowed 也不在 blocked 的节点，
			// confirm 就会把它当「不可用」报 ROLE_DEVICE_UNAVAILABLE，而不是权限。
			if len(allowed)+len(blocked) != 1 {
				t.Fatalf("allowed=%d blocked=%d, want exactly one side", len(allowed), len(blocked))
			}
		})
	}
}

func TestFilterDiscussionJoinNodesEmptyInput(t *testing.T) {
	allowed, blocked := filterDiscussionJoinNodes(nil, "rc-owner")
	if len(allowed) != 0 || len(blocked) != 0 {
		t.Fatalf("nil input must yield empty maps, got allowed=%d blocked=%d", len(allowed), len(blocked))
	}
	if allowed == nil || blocked == nil {
		t.Fatal("maps must be non-nil: callers index them without a nil check")
	}
}

// ---- 接线：候选池真的过闸 ------------------------------------------------------

// newCandidateService 装配一个只带 nodeOwner 的创建服务。
//
// client 留 nil，nodeIsOnline 会回落 deploy_status（全部 online），这样候选池的
// 差异只可能来自权限闸门，不会混进「融云 presence 查不到」的噪声。
func newCandidateService(nodes []db.RongcloudNode) *roundtableCreationService {
	return &roundtableCreationService{
		nodeOwner: func(context.Context, string) ([]db.RongcloudNode, error) {
			return nodes, nil
		},
	}
}

func TestRoleCandidateNodesForFiltersByDiscussionPermission(t *testing.T) {
	const owner = "rc-owner"
	svc := newCandidateService([]db.RongcloudNode{
		gateNode("own-closed", owner, false),        // 主人自己关着 → 仍可选
		gateNode("shared-open", "rc-peer", true),    // 别人的、开着 → 可选
		gateNode("shared-closed", "rc-peer", false), // 别人的、关着 → 必须消失
		gateNode("unclaimed", "", false),            // 未认领 → 可选
	})
	host := roundtableHostConfig{nodeID: "host-node"}

	got, code := svc.roleCandidateNodesFor(context.Background(), owner, host, nil, false)
	if code != "" {
		t.Fatalf("unexpected code %q", code)
	}
	gotSet := map[string]bool{}
	for _, id := range got {
		gotSet[id] = true
	}
	for _, want := range []string{"own-closed", "shared-open", "unclaimed"} {
		if !gotSet[want] {
			t.Fatalf("candidate %q missing, got %v", want, got)
		}
	}
	if gotSet["shared-closed"] {
		t.Fatalf("permission-denied node must not be a recommendation candidate, got %v", got)
	}
}

func TestRoleCandidateNodesForAllDeniedReportsNoDevice(t *testing.T) {
	const owner = "rc-owner"
	svc := newCandidateService([]db.RongcloudNode{
		gateNode("shared-a", "rc-peer", false),
		gateNode("shared-b", "rc-peer", false),
	})
	host := roundtableHostConfig{nodeID: "host-node"}

	// 显式点名这两台：以前它们会进候选、推荐成功、confirm 才炸；现在在候选池
	// 这一层就为空，前端拿到的是「没有可参与讨论的设备」而不是「请检查…」。
	got, code := svc.roleCandidateNodesFor(context.Background(), owner, host,
		[]string{"shared-a", "shared-b"}, true)
	if code != "NO_ELIGIBLE_USER_DEVICES" {
		t.Fatalf("code = %q (candidates=%v), want NO_ELIGIBLE_USER_DEVICES", code, got)
	}
}

func TestRoleCandidateNodesForKeepsOwnAgentWhenOthersDenied(t *testing.T) {
	const owner = "rc-owner"
	svc := newCandidateService([]db.RongcloudNode{
		gateNode("own", owner, false),
		gateNode("shared", "rc-peer", false),
	})
	host := roundtableHostConfig{nodeID: "host-node"}

	got, code := svc.roleCandidateNodesFor(context.Background(), owner, host,
		[]string{"own", "shared"}, true)
	if code != "" {
		t.Fatalf("unexpected code %q", code)
	}
	if len(got) != 1 || got[0] != "own" {
		t.Fatalf("got %v, want only the caller's own node", got)
	}
}
