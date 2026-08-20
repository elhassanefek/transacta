ALTER TABLE webhook_events
    DROP COLUMN IF EXISTS last_error;

ALTER TABLE tenants
    DROP COLUMN IF EXISTS webhook_secret,
    DROP COLUMN IF EXISTS webhook_url;