-- 603 的回滚：审批表是「申请-同意」状态的唯一载体，删表即回到 501 的旧行为
-- （friend/sendRequest 等四个 action 会重新走「暂不支持」分支）。
-- 已建立的好友边不会被回收 —— 它们是融云侧的边，本表只是审批过程的记录。
DROP INDEX IF EXISTS idx_im_friend_request_from_user;
DROP INDEX IF EXISTS idx_im_friend_request_to_user;
DROP INDEX IF EXISTS uq_im_friend_request_pending;
DROP TABLE IF EXISTS im_friend_request;
