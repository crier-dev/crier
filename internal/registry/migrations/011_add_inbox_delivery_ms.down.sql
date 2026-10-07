-- CR-CHAT-020 down-migration: remove the per-message delivery latency.
--
-- Dropping the column drops every recorded delivery_ms, which is exactly the
-- pre-CR-CHAT-020 behaviour (the latency is measured on the deliver path and
-- is not recorded). Entries that already carried a delivery_ms read back as
-- absent after this migration, which the InboxEntry.DeliveryMs omitempty
-- encoding spells the same way as "never measured" — one representation.
ALTER TABLE inbox_entries
    DROP COLUMN IF EXISTS delivery_ms;
