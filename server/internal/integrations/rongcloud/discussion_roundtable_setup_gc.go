package rongcloud

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 半成品讨论组回收 GC。
//
// 判定：讨论实例处于 configuring 且 last_interaction_at 超过 10 分钟（拍板项，
// PRD E1）。进入房间(resumeSetup)/提交议题/推荐/确认/前端心跳(touchSetup 60s)
// 都会刷新该时间，所以「用户正在页面上操作」的半成品永远不会被扫到。
//
// 回收动作（每个半成品，全部幂等）：
//  1. CAS → cancelled(cancel_reason='abandoned')（CAS 失败说明已被确认/已回收，跳过）；
//  2. emit discussion_cancelled{reason:'abandoned'}（幂等键 gc-abandon:<id>）；
//  3. 先广播再删房（删房后前端进房会走「已回收」空态，事件流仍可读）；
//  4. 房间 status='deleted' → 配额计数自动释放；
//  5. 融云侧 destroyChatroom（best-effort）。
//
// 扫描是持久表上的查询（复用 runInteractiveSweep 的模式），进程重启安全，无内存态。
const (
	// discussionSetupGCInterval 是 GC 扫描周期（拍板项 5 分钟）。
	discussionSetupGCInterval = 5 * time.Minute
	// setupAbandonTimeout 是半成品回收超时（拍板项 10 分钟，PRD E1 以拍板为准）。
	setupAbandonTimeout = 10 * time.Minute
	// setupAbandonReason 是 GC 回收写入 cancel_reason / 事件 payload 的取值。
	setupAbandonReason = "abandoned"
	// setupUserAbandonReason 是用户主动放弃半成品写入 cancel_reason / 事件 payload 的取值。
	setupUserAbandonReason = "user_abandoned"
)

// sweepAbandonedSetups 扫描并回收一批超时半成品（单次最多 100 行，见查询）。
func (rt *discussionRuntime) sweepAbandonedSetups(ctx context.Context) {
	if rt == nil || rt.queries == nil {
		return
	}
	cutoff := time.Now().Add(-setupAbandonTimeout)
	rows, err := rt.queries.ListAbandonedRongCloudDiscussionSetups(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
	if err != nil {
		rt.logger.Warn("discussion: setup GC scan failed", "error", err)
		return
	}
	for _, row := range rows {
		rt.reclaimAbandonedSetup(ctx, row.DiscussionID)
	}
}

// reclaimAbandonedSetup 回收一个超时半成品（GC 路径，actor=system）。
func (rt *discussionRuntime) reclaimAbandonedSetup(ctx context.Context, discussionID string) {
	rt.reclaimSetup(ctx, discussionID, setupAbandonReason, "system", "gc-abandon:"+discussionID)
}

// reclaimSetup 回收一个半成品讨论组：CAS → cancelled(cancel_reason) → 广播
// discussion_cancelled → 删房（释放配额）→ 融云销毁。所有步骤幂等：CAS 未命中
// （并发确认/并发回收/重复命令）直接返回 false；事件按幂等键去重；删房/融云销毁可重入。
// 返回是否真正执行了状态推进（CAS 命中）。
func (rt *discussionRuntime) reclaimSetup(ctx context.Context, discussionID, reason, actorID, idempotencyKey string) bool {
	cancelled, err := rt.queries.CancelRongCloudDiscussionSetup(ctx, db.CancelRongCloudDiscussionSetupParams{
		DiscussionID: discussionID,
		CancelReason: reason,
	})
	if err != nil {
		// CAS 失败：讨论已被确认开始或已被另一轮回收 —— 不是错误。
		return false
	}
	d := discussionRecordFromRow(cancelled)
	rec, fresh, err := rt.interactiveEmit(ctx, d, interactiveEvent{
		EventType: "discussion_cancelled",
		ActorID:   actorID,
		Payload:   map[string]interface{}{"reason": reason},
		Key:       idempotencyKey,
		StateVer:  d.Version,
	})
	if err != nil {
		rt.logger.Warn("discussion: setup cancel event emit failed", "discussionId", discussionID, "error", err)
	} else if fresh {
		// 删除前先发：广播失败不阻断回收（前端轮询/REST 重同步会兜底）。
		rt.interactiveBroadcast(ctx, d, rec)
	}
	if err := rt.queries.SoftDeleteRongCloudChatroomByRongCloudID(ctx, d.ChatroomID); err != nil {
		rt.logger.Warn("discussion: setup reclaim room soft delete failed",
			"discussionId", discussionID, "chatroomId", d.ChatroomID, "error", err)
	}
	if rt.client != nil {
		if err := rt.client.destroyChatroom(ctx, d.ChatroomID); err != nil {
			rt.logger.Warn("discussion: setup reclaim rongcloud destroy failed",
				"discussionId", discussionID, "chatroomId", d.ChatroomID, "error", err)
		}
	}
	rt.logger.Info("discussion: configuring setup reclaimed",
		"discussionId", discussionID, "chatroomId", d.ChatroomID, "reason", reason)
	return true
}
