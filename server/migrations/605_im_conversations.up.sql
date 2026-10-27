-- 融云会话 ID 映射表（移植自旧 Python database.py:338 的同名表）。
--
-- 为什么 Go 端要补这张表：旧后端有 service=conversation 的 save/get
-- （clawmessenger-server/rongcloud/command_handler.py:5754-5810），迁移时漏了，
-- 于是客户端每次「个人资料 → 发消息」都会收到
-- 404「未知的服务或操作: conversation/save」，小程序控制台报红
-- （2026-10-26 现场）。这条链路只是把「融云会话号」落一份副本，会话本身
-- 仍由融云 SDK 与 conversationStore 管理。
--
-- 列与旧表逐字段一致（id / target_id / conversation_type / conversation_id /
-- created_at + UNIQUE(target_id, conversation_type)）：Go 端与旧服务可能连
-- 同一个库，多一列少一列都会让 ON CONFLICT 失配。
-- IF NOT EXISTS 是为了「表已被旧服务建过」时幂等跳过（同 604 的处理）。
--
-- 注意这张表与 conversation_manager.py 里的同名表**不是一回事**：那个模块
-- 写的是 conversation_id/user_id/node_id/title/status 那套列，与 database.py
-- 的建表语句互相矛盾（旧代码自身的遗留问题）。Go 端只对接 database.py 这套。

CREATE TABLE IF NOT EXISTS im_conversations (
    id                BIGSERIAL PRIMARY KEY,
    target_id         TEXT NOT NULL,
    conversation_type INTEGER NOT NULL DEFAULT 1,
    conversation_id   TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (target_id, conversation_type)
);

COMMENT ON TABLE im_conversations IS
  '融云会话号映射：target_id（对方融云 id / 群号）+ conversation_type → conversation_id。';

-- 旧实现把 created_at 当「最后写入时间」用（ON CONFLICT 时改写），这里保持
-- 同一列语义，不额外加 updated_at —— 加列会让旧服务的 ON CONFLICT 列清单失配。
COMMENT ON COLUMN im_conversations.created_at IS
  '首次/最近一次写入时间（旧实现 ON CONFLICT 时刷新本列）。';
