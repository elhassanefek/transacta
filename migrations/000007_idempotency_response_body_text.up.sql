-- JSONB canonicalizes on write (re-serializes with its own whitespace,
-- e.g. inserting a space after every ':' and ','), so a cached response
-- body read back out is not byte-identical to what the handler actually
-- returned -- exactly what idempotent replay is supposed to guarantee.
-- The idempotency middleware always treats this column as an opaque
-- byte string (see internal/middleware/idempotency/repository.go), never
-- queries into its JSON structure, so there's no reason to pay that
-- cost. TEXT preserves the response body exactly as written.
ALTER TABLE idempotency_keys ALTER COLUMN response_body TYPE TEXT USING response_body::text;
