-- Optional per-agent A2A opt-in (INT-A2A-001, specs/A2A-OPTION.md §4.2). The
-- `a2a` block on a registry row is the per-agent half of the option's gate;
-- the server-side half is CR_A2A_ENABLED (default false). The column is
-- nullable — NULL means "this agent did not opt in", which is every row that
-- existed before A2A and every registration that omits the block. An explicit
-- JSON null is therefore meaningful on write (PATCH clears the block) and
-- round-trips as an absent block on read, the same wire shape the in-memory
-- backend serves.
--
-- Additive only: no existing column is touched, so an existing database gains
-- a NULL column and every pre-existing row keeps behaving exactly as before.
ALTER TABLE agents
    ADD COLUMN a2a JSONB NULL;
