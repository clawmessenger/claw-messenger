-- 好友申请审批表：让「加 AI 节点为好友」变成「由节点拥有者审批」。
--
-- 为什么必须建表：好友关系的权威数据源是融云好友边，融云服务端 API 能
-- **发出**申请（/friend/add.json optType=1），但没有「读申请列表 / 同意 /
-- 拒绝」的对等接口（internal/integrations/rongcloud/user_api.go:72-120 只有
-- add/get/remove）。所以「谁在申请哪台设备、拥有者批没批」这个状态只能自己存。
-- 见 .agent-runs/2026-10-23/task-003/backend-im-service-pattern.md §6.2。
--
-- 这条链路要解决的具体问题：移动端扫码加节点时，申请只能发给「拥有者账号」，
-- 拥有者在融云「新的朋友」里同意后，建立的是**人↔人**的好友边；请求方拿不到
-- 那个 AI 节点（节点好友边只由绑定/认领流程写给拥有者自己，node_service.go:1403）。
-- 有了这张表，approveRequest 才能在拥有者同意后补建「请求方↔节点」的双向边。
--
-- status：0 待审批 / 1 已通过 / 2 已拒绝。与旧 Python 的
-- im_friend_requests 保持同一套取值（clawmessenger-server/database.py:547），
-- 便于日后迁移历史数据。
--
-- device_node_id 存节点的融云 id（不是 rongcloud_node.node_id）：申请入口拿到
-- 的就是分享二维码里的 node rc id，审批时要拿它建好友边。

CREATE TABLE IF NOT EXISTS im_friend_request (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    from_user_id    TEXT NOT NULL,
    to_user_id      TEXT NOT NULL,
    device_node_id  TEXT NOT NULL,
    message         TEXT NOT NULL DEFAULT '',
    status          SMALLINT NOT NULL DEFAULT 0 CHECK (status IN (0, 1, 2)),
    decided_at      TIMESTAMPTZ,
    decided_by      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE im_friend_request IS
  'AI 节点好友申请（拥有者审批）。申请人/审批人是 claw_im_users.user_id，device_node_id 是节点的融云 id。';
COMMENT ON COLUMN im_friend_request.device_node_id IS
  '被申请的 AI 节点融云 id。审批通过后据此补建 请求方↔节点 双向好友边。';

-- 同一个请求方对同一台设备只允许存在一条待审批记录：重复点「发送申请」
-- 走 ON CONFLICT 更新（见 friend_request.go 的 Create），不会堆出一串僵尸行。
CREATE UNIQUE INDEX IF NOT EXISTS uq_im_friend_request_pending
    ON im_friend_request (from_user_id, device_node_id)
    WHERE status = 0;

-- 拥有者的「新的朋友」列表：按审批人 + 状态倒序。
CREATE INDEX IF NOT EXISTS idx_im_friend_request_to_user
    ON im_friend_request (to_user_id, status, created_at DESC);

-- 请求方对账：查「我对这台设备有没有待审批申请」。
CREATE INDEX IF NOT EXISTS idx_im_friend_request_from_user
    ON im_friend_request (from_user_id, device_node_id, status);
