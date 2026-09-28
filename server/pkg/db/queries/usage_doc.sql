-- usage_doc: in-product usage documentation maintained by workspace admins.
-- Global docs (workspace_id IS NULL) are shared across workspaces; workspace
-- docs are scoped. No foreign keys (repo rule); integrity in app layer.

-- Create a doc scoped to a workspace. Global docs are seeded out-of-band.
-- name: CreateUsageDoc :one
INSERT INTO usage_doc (workspace_id, slug, title, category, content, sort_order, status, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetUsageDocByID :one
SELECT * FROM usage_doc WHERE id = $1;

-- name: GetUsageDocBySlug :one
SELECT * FROM usage_doc
WHERE slug = $1
  AND workspace_id IS NOT DISTINCT FROM $2::uuid
  AND status = 'published';

-- name: GetUsageDocBySlugAnyStatus :one
SELECT * FROM usage_doc
WHERE slug = $1
  AND workspace_id IS NOT DISTINCT FROM $2::uuid;

-- Published docs visible to members: workspace docs plus global docs.
-- name: ListPublishedUsageDocsByWorkspace :many
SELECT * FROM usage_doc
WHERE (workspace_id = $1::uuid OR workspace_id IS NULL)
  AND status = 'published'
ORDER BY category ASC, sort_order ASC, title ASC;

-- All docs (any status) for admin management.
-- name: ListUsageDocsByWorkspace :many
SELECT * FROM usage_doc
WHERE (workspace_id = $1::uuid OR workspace_id IS NULL)
ORDER BY category ASC, sort_order ASC, title ASC;

-- name: UpdateUsageDoc :one
UPDATE usage_doc
SET slug = $2, title = $3, category = $4, content = $5, sort_order = $6, status = $7, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteUsageDoc :execrows
DELETE FROM usage_doc WHERE id = $1;
