-- RongCloud integration tables (Phase 2a). No foreign keys (MUL-3515 §4);
-- integrity is enforced in the application layer. See Phase 2a design spec
-- for the entity relationships these tables model.

CREATE TABLE IF NOT EXISTS rongcloud_user (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL,
    rongcloud_user_id   TEXT NOT NULL UNIQUE,
    name                TEXT,
    portrait_uri        TEXT,
    token_encrypted     TEXT,
    is_system_reserved  BOOLEAN NOT NULL DEFAULT FALSE,
    is_ai_node          BOOLEAN NOT NULL DEFAULT FALSE,
    node_type           TEXT NOT NULL DEFAULT 'human' CHECK (node_type IN ('system', 'ai', 'human')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rongcloud_node (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL,
    owner_user_id       UUID NOT NULL,
    rongcloud_user_id   TEXT NOT NULL UNIQUE,
    node_id             TEXT NOT NULL,
    ai_type             TEXT,
    capabilities        JSONB NOT NULL DEFAULT '{}',
    deploy_status       TEXT NOT NULL DEFAULT 'offline' CHECK (deploy_status IN ('online', 'offline', 'error')),
    binding_version     INT NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rongcloud_chatroom (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL,
    rongcloud_chatroom_id TEXT NOT NULL UNIQUE,
    owner_user_id       UUID NOT NULL,
    host_node_id        UUID,
    max_rounds          INT NOT NULL DEFAULT 0,
    conversation_kind   TEXT,
    config              JSONB NOT NULL DEFAULT '{}',
    status              TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deleted')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rongcloud_chatroom_member (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chatroom_id         UUID NOT NULL,
    node_id             UUID,
    member_type         TEXT NOT NULL CHECK (member_type IN ('user', 'ai')),
    role_name           TEXT,
    role_instructions   TEXT,
    capabilities        JSONB NOT NULL DEFAULT '{}',
    model               TEXT,
    speaking_order      INT,
    enabled             BOOLEAN NOT NULL DEFAULT TRUE,
    discussion_model    TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (chatroom_id, node_id)
);

CREATE TABLE IF NOT EXISTS rongcloud_device (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id                UUID NOT NULL,
    owner_user_id               UUID NOT NULL,
    node_id                     UUID NOT NULL,
    device_name                 TEXT NOT NULL,
    device_type                 TEXT,
    credential_id               TEXT,
    credential_secret_encrypted TEXT,
    status                      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'deleted')),
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rongcloud_pairing_session (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL,
    ticket              TEXT NOT NULL UNIQUE,
    status              TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'claimed', 'expired', 'cancelled')),
    client_claim_key    TEXT,
    idempotency_key     TEXT,
    candidate_node_ids  JSONB NOT NULL DEFAULT '[]',
    expires_at          TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rongcloud_node_model_catalog (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    node_id     UUID NOT NULL,
    model_id    TEXT NOT NULL,
    provider    TEXT,
    model_name  TEXT,
    config      JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (node_id, model_id)
);

CREATE TABLE IF NOT EXISTS rongcloud_system_config (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL,
    config_key      TEXT NOT NULL,
    node_id         UUID,
    config          JSONB NOT NULL DEFAULT '{}',
    config_version  INT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, config_key)
);
