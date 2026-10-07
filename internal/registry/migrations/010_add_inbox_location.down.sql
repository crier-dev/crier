-- CR-CHAT-029 down-migration: remove the stored delivery location.
--
-- Dropping the column drops every recorded location, which is exactly the
-- pre-CR-CHAT-029 behaviour (the location is derived on delivery and lost
-- when the message is stored).
ALTER TABLE inbox_entries
    DROP COLUMN IF EXISTS location;
