-- COURIER-TAGS (CRIER-TAGS-1) down-migration: drop the stored message tags.
--
-- Dropping the column drops every recorded tag, which is exactly the
-- pre-CRIER-TAGS-1 behaviour: the message still exists, and only the
-- classification it carried is gone. No message is deleted and no other
-- column is touched.
ALTER TABLE inbox_entries
    DROP COLUMN IF EXISTS tags;
