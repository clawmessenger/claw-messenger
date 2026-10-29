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

func TestRoleCandidateNodesForAllDeniedReportsPermissionNotOffline(t *testing.T) {
	const owner = "rc-owner"
	svc := newCandidateService([]db.RongcloudNode{
		gateNode("shared-a", "rc-peer", false),
		gateNode("shared-b", "rc-peer", false),
	})
	host := roundtableHostConfig{nodeID: "host-node"}

	// 显式点名这两台：以前它们会进候选、推荐成功、confirm 才炸；上一批挪到候选池
	// 这一层剔空，但报的是 NO_ELIGIBLE_USER_DEVICES（「没有在线设备」）—— 设备明明
	// 在线，缺的是主人的开关，用户被引去反复重启设备。现在空集由权限造成时换一个码。
	got, code := svc.roleCandidateNodesFor(context.Background(), owner, host,
		[]string{"shared-a", "shared-b"}, true)
	if code != "NO_PERMITTED_USER_DEVICES" {
		t.Fatalf("code = %q (candidates=%v), want NO_PERMITTED_USER_DEVICES", code, got)
	}
}

// 「被拦的确实在线、没被拦的只是离线」时，空集的原因是设备而不是权限 ——
// 这时候仍必须报 NO_ELIGIBLE_USER_DEVICES，否则用户会去找一个不存在的开关。
func TestRoleCandidateNodesForDeniedMixWithOfflineKeepsOfflineCode(t *testing.T) {
	const owner = "rc-owner"
	svc := newCandidateService([]db.RongcloudNode{
		gateNode("shared-closed", "rc-peer", false), // 在线但被拦
	})
	host := roundtableHostConfig{nodeID: "host-node"}

	// 点名一台**根本没进 eligible** 的设备（不在 nodeOwner 列表 = 不在线/能力不足）：
	// 交集为空说明与权限无关。
	got, code := svc.roleCandidateNodesFor(context.Background(), owner, host,
		[]string{"offline-node"}, true)
	if code != "NO_ELIGIBLE_USER_DEVICES" {
		t.Fatalf("code = %q (candidates=%v), want NO_ELIGIBLE_USER_DEVICES", code, got)
	}
}

// 首推（不点名设备、由服务端自选）同样要能说出「是权限不是离线」：
// 这条路径正是 S1 提交议题后的第一次推荐，用户遇到空集时最容易被误导。
func TestRoleCandidateNodesForAutoPickAllDeniedReportsPermission(t *testing.T) {
	const owner = "rc-owner"
	svc := newCandidateService([]db.RongcloudNode{
		gateNode("shared-a", "rc-peer", false),
		gateNode("shared-b", "rc-peer", false),
	})
	host := roundtableHostConfig{nodeID: "host-node"}

	got, code := svc.roleCandidateNodesFor(context.Background(), owner, host, nil, false)
	if code != "NO_PERMITTED_USER_DEVICES" {
		t.Fatalf("code = %q (candidates=%v), want NO_PERMITTED_USER_DEVICES", code, got)
	}
}

// 部分被拦、部分可选：候选非空，什么都不该报 —— 权限只影响名单，不该污染成功路径。
func TestRoleCandidateNodesForPartialDeniedStaysSilent(t *testing.T) {
	const owner = "rc-owner"
	svc := newCandidateService([]db.RongcloudNode{
		gateNode("own", owner, true),
		gateNode("shared-closed", "rc-peer", false),
	})
	host := roundtableHostConfig{nodeID: "host-node"}

	got, code := svc.roleCandidateNodesFor(context.Background(), owner, host, nil, false)
	if code != "" {
		t.Fatalf("unexpected code %q", code)
	}
	if len(got) != 1 || got[0] != "own" {
		t.Fatalf("got %v, want [own]", got)
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

// ---- 显示侧标记（/api/claw/nodes 的 discussionJoinBlocked） --------------------

// 显示侧必须是写路径的同一把尺子：只把「别人的 + 关了」标出来。
// 主人豁免要特别钉住 —— 把自己名下的 agent 标黑，等于把用户还能用的东西藏起来。
func TestDiscussionJoinBlockedFlag(t *testing.T) {
	cases := []struct {
		name   string
		node   db.RongcloudNode
		viewer string
		want   bool
	}{
		{"别人的 + 关着 → 标记", gateNode("n1", "rc-peer", false), "rc-owner", true},
		{"别人的 + 开着 → 不标", gateNode("n1", "rc-peer", true), "rc-owner", false},
		{"自己的 + 关着 → 不标（主人豁免）", gateNode("n1", "rc-owner", false), "rc-owner", false},
		{"未认领 → 不标（没有能点开关的人）", gateNode("n1", "", false), "rc-owner", false},
		// 内部调用（workspace 全量视图）没有「我是谁」，一律不打标。
		{"viewer 为空 → 不标", gateNode("n1", "rc-peer", false), "", false},
		{"viewer 只有空白 → 不标", gateNode("n1", "rc-peer", false), "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := discussionJoinBlockedFlag(tc.node, tc.viewer); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// 标记必须与闸门**逐格一致**：任何一个节点上两者不一致，就会出现
// 「列表里看不出来，选的时候被拦下」或者「列表里消失了，其实能进」。
func TestDiscussionJoinBlockedFlagMatchesGate(t *testing.T) {
	const viewer = "rc-owner"
	nodes := []db.RongcloudNode{
		gateNode("own-open", "rc-owner", true),
		gateNode("own-closed", "rc-owner", false),
		gateNode("peer-open", "rc-peer", true),
		gateNode("peer-closed", "rc-peer", false),
		gateNode("unclaimed", "", false),
	}
	for _, node := range nodes {
		allowed, blocked := filterDiscussionJoinNodes(
			map[string]db.RongcloudNode{node.NodeID: node}, viewer)
		_, gatePass := allowed[node.NodeID]
		_, gateDeny := blocked[node.NodeID]
		flag := discussionJoinBlockedFlag(node, viewer)
		// 标记 = 被闸门拦下；被放行的绝不能带标记。
		if flag != gateDeny || flag == gatePass {
			t.Fatalf("%s: flag=%v allowed=%v blocked=%v —— 显示侧与闸门判定不一致",
				node.NodeID, flag, gatePass, gateDeny)
		}
	}
}
