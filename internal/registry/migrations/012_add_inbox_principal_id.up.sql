-- CR-CHAT-007: persist the HUMAN principal a delivery was made by on the
-- stored inbox record.
--
-- A delivery may carry `principal_id` (POST /agents/{id}/inbox,
-- specs/CHAT-PERMISSIONS.md §6.3). Before this column it was a WIRE tag only:
-- the human who spoke as an agent was lost the moment the delivery was stored,
-- which is exactly the impersonation hole T3 ("impersonation with no audit").
-- The column records the principal WITH the message, resolved through its live
-- binding by the delivery ACL before anything is stored — the field is
-- provenance, never routing (the sender stays the agent; the bus never sees a
-- principal as an identity, §2.1).
--
-- NULLABLE with no default: a pre-existing row, and any delivery that names no
-- principal (anonymous or agent-initiated), reads back as NULL rather than an
-- invented value, so "no human" has exactly one representation. The entry's
-- omitempty encoding renders NULL as an absent key, which keeps every
-- pre-CR-CHAT-007 message byte-identical on the wire.
ALTER TABLE inbox_entries
    ADD COLUMN principal_id TEXT NULL;

ALTER TABLE dead_letters
    ADD COLUMN principal_id TEXT NULL;

