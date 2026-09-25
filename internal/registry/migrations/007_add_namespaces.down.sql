ALTER TABLE dead_letters
    DROP CONSTRAINT IF EXISTS dead_letters_namespace_not_blank,
    DROP COLUMN IF EXISTS namespace;

ALTER TABLE inbox_entries
    DROP CONSTRAINT IF EXISTS inbox_entries_namespace_not_blank,
    DROP COLUMN IF EXISTS namespace;

ALTER TABLE agents
    DROP CONSTRAINT IF EXISTS agents_namespace_not_blank,
    DROP COLUMN IF EXISTS namespace;
