CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS usage_doc_workspace_slug_idx ON usage_doc (COALESCE(workspace_id, '00000000-0000-0000-0000-000000000000'::uuid), slug);
