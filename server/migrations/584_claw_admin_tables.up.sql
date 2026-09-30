-- 虾说 admin 兼容层：管理员、角色、审计日志表。
CREATE TABLE IF NOT EXISTS claw_admin_users (
    admin_id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    nickname TEXT,
    password_hash TEXT NOT NULL,
    role_id TEXT NOT NULL DEFAULT 'super_admin',
    status TEXT NOT NULL DEFAULT 'active',
    last_login_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS claw_admin_roles (
    role_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    role_name TEXT NOT NULL,
    description TEXT,
    status TEXT NOT NULL DEFAULT 'active',
    permissions TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS claw_admin_audit_logs (
    id BIGSERIAL PRIMARY KEY,
    admin_id TEXT NOT NULL,
    action TEXT NOT NULL,
    resource_type TEXT,
    resource_id TEXT,
    detail JSONB,
    ip_address TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS claw_admin_audit_logs_created_at_idx ON claw_admin_audit_logs (created_at DESC);

-- 默认超管 admin / admin123（PBKDF2-SHA256，与 claw_compat 同格式；哈希由首次启动种子，或用固定值）
INSERT INTO claw_admin_roles (role_id, name, role_name, description, permissions)
VALUES (
    'super_admin', 'super_admin', '超级管理员', '全部权限',
    ARRAY['admin:read','admin:write','role:read','role:write','user:read','user:write',
          'node:read','node:write','group:read','group:write','message:read',
          'system:read','system:write','stats:read','audit:read']
)
ON CONFLICT (role_id) DO NOTHING;
