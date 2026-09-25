-- Task ownership (CR-FEAT-025): the two columns a durable backend needs to
-- report on a message whose owner never came back, plus the dead-letter
-- destination those messages land in.
--
-- `sender` records WHO delivered the message. It is not routing metadata — the
-- webhook envelope carries its own copy — it is the address a terminal outcome
-- is reported to: a TTL expiry has to write a MESSAGE_EXPIRED receipt into the
-- sender's inbox, and a purge that cannot name the sender cannot report
-- anything, which is exactly how an expired message became a mystery before
-- this. Nullable: a delivery that names no sender stores NULL, and an
-- unaddressed expiry produces no receipt (there is nowhere to send it).
--
-- `idempotency_key` records which sender-supplied key produced the message.
-- Deduplication itself happens at deliver time; the column is the provenance
-- that lets a dead letter state which key its message came from.
--
-- Both columns are additive and nullable, so an existing database gains two
-- NULL columns and every pre-existing row behaves exactly as before. Nothing
-- is backfilled: NULL means "not recorded", never a fabricated value.
ALTER TABLE inbox_entries
    ADD COLUMN sender TEXT NULL;

ALTER TABLE inbox_entries
    ADD COLUMN idempotency_key TEXT NULL;

-- The dead-letter destination: messages the expiry sweep removed while they
-- were still unacknowledged, preserved with the payload the sender delivered
-- and the correlation ids of the delivery.
--
-- Deliberately NOT foreign-keyed to agents: this table exists for the case
-- where the receiving side stopped consuming, so unregistering (or deleting) an
-- agent must not take the record of its undelivered work with it.
--
-- `message_id` is the primary key, which is what makes a dead letter
-- exactly-once: the same message cannot be recorded twice, so it cannot produce
-- a second expiry receipt either.
CREATE TABLE dead_letters (
    message_id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    sender TEXT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    dead_lettered_at TIMESTAMPTZ NOT NULL,
    reason TEXT NOT NULL,
    idempotency_key TEXT NULL,

    CONSTRAINT dead_letters_message_id_not_blank CHECK (length(btrim(message_id)) > 0),
    CONSTRAINT dead_letters_agent_id_not_blank CHECK (length(btrim(agent_id)) > 0),
    CONSTRAINT dead_letters_reason_not_blank CHECK (length(btrim(reason)) > 0)
);

-- Read path: the newest records of one agent's inbox, which is the order the
-- dead-letter endpoint answers in.
CREATE INDEX dead_letters_agent_idx
    ON dead_letters (agent_id, dead_lettered_at DESC);

-- Retention path: the sweep deletes the records it produced once they are
-- older than the documented retention window.
CREATE INDEX dead_letters_dead_lettered_at_idx
    ON dead_letters (dead_lettered_at);
