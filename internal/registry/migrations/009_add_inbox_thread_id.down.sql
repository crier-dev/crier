-- CR-CHAT-019 down-migration: remove the stored thread id.
--
-- Dropping the column drops every recorded thread, which is exactly the
-- pre-CR-CHAT-019 behaviour (the thread is a wire tag only and is lost when the
-- delivery is stored).
ALTER TABLE inbox_entries
    DROP COLUMN IF EXISTS thread_id;
