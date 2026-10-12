-- 圆桌移植 P3：service=chatroom 命令通道用的查询。
--
-- 设计要点：命令通道以 **rongcloud_chatroom_id（TEXT 业务键）** 为入口，
-- 而非内部 UUID。所有 lookup / mutate 都按这个键走，避免 Go 既有
-- GetRongCloudChatroomByID（按 UUID）那种用不上的查询。
-- ownerId 在响应里要回融云 user id（TEXT），所以 chatroom 查询带 join
-- rongcloud_user；hostNodeId 要回 TEXT node_id，所以 join rongcloud_node。

-- name: GetActiveRongCloudChatroomViewByRongCloudID :one
SELECT c.*,
       u.rongcloud_user_id AS owner_rc_user_id,
       n.node_id          AS host_business_node_id
FROM rongcloud_chatroom c
LEFT JOIN rongcloud_user  u ON u.id = c.owner_user_id
LEFT JOIN rongcloud_node  n ON n.id = c.host_node_id
WHERE c.rongcloud_chatroom_id = $1 AND c.status = 'active';

-- name: ListRongCloudChatroomViewsForUser :many
SELECT DISTINCT c.*,
       u.rongcloud_user_id AS owner_rc_user_id,
       n.node_id          AS host_business_node_id
FROM rongcloud_chatroom c
LEFT JOIN rongcloud_user          u ON u.id = c.owner_user_id
LEFT JOIN rongcloud_node          n ON n.id = c.host_node_id
LEFT JOIN rongcloud_chatroom_member m ON m.chatroom_id = c.id
WHERE c.status = 'active'
  AND (c.owner_user_id = @owner_user_id
       OR (m.member_id = @member_id AND m.member_id IS NOT NULL AND m.enabled = true))
ORDER BY c.created_at DESC;

-- name: UpdateRongCloudChatroomProfileByRongCloudID :one
UPDATE rongcloud_chatroom
SET name = @name, portrait_uri = @portrait_uri, max_rounds = @max_rounds, updated_at = now()
WHERE rongcloud_chatroom_id = @rongcloud_chatroom_id AND status = 'active'
RETURNING *;

-- name: SetRongCloudChatroomHostByRongCloudID :one
UPDATE rongcloud_chatroom
SET host_node_id = @host_node_id, updated_at = now()
WHERE rongcloud_chatroom_id = @rongcloud_chatroom_id AND status = 'active'
RETURNING *;

-- name: SoftDeleteRongCloudChatroomByRongCloudID :exec
UPDATE rongcloud_chatroom SET status = 'deleted', updated_at = now()
WHERE rongcloud_chatroom_id = $1;

-- name: GetRongCloudChatroomMemberByChatroomAndMemberID :one
SELECT * FROM rongcloud_chatroom_member
WHERE chatroom_id = $1 AND member_id = $2 AND member_id IS NOT NULL;

-- name: UpsertRongCloudChatroomMemberByMemberID :one
INSERT INTO rongcloud_chatroom_member (
    chatroom_id, node_id, member_type, role_name, role_instructions,
    capabilities, model, speaking_order, enabled, discussion_model,
    member_id, nickname, portrait_uri
) VALUES (
    @chatroom_id, @node_id, @member_type, @role_name, @role_instructions,
    @capabilities, @model, @speaking_order, @enabled, @discussion_model,
    @member_id, @nickname, @portrait_uri
)
ON CONFLICT (chatroom_id, member_id) WHERE member_id IS NOT NULL DO UPDATE SET
    node_id           = EXCLUDED.node_id,
    member_type       = EXCLUDED.member_type,
    nickname          = EXCLUDED.nickname,
    portrait_uri      = EXCLUDED.portrait_uri,
    role_name         = EXCLUDED.role_name,
    role_instructions = EXCLUDED.role_instructions,
    capabilities      = EXCLUDED.capabilities,
    enabled           = EXCLUDED.enabled,
    discussion_model  = EXCLUDED.discussion_model,
    updated_at        = now()
RETURNING *;

-- name: UpdateRongCloudChatroomMemberRoleByChatroomAndNodeID :one
UPDATE rongcloud_chatroom_member
SET role_name = @role_name, role_instructions = @role_instructions,
    enabled = @enabled, updated_at = now()
WHERE chatroom_id = @chatroom_id AND node_id = @node_id
RETURNING *;

-- name: DeleteRongCloudChatroomMemberByMemberID :exec
DELETE FROM rongcloud_chatroom_member
WHERE chatroom_id = @chatroom_id AND member_id = @member_id AND member_id IS NOT NULL;

-- name: ListRongCloudChatroomMembersWithNode :many
SELECT m.*, n.node_id AS business_node_id, n.rongcloud_user_id AS node_rc_user_id
FROM rongcloud_chatroom_member m
LEFT JOIN rongcloud_node n ON n.id = m.node_id
WHERE m.chatroom_id = $1
ORDER BY m.speaking_order ASC NULLS LAST, m.created_at ASC;
