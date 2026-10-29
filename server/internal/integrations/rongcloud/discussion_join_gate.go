package rongcloud

import (
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 讨论组的「加入侧」权限闸门（allow_discussion）。
//
// 为什么需要它：讨论组的角色候选与确认落成员，一直只判「属于本人 + 在线 + 能力
// 齐备 + 不是 host」。而**「属于本人」是按融云好友边算的**（friendEdgeNodeOwner），
// 好友边里既有自己的 agent，也有别人分享过来的 agent —— 后者只要还在好友列表里，
// 就能被推荐、被确认、被拉进讨论组。进去之后派发侧的 fail-closed 让它一句话不说，
// 用户看到的是「这个智能体明明在组里，就是不吭声」，既没解释也没有可点的入口。
//
// 这正是群聊 addMember 那个洞的另一半（group_join_gate.go）。两处口径必须逐条
// 同源，否则会出现「群里拉不进来、讨论组里拉得进来」这种更难解释的组合。
//
// 判据：
//   - 未认领（claimed_by_user_id 为空）→ 放行。没有主人就没有能点开关的人，
//     拦下来等于把历史节点永久锁在讨论组之外，而用户手上没有任何可操作的入口。
//   - 认领者就是发起人 → 放行。开关约束的是**别的账号**：主人自己建的讨论组，
//     即使他自己的开关关着，agent 照样能参与（与派发侧 agentPermissionSet.allows
//     的 actorRCID 主人豁免同一条规则）。
//   - 其余 → 看 allow_discussion。
//
// **不判 allow_chatroom**：这里确定是在配讨论组（候选设备要 `roundtable` 能力、
// 要接 v3 圆桌），不是「以后可能当讨论组用」的聊天室。那个 ∪ 口径属于建聊天室
// 的那一刻（chatroom_commands.go 的 createRoundtable），两者不能混。
func filterDiscussionJoinNodes(nodes map[string]db.RongcloudNode, viewerRCID string) (allowed, blocked map[string]db.RongcloudNode) {
	allowed = make(map[string]db.RongcloudNode, len(nodes))
	blocked = make(map[string]db.RongcloudNode)
	viewer := strings.TrimSpace(viewerRCID)
	for nodeID, node := range nodes {
		if nodeDiscussionJoinAllowed(node, viewer) {
			allowed[nodeID] = node
			continue
		}
		blocked[nodeID] = node
	}
	return allowed, blocked
}

// nodeDiscussionJoinAllowed 是单节点的判据。
//
// viewerRCID 为空（拿不到发起人）时**不豁免**：这时候「认领者就是发起人」无从
// 判断，按开关判是唯一 fail-closed 的选择 —— 与群聊侧「FromUserID 为空不豁免」
// 保持一致。
func nodeDiscussionJoinAllowed(node db.RongcloudNode, viewerRCID string) bool {
	owner := ""
	if node.ClaimedByUserID.Valid {
		owner = strings.TrimSpace(node.ClaimedByUserID.String)
	}
	if owner == "" {
		return true
	}
	if viewerRCID != "" && owner == viewerRCID {
		return true
	}
	return node.AllowDiscussion
}

// discussionJoinBlockedFlag 是**显示侧**的唯一判据：这台节点要不要在
// `/api/claw/nodes` 上打 `discussionJoinBlocked`，让 C 端把它排除出候选。
//
// 与写路径（候选池 / confirm）共用 nodeDiscussionJoinAllowed，只是把「viewer 为空」
// 这一种内部调用的情形挡掉：workspace 全量视图没有「我是谁」，没有主人豁免可谈，
// 打标只会让内部调用方看到一堆莫名其妙的标记。
//
// 标记的代价是**不对称**的：漏标只让用户多点一次，随后被闸门挡下并说明原因；
// 误标会把主人自己还能用的 agent 藏起来 —— 「东西不见了又没有任何解释」比多点
// 一次糟得多。所以判不清的一律不打标（未认领、viewer 为空都归这一类）。
func discussionJoinBlockedFlag(node db.RongcloudNode, viewerRCID string) bool {
	if strings.TrimSpace(viewerRCID) == "" {
		return false
	}
	return !nodeDiscussionJoinAllowed(node, viewerRCID)
}
