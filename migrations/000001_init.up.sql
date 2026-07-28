CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE tenants (
                         id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
                         name        TEXT NOT NULL,
                         api_key_hash TEXT NOT NULL,
                         created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE accounts (
                          id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
                          tenant_id   UUID NOT NULL REFERENCES tenants(id),
                          name        TEXT NOT NULL,
                          created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE transactions (
                              id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
                              tenant_id   UUID NOT NULL REFERENCES tenants(id),
                              status      TEXT NOT NULL DEFAULT 'pending',
                              created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE entries (
                         id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
                         tenant_id UUID NOT NULL REFERENCES tenants(id),
                         transaction_id  UUID NOT NULL REFERENCES transactions(id),
                         account_id      UUID NOT NULL REFERENCES accounts(id),
                         amount_minor    BIGINT NOT NULL, -- positive = credit, negative = debit
                         created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE idempotency_keys (
                                  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
                                  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
                                  key TEXT NOT NULL,
                                  request_hash TEXT NOT NULL,
                                  response_code INT NULL,
                                  response_body JSONB NULL,
                                  status TEXT NOT NULL DEFAULT 'processing' CHECK (status IN ('processing', 'completed')),
                                  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
                                  expires_at TIMESTAMPTZ NOT NULL,
                                  CONSTRAINT idx_tenant_idempotency UNIQUE (tenant_id, key)
);
CREATE TABLE webhook_events (
                                id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
                                tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
                                event_type TEXT NOT NULL,
                                payload JSONB NOT NULL,
                                status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'delivered', 'failed')),
                                attempt_count INT NOT NULL DEFAULT 0,
                                next_retry_at TIMESTAMPTZ NOT NULL DEFAULT now(),
                                created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE audit_log (
                           id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
                           tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
                           action TEXT NOT NULL,
                           actor_id TEXT NOT NULL,
                           payload JSONB NOT NULL,
                           created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_outbox_pending ON webhook_events(status, next_retry_at) WHERE status = 'pending';
CREATE INDEX idx_accounts_tenant ON accounts(tenant_id);
CREATE INDEX idx_transactions_tenant ON transactions(tenant_id);
CREATE INDEX idx_entries_transaction ON entries(transaction_id);
CREATE INDEX idx_entries_account ON entries(account_id);