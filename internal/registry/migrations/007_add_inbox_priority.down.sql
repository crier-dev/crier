-- CR-FEAT-035 down-migration: remove the retrieval priority.
--
-- The index and the constraint go with the column; dropping the column drops
-- every stored priority, which is exactly the pre-CR-FEAT-035 behaviour (a
-- retrieval order of pure arrival order).
DROP INDEX IF EXISTS inbox_entries_priority_idx;

ALTER TABLE inbox_entries
    DROP CONSTRAINT IF EXISTS inbox_entries_priority_range;

ALTER TABLE inbox_entries
    DROP COLUMN IF EXISTS priority;
