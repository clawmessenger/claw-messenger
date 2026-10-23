-- 600 的回滚：投影列纯属派生数据，丢回好友边反查即可，无需回填。
DROP INDEX IF EXISTS idx_rongcloud_node_claimed_by;
ALTER TABLE rongcloud_node DROP COLUMN IF EXISTS claimed_by_user_id;
