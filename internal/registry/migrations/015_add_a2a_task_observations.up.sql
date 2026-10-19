-- Adapter observations share the inbox database, not the lifetime of an SSE request.
-- No FK to inbox_entries: Ack/CloseEntry intentionally delete those rows.
-- No automatic expiry: evidence remains until explicit database removal.
CREATE TABLE a2a_task_observations (
    agent_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('acknowledged', 'canceled', 'unknown')),
    evidence JSONB NOT NULL,
    PRIMARY KEY (agent_id, task_id)
);
