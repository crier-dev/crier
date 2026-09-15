-- Optional per-agent configs (DF-CRIER-151): the push-delivery webhook
-- endpoint (CR-FEAT-001) and the LLM message-guard policy (CR-FEAT-010,
-- spec §4.1). Both are nullable — NULL means "not configured". A PATCH that
-- omits or nulls `webhook` clears the column (spec §7), so an explicit NULL
-- is meaningful on write and must round-trip as an absent config on read.
ALTER TABLE agents
    ADD COLUMN webhook JSONB NULL,
    ADD COLUMN guard JSONB NULL;
