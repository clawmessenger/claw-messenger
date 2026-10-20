-- =====================
-- 用户级配额（三类资源：讨论组/群聊/聊天室）
-- 对应迁移 596_claw_im_user_quota / 597_claw_im_plan。
-- 取值语义：-1=不限，0=禁止，N=上限，NULL=跟随套餐/内置默认（3/5/1）。
-- 优先级：按用户覆盖 > 套餐 > 内置默认。
-- =====================

-- name: GetClawIMUserQuota :one
SELECT * FROM claw_im_user_quota WHERE user_id = $1;

-- name: UpsertClawIMUserQuota :one
-- 管理端保存：三类上限整体覆盖写入（NULL 表示清除覆盖、回到跟随默认）。
INSERT INTO claw_im_user_quota (user_id, discussion_limit, group_limit, chatroom_limit, updated_by)
VALUES (@user_id, @discussion_limit, @group_limit, @chatroom_limit, @updated_by)
ON CONFLICT (user_id) DO UPDATE SET
    discussion_limit = EXCLUDED.discussion_limit,
    group_limit      = EXCLUDED.group_limit,
    chatroom_limit   = EXCLUDED.chatroom_limit,
    updated_by       = EXCLUDED.updated_by,
    updated_at       = now()
RETURNING *;

-- name: CountOwnedGroupsByOwnerID :one
-- 配额计数（群聊）：rongcloud_group 属主列是 owner_id（claw_im_users.user_id 域，
-- 迁移 588），无 status 列 —— 解散即物理删行，所以计数就是全部名下群。
SELECT COUNT(*)::bigint AS count
FROM rongcloud_group
WHERE owner_id = $1;

-- name: GetClawIMUserPlanID :one
-- 用户分配套餐（597）：NULL = 跟随默认套餐。
SELECT plan_id FROM claw_im_users WHERE user_id = $1;

-- name: GetClawIMPlan :one
SELECT * FROM claw_im_plan WHERE id = $1;

-- name: GetDefaultClawIMPlan :one
-- 默认套餐全库唯一（claw_im_plan_one_default 部分唯一索引）。
SELECT * FROM claw_im_plan WHERE is_default AND status = 'active' LIMIT 1;

-- name: AdvisoryLockClawIMUserQuota :exec
-- 配额 Check 的临界区串行化：同一用户的并发创建在「查上限+数用量」期间互斥
-- （pg_advisory_xact_lock 随事务结束自动释放）。必须在事务内调用。
SELECT pg_advisory_xact_lock(hashtext($1));
