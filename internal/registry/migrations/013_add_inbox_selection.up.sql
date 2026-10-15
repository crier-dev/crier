-- CR-FEAT-036: persist the optional selection hints of a lane dispatch, and the
-- Crier correlation a message answers.
--
-- `selection` carries the dispatch's optional `model` and `provider` hints as
-- one JSONB object (`{"model":…,"provider":…}`). It is stored as a whole object
-- rather than two TEXT columns because TWO of the contract's distinctions are
-- lost by text columns: an ABSENT hint and a JSON `null` hint are different
-- inputs (the router may distinguish "the caller said nothing" from "the caller
-- explicitly said the default"), and SQL NULL cannot hold both. The object also
-- keeps the "absent member" spelling, which the entry's `omitempty` encoding
-- already uses on the wire.
--
-- The column is NULLABLE with no default: a delivery that names no hint stores
-- SQL NULL — "not sent" has exactly one representation — and reads back exactly
-- as it was before this column existed.
ALTER TABLE inbox_entries
    ADD COLUMN selection JSONB NULL;

-- `reply_to` records the Crier message id (or correlation id) a message
-- answers. It is provenance, the same class as `sender` and `thread_id`, and it
-- is never routing or authorization: the reply is a separate delivery, and
-- whether it is allowed is decided by the verified sender identity and the
-- delivery ACL. NULLABLE: a delivery that answers nothing stores NULL, so
-- "no correlation" has one representation.
ALTER TABLE inbox_entries
    ADD COLUMN reply_to TEXT NULL;
