-- 圆桌讨论：共享投递缓冲。对应旧后端 011 迁移。
--
-- 解决两个 v2/v3 协议的传输问题：
--   1) 单条消息体过大 → 分帧（wire buffer）：一个 message_id 的多片 frame 攒齐、校验后
--      才算 ready；供超大 JSON 信封安全过融云。
--   2) 增量流式文本 → delta buffer：同一 (discussion_id, request_id, node_seq) 的增量
--      去重、幂等、按序重放，避免丢片或重复入事件流。
--
-- 单行锁表把「检查缓冲 + 写入」放进一个事务里的串行点，避免并发写者互相覆盖。

CREATE TABLE IF NOT EXISTS rongcloud_discussion_delivery_lock (
    id INTEGER PRIMARY KEY CHECK (id = 1)
);
INSERT INTO rongcloud_discussion_delivery_lock (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

-- expires_at 是毫秒时间戳（BIGINT），与旧实现一致 —— 清理任务按它扫描过期分片。
CREATE TABLE IF NOT EXISTS rongcloud_discussion_wire_buffer (
    sender_id  TEXT NOT NULL,
    message_id TEXT NOT NULL,
    frames     TEXT NOT NULL,
    byte_count BIGINT NOT NULL CHECK (byte_count >= 0),
    ready      INTEGER NOT NULL CHECK (ready IN (0, 1)),
    expires_at BIGINT NOT NULL,
    PRIMARY KEY (sender_id, message_id)
);

CREATE INDEX IF NOT EXISTS rongcloud_discussion_wire_buffer_expiry_idx
    ON rongcloud_discussion_wire_buffer (expires_at);

CREATE TABLE IF NOT EXISTS rongcloud_discussion_delta_buffer (
    discussion_id   TEXT NOT NULL,
    request_id      TEXT NOT NULL,
    assignment_id   TEXT NOT NULL,
    actor_id        TEXT NOT NULL,
    node_seq        BIGINT NOT NULL CHECK (node_seq >= 0),
    payload         TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    byte_count      BIGINT NOT NULL CHECK (byte_count >= 0),
    emitted         INTEGER NOT NULL CHECK (emitted IN (0, 1)),
    expires_at      BIGINT NOT NULL,
    PRIMARY KEY (discussion_id, request_id, node_seq),
    UNIQUE (discussion_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS rongcloud_discussion_delta_buffer_expiry_idx
    ON rongcloud_discussion_delta_buffer (expires_at);
