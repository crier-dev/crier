-- CR-CHAT-020: per-message delivery latency on the inbox entry.
--
-- CHAT-INTERFACE.md §4 row 18: the UI draws a per-message latency figure
-- ("⚡ 86ms") and the bus had no per-delivery timing to back it. The deliver
-- handler measures the whole POST (decode → guard → durable write) and
-- records the whole milliseconds on the entry; Retrieve hands the stored
-- value back unchanged, so a client reading GET /agents/{id}/inbox sees the
-- latency of the delivery that produced the message, not of its own read.
--
-- Nullable and written once at delivery time: a message that entered the
-- inbox another way (the federation hold queue, a webhook-failure notice, a
-- receipt) reads back NULL, which the entry's omitempty encoding renders as
-- absent — "not measured", never a fabricated zero.
ALTER TABLE inbox_entries
    ADD COLUMN delivery_ms BIGINT NULL;
