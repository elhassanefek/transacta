DELETE FROM role_permissions
WHERE permission_id = (SELECT id FROM permissions WHERE name = 'audit:read');

DELETE FROM permissions WHERE name = 'audit:read';
