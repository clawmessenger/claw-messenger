-- 按用户配额覆盖表（P0）：管理员在用户详情页调整三类资源的创建上限。
--
-- 取值语义（三类一致）：-1=不限，0=禁止，N=上限，NULL=跟随套餐/内置默认（3/5/1）。
-- 优先级：本表覆盖 > claw_im_plan 套餐（597）> 内置默认。
-- updated_by/updated_at 是最小审计（拍板项 E7，不建独立审计表）。

CREATE TABLE IF NOT EXISTS claw_im_user_quota (
    user_id          TEXT PRIMARY KEY REFERENCES claw_im_users(user_id),
    discussion_limit INTEGER CHECK (discussion_limit >= -1),
    group_limit      INTEGER CHECK (group_limit >= -1),
    chatroom_limit   INTEGER CHECK (chatroom_limit >= -1),
    updated_by       TEXT NOT NULL DEFAULT '',
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
