-- C 端认领者投影：getDeviceOwner 的 O(1) 读路径。
--
-- 背景：设备归属的权威真相是融云好友边（owner → node 的单向边，node_claim.go
-- 的反查）。融云平台没有「谁加了我」的查询，所以只能对每个 C 端账号各调一次
-- /friend/get.json 再把边翻过来 —— friend/friend_device_owner 走这条路径时是
-- O(N) 次外部请求（单次 10s，客户端 180s 先超时）。
--
-- 本列是该反查结果的**本地投影**（不是新的真相）：写入发生在所有好友边变更处
-- （绑定、管理端转移、解绑/注销、系统主持解绑、手删好友），读取时命中即 O(1)，
-- 未命中再降级走一次反查并把结果惰性回填进来，存量数据因此自愈。
--
-- 类型必须是 TEXT：认领者是 claw_im_users.user_id（583 定义成 TEXT，同时也是
-- 融云 user id）。注意它与 owner_user_id 无关 —— 后者是 multica 工作区管理员
-- （node_claim.go:22-26），不是 C 端认领者。

ALTER TABLE rongcloud_node ADD COLUMN IF NOT EXISTS claimed_by_user_id TEXT;

COMMENT ON COLUMN rongcloud_node.claimed_by_user_id IS
  'C 端认领者（claw_im_users.user_id / 融云 id）。设备归属的本地投影，权威真相仍是融云好友边。';

CREATE INDEX IF NOT EXISTS idx_rongcloud_node_claimed_by
  ON rongcloud_node (claimed_by_user_id);
