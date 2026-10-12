-- 圆桌讨论（v3）子系统：核心表。
--
-- 为什么现在建：Go 服务端此前只有一条极简讨论链路 —— 按 speaking_order 固定轮流
-- 点名（rongcloud_discussion_event 直接按聊天室存事件），没有「讨论实例」这个概念。
-- 真正的圆桌（话题/目标、角色快照、乐观锁推进、逐轮文档、产物合成、v3 交互式轮次）
-- 只存在于旧 Python 后端（clawmessenger-server/discussion/，8339 行）。本迁移开始把它
-- 的数据模型搬进 Go 的 public schema。移植计划见 docs/engineering/roundtable-go-port-plan.md。
--
-- 为什么不复用旧后端的 im_* 表：两套 schema 本就在同一实例里并存（im_* 在 admin_test_*
-- schema，Go 在 public），字段风格混杂（驼峰 + 下划线）、归属权不清；且线上 im_discussion_events
-- 只有 13 行测试数据，不值得迁。决策：public 新建表，旧数据仅归档。
--
-- 为什么用 TEXT 业务主键（而不是 rongcloud_* 惯用的 id uuid + 业务键 UNIQUE）：
-- discussion_id / chatroom_id 都是应用侧生成的对外标识，旧后端的表间关系、命令通道契约、
-- C 端前端都直接以这些字符串为准（C 端的 chatroomId 就是融云聊天室 id）。保持 TEXT 主键
-- 能让仓库层 SQL 与旧实现一一对应，显著减少移植期出错。

-- 1. 聊天室补列。旧 im_chatrooms 有 name / portrait_uri，Go 没有 → 聊天室改名、设头像
--    无处可落。max_rounds 已存在，不重复加。
ALTER TABLE rongcloud_chatroom
    ADD COLUMN IF NOT EXISTS name         TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS portrait_uri TEXT NOT NULL DEFAULT '';

-- 2. 成员补列。旧 im_chatroom_members 用 member_id（融云 user id）标识成员，并据此落
--    「人类成员」；Go 之前只有 node_id（UUID）→ 人类成员无法表示（invite/join 需要）。
ALTER TABLE rongcloud_chatroom_member
    ADD COLUMN IF NOT EXISTS member_id    TEXT,
    ADD COLUMN IF NOT EXISTS nickname     TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS portrait_uri TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS rongcloud_chatroom_member_member_id_idx
    ON rongcloud_chatroom_member (member_id);

-- 3. 讨论实例。对应旧 im_discussions。
--    刻意不加外键：与 rongcloud_* 其余表一致，引用完整性由应用层保证（见 564 的头注释）。
CREATE TABLE IF NOT EXISTS rongcloud_discussion (
    discussion_id        TEXT PRIMARY KEY,
    -- 融云聊天室 id（= rongcloud_chatroom.rongcloud_chatroom_id），不是内部 UUID。
    chatroom_id          TEXT NOT NULL,
    workspace_id         UUID NOT NULL,
    created_by           TEXT NOT NULL,
    topic                TEXT NOT NULL DEFAULT '',
    goal                 TEXT NOT NULL DEFAULT '',
    -- user_confirmed 模式没有轮次上限 → 可空（旧后端 012 迁移同样放开了 NOT NULL）。
    max_rounds           INTEGER,
    mode                 TEXT NOT NULL DEFAULT 'moderated'
                              CHECK (mode IN ('moderated', 'roundtable')),
    status               TEXT NOT NULL DEFAULT 'planning' CHECK (status IN (
                             'planning', 'running', 'summarizing', 'awaiting_user',
                             'synthesizing', 'completed', 'cancelled', 'failed')),
    current_round        INTEGER NOT NULL DEFAULT 0,
    -- 乐观锁：所有推进都基于 compare-and-set(version)。冲突方必须回 409 并附最新快照。
    version              INTEGER NOT NULL DEFAULT 1,
    role_snapshot        JSONB NOT NULL DEFAULT '{}'::jsonb,
    host_config_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    recommendation_id    TEXT,
    client_request_id    TEXT,
    round_policy         TEXT NOT NULL DEFAULT 'legacy_capped'
                              CHECK (round_policy IN ('legacy_capped', 'user_confirmed')),
    round_revision       INTEGER NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- legacy_capped 必须有轮次上限，user_confirmed 必须没有（旧后端 012 的同名约束）。
    CONSTRAINT ck_rongcloud_discussion_round_policy CHECK (
        (round_policy = 'legacy_capped' AND max_rounds IS NOT NULL AND max_rounds > 0)
        OR (round_policy = 'user_confirmed' AND max_rounds IS NULL)
    )
);

-- 一个聊天室同一时刻只能有一个活跃讨论（旧后端 uq_im_discussions_one_active）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_rongcloud_discussion_one_active
    ON rongcloud_discussion (chatroom_id)
    WHERE status IN ('planning', 'running', 'summarizing', 'awaiting_user', 'synthesizing');

-- 幂等：同一创建者 + client_request_id 只能有一条（旧后端 uq_im_discussions_creator_request）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_rongcloud_discussion_creator_request
    ON rongcloud_discussion (created_by, client_request_id)
    WHERE client_request_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS rongcloud_discussion_chatroom_idx
    ON rongcloud_discussion (chatroom_id, created_at DESC);
CREATE INDEX IF NOT EXISTS rongcloud_discussion_workspace_idx
    ON rongcloud_discussion (workspace_id, created_at DESC);

-- 4. 事件表升级。旧的 rongcloud_discussion_event 是「按聊天室」的极简流（round_number /
--    speaking_order / content），没有讨论实例、没有顺序号、没有幂等键。新链路需要
--    im_discussion_events 的语义（seq 递增、actor/target、payload、idempotency_key、v3 信封）。
--
--    选择**升级复用**而非另建一张表：两套事件流会让 coordinator 的兼容成本翻倍，而该表
--    线上 0 行，放宽约束没有数据代价。
--
--    chatroom_id / workspace_id 放开 NOT NULL：新链路只以 discussion_id 为键写事件
--    （融云 id 与内部 UUID 的映射不该渗进事件写入），旧链路仍照常填这两个列。
ALTER TABLE rongcloud_discussion_event
    ALTER COLUMN chatroom_id  DROP NOT NULL,
    ALTER COLUMN workspace_id DROP NOT NULL,
    ADD COLUMN IF NOT EXISTS discussion_id   TEXT,
    ADD COLUMN IF NOT EXISTS seq             BIGINT,
    ADD COLUMN IF NOT EXISTS actor_id        TEXT,
    ADD COLUMN IF NOT EXISTS target_id       TEXT,
    ADD COLUMN IF NOT EXISTS payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS idempotency_key TEXT,
    ADD COLUMN IF NOT EXISTS v3_envelope     JSONB;

-- 放开 event_type 白名单：旧 CHECK 只允许 11 种，新链路事件类型更多
-- （host_decision / contribution_* / artifact_updated / round_checkpoint / user_interjection …）。
-- 旧后端的 im_discussion_events 对 event_type 也不设约束，类型合法性由应用层
-- （protocol_v3.go）保证。
ALTER TABLE rongcloud_discussion_event
    DROP CONSTRAINT IF EXISTS rongcloud_discussion_event_event_type_check;

-- 与旧后端 im_discussion_events 的两条唯一约束对齐。discussion_id 为 NULL 的旧行在
-- Postgres 里互不冲突（NULL 视为彼此不同），所以旧链路不受影响。
CREATE UNIQUE INDEX IF NOT EXISTS uq_rongcloud_discussion_event_seq
    ON rongcloud_discussion_event (discussion_id, seq);
CREATE UNIQUE INDEX IF NOT EXISTS uq_rongcloud_discussion_event_idem
    ON rongcloud_discussion_event (discussion_id, idempotency_key);

-- 5. 产物。对应旧 im_artifacts：讨论最终的纪要/报告（host 的 final_synthesis 产出）。
CREATE TABLE IF NOT EXISTS rongcloud_artifact (
    artifact_id   TEXT PRIMARY KEY,
    discussion_id TEXT NOT NULL,
    artifact_type TEXT NOT NULL,
    title         TEXT NOT NULL DEFAULT '',
    content       TEXT NOT NULL DEFAULT '',
    version       INTEGER NOT NULL DEFAULT 1,
    base_version  INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (discussion_id, artifact_type)
);
