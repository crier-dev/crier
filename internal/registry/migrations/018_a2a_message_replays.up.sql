-- DF-CRIER-370: registry-owned, bounded A2A send reservations and receipts.
-- No agent FK: unregister/re-register must not reopen a replay window early.
-- Nanosecond integer deadline preserves the same retention boundary on SQLite.
CREATE TABLE a2a_message_replays (
    agent_id TEXT NOT NULL,
    message_key TEXT NOT NULL,
    token TEXT NOT NULL,
    expires_at BIGINT NOT NULL,
    record BYTEA,
    PRIMARY KEY (agent_id, message_key)
);
CREATE INDEX a2a_message_replays_expiry_idx ON a2a_message_replays(expires_at);
