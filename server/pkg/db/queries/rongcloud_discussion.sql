-- =====================
-- 圆桌讨论（v3）：讨论实例 / 事件 / 产物
-- =====================
-- 对应迁移 589_rongcloud_discussion_core。这里只放 P4（讨论生命周期）立刻要用的三张
-- 表；逐轮文档、命令 outbox、节点请求、角色推荐、共享投递的查询随各自的消费方
-- （P5/P7）一起加，避免无消费者的空转代码。

-- name: CreateRongCloudDiscussion :one
-- setup_step 仅引导式链路写 'topic'（draft 断点）；旧链路恒传 ''（列默认）。
INSERT INTO rongcloud_discussion (
    discussion_id, chatroom_id, workspace_id, created_by, topic, goal, max_rounds,
    mode, status, role_snapshot, host_config_snapshot, recommendation_id,
    client_request_id, round_policy, round_revision, setup_step
) VALUES (
    @discussion_id, @chatroom_id, @workspace_id, @created_by, @topic, @goal, @max_rounds,
    @mode, @status, @role_snapshot, @host_config_snapshot, @recommendation_id,
    @client_request_id, @round_policy, @round_revision, @setup_step
) RETURNING *;

-- name: GetRongCloudDiscussion :one
SELECT * FROM rongcloud_discussion WHERE discussion_id = $1;

-- name: GetActiveRongCloudDiscussionByChatroom :one
-- configuring 半成品同样视为活跃：与 discussionLiveStatuses / 迁移 595 唯一索引口径一致，
-- 保证同一聊天室「空组直建半成品 + 旧链路 start」互斥，且 cancel action 可作用于半成品。
SELECT * FROM rongcloud_discussion
WHERE chatroom_id = $1
  AND status IN ('configuring', 'planning', 'running', 'summarizing', 'awaiting_user', 'synthesizing')
ORDER BY created_at DESC
LIMIT 1;

-- name: ListRongCloudDiscussionsByChatroom :many
SELECT * FROM rongcloud_discussion
WHERE chatroom_id = $1
ORDER BY created_at DESC;

-- name: ListActiveRongCloudDiscussions :many
SELECT * FROM rongcloud_discussion
WHERE status IN ('planning', 'running', 'summarizing', 'awaiting_user', 'synthesizing')
ORDER BY updated_at ASC;

-- name: UpdateRongCloudDiscussionState :one
-- 乐观锁：仅当 version 未变时才推进，并把 version 加一。并发推进的失败方拿到
-- pgx.ErrNoRows（调用方映射为 409 并回最新快照），这正是前端做重同步所依赖的语义。
UPDATE rongcloud_discussion
SET status         = @status,
    current_round  = @current_round,
    round_revision = @round_revision,
    version        = version + 1,
    updated_at     = now()
WHERE discussion_id = @discussion_id
  AND version = @expected_version
RETURNING *;

-- name: DeleteRongCloudDiscussion :exec
DELETE FROM rongcloud_discussion WHERE discussion_id = $1;

-- name: AppendRongCloudDiscussionEvent :one
-- 新链路的事件写入：只以 discussion_id 为键，chatroom_id / workspace_id 留空
-- （融云 id 与内部 UUID 的映射不渗进事件流）。seq 由调用方在同一事务里取
-- GetRongCloudDiscussionEventMaxSeq + 1 得到。
INSERT INTO rongcloud_discussion_event (
    discussion_id, seq, event_type, actor_id, target_id, payload, idempotency_key, v3_envelope
) VALUES (
    @discussion_id, @seq, @event_type, @actor_id, @target_id, @payload, @idempotency_key, @v3_envelope
) RETURNING *;

-- name: ListRongCloudDiscussionEventsAfterSeq :many
SELECT * FROM rongcloud_discussion_event
WHERE discussion_id = $1
  AND seq > $2
ORDER BY seq ASC;

-- name: GetRongCloudDiscussionEventMaxSeq :one
SELECT COALESCE(MAX(seq), 0)::bigint AS max_seq
FROM rongcloud_discussion_event
WHERE discussion_id = $1;

-- name: UpsertRongCloudArtifact :one
-- 一个讨论每种 artifact_type 只有一行（UNIQUE(discussion_id, artifact_type)）。合成
-- 结果可能被多次更新，version 递增、base_version 记录它基于哪一版。
INSERT INTO rongcloud_artifact (
    artifact_id, discussion_id, artifact_type, title, content, version, base_version
) VALUES (
    @artifact_id, @discussion_id, @artifact_type, @title, @content, @version, @base_version
)
ON CONFLICT (discussion_id, artifact_type) DO UPDATE SET
    title        = EXCLUDED.title,
    content      = EXCLUDED.content,
    version      = EXCLUDED.version,
    base_version = EXCLUDED.base_version,
    updated_at   = now()
RETURNING *;

-- name: GetRongCloudArtifact :one
SELECT * FROM rongcloud_artifact WHERE artifact_id = $1;

-- name: GetLatestRongCloudArtifactByDiscussion :one
SELECT * FROM rongcloud_artifact
WHERE discussion_id = $1
ORDER BY updated_at DESC
LIMIT 1;

-- name: GetClawIMUserByUserID :one
-- P4：C 端真人的身份兜底。claw 注册时融云 id = user_id 本身，但这批账号不在
-- rongcloud_user 镜像表里（clawRong 直接调融云 API 不写镜像），所以 service=
-- discussion 的 actor 解析必须同时能查到这张表。讨论身份域统一用融云侧 TEXT id，
-- 对 claw 用户而言就是 user_id。
SELECT user_id, username, nickname, portrait_uri, status
FROM claw_im_users
WHERE user_id = $1 AND status = 'active'
LIMIT 1;

-- name: ListActiveRongCloudDiscussionTargets :many
-- P4：事件广播目标 = 该讨论所属聊天室的启用成员（AI 成员的融云账号）+ owner
-- （融云 id）。host_turn_requested 不广播（只私发给 host），其余事件广播给
-- 聊天室。这里取出 AI 成员的 rc 账号用于私发 your_turn / discussion_cancel。
SELECT n.rongcloud_user_id AS node_rc_user_id, m.member_id, m.member_type, m.enabled
FROM rongcloud_chatroom_member m
JOIN rongcloud_chatroom c ON c.id = m.chatroom_id
JOIN rongcloud_node n ON n.id = m.node_id
WHERE c.rongcloud_chatroom_id = $1 AND m.enabled = TRUE;

-- name: GetRongCloudChatroomViewByRongCloudID :one
-- P4：讨论的 room access 校验（status 不过滤 active：讨论存在即可访问历史）。
SELECT c.*,
       u.rongcloud_user_id AS owner_rc_user_id,
       n.node_id          AS host_business_node_id
FROM rongcloud_chatroom c
LEFT JOIN rongcloud_user  u ON u.id = c.owner_user_id
LEFT JOIN rongcloud_node  n ON n.id = c.host_node_id
WHERE c.rongcloud_chatroom_id = $1;

-- name: GetFirstActiveRongCloudInstallationWorkspace :one
-- P4：claw 用户镜像行补建时需要的 workspace 归属（单安装假设，与 HTTP 层
-- clawSingleInstallWorkspace 同一查询）。
SELECT ci.workspace_id
FROM channel_installation ci
JOIN workspace w ON w.id = ci.workspace_id
WHERE ci.status = 'active' AND ci.channel_type = 'rongcloud'
ORDER BY ci.created_at ASC
LIMIT 1;

-- name: GetRoundtableCreateRequest :one
SELECT * FROM rongcloud_roundtable_create_request
WHERE user_id = $1 AND client_request_id = $2;

-- name: InsertRoundtableCreateRequest :one
-- 幂等 claim：同 (user_id, client_request_id) 只插一行；唯一冲突时返回既存行。
INSERT INTO rongcloud_roundtable_create_request (user_id, client_request_id, recommendation_id, status, request_payload, request_payload_hash, chatroom_id, discussion_id)
VALUES ($1, $2, $3, 'pending', $4, $5, $6, $7)
ON CONFLICT (user_id, client_request_id) DO UPDATE SET updated_at = now()
RETURNING *;

-- name: TransitionRoundtableCreateRequest :one
-- CAS 推进：只有 expected_status 匹配时才推进到 next_status。
UPDATE rongcloud_roundtable_create_request
SET status = $3, chatroom_id = COALESCE($4, chatroom_id), discussion_id = COALESCE($5, discussion_id), error_code = NULL, updated_at = now()
WHERE user_id = $1 AND client_request_id = $2 AND status = $6
RETURNING *;

-- name: InsertRongCloudRoleRecommendation :one
-- 落推荐结果：request_id 唯一约束兜底重复回包。
INSERT INTO rongcloud_role_recommendation (recommendation_id, request_id, user_id, host_node_id, host_ai_user_id, config_version, topic, goal, roles, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (request_id) DO NOTHING
RETURNING *;

-- name: GetRongCloudRoleRecommendation :one
SELECT * FROM rongcloud_role_recommendation WHERE recommendation_id = $1;

-- name: GetRongCloudRoleRecommendationByRequest :one
SELECT * FROM rongcloud_role_recommendation WHERE request_id = $1;

-- name: ListRongCloudRoundDocumentsForDiscussion :many
SELECT * FROM rongcloud_discussion_round_document
WHERE discussion_id = $1 ORDER BY round ASC, revision DESC;

-- name: GetRongCloudRoundDocument :one
SELECT * FROM rongcloud_discussion_round_document WHERE document_id = $1;

-- name: GetRongCloudArtifactByID :one
SELECT * FROM rongcloud_artifact WHERE artifact_id = $1;

-- name: GetRoundtableCreateRequestByChatroom :one
SELECT * FROM rongcloud_roundtable_create_request WHERE chatroom_id = $1 ORDER BY updated_at DESC LIMIT 1;

-- name: GetRongCloudDiscussionEventByIdempotency :one
-- P7：交互式轮次的所有推进都按 idempotency_key 做重放保护（融云回调 at-least-once、
-- 设备重连会整段重放离线历史），落事件前先按幂等键查一次就能把重放变成无操作。
SELECT * FROM rongcloud_discussion_event
WHERE discussion_id = $1 AND idempotency_key = $2;

-- name: InsertRongCloudDiscussionCommand :one
-- P7 outbox：派发（kind=dispatch）/ 取消（kind=cancel）先落库再发送，进程重启不丢。
-- command_id 就是 v3 信封里的 requestId —— 设备回包按它索引到这条命令。
INSERT INTO rongcloud_discussion_command (
    command_id, discussion_id, request_id, kind, payload, available_at
) VALUES (
    @command_id, @discussion_id, @request_id, @kind, @payload, @available_at
)
ON CONFLICT (command_id) DO NOTHING
RETURNING *;

-- name: GetRongCloudDiscussionCommand :one
SELECT * FROM rongcloud_discussion_command WHERE command_id = $1;

-- name: SettleRongCloudDiscussionCommand :execrows
-- 命令终结：设备回包或判定超时后把命令置为终结态（acknowledged / failed）。
UPDATE rongcloud_discussion_command
SET state = @state, lease_until = NULL
WHERE command_id = @command_id;

-- name: ListRongCloudDiscussionCommandsByDiscussion :many
-- 取该讨论的全部命令，供「当前活跃派发」判定（引擎按 payload->>'round' 与 state 过滤）。
SELECT * FROM rongcloud_discussion_command
WHERE discussion_id = $1
ORDER BY created_at ASC, command_id ASC;

-- name: ListDueRongCloudDiscussionCommands :many
-- P7 恢复扫描：到期的未终结派发命令（available_at 在派发时被置为「超时时刻」）。
SELECT * FROM rongcloud_discussion_command
WHERE kind = 'dispatch'
  AND state IN ('pending', 'sent')
  AND available_at <= now()
ORDER BY available_at ASC
LIMIT $1;

-- name: InsertRongCloudRoundDocument :one
-- P7：主持人的本轮纪要。唯一键 (discussion_id, round, revision) 让重放变无操作。
INSERT INTO rongcloud_discussion_round_document (
    document_id, discussion_id, round, revision, source_request_id, title, content
) VALUES (
    @document_id, @discussion_id, @round, @revision, @source_request_id, @title, @content
)
ON CONFLICT (discussion_id, round, revision) DO NOTHING
RETURNING *;

-- name: GetRongCloudRoundDocumentByIdentity :one
-- 幂等查回：同一 source_request_id 或同一 (round, revision) 的既存文档。
SELECT * FROM rongcloud_discussion_round_document
WHERE discussion_id = $1
  AND (source_request_id = $2 OR (round = $3 AND revision = $4))
ORDER BY created_at ASC
LIMIT 1;

-- name: InsertRongCloudNodeRequest :one
-- P7：服务器→节点请求-应答的持久记录（角色推荐经此下发）。
INSERT INTO rongcloud_node_request (
    request_id, request_kind, requester_user_id, target_node_id, target_ai_user_id,
    status, request_payload, expires_at
) VALUES (
    @request_id, @request_kind, @requester_user_id, @target_node_id, @target_ai_user_id,
    @status, @request_payload, @expires_at
)
ON CONFLICT (request_id) DO NOTHING
RETURNING *;

-- name: UpdateRongCloudNodeRequestResult :execrows
UPDATE rongcloud_node_request
SET status = @status,
    response_payload = @response_payload,
    error_code = @error_code,
    updated_at = now()
WHERE request_id = @request_id;
