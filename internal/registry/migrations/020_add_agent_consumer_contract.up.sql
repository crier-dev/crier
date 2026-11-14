-- INBOX-CRIER-001: the inbox CONSUMER CONTRACT on the registry entry.
--
-- A delivery being accepted is not evidence that anybody reads it. This column
-- records, per agent, who reads the inbox (`reader`: agent | harness), the
-- cadence the consumer expects (`read_cadence_secs`) and the maximum time an
-- unacknowledged message may sit (`ttl_seconds`). The expiry sweep enforces the
-- TTL by promoting overdue entries into the existing dead_letters destination
-- and writing one MESSAGE_EXPIRED receipt to the sender.
--
-- JSONB and NULLABLE with no default, exactly the posture migration 003 took
-- for webhook/guard: NULL means "no contract declared", which resolves to the
-- deployment default (CR_INBOX_CONTRACT_TTL_S, 24h) and keeps every
-- pre-INBOX-CRIER-001 registration byte-identical on the wire and behaviourally
-- unchanged (24h is the documented DefaultMessageTTL).
ALTER TABLE agents
    ADD COLUMN consumer JSONB NULL;
ALTER TABLE agents
    ADD COLUMN archived_reason TEXT NULL;
