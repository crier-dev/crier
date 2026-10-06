-- CR-CHAT-019: persist the message's thread on the stored inbox record.
--
-- A delivery may name a `thread_id` (POST /agents/{id}/inbox, CR-FEAT-004). It
-- was a WIRE tag only: `InboxEntry` had no such field, so a thread existed in
-- the request and nowhere else, and a thread tree could not be reconstructed
-- from storage at all (specs/CHAT-INTERFACE.md §4 row 15). The column records it
-- WITH the message, which is what makes the thread reconstructable from the
-- inbox alone.
--
-- NULLABLE with no default: a pre-existing row, and any delivery that names no
-- thread, reads back as NULL rather than an invented value, so "not recorded"
-- has exactly one representation. A thread root's thread_id is its own message
-- id and a reply carries the thread it answers (specs/CHAT-SESSIONS.md §4.3,
-- D11), so the column is opaque text — the shape is the API's contract, not a
-- foreign-key check this table cannot make (a thread's root may live in another
-- agent's inbox).
ALTER TABLE inbox_entries
    ADD COLUMN thread_id TEXT NULL;
