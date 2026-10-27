-- 通讯录分组：让「运维 / 我的 Agent / 自定义分组」跨设备同步。
--
-- 为什么落库而不是 localStorage：用户明确要求换电脑、换手机也能看到自己分的组。
-- 一个人可能有多台设备、多个 agent，分组是账号级偏好，不是某台机器的本地状态。
--
-- 两张表的分工：
--   im_contact_group         分组本身（名字、排序、是不是默认分组）
--   im_contact_group_member  归属关系。主键 (user_id, node_id) 决定**一台设备
--                            最多属于一个分组**；行不存在 = 落在默认的
--                            「我的 Agent」分组里，不需要为它写一行。
--
-- slot 是默认分组的稳定标识：用户能把「运维」改成任何名字，但前端要靠 slot
-- 认出「这是那个不能删的默认分组」。自定义分组 slot 为 NULL。
--
-- 「运维」分组刻意**不存成员**：它是视图 —— 按机器把全部 agent 节点聚合起来，
-- 由前端从 /api/claw/nodes 现算（同机 agent 共享 machine_name）。这一行只用来
-- 保存用户改过的名字，所以 members 恒为空。

CREATE TABLE IF NOT EXISTS im_contact_group (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     TEXT NOT NULL,
    name        TEXT NOT NULL,
    slot        TEXT,
    sort_order  INT  NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT im_contact_group_slot_check
        CHECK (slot IS NULL OR slot IN ('maintenance', 'my_agent')),
    -- 空名在前端是不可见的空行、在服务端是「哪个分组」说不清，两头都不该存在。
    CONSTRAINT im_contact_group_name_check
        CHECK (length(btrim(name)) > 0 AND length(name) <= 24)
);

COMMENT ON TABLE im_contact_group IS
  '通讯录分组（运维 / 我的 Agent / 用户自建）。slot 非空的是默认分组，不可删除。';
COMMENT ON COLUMN im_contact_group.slot IS
  '默认分组标识：maintenance=运维（按机器聚合的视图，不存成员）、my_agent=我的 Agent。NULL=用户自建。';
COMMENT ON COLUMN im_contact_group.sort_order IS
  '显式排序。不靠 created_at 兜底：默认分组要先于自建分组出现，而它们可能是后创建的。';

-- 每个用户每种默认分组最多一行。部分索引：自定义分组 slot 为 NULL，不参与唯一。
CREATE UNIQUE INDEX IF NOT EXISTS uq_im_contact_group_slot
    ON im_contact_group (user_id, slot)
    WHERE slot IS NOT NULL;

-- 同一用户分组名不重复：微信桌面端也不允许，重复名会让「把设备移到 XX 组」
-- 变成一次二选一的猜谜。
CREATE UNIQUE INDEX IF NOT EXISTS uq_im_contact_group_name
    ON im_contact_group (user_id, name);

CREATE INDEX IF NOT EXISTS idx_im_contact_group_user
    ON im_contact_group (user_id, sort_order, created_at);

CREATE TABLE IF NOT EXISTS im_contact_group_member (
    user_id     TEXT NOT NULL,
    node_id     TEXT NOT NULL,
    group_id    UUID NOT NULL REFERENCES im_contact_group(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, node_id)
);

COMMENT ON TABLE im_contact_group_member IS
  '设备（agent 节点）→ 分组的归属。行不存在表示落在默认「我的 Agent」分组。';
COMMENT ON COLUMN im_contact_group_member.node_id IS
  'rongcloud_node.node_id（与 /api/claw/nodes 返回的 node_id 同源，不是融云 id）。';

CREATE INDEX IF NOT EXISTS idx_im_contact_group_member_group
    ON im_contact_group_member (group_id);
