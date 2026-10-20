-- 套餐表（P1 数据层先行）：quota.Resolve 的三级优先级是 按用户覆盖 > 套餐 > 内置默认。
-- 本期 plan_id 恒为 NULL（全部落默认套餐），套餐 CRUD 管理界面属 P1 后续迭代。
--
-- is_default 全库唯一用部分唯一索引保证（Postgres 唯一索引里多行 false 互不冲突）。
-- 默认套餐数值为拍板项：讨论组 3 / 群聊 5 / 聊天室 1。

CREATE TABLE IF NOT EXISTS claw_im_plan (
    id               SERIAL PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    discussion_limit INTEGER NOT NULL DEFAULT 3 CHECK (discussion_limit >= -1),
    group_limit      INTEGER NOT NULL DEFAULT 5 CHECK (group_limit >= -1),
    chatroom_limit   INTEGER NOT NULL DEFAULT 1 CHECK (chatroom_limit >= -1),
    is_default       BOOLEAN NOT NULL DEFAULT false,
    status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE claw_im_users ADD COLUMN IF NOT EXISTS plan_id INTEGER;  -- NULL=默认套餐

CREATE UNIQUE INDEX IF NOT EXISTS claw_im_plan_one_default
    ON claw_im_plan (is_default) WHERE is_default;

INSERT INTO claw_im_plan (name, discussion_limit, group_limit, chatroom_limit, is_default)
VALUES ('免费版', 3, 5, 1, true)
ON CONFLICT (name) DO NOTHING;
