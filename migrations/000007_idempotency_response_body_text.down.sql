ALTER TABLE idempotency_keys ALTER COLUMN response_body TYPE JSONB USING response_body::jsonb;
