-- A2A output evidence is independent of immutable lifecycle observations.
-- No inbox FK: an answer may arrive after Ack deleted the request entry.
CREATE TABLE a2a_task_outputs (
    agent_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    artifact JSONB NOT NULL,
    PRIMARY KEY (agent_id, task_id)
);
