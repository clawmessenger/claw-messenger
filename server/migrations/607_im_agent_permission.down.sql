-- 回滚绑定智能体权限：删掉 607 加的全部列。
--
-- 只删列，不做「反向 backfill」：默认值随列一起消失，没有需要复原的中间状态。
-- 回滚后的行为等价于「服务端从没引入过权限维度」—— 派发侧会走回加列前的逻辑
-- （不看权限），这与 down 的语义一致。若派发侧代码先于迁移回滚，读不到列会直接
-- 报 SQL 错误而不是静默放行，所以回滚顺序必须是：先回滚代码，再 down 迁移。

ALTER TABLE rongcloud_node
  DROP COLUMN IF EXISTS allow_private,
  DROP COLUMN IF EXISTS allow_group,
  DROP COLUMN IF EXISTS allow_discussion,
  DROP COLUMN IF EXISTS allow_chatroom,
  DROP COLUMN IF EXISTS allow_fs,
  DROP COLUMN IF EXISTS behavior_prompt;
