-- 龙虾档案表（claw_lobster）+ 微信小程序登录绑定键（claw_im_users.wechat_openid）。
--
-- 背景：uniapp 的「龙虾」模块（subPackages/lobster/pages/lobster/{index,edit,detail}.vue）
-- 通过 IM system 命令 service=lobster 做 CRUD（getList/getDetail/create/update/delete），
-- 后端此前既没有这个 service 也没有表，页面恒回 404「未知的服务或操作」。
--
-- 归属口径与 claw_im_user_quota（596）一致：C 端主键是 TEXT（claw_im_users.user_id
-- 本身就是融云 id），不是 UUID，所以 user_id 用 TEXT 并挂外键；所有读写都必须按
-- user_id 过滤，越权访问一律按「记录不存在」（404）处理，不暴露 403。
--
-- 软删除：deleted_at 非空即视为回收，列表/详情/更新/删除四条路径都带
-- deleted_at IS NULL，删除只写时间戳（不物理删行，保留审计与误删恢复空间）。
--
-- weight 用 DOUBLE PRECISION：客户端表单允许小数（单位克），整型会静默截断。
-- status 是客户端枚举（healthy / warning / sick / dead，见 edit.vue 的
-- statusOptions）；不建 CHECK 约束，避免客户端新增状态时被库拒绝。

CREATE TABLE IF NOT EXISTS claw_lobster (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES claw_im_users (user_id),
    name       TEXT NOT NULL,
    variety    TEXT NOT NULL DEFAULT '',
    weight     DOUBLE PRECISION NOT NULL DEFAULT 0,
    status     TEXT NOT NULL DEFAULT 'healthy',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

-- 列表查询的完整索引：按属主 + 未删除 + 倒序时间（getList 的实际形状）。
CREATE INDEX IF NOT EXISTS claw_lobster_user_id_idx
    ON claw_lobster (user_id, created_at DESC)
    WHERE deleted_at IS NULL;

-- 微信小程序登录（service=user, action=wxLogin）的绑定键。
-- openid 只对「微信登录」账号有意义，存量用户名/密码账号保持 NULL；
-- Postgres 的唯一索引把 NULL 视为互不相同，因此不限 NULL 行数。
ALTER TABLE claw_im_users ADD COLUMN IF NOT EXISTS wechat_openid TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS claw_im_users_wechat_openid_key
    ON claw_im_users (wechat_openid);
