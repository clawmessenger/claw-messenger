-- 605 的回滚：这张表只是「会话号映射」的副本，融云 SDK 自带会话号，删掉
-- 不会让任何会话不可用 —— conversation/save 会重新走 404「未知的服务或操作」。
DROP TABLE IF EXISTS im_conversations;
