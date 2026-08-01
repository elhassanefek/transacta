ALTER TABLE transactions
    ADD CONSTRAINT chk_tx_status
        CHECK (status IN ('pending', 'posted', 'failed'));

ALTER TABLE entries
    ADD CONSTRAINT chk_entry_amount_nonzero
        CHECK (amount_minor <> 0);

CREATE TABLE dead_letter_events (
                                    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
                                    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
                                    original_event_id UUID NOT NULL,
                                    event_type      TEXT NOT NULL,
                                    payload         JSONB NOT NULL,
                                    attempt_count   INT NOT NULL,
                                    last_error      TEXT,
                                    failed_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_dead_letter_tenant ON dead_letter_events(tenant_id);