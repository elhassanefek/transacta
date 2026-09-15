INSERT INTO permissions (name, description) VALUES
    ('audit:read', 'View the tenant''s audit trail');

-- admin only: the audit trail includes every user's actions within the
-- tenant, not just the caller's own, so it's deliberately not granted to
-- viewer or service by default.
INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM roles r, permissions p
WHERE r.name = 'admin' AND p.name = 'audit:read';
