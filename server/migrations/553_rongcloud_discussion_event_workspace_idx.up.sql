CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_rongcloud_discussion_event_workspace
    ON rongcloud_discussion_event (workspace_id, created_at);
