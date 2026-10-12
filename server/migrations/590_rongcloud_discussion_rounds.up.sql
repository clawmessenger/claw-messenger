-- 圆桌讨论：逐轮文档与命令 outbox。对应旧后端 012 迁移的两张表。
--
-- 逐轮文档：user_confirmed（交互式）模式每一轮由主持人产出一份文档，用户在每轮之间
-- 确认后才继续。文档内容通过 HTTP 拉取（/api/round-documents/...），不走命令通道 ——
-- 这正是旧后端的设计，前端也据此实现（RoundDocumentHistory）。
CREATE TABLE IF NOT EXISTS rongcloud_discussion_round_document (
    document_id       TEXT PRIMARY KEY,
    discussion_id     TEXT NOT NULL,
    round             INTEGER NOT NULL CHECK (round > 0),
    revision          INTEGER NOT NULL CHECK (revision >= 0),
    source_request_id TEXT NOT NULL,
    title             TEXT NOT NULL DEFAULT '',
    content           TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (discussion_id, round, revision)
);

CREATE INDEX IF NOT EXISTS rongcloud_discussion_round_document_discussion_idx
    ON rongcloud_discussion_round_document (discussion_id, round, revision);

-- 命令 outbox：取消/派发这类「必须送达节点、且要能重试」的动作先落库，再由 worker
-- 认领发送（短租约 + 超时重派）。对应旧后端 discussion/commands.py 的
-- DiscussionCommandRepository。没有这张表，进程重启就会丢掉未送达的取消指令。
CREATE TABLE IF NOT EXISTS rongcloud_discussion_command (
    command_id   TEXT PRIMARY KEY,
    discussion_id TEXT NOT NULL,
    request_id   TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('cancel', 'dispatch')),
    payload      JSONB NOT NULL DEFAULT '{}'::jsonb,
    state        TEXT NOT NULL DEFAULT 'pending'
                     CHECK (state IN ('pending', 'leased', 'sent', 'acknowledged', 'failed')),
    attempts     INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- worker 的认领查询：state=ready AND available_at <= now()，按就绪时间排队。
CREATE INDEX IF NOT EXISTS rongcloud_discussion_command_dispatch_idx
    ON rongcloud_discussion_command (state, available_at);

CREATE INDEX IF NOT EXISTS rongcloud_discussion_command_discussion_idx
    ON rongcloud_discussion_command (discussion_id);
