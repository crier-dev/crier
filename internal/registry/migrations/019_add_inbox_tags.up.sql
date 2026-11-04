-- COURIER-TAGS §3 (CRIER-TAGS-1): persist the message TAGS of a delivery.
--
-- A tag classifies a message; it never routes and never grants
-- (specs/COURIER-TAGS.md §1). The tags ride the ROUTE RECORD — the same
-- classification the session transcript records — so that
-- `GET /agents/{id}/inbox?tag=build` can answer for a QUEUED message without
-- reading the session it came from, which may live in another realm or may
-- already be gone.
--
-- JSONB and NULLABLE with no default: a pre-existing row reads back as NULL
-- ("never tagged"), never an invented empty list, exactly the posture the
-- location (010) and selection (013) columns took. The shape is the API's
-- contract (a JSON array of tag strings in `InboxEntry.Tags`), not something
-- this table enforces — the same trust the `capabilities` column places in its
-- writer.
ALTER TABLE inbox_entries
    ADD COLUMN tags JSONB NULL;
