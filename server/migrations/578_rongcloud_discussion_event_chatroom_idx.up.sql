CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_rongcloud_discussion_event_chatroom
    ON rongcloud_discussion_event (chatroom_id, created_at);
