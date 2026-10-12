-- 圆桌讨论：系统级请求与幂等创建。对应旧后端 009 / 010 迁移。
--
-- 三张表都是「服务器向某个 AI 节点发请求、等回包」这条链路的持久化，以及
-- 「创建圆桌」的多阶段幂等状态机。它们让一次创建在进程重启/超时/重试后仍然只有
-- 一个结果 —— 没有这些表，重复提交会建出多个聊天室。

-- 节点请求-应答。服务器向一个节点的 AI 发起请求（角色推荐、模型目录、运行时恢复），
-- 在 expires_at 之前等它回包。对应旧 im_node_requests。
CREATE TABLE IF NOT EXISTS rongcloud_node_request (
    request_id        TEXT PRIMARY KEY,
    request_kind      TEXT NOT NULL,
    requester_user_id TEXT NOT NULL,
    target_node_id    TEXT NOT NULL,
    target_ai_user_id TEXT NOT NULL,
    status            TEXT NOT NULL,
    request_payload   JSONB NOT NULL DEFAULT '{}'::jsonb,
    response_payload  JSONB,
    error_code        TEXT,
    expires_at        TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS rongcloud_node_request_target_status_idx
    ON rongcloud_node_request (target_node_id, status);
CREATE INDEX IF NOT EXISTS rongcloud_node_request_expires_idx
    ON rongcloud_node_request (expires_at);

-- 角色推荐结果。recommendRoles 命令让主持人节点提出一组角色，结果落这里，
-- 由 recommendation_id 关联到随后的 createRoundtable（推荐有有效期，过期即作废）。
CREATE TABLE IF NOT EXISTS rongcloud_role_recommendation (
    recommendation_id TEXT PRIMARY KEY,
    request_id        TEXT NOT NULL UNIQUE,
    user_id           TEXT NOT NULL,
    host_node_id      TEXT NOT NULL,
    host_ai_user_id   TEXT NOT NULL,
    config_version    TEXT NOT NULL,
    topic             TEXT NOT NULL DEFAULT '',
    goal              TEXT NOT NULL DEFAULT '',
    roles             JSONB NOT NULL DEFAULT '[]'::jsonb,
    expires_at        TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS rongcloud_role_recommendation_user_expires_idx
    ON rongcloud_role_recommendation (user_id, expires_at);

-- 创建圆桌的幂等记录。主键 (user_id, client_request_id) 保证同一个客户端请求
-- 无论重放几次都只推进同一个状态机（pending → chatroom_ready → members_ready →
-- discussion_ready → ready）。request_payload_hash 用于识别「同 key 不同内容」的冲突。
CREATE TABLE IF NOT EXISTS rongcloud_roundtable_create_request (
    user_id              TEXT NOT NULL,
    client_request_id    TEXT NOT NULL,
    recommendation_id    TEXT,
    status               TEXT NOT NULL,
    request_payload      JSONB NOT NULL DEFAULT '{}'::jsonb,
    request_payload_hash TEXT,
    chatroom_id          TEXT,
    discussion_id        TEXT,
    error_code           TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, client_request_id)
);

CREATE INDEX IF NOT EXISTS rongcloud_roundtable_create_request_recommendation_idx
    ON rongcloud_roundtable_create_request (recommendation_id);
