-- 讨论组引导式创建：讨论实例新增 configuring 态（draft 语义）+ 引导断点字段。
--
-- 为什么插在状态机最前端：新链路「空组直建」先建房建讨论，再由系统账号以卡片
-- 引导用户填议题/选角色，确认前讨论还不能进入 planning（v3 引擎不得开局）。
-- configuring 与 planning 互斥地占用「一房间一活跃讨论」的唯一索引 —— 半成品
-- 期间同房间不能再开第二个讨论（无论新旧链路）。
--
-- 三个新列的语义：
--   setup_step          引导断点：'' (未开始/已完成) / topic / roles / confirm
--   last_interaction_at 最近一次引导交互（进入房间/提交议题/推荐/确认/心跳），
--                       半成品 GC 只回收它超过 10 分钟的 configuring 讨论；
--   cancel_reason       取消原因（'abandoned'=GC 回收，'user_abandoned'=用户主动放弃）。
--   核对结论：590–592 迁移均无同名列，这里直接新增（DDL 仍 IF NOT EXISTS 兜底）。

ALTER TABLE rongcloud_discussion
    DROP CONSTRAINT IF EXISTS rongcloud_discussion_status_check;
ALTER TABLE rongcloud_discussion
    ADD CONSTRAINT rongcloud_discussion_status_check CHECK (status IN (
        'configuring',  -- 新增：引导配置中（draft 语义），插在 planning 之前
        'planning', 'running', 'summarizing', 'awaiting_user',
        'synthesizing', 'completed', 'cancelled', 'failed'));

ALTER TABLE rongcloud_discussion
    ADD COLUMN IF NOT EXISTS setup_step TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_interaction_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS cancel_reason TEXT NOT NULL DEFAULT '';

-- Postgres 没有 ADD CONSTRAINT IF NOT EXISTS，用 pg_constraint 判定兜底。
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'rongcloud_discussion_setup_step_check') THEN
        ALTER TABLE rongcloud_discussion
            ADD CONSTRAINT rongcloud_discussion_setup_step_check
            CHECK (setup_step IN ('', 'topic', 'roles', 'confirm'));
    END IF;
END $$;

-- 一房间一活跃讨论：configuring 也算活跃（同房间不能有两个半成品）。
DROP INDEX IF EXISTS uq_rongcloud_discussion_one_active;
CREATE UNIQUE INDEX uq_rongcloud_discussion_one_active
    ON rongcloud_discussion (chatroom_id)
    WHERE status IN ('configuring', 'planning', 'running', 'summarizing',
                     'awaiting_user', 'synthesizing');

-- 半成品 GC 扫描索引：只扫 configuring，按最后交互时间过滤。
CREATE INDEX IF NOT EXISTS rongcloud_discussion_configuring_gc_idx
    ON rongcloud_discussion (last_interaction_at)
    WHERE status = 'configuring';
