CREATE TABLE IF NOT EXISTS rongcloud_discussion_event (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chatroom_id     UUID NOT NULL,
    workspace_id    UUID NOT NULL,
    event_type      TEXT NOT NULL CHECK (event_type IN (
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
    )),
    round_number    INT NOT NULL DEFAULT 0,
    speaking_order  INT NOT NULL DEFAULT 0,
    node_id         UUID,
    content         JSONB NOT NULL DEFAULT '{}'::jsonb,
    msg_uid         TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
