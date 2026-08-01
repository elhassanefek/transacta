DROP TABLE IF EXISTS dead_letter_events;

ALTER TABLE entries
DROP CONSTRAINT IF EXISTS chk_entry_amount_nonzero;

ALTER TABLE transactions
DROP CONSTRAINT IF EXISTS chk_tx_status;