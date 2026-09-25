-- Realms (CR-FEAT-029): the namespace an agent is registered in, the namespace
-- a message was delivered into, and the namespace a dead letter died in.
--
-- NULL is the DEFAULT namespace — the single implicit realm every deployment
-- had before namespaces existed. That is deliberate and load-bearing: every
-- pre-existing row is NULL, so it reads back as "" (the canonical spelling of
-- the default realm, internal/namespace.Canonical) and behaves exactly as it
-- did before this migration. An existing database therefore gains three NULL
-- columns and changes no answer it gave before.
--
-- Nothing is backfilled: NULL means "the default realm", never a fabricated
-- realm name. A non-NULL value is a namespace the operator declared in
-- CR_NAMESPACES / CR_NAMESPACES_FILE — registration refuses a name the server
-- does not serve, so a stored value is always one this deployment can resolve.
--
-- No index is added. The only per-namespace read is GET /namespaces, which
-- counts agents from the rows it already lists; the delivery path resolves a
-- namespace from the TARGET row it is already fetching by primary key. An index
-- nothing reads would be dead weight on every write.

ALTER TABLE agents
    ADD COLUMN namespace TEXT NULL,
    ADD CONSTRAINT agents_namespace_not_blank
        CHECK (namespace IS NULL OR length(btrim(namespace)) > 0);

ALTER TABLE inbox_entries
    ADD COLUMN namespace TEXT NULL,
    ADD CONSTRAINT inbox_entries_namespace_not_blank
        CHECK (namespace IS NULL OR length(btrim(namespace)) > 0);

ALTER TABLE dead_letters
    ADD COLUMN namespace TEXT NULL,
    ADD CONSTRAINT dead_letters_namespace_not_blank
        CHECK (namespace IS NULL OR length(btrim(namespace)) > 0);
