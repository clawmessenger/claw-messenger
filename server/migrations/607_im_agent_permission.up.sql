-- 绑定智能体的使用权限（私聊 / 群聊 / 讨论组 / 聊天室 / 读写文件）。
--
-- 为什么落库而不是留在设备本地：这五个开关是**主人对自己 agent 的授权**，
-- 而「谁能拉它干活」这件事发生在服务端（群聊中继、聊天室中继、讨论组 v3 派发
-- 都按成员表/角色板扇出，与设备本机配置无关）。放在设备上就等于把闸门交给
-- 被闸门约束的那一端 —— 分享出去的 agent 被别人的群拉走时，设备完全不知情。
--
-- 为什么在 rongcloud_node 而不是新表：权限是**节点属性**（一个 agent 一行节点），
-- 读路径天然已经在手（resolveNode / ListRongCloudNodesByMachineID 都是 SELECT *）。
-- 新表会让讨论组每轮每人多一次 SELECT —— 8 人 × 10 轮就是 80 次，而这个判定
-- 是「每轮每人」都要做的。加列之后批量读一次 map 即可。
--
-- 默认全关、存量全开：
--   * 默认关是新语义 —— 新绑定的 agent 不该默认就能被任何人拉进群/讨论组；
--   * 但存量必须 backfill 全开。上线瞬间如果把在用 agent 全判成「无权限」，
--     所有群聊/讨论组会集体不响应，用户看到的是「服务挂了」而不是「权限关了」。
--     存量 agent 的授权事实上早已由「被主人拉进这些房间」表达过，这里只是
--     把这层既有事实补记成显式状态。
--
-- behavior_prompt 是每个 agent 的行为规范（每轮注入 prompt 的固定段落），
-- 空串表示「用服务端内置默认模板」，默认值因此是 '' 而不是模板正文：
-- 模板改文案时，所有没自定义过的 agent 应该跟着一起变。

ALTER TABLE rongcloud_node
  ADD COLUMN IF NOT EXISTS allow_private    BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS allow_group      BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS allow_discussion BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS allow_chatroom   BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS allow_fs         BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS behavior_prompt  TEXT    NOT NULL DEFAULT '';

COMMENT ON COLUMN rongcloud_node.allow_private IS
  '主人是否允许该智能体处理私聊消息（设备端据此判定，见 /api/claw/device/control 的 ownerUserId）。';
COMMENT ON COLUMN rongcloud_node.allow_group IS
  '主人是否允许该智能体在普通群聊（融云 group）中按序发言。';
COMMENT ON COLUMN rongcloud_node.allow_discussion IS
  '主人是否允许该智能体加入讨论组并参与正式轮次（v3 圆桌）。';
COMMENT ON COLUMN rongcloud_node.allow_chatroom IS
  '主人是否允许该智能体在聊天室（非讨论态）里参与闲聊接龙。';
COMMENT ON COLUMN rongcloud_node.allow_fs IS
  '主人是否允许该智能体读写本机文件（设备端执行权限，服务端只负责下发）。';
COMMENT ON COLUMN rongcloud_node.behavior_prompt IS
  '该智能体的行为规范，每轮注入提示词；空串表示使用服务端默认模板。';

-- 存量 AI 节点 backfill 全开（ai_type 为空的是机器主节点，不是 agent，不动）。
-- COALESCE/NULLIF 与全仓「AI 节点」判定口径一致（见 clawGroupListAiMembers）。
UPDATE rongcloud_node
   SET allow_private=true, allow_group=true, allow_discussion=true,
       allow_chatroom=true, allow_fs=true
 WHERE COALESCE(NULLIF(ai_type,''),'') <> '';
