-- 虾说兼容层用户表：承接旧 Python 后端 im_users 的公开契约。
-- 独立于 multica "user" 表，避免污染上游 schema。
CREATE TABLE IF NOT EXISTS claw_im_users (
    user_id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    nickname TEXT,
    portrait_uri TEXT,
    email TEXT,
    phone TEXT,
    signature TEXT DEFAULT '',
    gender TEXT DEFAULT '',
    birthday TEXT DEFAULT '',
    password_hash TEXT NOT NULL,
    rongcloud_token TEXT,
    status TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS claw_im_users_username_idx ON claw_im_users (username);
