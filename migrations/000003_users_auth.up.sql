CREATE TABLE roles (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE permissions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE, -- e.g. 'transactions:write'
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE role_permissions (
    role_id       UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    permission_id UUID NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
    PRIMARY KEY (role_id, permission_id)
);

CREATE INDEX idx_role_permissions_permission ON role_permissions(permission_id);

CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    role_id       UUID NOT NULL REFERENCES roles(id),
    email         TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Composite unique, not just a PK on id alone: this is what lets
    -- refresh_tokens below reference (id, tenant_id) together as a
    -- single FK target, which is what makes the tenant-consistency
    -- guarantee possible.
    CONSTRAINT uq_users_id_tenant UNIQUE (id, tenant_id)
);

-- Case-insensitive uniqueness: 'john@acme.com' and 'John@acme.com' must
-- not be treated as two different users under the same tenant.
CREATE UNIQUE INDEX uq_users_tenant_lower_email ON users (tenant_id, LOWER(email));
CREATE INDEX idx_users_tenant ON users(tenant_id);
CREATE INDEX idx_users_role ON users(role_id);

CREATE TABLE refresh_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL,
    tenant_id   UUID NOT NULL,
    token_hash  TEXT NOT NULL UNIQUE,
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'rotated', 'revoked')),
    -- ON DELETE SET NULL: a future cleanup job deleting old rotated/
    -- revoked tokens must not fail with a FK violation just because an
    -- older token's replaced_by still points at the row being deleted.
    replaced_by UUID REFERENCES refresh_tokens(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    CONSTRAINT fk_refresh_tokens_user_tenant
        FOREIGN KEY (user_id, tenant_id) REFERENCES users(id, tenant_id) ON DELETE CASCADE
);

CREATE INDEX idx_refresh_tokens_user ON refresh_tokens(user_id);
CREATE INDEX idx_refresh_tokens_tenant ON refresh_tokens(tenant_id);

-- Starter roles/permissions so a fresh install is usable immediately.
-- These are seed data, not a hardcoded schema constraint -- more roles
-- and permissions can be inserted later without touching this migration.
INSERT INTO roles (name, description) VALUES
    ('admin',   'Full access within the tenant, including user management'),
    ('service', 'Programmatic access to transaction/account operations'),
    ('viewer',  'Read-only access within the tenant');

INSERT INTO permissions (name, description) VALUES
    ('transactions:read',  'View transactions and entries'),
    ('transactions:write', 'Execute transfers and post/fail transactions'),
    ('accounts:read',      'View accounts and balances'),
    ('accounts:write',     'Create accounts'),
    ('users:manage',       'Create, disable, and manage users within the tenant');

INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM roles r, permissions p
WHERE (r.name = 'admin')
   OR (r.name = 'service' AND p.name IN ('transactions:read', 'transactions:write', 'accounts:read', 'accounts:write'))
   OR (r.name = 'viewer'  AND p.name IN ('transactions:read', 'accounts:read'));