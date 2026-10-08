-- CR-CHAT-007 down-migration: remove the stored human principal.
--
-- Dropping the columns drops every recorded principal_id, which is exactly the
-- pre-CR-CHAT-007 behaviour (the human who spoke as an agent was recorded
-- nowhere). Entries that carried a principal read back as absent after this
-- migration, which the InboxEntry.PrincipalID omitempty encoding spells the
-- same way as "no human" — one representation.
ALTER TABLE inbox_entries
    DROP COLUMN IF EXISTS principal_id;

ALTER TABLE dead_letters
    DROP COLUMN IF EXISTS principal_id;
