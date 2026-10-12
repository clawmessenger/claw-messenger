-- 圆桌移植 P3：让 (chatroom_id, member_id) 可作为 upsert 锚点。
--
-- 背景：564 给 rongcloud_chatroom_member 建的是 UNIQUE(chatroom_id, node_id)。
-- 这对 AI 成员够用（node_id 非空），但人类成员（member_type='user'）的 node_id
-- 为 NULL，Postgres 把 NULL 视为互不相同 → 同一个用户可以被 join/invite 重复插入。
-- 旧 Python 后端的 im_chatroom_members 用的是 UNIQUE(chatroom_id, member_id)，
-- P2 给 Go 表加了 member_id 列但没补这个唯一约束。P3 的 join/invite/removeMember
-- 要按 member_id 做 upsert / 删除，没有唯一约束就无法安全 ON CONFLICT。
--
-- 部分唯一索引（WHERE member_id IS NOT NULL）：589 之前的旧行 member_id 为 NULL，
-- 不受约束；只对显式落了 member_id 的行生效，与旧 Python 表语义一致。
CREATE UNIQUE INDEX IF NOT EXISTS uq_rongcloud_chatroom_member_member_id
    ON rongcloud_chatroom_member (chatroom_id, member_id)
    WHERE member_id IS NOT NULL;
