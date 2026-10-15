-- CR-FEAT-036 down-migration: remove the stored selection hints and reply
-- correlation.
--
-- Dropping the columns drops every recorded hint and correlation, which is
-- exactly the pre-CR-FEAT-036 behaviour (the dispatch's model/provider hints
-- and the id a result answers were a WIRE tag only — they were lost the moment
-- the delivery was stored). Entries that carried them read back with the
-- members absent after this migration, which the entry's `omitempty` encoding
-- spells the same way as "never sent": one representation for "not recorded".
ALTER TABLE inbox_entries
    DROP COLUMN IF EXISTS selection;

ALTER TABLE inbox_entries
    DROP COLUMN IF EXISTS reply_to;
