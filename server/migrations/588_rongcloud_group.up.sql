-- 融云群组在本地的镜像。
--
-- 为什么需要这张表：融云没有「列出全部群」的平台接口，只能按群 ID 查、或查某
-- 用户加入了哪些群，所以管理端要分页列出群组就必须在本地留一份镜像 —— 旧后端
-- 的 im_groups / im_group_members 就是干这个的（另见 scripts/sync_group.py）。
--
-- 为什么现在才建：Go 服务端此前**没有**群组这条链路。rongcloud/group_api.go 里
-- 融云 group API 写好了却无人调用；C 端 web 的「建群」走系统命令通道
-- （service=group, action=create），而 Go 的 systemHandler 只认
-- ping/chatroom/device/discussion/ai，落进 unknown service 分支。本次把这条链路
-- 补上，镜像表才有东西可写。
--
-- 不加 workspace_id：群组是融云层面的资源，成员用融云 user id 标识，而
-- claw_im_users 本身也没有 workspace 归属；镜像与 rongcloud_node 不同，后者是
-- 每台机器装进某个 workspace 的接入记录。旧后端的 im_groups 同样没有 workspace。

CREATE TABLE IF NOT EXISTS rongcloud_group (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 融云群 ID：本平台自己生成的纯数字串（与用户/节点 ID 同风格），
    -- 同时是融云侧的唯一键。
    group_id            text NOT NULL UNIQUE,
    group_name          text NOT NULL,
    -- 群主的融云 user id（claw_im_users.user_id）。旧后端以融云消息发送者为准，
    -- 不信任客户端上报的 ownerId。
    owner_id            text NOT NULL,
    portrait_uri        text NOT NULL DEFAULT '',
    description         text NOT NULL DEFAULT '',
    member_count        integer NOT NULL DEFAULT 0,
    -- 圆桌讨论（openclaw）每次开几轮、是否暂停。旧后端 im_groups 的对应列。
    openclaw_max_rounds integer,
    openclaw_stopped    boolean NOT NULL DEFAULT FALSE,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- 管理端按创建时间倒序列出并支持按群 ID/名称搜索。
CREATE INDEX IF NOT EXISTS rongcloud_group_created_at_idx
    ON rongcloud_group (created_at DESC);
CREATE INDEX IF NOT EXISTS rongcloud_group_name_idx
    ON rongcloud_group (group_name);

CREATE TABLE IF NOT EXISTS rongcloud_group_member (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    group_id   text NOT NULL REFERENCES rongcloud_group (group_id) ON DELETE CASCADE,
    user_id    text NOT NULL,
    nickname   text NOT NULL DEFAULT '',
    role       text NOT NULL DEFAULT 'member',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (group_id, user_id)
);

-- 「我加入了哪些群」按 user_id 反查（旧后端 _handle_group_getMyGroups 的查询）。
CREATE INDEX IF NOT EXISTS rongcloud_group_member_user_idx
    ON rongcloud_group_member (user_id);
