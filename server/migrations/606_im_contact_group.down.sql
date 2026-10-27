-- 606 的回滚：分组是纯偏好数据，删掉即回到「所有设备都在默认分组里」的旧行为。
-- 先删成员表再删分组表 —— 虽然 ON DELETE CASCADE 也会带走成员，但显式顺序
-- 让「down 只依赖自己创建的对象」这件事可读。
--
-- 注意：回滚后前端会退回到「分组接口 404 → 只用本地缓存」的降级路径，
-- 不需要同时回滚前端（见 claw_contact_group.go 顶部说明）。
DROP INDEX IF EXISTS idx_im_contact_group_member_group;
DROP TABLE IF EXISTS im_contact_group_member;

DROP INDEX IF EXISTS idx_im_contact_group_user;
DROP INDEX IF EXISTS uq_im_contact_group_name;
DROP INDEX IF EXISTS uq_im_contact_group_slot;
DROP TABLE IF EXISTS im_contact_group;
