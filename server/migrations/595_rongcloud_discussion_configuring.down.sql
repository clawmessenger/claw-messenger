-- 回滚 595：先把仍是 configuring 的半成品收尾（否则状态 CHECK 回滚会被存量行挡住），
-- 再逆向删除索引、约束与三个新列。

UPDATE rongcloud_discussion
SET status = 'cancelled', cancel_reason = 'abandoned', updated_at = now()
WHERE status = 'configuring';

DROP INDEX IF EXISTS rongcloud_discussion_configuring_gc_idx;

DROP INDEX IF EXISTS uq_rongcloud_discussion_one_active;
CREATE UNIQUE INDEX uq_rongcloud_discussion_one_active
    ON rongcloud_discussion (chatroom_id)
    WHERE status IN ('planning', 'running', 'summarizing', 'awaiting_user', 'synthesizing');

ALTER TABLE rongcloud_discussion
    DROP CONSTRAINT IF EXISTS rongcloud_discussion_setup_step_check;

ALTER TABLE rongcloud_discussion
    DROP CONSTRAINT IF EXISTS rongcloud_discussion_status_check;
ALTER TABLE rongcloud_discussion
    ADD CONSTRAINT rongcloud_discussion_status_check CHECK (status IN (
        'planning', 'running', 'summarizing', 'awaiting_user',
        'synthesizing', 'completed', 'cancelled', 'failed'));

ALTER TABLE rongcloud_discussion
    DROP COLUMN IF EXISTS setup_step,
    DROP COLUMN IF EXISTS last_interaction_at,
    DROP COLUMN IF EXISTS cancel_reason;
