-- CR-FEAT-035: retrieval priority on inbox messages.
--
-- A delivery may name a `priority` (POST /agents/{id}/inbox, integer 0..9).
-- Retrieve hands back the highest priority messages first, then FIFO by
-- delivery_sequence within a priority.
--
-- NOT NULL DEFAULT 0 is the whole migration contract: every row already in the
-- inbox reads back as priority 0, which is the ordering it has always had, so an
-- existing database behaves exactly as it did before this column existed. The
-- CHECK constraint holds the invariant the HTTP boundary enforces (`priority
-- must be 0..9`); the range is duplicated here on purpose — the API can be
-- bypassed by a direct write, and a row outside the documented range would be
-- an ordering no client could predict.
ALTER TABLE inbox_entries
    ADD COLUMN priority SMALLINT NOT NULL DEFAULT 0;

ALTER TABLE inbox_entries
    ADD CONSTRAINT inbox_entries_priority_range CHECK (priority >= 0 AND priority <= 9);

-- Required retrieve index: the claim query orders by (priority DESC,
-- delivery_sequence) inside one agent's unacked rows, so without this an agent
-- with a long backlog sorts its whole queue on every retrieve (the same class of
-- problem inbox_entries_fifo_idx solves for the FIFO order). Partial on
-- acked = FALSE, matching the claim predicate.
CREATE INDEX inbox_entries_priority_idx
    ON inbox_entries (agent_id, priority DESC, delivery_sequence)
    WHERE acked = FALSE;
