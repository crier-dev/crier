-- CR-FEAT-036: persist the optional selection hints of a lane dispatch, and the
-- Crier correlation a message answers.
--
-- `selection` carries the dispatch's optional `model` and `provider` hints as
-- one JSONB object (`{"model":…,"provider":…}`). It is stored as a whole object
-- rather than two TEXT columns. Absent and JSON null inputs both decode to
-- omitted members; explicit and blank strings remain unchanged. JSONB retains
-- the distinction between an omitted member and a present blank string.
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
