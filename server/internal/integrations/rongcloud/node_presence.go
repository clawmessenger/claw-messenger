package rongcloud

// node_presence.go 统一「节点现在是否在线」的判定口径。
//
// rongcloud_node.deploy_status 在 Python 原版里并不是权威状态，而是「最近一次
// 观测到的在线状态」缓存：
//   - system_host._node_is_available 先查融云 presence，再 _sync_cached_presence
//     把结果 UPDATE 回 im_nodes.deploy_status；
//   - /api/claw/node/<id>/online 那一类查询同理，每次都把 presence 写回列。
//
// Go 移植时保留了这个列、却没有任何写入者（建节点时写死 offline，之后无人更新），
// 于是所有读列的判定都变成恒假：圆桌布置弹窗点「推荐角色」必报 SYSTEM_HOST_OFFLINE，
// 圆桌创建时也挑不出任何可用设备。这里按原版语义把权威信号换回融云 presence ——
// 服务端下发到设备的命令本身就走的融云 P2P，设备进程掉线就是收不到。

import (
	"context"
	"log/slog"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// nodeIsOnline 报告节点当前是否在线。client/rongcloud id 缺失时（单测或未装配
// 融云凭据）回落读 deploy_status 列，保持历史行为。
//
// presence 查询失败按离线处理（fail closed）：命令发不出去，讨论/房间都驱动不了，
// 谎报在线只会让流程走到更靠后的地方再失败。这一点与原版 _rongcloud_online 一致。
//
// 判定语义**没有变**：权威信号仍然是融云 /user/checkOnline.json。变的只是同一次
// 观测在 nodePresenceCacheTTL（4s）内会被复用 —— 一轮推荐要对 N 台候选设备做
// 同一件事，串行逐台问一遍是 1-4s × N 的主要来源。失败结果不缓存（见
// node_presence_cache.go 的文件头注释）。
func nodeIsOnline(ctx context.Context, client *rongcloudAPIClient, node db.RongcloudNode) bool {
	if client == nil || node.RongcloudUserID == "" {
		return node.DeployStatus == "online"
	}
	if online, hit := client.presence.lookup(node.RongcloudUserID); hit {
		return online
	}
	online, err := client.checkOnline(ctx, node.RongcloudUserID)
	if err != nil {
		// 查询失败按离线处理（fail closed），但必须留痕：静默吞掉会让「节点真的
		// 掉线」与「融云 API 打不通/凭据不对」看起来完全一样，排查时只能靠猜。
		slog.Warn("rongcloud: 节点在线查询失败，按离线处理",
			"node_id", node.NodeID,
			"rongcloud_user_id", node.RongcloudUserID,
			"error", err)
		return false
	}
	client.presence.store(node.RongcloudUserID, online)
	return online
}

// syncNodePresenceCache 把一次 presence 观测写回 rongcloud_node.deploy_status。
// 只在观测值与列值不一致时写，避免热点路径上产生无谓的 UPDATE。
// 写失败不影响判定（列只是缓存，不是权威状态）。
func syncNodePresenceCache(ctx context.Context, queries *db.Queries, node db.RongcloudNode, online bool) {
	if queries == nil {
		return
	}
	want := "offline"
	if online {
		want = "online"
	}
	if node.DeployStatus == want {
		return
	}
	_, _ = queries.UpdateRongCloudNodeDeployStatus(ctx, db.UpdateRongCloudNodeDeployStatusParams{
		ID:           node.ID,
		DeployStatus: want,
	})
}
