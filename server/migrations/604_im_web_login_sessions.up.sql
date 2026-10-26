-- Web 扫码登录的一次性票据表（移植自旧 Python web_qr_login.py 的同名表）。
--
-- 只存 ticket 的 sha256，不存原文：这张表一旦泄露，等效于「所有未过期二维码
-- 都能被直接兑换成登录态」。
--
-- 生命周期：pending →（移动端扫码确认）confirmed →（Web 兑换）consumed；
-- 另有一条旁路 expired（查询时按 expires_at 惰性翻写，没有定时任务）。
--
-- 注意与旧 Python 的差异：主键用 BIGSERIAL 而非 SERIAL。两边的自增互不干扰，
-- 且 id 不对外暴露；保留 IF NOT EXISTS 是为了「表已被旧服务建过」时幂等跳过。

CREATE TABLE IF NOT EXISTS im_web_login_sessions (
    id BIGSERIAL PRIMARY KEY,
    ticket_hash TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'confirmed', 'consumed', 'expired')),
    user_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    confirmed_at TIMESTAMPTZ,
    consumed_at TIMESTAMPTZ
);

-- 创建会话时顺手清理一天前的过期记录，走这个索引。
CREATE INDEX IF NOT EXISTS idx_im_web_login_sessions_expires_at
    ON im_web_login_sessions (expires_at);

-- 状态查询与确认按 (status, expires_at) 过滤。
CREATE INDEX IF NOT EXISTS idx_im_web_login_sessions_status_expires
    ON im_web_login_sessions (status, expires_at);
