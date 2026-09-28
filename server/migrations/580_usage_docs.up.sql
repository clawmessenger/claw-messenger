-- In-product usage documentation, edited by workspace admins and read by all
-- members. Global docs (workspace_id IS NULL) apply to every workspace; the
-- application layer treats them as read-only templates.

CREATE TABLE IF NOT EXISTS usage_doc (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID,
    slug                TEXT NOT NULL,
    title               TEXT NOT NULL,
    category            TEXT NOT NULL DEFAULT 'general',
    content             TEXT NOT NULL DEFAULT '',
    sort_order          INT NOT NULL DEFAULT 0,
    status              TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published')),
    created_by          UUID,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
