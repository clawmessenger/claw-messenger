-- RongCloud integration queries (Phase 2a). No foreign keys (MUL-3515 §4).

-- =====================
-- rongcloud_user
-- =====================

-- name: CreateRongCloudUser :one
INSERT INTO rongcloud_user (workspace_id, rongcloud_user_id, name, portrait_uri, token_encrypted, is_system_reserved, is_ai_node, node_type)
VALUES (@workspace_id, @rongcloud_user_id, @name, @portrait_uri, @token_encrypted, @is_system_reserved, @is_ai_node, @node_type)
RETURNING *;

-- name: GetRongCloudUserByRongCloudID :one
SELECT * FROM rongcloud_user WHERE rongcloud_user_id = $1;

-- name: GetRongCloudUserByWorkspaceAndID :one
SELECT * FROM rongcloud_user WHERE workspace_id = $1 AND id = $2;

-- name: ListRongCloudUsersByWorkspace :many
SELECT * FROM rongcloud_user WHERE workspace_id = $1 ORDER BY created_at ASC;

-- name: ListRongCloudSystemUsers :many
SELECT * FROM rongcloud_user WHERE workspace_id = $1 AND is_system_reserved = TRUE ORDER BY created_at ASC;

-- name: UpdateRongCloudUserToken :one
UPDATE rongcloud_user SET token_encrypted = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: UpdateRongCloudUserStatus :one
UPDATE rongcloud_user SET is_system_reserved = $1, is_ai_node = $2, node_type = $3, updated_at = now() WHERE id = $4 RETURNING *;

-- name: DeleteRongCloudUser :exec
DELETE FROM rongcloud_user WHERE id = $1;

-- =====================
-- rongcloud_node
-- =====================

-- name: CreateRongCloudNode :one
INSERT INTO rongcloud_node (workspace_id, owner_user_id, rongcloud_user_id, node_id, ai_type, capabilities, deploy_status, binding_version)
VALUES (@workspace_id, @owner_user_id, @rongcloud_user_id, @node_id, @ai_type, @capabilities, @deploy_status, @binding_version)
RETURNING *;

-- name: GetRongCloudNodeByID :one
SELECT * FROM rongcloud_node WHERE id = $1;

-- name: GetRongCloudNodeByNodeID :one
SELECT * FROM rongcloud_node WHERE node_id = $1;

-- name: GetRongCloudNodeByRongCloudUserID :one
SELECT * FROM rongcloud_node WHERE rongcloud_user_id = $1;

-- name: ListRongCloudNodesByWorkspace :many
SELECT * FROM rongcloud_node WHERE workspace_id = $1 ORDER BY created_at ASC;

-- name: ListRongCloudNodesByOwner :many
SELECT * FROM rongcloud_node WHERE owner_user_id = $1 ORDER BY created_at ASC;

-- name: UpdateRongCloudNodeDeployStatus :one
UPDATE rongcloud_node SET deploy_status = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: UpdateRongCloudNodeCapabilities :one
UPDATE rongcloud_node SET capabilities = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: UpdateRongCloudNodeBindingVersion :one
UPDATE rongcloud_node SET binding_version = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: DeleteRongCloudNode :exec
DELETE FROM rongcloud_node WHERE id = $1;

-- =====================
-- rongcloud_chatroom
-- =====================

-- name: CreateRongCloudChatroom :one
INSERT INTO rongcloud_chatroom (workspace_id, rongcloud_chatroom_id, owner_user_id, host_node_id, max_rounds, conversation_kind, config, status)
VALUES (@workspace_id, @rongcloud_chatroom_id, @owner_user_id, @host_node_id, @max_rounds, @conversation_kind, @config, @status)
RETURNING *;

-- name: GetRongCloudChatroomByID :one
SELECT * FROM rongcloud_chatroom WHERE id = $1;

-- name: GetRongCloudChatroomByRongCloudID :one
SELECT * FROM rongcloud_chatroom WHERE rongcloud_chatroom_id = $1;

-- name: ListRongCloudChatroomsByWorkspace :many
SELECT * FROM rongcloud_chatroom WHERE workspace_id = $1 AND status = 'active' ORDER BY created_at ASC;

-- name: ListRongCloudChatroomsByOwner :many
SELECT * FROM rongcloud_chatroom WHERE owner_user_id = $1 AND status = 'active' ORDER BY created_at ASC;

-- name: UpdateRongCloudChatroom :one
UPDATE rongcloud_chatroom SET host_node_id = @host_node_id, max_rounds = @max_rounds, conversation_kind = @conversation_kind, config = @config, updated_at = now() WHERE id = @id RETURNING *;

-- name: DeleteRongCloudChatroom :exec
UPDATE rongcloud_chatroom SET status = 'deleted', updated_at = now() WHERE id = $1;

-- =====================
-- rongcloud_chatroom_member
-- =====================

-- name: CreateRongCloudChatroomMember :one
INSERT INTO rongcloud_chatroom_member (chatroom_id, node_id, member_type, role_name, role_instructions, capabilities, model, speaking_order, enabled, discussion_model)
VALUES (@chatroom_id, @node_id, @member_type, @role_name, @role_instructions, @capabilities, @model, @speaking_order, @enabled, @discussion_model)
RETURNING *;

-- name: ListRongCloudChatroomMembers :many
SELECT * FROM rongcloud_chatroom_member WHERE chatroom_id = $1 ORDER BY speaking_order ASC NULLS LAST, created_at ASC;

-- name: DeleteRongCloudChatroomMembersByChatroom :exec
DELETE FROM rongcloud_chatroom_member WHERE chatroom_id = $1;

-- name: DeleteRongCloudChatroomMember :exec
DELETE FROM rongcloud_chatroom_member WHERE chatroom_id = $1 AND node_id = $2;

-- =====================
-- rongcloud_device
-- =====================

-- name: CreateRongCloudDevice :one
INSERT INTO rongcloud_device (workspace_id, owner_user_id, node_id, device_name, device_type, credential_id, credential_secret_encrypted, status)
VALUES (@workspace_id, @owner_user_id, @node_id, @device_name, @device_type, @credential_id, @credential_secret_encrypted, @status)
RETURNING *;

-- name: GetRongCloudDeviceByID :one
SELECT * FROM rongcloud_device WHERE id = $1;

-- name: ListRongCloudDevicesByWorkspace :many
SELECT * FROM rongcloud_device WHERE workspace_id = $1 AND status != 'deleted' ORDER BY created_at ASC;

-- name: ListRongCloudDevicesByOwner :many
SELECT * FROM rongcloud_device WHERE owner_user_id = $1 AND status != 'deleted' ORDER BY created_at ASC;

-- name: DeleteRongCloudDevice :exec
UPDATE rongcloud_device SET status = 'deleted', updated_at = now() WHERE id = $1;

-- =====================
-- rongcloud_pairing_session
-- =====================

-- name: CreateRongCloudPairingSession :one
INSERT INTO rongcloud_pairing_session (workspace_id, ticket, status, client_claim_key, idempotency_key, candidate_node_ids, expires_at)
VALUES (@workspace_id, @ticket, @status, @client_claim_key, @idempotency_key, @candidate_node_ids, @expires_at)
RETURNING *;

-- name: GetRongCloudPairingSessionByTicket :one
SELECT * FROM rongcloud_pairing_session WHERE ticket = $1;

-- name: UpdateRongCloudPairingSessionStatus :one
UPDATE rongcloud_pairing_session SET status = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: DeleteRongCloudPairingSession :exec
DELETE FROM rongcloud_pairing_session WHERE id = $1;

-- =====================
-- rongcloud_node_model_catalog
-- =====================

-- name: CreateRongCloudNodeModelCatalog :one
INSERT INTO rongcloud_node_model_catalog (workspace_id, node_id, model_id, provider, model_name, config)
VALUES (@workspace_id, @node_id, @model_id, @provider, @model_name, @config)
RETURNING *;

-- name: ListRongCloudNodeModelCatalogsByNode :many
SELECT * FROM rongcloud_node_model_catalog WHERE node_id = $1 ORDER BY created_at ASC;

-- name: DeleteRongCloudNodeModelCatalog :exec
DELETE FROM rongcloud_node_model_catalog WHERE id = $1;

-- =====================
-- rongcloud_system_config
-- =====================

-- name: UpsertRongCloudSystemConfig :one
INSERT INTO rongcloud_system_config (workspace_id, config_key, node_id, config, config_version)
VALUES (@workspace_id, @config_key, @node_id, @config, @config_version)
ON CONFLICT (workspace_id, config_key) DO UPDATE SET
    node_id = EXCLUDED.node_id,
    config = EXCLUDED.config,
    config_version = EXCLUDED.config_version,
    updated_at = now()
RETURNING *;

-- name: GetRongCloudSystemConfig :one
SELECT * FROM rongcloud_system_config WHERE workspace_id = $1 AND config_key = $2;

-- name: ListRongCloudSystemConfigs :many
SELECT * FROM rongcloud_system_config WHERE workspace_id = $1 ORDER BY config_key ASC;

-- name: DeleteRongCloudSystemConfig :exec
DELETE FROM rongcloud_system_config WHERE workspace_id = $1 AND config_key = $2;

-- =====================
-- rongcloud_discussion_event
-- =====================

-- name: CreateRongCloudDiscussionEvent :one
INSERT INTO rongcloud_discussion_event (
    chatroom_id, workspace_id, event_type, round_number, speaking_order,
    node_id, content, msg_uid
) VALUES (
    @chatroom_id, @workspace_id, @event_type, @round_number, @speaking_order,
    @node_id, @content, @msg_uid
) RETURNING *;

-- name: ListRongCloudDiscussionEventsByChatroom :many
SELECT * FROM rongcloud_discussion_event
WHERE chatroom_id = $1
ORDER BY created_at ASC;
