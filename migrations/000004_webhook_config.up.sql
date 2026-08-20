ALTER TABLE tenants
    ADD COLUMN webhook_url    TEXT CHECK (webhook_url IS NULL OR webhook_url LIKE 'https://%'),
    ADD COLUMN webhook_secret TEXT;

ALTER TABLE webhook_events
    ADD COLUMN last_error TEXT;