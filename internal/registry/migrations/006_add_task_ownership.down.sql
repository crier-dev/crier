-- Reverse of 006_add_task_ownership: drop the dead-letter destination and the
-- two ownership columns. Losing the columns loses only provenance that a
-- pre-CR-FEAT-025 row never had; losing the table loses the dead letters
-- themselves, which is why this direction is a deliberate rollback.
DROP TABLE IF EXISTS dead_letters;

ALTER TABLE inbox_entries
    DROP COLUMN IF EXISTS idempotency_key;

ALTER TABLE inbox_entries
    DROP COLUMN IF EXISTS sender;
