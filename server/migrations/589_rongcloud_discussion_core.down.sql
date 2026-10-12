DROP TABLE IF EXISTS rongcloud_artifact;

DROP INDEX IF EXISTS uq_rongcloud_discussion_event_idem;
DROP INDEX IF EXISTS uq_rongcloud_discussion_event_seq;

-- 事件表退回旧形状：去掉新链路列，恢复两个 NOT NULL 与 event_type 白名单。
ALTER TABLE rongcloud_discussion_event
    DROP COLUMN IF EXISTS v3_envelope,
    DROP COLUMN IF EXISTS idempotency_key,
    DROP COLUMN IF EXISTS payload,
    DROP COLUMN IF EXISTS target_id,
    DROP COLUMN IF EXISTS actor_id,
    DROP COLUMN IF EXISTS seq,
    DROP COLUMN IF EXISTS discussion_id,
    ALTER COLUMN workspace_id SET NOT NULL,
    ALTER COLUMN chatroom_id  SET NOT NULL;

ALTER TABLE rongcloud_discussion_event
    ADD CONSTRAINT rongcloud_discussion_event_event_type_check CHECK (event_type IN (
        'discussion_started',
        'round_started',
        'turn_started',
        'turn_completed',
        'turn_skipped',
        'round_completed',
        'discussion_ended',
        'discussion_paused',
        'discussion_resumed',
        'host_changed',
        'error'
    ));

DROP INDEX IF EXISTS rongcloud_discussion_workspace_idx;
DROP INDEX IF EXISTS rongcloud_discussion_chatroom_idx;
DROP INDEX IF EXISTS uq_rongcloud_discussion_creator_request;
DROP INDEX IF EXISTS uq_rongcloud_discussion_one_active;
DROP TABLE IF EXISTS rongcloud_discussion;

DROP INDEX IF EXISTS rongcloud_chatroom_member_member_id_idx;
ALTER TABLE rongcloud_chatroom_member
    DROP COLUMN IF EXISTS portrait_uri,
    DROP COLUMN IF EXISTS nickname,
    DROP COLUMN IF EXISTS member_id;

ALTER TABLE rongcloud_chatroom
    DROP COLUMN IF EXISTS portrait_uri,
    DROP COLUMN IF EXISTS name;
