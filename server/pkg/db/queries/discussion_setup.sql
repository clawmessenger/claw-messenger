-- =====================
-- 讨论组引导式创建（configuring 态）：断点查询 / CAS 推进 / 心跳 / GC 扫描
-- 对应迁移 595_rongcloud_discussion_configuring。
-- =====================

-- name: GetConfiguringRongCloudDiscussionByChatroom :one
-- resumeSetup / submitSetupTopic 的入口查询：一个房间的半成品最多一条
-- （uq_rongcloud_discussion_one_active 已把 configuring 纳入活跃集）。
SELECT * FROM rongcloud_discussion
WHERE chatroom_id = $1 AND status = 'configuring'
ORDER BY created_at DESC
LIMIT 1;

-- name: SubmitRongCloudDiscussionSetupTopic :one
-- S1 提交议题：CAS 写 topic/goal 并把断点推到 roles，同时刷新交互时间。
-- 只允许从 topic 步进（重复提交/乱序提交返回 0 行，调用方按幂等成功或 409 处理）。
UPDATE rongcloud_discussion
SET topic              = @topic,
    goal               = @goal,
    setup_step         = 'roles',
    last_interaction_at = now(),
    version            = version + 1,
    updated_at         = now()
WHERE discussion_id = @discussion_id
  AND status = 'configuring'
  AND setup_step IN ('topic', 'roles')
RETURNING *;

-- name: ConfirmRongCloudDiscussionSetup :one
-- S3 确认开始：configuring → planning 的乐观锁推进，同时固化角色板与主持快照。
-- 只有 version 未变且仍在 configuring 才写入（并发确认/GC 回收的失败方拿到 0 行）。
UPDATE rongcloud_discussion
SET status              = 'planning',
    setup_step          = '',
    role_snapshot       = @role_snapshot,
    host_config_snapshot = @host_config_snapshot,
    recommendation_id   = @recommendation_id,
    last_interaction_at = now(),
    version             = version + 1,
    updated_at          = now()
WHERE discussion_id = @discussion_id
  AND version = @expected_version
  AND status = 'configuring'
RETURNING *;

-- name: TouchRongCloudDiscussionSetup :execrows
-- 心跳/进入房间/任何 setup 操作：只刷新 last_interaction_at（GC 防误删的唯一依据）。
UPDATE rongcloud_discussion
SET last_interaction_at = now(), updated_at = now()
WHERE discussion_id = $1
  AND status = 'configuring';

-- name: ListAbandonedRongCloudDiscussionSetups :many
-- GC 扫描：最后交互时间早于 cutoff（now()-10min，由调用方传入）的半成品。
SELECT * FROM rongcloud_discussion
WHERE status = 'configuring'
  AND last_interaction_at < @cutoff
ORDER BY last_interaction_at ASC
LIMIT 100;

-- name: CancelRongCloudDiscussionSetup :one
-- GC 回收/用户主动放弃：CAS 只在仍 configuring 时收尾，返回推进后的行。
UPDATE rongcloud_discussion
SET status       = 'cancelled',
    cancel_reason = @cancel_reason,
    setup_step   = '',
    version      = version + 1,
    updated_at   = now()
WHERE discussion_id = @discussion_id
  AND status = 'configuring'
RETURNING *;

-- name: ListMyConfiguringRongCloudDiscussions :many
-- GET /api/my/discussions?status=configuring：当前用户的半成品列表（会话列表合并用）。
SELECT * FROM rongcloud_discussion
WHERE created_by = $1
  AND status = 'configuring'
ORDER BY last_interaction_at DESC;

-- name: CountActiveDiscussionChatroomsByOwnerRcID :one
-- 配额计数（讨论组）：房主名下 status='active' 且 conversation_kind='discussion' 的房间。
-- configuring 半成品房间同为 active → 自动计入；GC 删房（status='deleted'）后自动释放。
SELECT COUNT(*)::bigint AS count
FROM rongcloud_chatroom c
JOIN rongcloud_user u ON u.id = c.owner_user_id
WHERE u.rongcloud_user_id = $1
  AND c.status = 'active'
  AND c.conversation_kind = 'discussion';

-- name: CountActivePlainChatroomsByOwnerRcID :one
-- 配额计数（普通聊天室）：讨论组不双计 chatroom（COALESCE 兜底历史 NULL 行）。
SELECT COUNT(*)::bigint AS count
FROM rongcloud_chatroom c
JOIN rongcloud_user u ON u.id = c.owner_user_id
WHERE u.rongcloud_user_id = $1
  AND c.status = 'active'
  AND COALESCE(c.conversation_kind, 'chatroom') <> 'discussion';
