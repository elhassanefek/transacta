CREATE INDEX idx_audit_log_tenant_created ON audit_log(tenant_id, created_at DESC);
