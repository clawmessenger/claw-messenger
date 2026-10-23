-- 回滚 599：只撤销本迁移新增的对象，不触碰任何既有数据。
DROP INDEX IF EXISTS claw_im_users_wechat_openid_key;
ALTER TABLE claw_im_users DROP COLUMN IF EXISTS wechat_openid;
DROP INDEX IF EXISTS claw_lobster_user_id_idx;
DROP TABLE IF EXISTS claw_lobster;
