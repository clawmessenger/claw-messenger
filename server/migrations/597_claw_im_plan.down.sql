-- 回滚 597：删除套餐表与 claw_im_users.plan_id 列。

ALTER TABLE claw_im_users DROP COLUMN IF EXISTS plan_id;
DROP TABLE IF EXISTS claw_im_plan;
